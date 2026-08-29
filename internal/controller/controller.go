package controller

import (
	"context"
	"log"
	"os"
	"time"

	"ignition.dev/ignition/internal/config"
	"ignition.dev/ignition/internal/k8s"
	"ignition.dev/ignition/internal/secrets"
	"ignition.dev/ignition/internal/store"
)

const defaultBalloonCooldown = 15 * time.Minute

// Options configure a Controller.
type Options struct {
	HolderID        string
	LeaseTTL        time.Duration
	MinWarm         int
	MaxWarm         int
	Now             func() time.Time
	ResolveImage    func(imageID string) string
	ImagePrefix     string
	GCPProject      string
	Region          string
	Secrets         secrets.Resolver
	Policies        k8s.Policies
	BalloonCooldown time.Duration
}

// Controller is the only process allowed to mutate sandbox Pods.
type Controller struct {
	store              store.ControllerStore
	pods               k8s.Pods
	nodes              k8s.Nodes
	opts               Options
	balloonExcessSince time.Time
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
	if opts.Policies == nil {
		if p, ok := pods.(k8s.Policies); ok {
			opts.Policies = p
		}
	}
	return &Controller{store: st, pods: pods, nodes: nodes, opts: opts}
}

// Reconcile is one level-triggered pass over product state vs cluster.
func (c *Controller) Reconcile(ctx context.Context) error {
	if c.opts.HolderID != "" {
		ok, err := c.store.HoldLease(ctx, c.opts.HolderID, c.opts.Now().UTC(), c.opts.LeaseTTL)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
	}
	sbs, err := c.store.ListSandboxesAll(ctx)
	if err != nil {
		return err
	}
	for _, sb := range sbs {
		if err := c.reconcileSandbox(ctx, sb); err != nil {
			log.Printf("controller: sandbox %s: %v", sb.ID, err)
		}
	}
	if err := c.pinSandboxNodes(); err != nil {
		log.Printf("controller: pin nodes: %v", err)
	}
	return c.reconcileBalloons(ctx, sbs)
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
	_, st, closer, err := store.OpenWithoutMigrate(context.Background(), cfg.DatabaseURL)
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
	c := New(st, cluster, cluster, Options{
		HolderID:        holder,
		MinWarm:         cfg.MinWarm,
		MaxWarm:         cfg.MaxWarm,
		ImagePrefix:     cfg.SandboxImagePrefix,
		GCPProject:      cfg.GCPProject,
		Region:          cfg.EnabledRegion,
		Secrets:         secretResolver,
		Policies:        cluster,
		BalloonCooldown: defaultBalloonCooldown,
		ResolveImage: func(imageID string) string {
			if !store.ValidImageID(imageID) {
				return ""
			}
			return config.ResolveSandboxImage(imageID, cfg.SandboxImagePrefix, cfg.EnabledRegion, cfg.GCPProject)
		},
	})
	return c.Loop(context.Background(), time.Second)
}
