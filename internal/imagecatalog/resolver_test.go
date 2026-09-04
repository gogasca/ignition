package imagecatalog_test

import (
	"context"
	"errors"
	"os"
	"testing"

	"ignition.dev/ignition/internal/imagecatalog"
)

func TestFakeResolveReturnsRegisteredImage(t *testing.T) {
	f := imagecatalog.NewFake()
	f.Images["docker.io/library/nginx:1.27"] = imagecatalog.Resolved{
		Digest:      "sha256:abc",
		RegistryRef: "docker.io/library/nginx@sha256:abc",
		Entrypoint:  []string{"/docker-entrypoint.sh"},
		Cmd:         []string{"nginx", "-g", "daemon off;"},
	}
	got, err := f.Resolve(context.Background(), "docker.io/library/nginx:1.27")
	if err != nil {
		t.Fatal(err)
	}
	if got.Digest != "sha256:abc" || got.RegistryRef != "docker.io/library/nginx@sha256:abc" {
		t.Fatalf("resolved = %+v", got)
	}
	if len(got.Entrypoint) != 1 || got.Entrypoint[0] != "/docker-entrypoint.sh" {
		t.Fatalf("entrypoint = %v", got.Entrypoint)
	}
}

func TestFakeResolveUnregisteredRefFails(t *testing.T) {
	f := imagecatalog.NewFake()
	if _, err := f.Resolve(context.Background(), "unregistered:latest"); err == nil {
		t.Fatal("want error for an unregistered reference")
	}
}

func TestFakeResolveHonorsInjectedError(t *testing.T) {
	f := imagecatalog.NewFake()
	want := errors.New("registry unavailable")
	f.Err["broken:latest"] = want
	_, err := f.Resolve(context.Background(), "broken:latest")
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
}

func TestRemoteResolverRejectsInvalidReference(t *testing.T) {
	var r imagecatalog.RemoteResolver
	if _, err := r.Resolve(context.Background(), "not a valid ref!!"); err == nil {
		t.Fatal("want error for an invalid reference")
	}
}

// TestRemoteResolverAgainstRealRegistry exercises RemoteResolver over the
// network. It is skipped unless IGNITION_TEST_NETWORK=1, since this sandbox
// (and CI, typically) should not depend on live registry reachability for a
// normal test run.
func TestRemoteResolverAgainstRealRegistry(t *testing.T) {
	if os.Getenv("IGNITION_TEST_NETWORK") != "1" {
		t.Skip("set IGNITION_TEST_NETWORK=1 to run tests against a real registry")
	}
	var r imagecatalog.RemoteResolver
	got, err := r.Resolve(context.Background(), "docker.io/library/hello-world:latest")
	if err != nil {
		t.Fatal(err)
	}
	if !got.StreamingEligible || got.IneligibleReason != "" {
		t.Fatalf("streaming eligibility = %v %q, want an ordinary public image to be eligible", got.StreamingEligible, got.IneligibleReason)
	}
	if got.Digest == "" || got.RegistryRef == "" {
		t.Fatalf("resolved = %+v", got)
	}
}
