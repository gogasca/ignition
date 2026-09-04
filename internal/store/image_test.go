package store_test

import (
	"context"
	"errors"
	"testing"

	"ignition.dev/ignition/internal/store"
)

func TestMemoryCreateImageRoundTrip(t *testing.T) {
	m := store.NewMemory()
	ctx := context.Background()
	img, err := m.CreateImage(ctx, store.CreateImageInput{
		ProjectID: "prj_dev", ImageID: "img_nginx",
		SourceRef: "docker.io/library/nginx:1.27", Digest: "sha256:abc",
		RegistryRef:       "docker.io/library/nginx@sha256:abc",
		Entrypoint:        []string{"/docker-entrypoint.sh"},
		Cmd:               []string{"nginx", "-g", "daemon off;"},
		StreamingEligible: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if img.State != "READY" || img.RegistryRef != "docker.io/library/nginx@sha256:abc" {
		t.Fatalf("image = %+v", img)
	}
	if !img.StreamingEligible {
		t.Fatal("streamingEligible not persisted")
	}
	got, err := m.GetImage(ctx, "prj_dev", "img_nginx")
	if err != nil {
		t.Fatal(err)
	}
	if got.Digest != "sha256:abc" || len(got.Entrypoint) != 1 || got.Entrypoint[0] != "/docker-entrypoint.sh" {
		t.Fatalf("got = %+v", got)
	}
}

func TestMemoryCreateImagePersistsIneligibleReason(t *testing.T) {
	m := store.NewMemory()
	ctx := context.Background()
	_, err := m.CreateImage(ctx, store.CreateImageInput{
		ProjectID: "prj_dev", ImageID: "img_legacy", SourceRef: "legacy:v1",
		Digest: "sha256:x", RegistryRef: "legacy@sha256:x",
		StreamingEligible: false, IneligibleReason: "schema version 1 manifest is not eligible for GKE image streaming",
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := m.GetImage(ctx, "prj_dev", "img_legacy")
	if err != nil {
		t.Fatal(err)
	}
	if got.StreamingEligible || got.IneligibleReason == "" {
		t.Fatalf("got = %+v", got)
	}
}

func TestMemoryCreateImageRejectsDuplicateImageID(t *testing.T) {
	m := store.NewMemory()
	ctx := context.Background()
	in := store.CreateImageInput{ProjectID: "prj_dev", ImageID: "img_dup", SourceRef: "nginx:1.27", Digest: "sha256:a", RegistryRef: "nginx@sha256:a"}
	if _, err := m.CreateImage(ctx, in); err != nil {
		t.Fatal(err)
	}
	in.SourceRef, in.Digest, in.RegistryRef = "nginx:1.28", "sha256:b", "nginx@sha256:b"
	if _, err := m.CreateImage(ctx, in); !errors.Is(err, store.ErrImageAlreadyExists) {
		t.Fatalf("err = %v, want ErrImageAlreadyExists", err)
	}
	// The original digest must not have been overwritten by the rejected call.
	got, err := m.GetImage(ctx, "prj_dev", "img_dup")
	if err != nil {
		t.Fatal(err)
	}
	if got.Digest != "sha256:a" {
		t.Fatalf("digest = %q, want unchanged sha256:a", got.Digest)
	}
}

func TestMemoryGetImageNotFound(t *testing.T) {
	m := store.NewMemory()
	if _, err := m.GetImage(context.Background(), "prj_dev", "missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestMemorySeedImageIsCatalogCompatible(t *testing.T) {
	m := store.NewMemory()
	m.SeedImage("prj_dev", "img_seed")
	got, err := m.GetImage(context.Background(), "prj_dev", "img_seed")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "READY" {
		t.Fatalf("state = %q", got.State)
	}
	if got.RegistryRef != "" {
		t.Fatalf("seeded image must have no pinned RegistryRef, got %q", got.RegistryRef)
	}
}

func TestValidImageID(t *testing.T) {
	ok := []string{"img_seed", "img", "a", "A1._-z"}
	for _, id := range ok {
		if err := store.CheckImageID(id); err != nil {
			t.Fatalf("%q: %v", id, err)
		}
	}
	bad := []string{"", "/etc/passwd", "a/b", "img@sha256:x", "reg:tag", "../x", " "}
	for _, id := range bad {
		if store.CheckImageID(id) == nil {
			t.Fatalf("%q accepted", id)
		}
	}
}

func TestValidSecretVersion(t *testing.T) {
	ok := []string{"", "latest", "1", "42"}
	for _, v := range ok {
		if !store.ValidSecretVersion(v) {
			t.Fatalf("%q rejected", v)
		}
	}
	bad := []string{"0", "01", "-1", "latest ", "1;drop", "latest/../x"}
	for _, v := range bad {
		if store.ValidSecretVersion(v) {
			t.Fatalf("%q accepted", v)
		}
	}
}
