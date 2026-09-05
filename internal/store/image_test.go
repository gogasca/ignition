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
		CompressedBytes:   12345,
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
	if img.CompressedBytes != 12345 {
		t.Fatalf("compressedBytes = %d, want 12345", img.CompressedBytes)
	}
	if img.LaunchCount != 0 {
		t.Fatalf("launchCount = %d, want 0 for a never-launched image", img.LaunchCount)
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

func TestMemoryLaunchCountIncrementsOnlyOnSuccessfulCreateSandbox(t *testing.T) {
	m := store.NewMemory()
	ctx := context.Background()
	m.SeedImage("prj_dev", "img_a")
	in := func(key string) store.CreateSandboxInput {
		return store.CreateSandboxInput{
			ProjectID: "prj_dev", Principal: "alice", IdemKey: key, IdemHash: key,
			ImageID: "img_a", Resources: spec(), MaxActive: 10,
		}
	}
	if _, err := m.CreateSandbox(ctx, in("k1")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.CreateSandbox(ctx, in("k2")); err != nil {
		t.Fatal(err)
	}
	// A failed create (unknown image) must not increment any counter.
	failing := in("k3")
	failing.ImageID = "img_missing"
	if _, err := m.CreateSandbox(ctx, failing); !errors.Is(err, store.ErrImageNotReady) {
		t.Fatalf("err = %v, want ErrImageNotReady", err)
	}
	got, err := m.GetImage(ctx, "prj_dev", "img_a")
	if err != nil {
		t.Fatal(err)
	}
	if got.LaunchCount != 2 {
		t.Fatalf("launchCount = %d, want 2", got.LaunchCount)
	}
}

func TestMemoryTopImagesByLaunchCount(t *testing.T) {
	m := store.NewMemory()
	ctx := context.Background()
	m.SeedImage("prj_dev", "img_low")
	m.SeedImage("prj_dev", "img_high")
	m.SeedImage("prj_dev", "img_mid")
	m.SeedImage("prj_other", "img_other") // a different project must not appear
	launch := func(projectID, imageID string, n int) {
		for i := 0; i < n; i++ {
			key := projectID + "-" + imageID + "-" + string(rune('a'+i))
			if _, err := m.CreateSandbox(ctx, store.CreateSandboxInput{
				ProjectID: projectID, Principal: "alice", IdemKey: key, IdemHash: key,
				ImageID: imageID, Resources: spec(), MaxActive: 100,
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	launch("prj_dev", "img_low", 1)
	launch("prj_dev", "img_high", 3)
	launch("prj_dev", "img_mid", 2)
	// Launched many times, but under a different project: must never outrank
	// prj_dev's own images in prj_dev's top-K.
	launch("prj_other", "img_other", 5)

	top, err := m.TopImagesByLaunchCount(ctx, "prj_dev", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(top) != 2 {
		t.Fatalf("top = %+v, want 2 results", top)
	}
	if top[0].ImageID != "img_high" || top[0].LaunchCount != 3 {
		t.Fatalf("top[0] = %+v", top[0])
	}
	if top[1].ImageID != "img_mid" || top[1].LaunchCount != 2 {
		t.Fatalf("top[1] = %+v", top[1])
	}
	for _, img := range top {
		if img.ProjectID != "prj_dev" {
			t.Fatalf("cross-project leak: %+v", img)
		}
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
