package imagecatalog

import (
	"context"
	"fmt"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// RemoteResolver resolves references against their source OCI registry.
// It authenticates with ambient credentials only (the local Docker config or
// the environment's default keychain, e.g. GCP Application Default
// Credentials for a private Artifact Registry repository the platform
// itself is authorized to read) — it never receives or forwards a tenant
// credential, and tenant code never sees this resolver.
type RemoteResolver struct{}

func (RemoteResolver) Resolve(ctx context.Context, ref string) (Resolved, error) {
	r, err := name.ParseReference(ref)
	if err != nil {
		return Resolved{}, fmt.Errorf("invalid image reference %q: %w", ref, err)
	}
	desc, err := remote.Get(r, remote.WithContext(ctx), remote.WithAuthFromKeychain(authn.DefaultKeychain))
	if err != nil {
		return Resolved{}, fmt.Errorf("resolve %s: %w", ref, err)
	}
	img, err := desc.Image()
	if err != nil {
		return Resolved{}, fmt.Errorf("resolve %s: not a single-platform OCI image manifest (an index needs an explicit platform selection, not yet supported): %w", ref, err)
	}
	digest, err := img.Digest()
	if err != nil {
		return Resolved{}, fmt.Errorf("resolve %s: digest: %w", ref, err)
	}
	cfg, err := img.ConfigFile()
	if err != nil {
		return Resolved{}, fmt.Errorf("resolve %s: config: %w", ref, err)
	}
	eligible, reason, err := streamingEligibility(desc.MediaType, img)
	if err != nil {
		return Resolved{}, fmt.Errorf("resolve %s: layers: %w", ref, err)
	}
	return Resolved{
		Digest:            digest.String(),
		RegistryRef:       r.Context().Digest(digest.String()).Name(),
		Entrypoint:        cfg.Config.Entrypoint,
		Cmd:               cfg.Config.Cmd,
		StreamingEligible: eligible,
		IneligibleReason:  reason,
	}, nil
}

// streamingEligibility is the static check from documented GKE image
// streaming requirements: "Container images that use the V2 Image Manifest,
// schema version 1 are not eligible" and "Images with duplicate or empty
// layers aren't supported; GKE downloads these without streaming." It does
// not (and cannot, without a running cluster) confirm streaming actually
// happens — see Resolved.StreamingEligible.
func streamingEligibility(mediaType types.MediaType, img v1.Image) (eligible bool, reason string, err error) {
	if mediaType.IsSchema1() {
		return false, "schema version 1 manifest is not eligible for GKE image streaming", nil
	}
	layers, err := img.Layers()
	if err != nil {
		return false, "", err
	}
	seen := map[v1.Hash]bool{}
	for _, l := range layers {
		digest, err := l.Digest()
		if err != nil {
			return false, "", err
		}
		size, err := l.Size()
		if err != nil {
			return false, "", err
		}
		if size == 0 {
			return false, "image has an empty layer, which GKE downloads without streaming", nil
		}
		if seen[digest] {
			return false, "image has a duplicate layer, which GKE downloads without streaming", nil
		}
		seen[digest] = true
	}
	return true, "", nil
}
