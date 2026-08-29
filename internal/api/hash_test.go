package api

import "testing"

func TestCanonicalHashStable(t *testing.T) {
	a := []byte(`{"b":2,"a":1}`)
	b := []byte(`{ "a": 1, "b": 2 }`)
	ha := canonicalHash("POST", "/v1/projects/p/sandboxes", a)
	hb := canonicalHash("POST", "/v1/projects/p/sandboxes", b)
	if ha != hb {
		t.Fatalf("hash %s != %s", ha, hb)
	}
	hc := canonicalHash("POST", "/v1/other", a)
	if ha == hc {
		t.Fatal("route must be part of the hash")
	}
}

func TestCanonicalHashInvalidJSONDoesNotEqualNull(t *testing.T) {
	invalid := canonicalHash("POST", "/x", []byte(`{`))
	asNull := canonicalHash("POST", "/x", []byte(`null`))
	if invalid == asNull {
		t.Fatalf("invalid JSON and null both hashed as %s", invalid)
	}
}
