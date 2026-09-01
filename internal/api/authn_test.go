package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"ignition.dev/ignition/internal/api"
	"ignition.dev/ignition/internal/auth"
	"ignition.dev/ignition/internal/config"
	"ignition.dev/ignition/internal/store"
)

// capAuth records the credential string the middleware handed it.
type capAuth struct {
	got string
	p   auth.Principal
}

func (c *capAuth) Authenticate(_ context.Context, cred string) (auth.Principal, error) {
	c.got = cred
	if c.p.Subject == "" {
		return auth.Principal{}, auth.ErrUnauthenticated
	}
	return c.p, nil
}

func TestMiddlewarePrefersIAPAssertion(t *testing.T) {
	mem := store.NewMemory()
	mem.SeedRole("prj", "owner@corp.example", auth.RoleOwner)
	ca := &capAuth{p: auth.Principal{Subject: "owner@corp.example", Domain: "corp.example"}}
	srv := api.New(config.Config{OIDCAudience: "https://api.ignition.dev"}, mem, ca)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	do := func(setIAP bool) {
		r, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/projects/prj/roleBindings", nil)
		r.Header.Set("Authorization", "Bearer plain-token")
		if setIAP {
			r.Header.Set("X-Goog-IAP-JWT-Assertion", "iap-assertion")
		}
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
	}

	do(true)
	if ca.got != "iap-assertion" {
		t.Fatalf("with IAP header, credential = %q, want iap-assertion", ca.got)
	}
	do(false)
	if ca.got != "Bearer plain-token" {
		t.Fatalf("without IAP header, credential = %q, want the bearer", ca.got)
	}
}
