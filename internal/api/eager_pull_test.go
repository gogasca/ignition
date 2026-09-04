package api_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ignition.dev/ignition/internal/api"
	"ignition.dev/ignition/internal/auth"
	"ignition.dev/ignition/internal/config"
	"ignition.dev/ignition/internal/imagecatalog"
	"ignition.dev/ignition/internal/store"
)

type eagerPullHarness struct {
	ts *httptest.Server
}

func newEagerPullHarness(t *testing.T) *eagerPullHarness {
	t.Helper()
	mem := store.NewMemory()
	mem.SeedRole("prj", "owner@corp.example", auth.RoleOwner)
	fake := imagecatalog.NewFake()
	fake.Images["eligible-huge:latest"] = imagecatalog.Resolved{
		Digest: "sha256:a", RegistryRef: "eligible-huge@sha256:a",
		StreamingEligible: true, CompressedBytes: 500_000_000_000, // 500 GB, but eligible
	}
	fake.Images["ineligible-huge:latest"] = imagecatalog.Resolved{
		Digest: "sha256:b", RegistryRef: "ineligible-huge@sha256:b",
		StreamingEligible: false, IneligibleReason: "schema version 1 manifest is not eligible for GKE image streaming",
		// 20 GB at the 50 MB/s test default is a 400s estimate: comfortably
		// exceeds a 60s deadline but still fits within the API's 600s
		// startupSeconds cap for the "sufficient deadline" case.
		CompressedBytes: 20_000_000_000,
	}
	fake.Images["ineligible-small:latest"] = imagecatalog.Resolved{
		Digest: "sha256:c", RegistryRef: "ineligible-small@sha256:c",
		StreamingEligible: false, IneligibleReason: "image has an empty layer, which GKE downloads without streaming",
		CompressedBytes: 10_000_000, // 10 MB; a 0.2s estimate, fits any real deadline
	}
	srv := api.NewWithResolver(config.Config{
		EnabledRegion:        "us-central1",
		GatewayURL:           "https://gateway.us-central1.ignition.dev",
		StreamTokenSecret:    "test-stream-secret",
		OIDCAudience:         "https://api.ignition.dev",
		MaxActiveSandboxes:   100,
		AssumedEagerPullMBps: 50,
	}, mem, auth.Static{Tokens: map[string]auth.Principal{
		"owner": {Subject: "owner@corp.example", Kind: auth.KindUser},
	}}, fake)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return &eagerPullHarness{ts: ts}
}

func (h *eagerPullHarness) req(t *testing.T, method, path, idemKey, body string) (*http.Response, map[string]any) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	r, err := http.NewRequest(method, h.ts.URL+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer owner")
	if idemKey != "" {
		r.Header.Set("Idempotency-Key", idemKey)
	}
	r.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if len(b) > 0 {
		_ = json.Unmarshal(b, &out)
	}
	return resp, out
}

// admitImage registers imageID against sourceRef through the real admission
// endpoint (not a direct store call), so these tests exercise the same path
// a client would.
func (h *eagerPullHarness) admitImage(t *testing.T, imageID, sourceRef string) {
	t.Helper()
	resp, body := h.req(t, http.MethodPost, "/v1/projects/prj/images", "admit-"+imageID,
		fmt.Sprintf(`{"imageId":%q,"sourceRef":%q}`, imageID, sourceRef))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("admit %s: status = %d, body = %v", imageID, resp.StatusCode, body)
	}
}

func sandboxBody(imageID string, startupSeconds int) string {
	return fmt.Sprintf(`{
		"imageId": %q,
		"resources": {"cpuMilli": 1000, "memoryMiB": 2048, "accelerator": {"count": 1, "type": "NVIDIA_L4"}},
		"timeouts": {"startupSeconds": %d}
	}`, imageID, startupSeconds)
}

func TestCreateSandboxRejectsWhenEagerPullExceedsDeadline(t *testing.T) {
	h := newEagerPullHarness(t)
	h.admitImage(t, "img_ineligible_huge", "ineligible-huge:latest")

	resp, body := h.req(t, http.MethodPost, "/v1/projects/prj/sandboxes", "sbx-1", sandboxBody("img_ineligible_huge", 60))
	if resp.StatusCode != http.StatusBadRequest || body["code"] != "IMAGE_UNAVAILABLE" {
		t.Fatalf("status = %d, body = %v", resp.StatusCode, body)
	}
}

func TestCreateSandboxAllowsWhenDeadlineSufficient(t *testing.T) {
	h := newEagerPullHarness(t)
	h.admitImage(t, "img_ineligible_huge", "ineligible-huge:latest")

	// Same 20 GB ineligible image, but a deadline generous enough for the
	// 400s estimate (20e9 bytes / 50 MB/s) to fit within the 600s cap.
	resp, body := h.req(t, http.MethodPost, "/v1/projects/prj/sandboxes", "sbx-2", sandboxBody("img_ineligible_huge", 600))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, body = %v", resp.StatusCode, body)
	}
}

func TestCreateSandboxAllowsEligibleImageRegardlessOfSize(t *testing.T) {
	h := newEagerPullHarness(t)
	h.admitImage(t, "img_eligible_huge", "eligible-huge:latest")

	// 500 GB but streaming-eligible: the estimate never applies.
	resp, body := h.req(t, http.MethodPost, "/v1/projects/prj/sandboxes", "sbx-3", sandboxBody("img_eligible_huge", 60))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, body = %v", resp.StatusCode, body)
	}
}

func TestCreateSandboxAllowsIneligibleSmallImage(t *testing.T) {
	h := newEagerPullHarness(t)
	h.admitImage(t, "img_ineligible_small", "ineligible-small:latest")

	resp, body := h.req(t, http.MethodPost, "/v1/projects/prj/sandboxes", "sbx-4", sandboxBody("img_ineligible_small", 60))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, body = %v", resp.StatusCode, body)
	}
}
