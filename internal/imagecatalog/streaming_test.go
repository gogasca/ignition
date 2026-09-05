package imagecatalog

import (
	"testing"

	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

func TestStreamingEligibilityRejectsSchema1(t *testing.T) {
	ok, reason, _, err := streamingEligibility(types.DockerManifestSchema1, empty.Image)
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
	ok, reason, _, err := streamingEligibility(types.OCIManifestSchema1, img)
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
	ok, reason, _, err := streamingEligibility(types.OCIManifestSchema1, img)
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
	ok, reason, _, err := streamingEligibility(types.OCIManifestSchema1, img)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || reason != "" {
		t.Fatalf("ok=%v reason=%q, want eligible with no reason", ok, reason)
	}
}

func TestStreamingEligibilitySumsCompressedSize(t *testing.T) {
	img, err := mutate.AppendLayers(empty.Image,
		static.NewLayer([]byte("12345"), types.OCILayer),
		static.NewLayer([]byte("1234567890"), types.OCILayer),
	)
	if err != nil {
		t.Fatal(err)
	}
	_, _, total, err := streamingEligibility(types.OCIManifestSchema1, img)
	if err != nil {
		t.Fatal(err)
	}
	if total != 15 {
		t.Fatalf("total = %d, want 15", total)
	}
}

// Size must still be summed (for CompressedBytes) even when the image is
// ineligible, since an ineligible image is exactly the case that needs a
// deadline estimate.
func TestStreamingEligibilitySumsSizeEvenWhenIneligible(t *testing.T) {
	img, err := mutate.AppendLayers(empty.Image, static.NewLayer([]byte("12345"), types.OCILayer))
	if err != nil {
		t.Fatal(err)
	}
	ok, _, total, err := streamingEligibility(types.DockerManifestSchema1, img)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("schema1 must be ineligible")
	}
	if total != 5 {
		t.Fatalf("total = %d, want 5", total)
	}
}
