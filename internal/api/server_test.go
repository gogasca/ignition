package api

import "testing"

func TestSplitCustom(t *testing.T) {
	id, method := splitCustom("sbx_abc:terminate")
	if id != "sbx_abc" || method != "terminate" {
		t.Fatalf("got %q %q", id, method)
	}
	id, method = splitCustom("sbx_abc")
	if id != "sbx_abc" || method != "" {
		t.Fatalf("got %q %q", id, method)
	}
}
