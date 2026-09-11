package imagecatalog

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRemoteResolverRejectsDisallowedHost(t *testing.T) {
	rr := RemoteResolver{Allowlist: []string{"index.docker.io"}}
	_, err := rr.Resolve(context.Background(), "gcr.io/distroless/static:nonroot")
	if !errors.Is(err, ErrSourceNotAllowed) {
		t.Fatalf("err = %v, want ErrSourceNotAllowed", err)
	}
	// Nothing about the failure should be a topology signal — it names only
	// the host the caller supplied.
	if errors.Is(err, ErrResolveFailed) {
		t.Fatalf("a disallowed host must not look like a resolve failure: %v", err)
	}
}

func TestRemoteResolverRefusesToDialNonPublicAddress(t *testing.T) {
	// Host check passes (127.0.0.1 is allowlisted) so we reach the dial, where
	// the guard must refuse the loopback connection.
	rr := RemoteResolver{Allowlist: []string{"127.0.0.1"}, Timeout: 3 * time.Second}
	_, err := rr.Resolve(context.Background(), "127.0.0.1:9/x/y:latest")
	if !errors.Is(err, ErrSourceNotAllowed) {
		t.Fatalf("err = %v, want ErrSourceNotAllowed (guard should re-tag a blocked dial)", err)
	}
}

func TestRemoteResolverInvalidReferenceIsSanitized(t *testing.T) {
	rr := RemoteResolver{}
	_, err := rr.Resolve(context.Background(), "not a valid ref!!")
	if !errors.Is(err, ErrResolveFailed) {
		t.Fatalf("err = %v, want ErrResolveFailed", err)
	}
}

func TestResolveTimeoutDefault(t *testing.T) {
	if got := (RemoteResolver{}).resolveTimeout(); got != DefaultResolveTimeout {
		t.Fatalf("default = %v, want %v", got, DefaultResolveTimeout)
	}
	if got := (RemoteResolver{Timeout: 5 * time.Second}).resolveTimeout(); got != 5*time.Second {
		t.Fatalf("explicit = %v, want 5s", got)
	}
}
