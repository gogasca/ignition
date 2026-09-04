package imagecatalog

import (
	"context"
	"fmt"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
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
	return Resolved{
		Digest:      digest.String(),
		RegistryRef: r.Context().Digest(digest.String()).Name(),
		Entrypoint:  cfg.Config.Entrypoint,
		Cmd:         cfg.Config.Cmd,
	}, nil
}
