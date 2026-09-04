package imagecatalog

import (
	"testing"

	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

func TestStreamingEligibilityRejectsSchema1(t *testing.T) {
	ok, reason, err := streamingEligibility(types.DockerManifestSchema1, empty.Image)
	if err != nil {
		t.Fatal(err)
	}
	if ok || reason == "" {
		t.Fatalf("ok=%v reason=%q, want ineligible with a reason", ok, reason)
	}
}

func TestStreamingEligibilityRejectsEmptyLayer(t *testing.T) {
	img, err := mutate.AppendLayers(empty.Image, static.NewLayer([]byte("content"), types.OCILayer), static.NewLayer(nil, types.OCILayer))
	if err != nil {
		t.Fatal(err)
	}
	ok, reason, err := streamingEligibility(types.OCIManifestSchema1, img)
	if err != nil {
		t.Fatal(err)
	}
	if ok || reason == "" {
		t.Fatalf("ok=%v reason=%q, want ineligible (empty layer)", ok, reason)
	}
}

func TestStreamingEligibilityRejectsDuplicateLayer(t *testing.T) {
	layer := static.NewLayer([]byte("same content"), types.OCILayer)
	img, err := mutate.AppendLayers(empty.Image, layer, layer)
	if err != nil {
		t.Fatal(err)
	}
	ok, reason, err := streamingEligibility(types.OCIManifestSchema1, img)
	if err != nil {
		t.Fatal(err)
	}
	if ok || reason == "" {
		t.Fatalf("ok=%v reason=%q, want ineligible (duplicate layer)", ok, reason)
	}
}

func TestStreamingEligibilityAcceptsOrdinaryImage(t *testing.T) {
	img, err := mutate.AppendLayers(empty.Image,
		static.NewLayer([]byte("layer one"), types.OCILayer),
		static.NewLayer([]byte("layer two"), types.OCILayer),
	)
	if err != nil {
		t.Fatal(err)
	}
	ok, reason, err := streamingEligibility(types.OCIManifestSchema1, img)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || reason != "" {
		t.Fatalf("ok=%v reason=%q, want eligible with no reason", ok, reason)
	}
}
