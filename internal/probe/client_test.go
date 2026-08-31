package probe

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func testClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return New(ts.URL, "prj_dev", WithStaticToken("tok"), WithPollInterval(time.Millisecond))
}

func TestHealthzOK(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Write([]byte("ok"))
	})
	if err := c.Healthz(context.Background()); err != nil {
		t.Fatalf("Healthz: %v", err)
	}
}

func TestErrorEnvelopeParsed(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"code":"QUOTA_EXCEEDED","message":"quota","requestId":"req_1","retryAfterSeconds":30}`))
	})
	_, _, err := c.CreateSandbox(context.Background(), "k", CreateSandboxReq{ImageID: "img_seed"})
	if err == nil {
		t.Fatal("want error")
	}
	if !CodeIs(err, "QUOTA_EXCEEDED") {
		t.Fatalf("CodeIs = false for %v", err)
	}
	if HTTPStatus(err) != http.StatusTooManyRequests {
		t.Fatalf("HTTPStatus = %d", HTTPStatus(err))
	}
}

func TestNonJSONErrorBody(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	})
	_, err := c.GetSandbox(context.Background(), "sbx_x")
	if err == nil || !CodeIs(err, "NON_JSON_ERROR") {
		t.Fatalf("want NON_JSON_ERROR, got %v", err)
	}
}

func TestCreateSandboxSendsIdempotencyKey(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Idempotency-Key") != "key-123" {
			t.Errorf("Idempotency-Key = %q", r.Header.Get("Idempotency-Key"))
		}
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte(`{"sandbox":{"id":"sbx_1","state":"CREATING"},"operation":{"id":"op_1","state":"PENDING"}}`))
	})
	sb, op, err := c.CreateSandbox(context.Background(), "key-123", CreateSandboxReq{ImageID: "img_seed"})
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	if sb.ID != "sbx_1" || op.ID != "op_1" {
		t.Fatalf("got %+v %+v", sb, op)
	}
}

func TestUnauthenticatedOmitsBearer(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Errorf("Authorization should be empty, got %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"code":"UNAUTHENTICATED","message":"nope"}`))
	})
	err := c.Unauthenticated(context.Background())
	if !CodeIs(err, "UNAUTHENTICATED") {
		t.Fatalf("want UNAUTHENTICATED, got %v", err)
	}
}

func TestPollSandboxRespectsContext(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"id":"sbx_1","state":"CREATING"}`))
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := c.PollSandbox(ctx, "sbx_1", func(sb SandboxView) (bool, error) { return false, nil })
	if err == nil {
		t.Fatal("want timeout error")
	}
}
