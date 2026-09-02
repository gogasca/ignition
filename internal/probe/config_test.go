package probe

import "testing"

func TestLoadDefaults(t *testing.T) {
	t.Setenv("IGNITION_PROBE_TARGET", "http://api:8080/")
	t.Setenv("IGNITION_PROBE_AUDIENCE", "https://api.staging.ignition.dev")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Target != "http://api:8080" {
		t.Fatalf("Target = %q", c.Target)
	}
	if c.Audience != "https://api.staging.ignition.dev" {
		t.Fatalf("Audience = %q", c.Audience)
	}
	if c.Project != "prj_dev" || c.ImageID != "img_seed" || c.Auth != "gcp-idtoken" {
		t.Fatalf("defaults: %+v", c)
	}
	if c.Interval.Minutes() != 5 || c.Journeys != "full" {
		t.Fatalf("defaults: %+v", c)
	}
}

func TestLoadValidation(t *testing.T) {
	if _, err := Load(); err == nil {
		t.Fatal("want error without IGNITION_PROBE_TARGET")
	}

	t.Setenv("IGNITION_PROBE_TARGET", "http://api:8080")
	if _, err := Load(); err == nil {
		t.Fatal("want error: gcp-idtoken (default) without IGNITION_PROBE_AUDIENCE")
	}

	t.Setenv("IGNITION_PROBE_AUTH", "static")
	if _, err := Load(); err == nil {
		t.Fatal("want error: static auth without token")
	}
	t.Setenv("IGNITION_PROBE_TOKEN", "t")
	if _, err := Load(); err != nil {
		t.Fatalf("static+token should load: %v", err)
	}

	t.Setenv("IGNITION_PROBE_AUTH", "gcp-idtoken")
	t.Setenv("IGNITION_PROBE_AUDIENCE", "https://api.staging.ignition.dev")
	t.Setenv("IGNITION_PROBE_JOURNEYS", "bogus")
	if _, err := Load(); err == nil {
		t.Fatal("want error for unknown journey spec")
	}
}
