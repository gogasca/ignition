package api

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"ignition.dev/ignition/internal/store"
)

func TestWriteSSEEmitsChangedSnapshots(t *testing.T) {
	state := "PENDING"
	reads := 0
	fetch := func() (any, error) {
		reads++
		if reads > 1 {
			state = "SUCCEEDED"
		}
		return store.Operation{ID: "op_test", State: state}, nil
	}
	terminal := func(v any) bool { return v.(store.Operation).State == "SUCCEEDED" }

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/watch", nil).WithContext(context.Background())
	writeSSE(w, r, "req_test", fetch, terminal)

	body := w.Body.String()
	if got := strings.Count(body, "event: snapshot"); got != 2 {
		t.Fatalf("snapshot count = %d, want 2; body=%s", got, body)
	}
	if !strings.Contains(body, `"state":"PENDING"`) || !strings.Contains(body, `"state":"SUCCEEDED"`) {
		t.Fatalf("stream did not contain both states: %s", body)
	}
	ids := eventIDs(body)
	if len(ids) != 2 || ids[0] == ids[1] {
		t.Fatalf("event IDs = %v, want two distinct IDs", ids)
	}
}

func TestWriteSSEResumesAfterLastEventID(t *testing.T) {
	v := store.Operation{ID: "op_test", State: "SUCCEEDED"}
	fetch := func() (any, error) { return v, nil }
	terminal := func(any) bool { return true }

	first := httptest.NewRecorder()
	writeSSE(first, httptest.NewRequest("GET", "/watch", nil), "req_test", fetch, terminal)
	ids := eventIDs(first.Body.String())
	if len(ids) != 1 {
		t.Fatalf("initial event IDs = %v", ids)
	}

	resumed := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/watch", nil)
	r.Header.Set("Last-Event-ID", ids[0])
	writeSSE(resumed, r, "req_test", fetch, terminal)
	if strings.Contains(resumed.Body.String(), "event: snapshot") {
		t.Fatalf("resumed stream replayed an acknowledged snapshot: %s", resumed.Body.String())
	}
}

func eventIDs(stream string) []string {
	var ids []string
	for _, line := range strings.Split(stream, "\n") {
		if strings.HasPrefix(line, "id: ") {
			ids = append(ids, strings.TrimPrefix(line, "id: "))
		}
	}
	return ids
}
