// Package imagecatalog resolves a client-supplied container image reference
// to immutable, verifiable delivery metadata for admission.
//
// v0 scope: pin by digest and read directly from the source registry. It does
// not copy manifests or blobs into an Ignition-owned same-region registry,
// does not verify signatures or provenance, and does not run a security scan
// — all of that is normative in the image data layer design and remains a
// gap versus it. See docs/design/ignition-design-image-datalayer.md.
package imagecatalog

import "context"

// Resolved is what admission needs from a source reference.
type Resolved struct {
	// Digest is the resolved image's content digest (e.g. "sha256:...").
	Digest string
	// RegistryRef is the immutable, digest-pinned reference the controller
	// schedules — the source registry path with the tag replaced by Digest.
	// Never a mutable tag.
	RegistryRef string
	// Entrypoint and Cmd are the image's own OCI process configuration,
	// recorded so a client can tell whether CreateSandbox.nativeEntrypoint is
	// required for this image before ever creating a sandbox.
	Entrypoint []string
	Cmd        []string
	// StreamingEligible/IneligibleReason are the static, documented GKE image
	// streaming eligibility check from admission step 7 in
	// docs/design/ignition-design-images-startup.md — schema v1 manifests and
	// duplicate/empty layers are known-ineligible. This is a static check
	// only: whether a launch actually streamed is observed separately at
	// launch time (not yet implemented — GKE exposes no admission-time mount
	// API, so real eligibility can only be confirmed by watching a launch).
	StreamingEligible bool
	IneligibleReason  string
}

// Resolver resolves a source image reference for admission. Implementations
// must not accept or forward tenant credentials.
type Resolver interface {
	Resolve(ctx context.Context, ref string) (Resolved, error)
}
