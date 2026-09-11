package imagecatalog

import (
	"errors"
	"net"
	"strings"
	"testing"
)

func TestCheckRegistryHost(t *testing.T) {
	allow := []string{"us-central1-docker.pkg.dev", "index.docker.io"}
	cases := []struct {
		host    string
		allow   []string
		wantErr bool
	}{
		{"us-central1-docker.pkg.dev", allow, false},
		{"US-CENTRAL1-DOCKER.PKG.DEV", allow, false}, // case-insensitive
		{"index.docker.io:443", allow, false},        // port ignored
		{"gcr.io", allow, true},
		{"169.254.169.254", allow, true},
		{"evil.example", allow, true},
		{"anything", nil, false}, // empty allowlist -> no host restriction
		{"anything", []string{}, false},
	}
	for _, c := range cases {
		err := checkRegistryHost(c.host, c.allow)
		if (err != nil) != c.wantErr {
			t.Errorf("checkRegistryHost(%q, %v) err=%v, wantErr=%v", c.host, c.allow, err, c.wantErr)
		}
		if err != nil && !errors.Is(err, ErrSourceNotAllowed) {
			t.Errorf("checkRegistryHost(%q) err is not ErrSourceNotAllowed: %v", c.host, err)
		}
	}
}

func TestBlockedIP(t *testing.T) {
	blocked := []string{
		"169.254.169.254", // GCE metadata
		"127.0.0.1", "0.0.0.0",
		"10.1.2.3", "172.16.5.5", "192.168.1.1",
		"100.64.0.1", "100.127.255.255", // CGNAT / GKE
		"::1", "fe80::1", "fd00::abcd", "ff02::1",
	}
	for _, s := range blocked {
		if !blockedIP(net.ParseIP(s)) {
			t.Errorf("blockedIP(%s) = false, want true", s)
		}
	}
	allowed := []string{"8.8.8.8", "1.1.1.1", "34.102.136.180", "2606:4700:4700::1111", "100.63.255.255", "100.128.0.0"}
	for _, s := range allowed {
		if blockedIP(net.ParseIP(s)) {
			t.Errorf("blockedIP(%s) = true, want false", s)
		}
	}
	if !blockedIP(nil) {
		t.Error("blockedIP(nil) = false, want true")
	}
}

func TestGuardedControl(t *testing.T) {
	if err := guardedControl("tcp", "169.254.169.254:80", nil); err == nil ||
		!strings.Contains(err.Error(), dialRefusedMarker) {
		t.Errorf("guardedControl(metadata) err=%v, want %q", err, dialRefusedMarker)
	}
	if err := guardedControl("tcp", "10.0.0.5:443", nil); err == nil {
		t.Error("guardedControl(private) err=nil, want error")
	}
	if err := guardedControl("tcp", "8.8.8.8:443", nil); err != nil {
		t.Errorf("guardedControl(public) err=%v, want nil", err)
	}
	if err := guardedControl("tcp", "not-an-ip:443", nil); err == nil ||
		!strings.Contains(err.Error(), dialNoIPMarker) {
		t.Errorf("guardedControl(hostname) err=%v, want %q", err, dialNoIPMarker)
	}
}

func TestGuardedTransportInstallsControl(t *testing.T) {
	tr := guardedTransport()
	if tr.DialContext == nil {
		t.Fatal("guardedTransport did not set DialContext")
	}
	// A direct dial to the metadata address must be refused by the guard.
	_, err := tr.DialContext(t.Context(), "tcp", "169.254.169.254:80")
	if err == nil || !strings.Contains(err.Error(), dialRefusedMarker) {
		t.Fatalf("guarded dial to metadata: err=%v, want %q", err, dialRefusedMarker)
	}
}
