package imagecatalog

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/google"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// DefaultResolveTimeout bounds a single Resolve when RemoteResolver.Timeout is
// unset. It is independent of the client request's context so a slow or
// black-holed registry cannot pin a request handler open.
const DefaultResolveTimeout = 30 * time.Second

// ErrSourceNotAllowed means the source reference names a registry host that is
// not permitted (allowlist) or an address the resolver refuses to dial
// (loopback / private / link-local / metadata). ErrResolveFailed means the
// registry read itself failed. The API surfaces a generic message for both and
// never echoes the underlying registry error to the client (it is logged
// server-side) — that raw error was a network topology oracle.
var (
	ErrSourceNotAllowed = errors.New("image source registry is not permitted")
	ErrResolveFailed    = errors.New("could not resolve image source reference")
)

// platformKeychain authenticates registry reads with the platform's ambient
// credentials: the GCP metadata / Application Default Credentials chain first
// (so a Workload Identity ignition-api can read a private Artifact Registry
// repository it is authorized for), falling back to a local Docker config.
// It never receives or forwards a tenant credential.
var platformKeychain = authn.NewMultiKeychain(google.Keychain, authn.DefaultKeychain)

// RemoteResolver resolves references against their source OCI registry.
// It authenticates with ambient credentials only — it never receives or
// forwards a tenant credential, and tenant code never sees this resolver.
type RemoteResolver struct {
	// Allowlist, when non-empty, is the set of registry hosts this resolver
	// will contact (exact, case-insensitive host match). Empty means no host
	// restriction — the non-public-address guard still applies unconditionally.
	Allowlist []string
	// Timeout bounds one Resolve independent of the caller's context. Zero
	// uses DefaultResolveTimeout.
	Timeout time.Duration
}

func (rr RemoteResolver) resolveTimeout() time.Duration {
	if rr.Timeout > 0 {
		return rr.Timeout
	}
	return DefaultResolveTimeout
}

func (rr RemoteResolver) Resolve(ctx context.Context, ref string) (Resolved, error) {
	r, err := name.ParseReference(ref)
	if err != nil {
		return Resolved{}, fmt.Errorf("%w: invalid image reference", ErrResolveFailed)
	}
	if err := checkRegistryHost(r.Context().RegistryStr(), rr.Allowlist); err != nil {
		return Resolved{}, err // already an ErrSourceNotAllowed
	}

	ctx, cancel := context.WithTimeout(ctx, rr.resolveTimeout())
	defer cancel()

	opts := []remote.Option{
		remote.WithContext(ctx),
		remote.WithAuthFromKeychain(platformKeychain),
		remote.WithTransport(guardedTransport()),
	}

	desc, err := remote.Get(r, opts...)
	if err != nil {
		return Resolved{}, resolveErr(err)
	}
	img, err := desc.Image()
	if err != nil {
		return Resolved{}, fmt.Errorf("%w: not a single-platform OCI image manifest (an index needs an explicit platform selection, not yet supported)", ErrResolveFailed)
	}
	digest, err := img.Digest()
	if err != nil {
		return Resolved{}, resolveErr(err)
	}
	cfg, err := img.ConfigFile()
	if err != nil {
		return Resolved{}, resolveErr(err)
	}
	eligible, reason, totalSize, err := streamingEligibility(desc.MediaType, img)
	if err != nil {
		return Resolved{}, resolveErr(err)
	}
	return Resolved{
		Digest:            digest.String(),
		RegistryRef:       r.Context().Digest(digest.String()).Name(),
		Entrypoint:        cfg.Config.Entrypoint,
		Cmd:               cfg.Config.Cmd,
		StreamingEligible: eligible,
		IneligibleReason:  reason,
		CompressedBytes:   totalSize,
	}, nil
}

// resolveErr keeps the underlying registry error attached (for the server log)
// while tagging it ErrResolveFailed so the API layer can recognise it and
// return a generic message instead of the raw text. A dial the guard refused
// surfaces here too; re-tag it as ErrSourceNotAllowed so the client sees the
// policy rejection, not a generic failure.
func resolveErr(err error) error {
	s := err.Error()
	if strings.Contains(s, dialRefusedMarker) || strings.Contains(s, dialNoIPMarker) {
		return fmt.Errorf("%w: %v", ErrSourceNotAllowed, err)
	}
	return fmt.Errorf("%w: %v", ErrResolveFailed, err)
}

// streamingEligibility is the static check from documented GKE image
// streaming requirements: "Container images that use the V2 Image Manifest,
// schema version 1 are not eligible" and "Images with duplicate or empty
// layers aren't supported; GKE downloads these without streaming." It does
// not (and cannot, without a running cluster) confirm streaming actually
// happens — see Resolved.StreamingEligible. It also sums compressed layer
// size in the same walk, for Resolved.CompressedBytes.
func streamingEligibility(mediaType types.MediaType, img v1.Image) (eligible bool, reason string, totalSize int64, err error) {
	layers, err := img.Layers()
	if err != nil {
		return false, "", 0, err
	}
	seen := map[v1.Hash]bool{}
	eligible, reason = true, ""
	if mediaType.IsSchema1() {
		eligible, reason = false, "schema version 1 manifest is not eligible for GKE image streaming"
	}
	for _, l := range layers {
		digest, err := l.Digest()
		if err != nil {
			return false, "", 0, err
		}
		size, err := l.Size()
		if err != nil {
			return false, "", 0, err
		}
		totalSize += size
		if !eligible {
			continue
		}
		if size == 0 {
			eligible, reason = false, "image has an empty layer, which GKE downloads without streaming"
		} else if seen[digest] {
			eligible, reason = false, "image has a duplicate layer, which GKE downloads without streaming"
		}
		seen[digest] = true
	}
	return eligible, reason, totalSize, nil
}
