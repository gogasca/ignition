package controller

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"ignition.dev/ignition/internal/adminz"
	"ignition.dev/ignition/internal/config"
	"ignition.dev/ignition/internal/k8s"
	"ignition.dev/ignition/internal/secrets"
	"ignition.dev/ignition/internal/store"
)

const defaultBalloonCooldown = 15 * time.Minute

// leaseRenewBatch bounds how many sandboxes reconcileOnce mutates between
// lease re-checks. ListSandboxesAll is now bounded by store.ReconcileWindow,
// but a pass can still legitimately run long under high active-sandbox
// counts; re-validating lease ownership mid-pass, rather than only once at
// the start, keeps a standby replica that has since taken over the lease
// from mutating Pods concurrently with the previous holder.
const leaseRenewBatch = 50

// Options configure a Controller.
type Options struct {
	HolderID string
	LeaseTTL time.Duration
	// MinWarm/MaxWarm bound the GPU (NVIDIA_L4) warm balloon buffer.
	MinWarm int
	MaxWarm int
	// MinWarmCPU/MaxWarmCPU bound the CPU (NONE) warm balloon buffer. Both
	// default to 0 (disabled): the CPU sandbox pool has historically scaled
	// to zero with no warm capacity, and enabling a buffer has a real node
	// cost, so it stays opt-in rather than silently changing existing
	// deployments' spend.
	MinWarmCPU      int
	MaxWarmCPU      int
	Now             func() time.Time
	ResolveImage    func(imageID string) string
	ImagePrefix     string
	GCPProject      string
	Region          string
	Secrets         secrets.Resolver
	BalloonCooldown time.Duration
	// Recorder and Metrics are optional observability sinks; nil in tests.
	Recorder *adminz.Recorder
	Metrics  *adminz.ReconcileMetrics
}

// Controller is the only process allowed to mutate sandbox Pods.
type Controller struct {
	store store.ControllerStore
	pods  k8s.Pods
	nodes k8s.Nodes
	opts  Options
	// balloonExcessSince is keyed by accelerator type: each warm class scales
	// and cools down independently.
	balloonExcessSince map[string]time.Time
	stats              statsHolder
}

func New(st store.ControllerStore, pods k8s.Pods, nodes k8s.Nodes, opts Options) *Controller {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.LeaseTTL <= 0 {
		opts.LeaseTTL = 10 * time.Second
	}
	if opts.ResolveImage == nil {
		prefix := opts.ImagePrefix
		if prefix == "" {
			prefix = "us-central1-docker.pkg.dev/ignition/sandboxes"
		}
		opts.ResolveImage = func(imageID string) string {
			if !store.ValidImageID(imageID) {
				return ""
			}
			return config.ResolveSandboxImage(imageID, prefix, opts.Region, opts.GCPProject)
		}
	}
	return &Controller{store: st, pods: pods, nodes: nodes, opts: opts}
}

// Reconcile is one level-triggered pass over product state vs cluster.
func (c *Controller) Reconcile(ctx context.Context) error {
	start := c.opts.Now()
	err, leaseHeld, byState := c.reconcileOnce(ctx)
	dur := c.opts.Now().Sub(start)
	c.stats.recordPass(dur, err, byState, leaseHeld)
	if c.opts.Metrics != nil {
		c.opts.Metrics.ObservePass(dur, err, byState, leaseHeld, c.stats.snapshot().ConsecutiveErrors)
	}
	return err
}

func (c *Controller) reconcileOnce(ctx context.Context) (error, bool, map[string]int) {
	leaseHeld := c.opts.HolderID == ""
	if c.opts.HolderID != "" {
		ok, err := c.store.HoldLease(ctx, c.opts.HolderID, c.opts.Now().UTC(), c.opts.LeaseTTL)
		if err != nil {
			return err, false, nil
		}
		if !ok {
			return nil, false, nil
		}
		leaseHeld = true
	}
	sbs, err := c.store.ListSandboxesAll(ctx)
	if err != nil {
		return err, leaseHeld, nil
	}
	byState := make(map[string]int, 7)
	for _, sb := range sbs {
		byState[sb.State]++
	}
	var reconcileErrs []error
	for i, sb := range sbs {
		if i > 0 && i%leaseRenewBatch == 0 && c.opts.HolderID != "" {
			ok, err := c.store.HoldLease(ctx, c.opts.HolderID, c.opts.Now().UTC(), c.opts.LeaseTTL)
			if err != nil {
				return err, false, byState
			}
			if !ok {
				// Lease moved to another holder mid-pass: stop mutating
				// Pods now rather than race the new holder on the rest of
				// this batch. The next tick picks up where this left off.
				return nil, false, byState
			}
		}
		if err := c.reconcileSandbox(ctx, sb); err != nil {
			log.Printf("controller: sandbox %s: %v", sb.ID, err)
			reconcileErrs = append(reconcileErrs, fmt.Errorf("sandbox %s: %w", sb.ID, err))
		}
	}
	if err := c.pinSandboxNodes(); err != nil {
		log.Printf("controller: pin nodes: %v", err)
		reconcileErrs = append(reconcileErrs, fmt.Errorf("pin nodes: %w", err))
	}
	if err := c.reconcileBalloons(ctx, sbs); err != nil {
		reconcileErrs = append(reconcileErrs, fmt.Errorf("reconcile balloons: %w", err))
	}
	return errors.Join(reconcileErrs...), leaseHeld, byState
}

// Loop ticks until ctx is cancelled.
func (c *Controller) Loop(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		interval = time.Second
	}
	if err := c.Reconcile(ctx); err != nil {
		log.Printf("controller: %v", err)
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			if err := c.Reconcile(ctx); err != nil {
				log.Printf("controller: %v", err)
			}
		}
	}
}

// Run starts the controller against GKE (in-cluster or KUBECONFIG).
// Product state is Cloud SQL when DATABASE_URL is set.
func Run(cfg config.Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	restCfg, err := k8s.RESTConfig(cfg.KubeconfigPath)
	if err != nil {
		return err
	}
	cluster, err := k8s.NewCluster(restCfg, cfg.K8sNamespace)
	if err != nil {
		return err
	}
	holder := os.Getenv("HOSTNAME")
	if holder == "" {
		holder = "controller-0"
	}
	_, st, closer, err := store.OpenWithoutSchema(context.Background(), cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer closer()
	backend := "memory"
	if cfg.DatabaseURL != "" {
		backend = "postgres"
	}
	log.Printf("ignition-controller: kubernetes namespace=%s holder=%s minWarm=%d store=%s", cfg.K8sNamespace, holder, cfg.MinWarm, backend)
	var secretResolver secrets.Resolver
	if cfg.GCPProject != "" {
		secretResolver = &secrets.GCP{Project: cfg.GCPProject}
	}

	reg := prometheus.NewRegistry()
	rec := adminz.NewRecorder(200)
	c := New(st, cluster, cluster, Options{
		HolderID:        holder,
		MinWarm:         cfg.MinWarm,
		MaxWarm:         cfg.MaxWarm,
		MinWarmCPU:      cfg.MinWarmCPU,
		MaxWarmCPU:      cfg.MaxWarmCPU,
		ImagePrefix:     cfg.SandboxImagePrefix,
		GCPProject:      cfg.GCPProject,
		Region:          cfg.EnabledRegion,
		Secrets:         secretResolver,
		BalloonCooldown: defaultBalloonCooldown,
		Recorder:        rec,
		Metrics:         adminz.NewReconcileMetrics(reg, rec),
		ResolveImage: func(imageID string) string {
			if !store.ValidImageID(imageID) {
				return ""
			}
			return config.ResolveSandboxImage(imageID, cfg.SandboxImagePrefix, cfg.EnabledRegion, cfg.GCPProject)
		},
	})

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	admin := adminz.New(adminz.Options{
		Name:     "ignition-controller",
		Registry: reg,
		Recorder: rec,
		HealthFn: func(context.Context) error {
			s := c.Stats()
			switch {
			case s.Passes == 0:
				return nil // still starting up
			case time.Since(s.LastPass) > 60*time.Second:
				return fmt.Errorf("last reconcile %s ago", time.Since(s.LastPass).Round(time.Second))
			case s.ConsecutiveErrors >= 5:
				return fmt.Errorf("%d consecutive reconcile errors", s.ConsecutiveErrors)
			default:
				return nil
			}
		},
		Status: c.StatusSections,
	})
	go func() {
		log.Printf("ignition-controller: admin listening on %s", cfg.AdminAddr)
		if err := adminz.Serve(ctx, cfg.AdminAddr, admin.Handler()); err != nil {
			log.Printf("ignition-controller: admin: %v", err)
		}
	}()

	if err := c.Loop(ctx, time.Second); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}
