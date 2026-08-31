package probe

import (
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"
)

func TestSelect(t *testing.T) {
	full, err := Select("full")
	if err != nil || len(full) != len(All()) {
		t.Fatalf("full: %d journeys, err %v", len(full), err)
	}
	lite, err := Select("lite")
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range lite {
		if j.Lifecycle {
			t.Fatalf("lite contains lifecycle journey %s", j.Name)
		}
	}
	if len(lite) == 0 || len(lite) >= len(full) {
		t.Fatalf("lite = %d, full = %d", len(lite), len(full))
	}

	named, err := Select("health,idempotency")
	if err != nil || len(named) != 2 || named[0].Name != "health" || named[1].Name != "idempotency" {
		t.Fatalf("named: %v err %v", names(named), err)
	}

	if _, err := Select("bogus"); err == nil {
		t.Fatal("want error for unknown journey")
	}
}

func TestAllJourneyNamesUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, j := range All() {
		if seen[j.Name] {
			t.Fatalf("duplicate journey name %s", j.Name)
		}
		seen[j.Name] = true
		if j.Run == nil {
			t.Fatalf("journey %s has nil Run", j.Name)
		}
	}
}

func TestAnyFailed(t *testing.T) {
	if AnyFailed([]Result{{OK: true}, {OK: true}}) {
		t.Fatal("all-ok reported failure")
	}
	if !AnyFailed([]Result{{OK: true}, {OK: false}}) {
		t.Fatal("missed a failure")
	}
}

func makeJWT(t *testing.T, typ string, claims map[string]any) string {
	t.Helper()
	hdr, _ := json.Marshal(map[string]string{"alg": "none", "typ": typ})
	pl, _ := json.Marshal(claims)
	enc := base64.RawURLEncoding.EncodeToString
	return enc(hdr) + "." + enc(pl) + ".sig"
}

func TestJWTHelpers(t *testing.T) {
	exp := time.Now().Add(45 * time.Minute).Unix()
	tok := makeJWT(t, "stream+jwt", map[string]any{"exp": exp, "action": "attach", "process_id": "prc_9"})

	if got := jwtExpiry(tok).Unix(); got != exp {
		t.Fatalf("jwtExpiry = %d, want %d", got, exp)
	}
	claims, typ, err := jwtClaims(tok)
	if err != nil {
		t.Fatal(err)
	}
	if typ != "stream+jwt" || claims["action"] != "attach" || claims["process_id"] != "prc_9" {
		t.Fatalf("claims = %v typ = %s", claims, typ)
	}

	if _, _, err := jwtClaims("not-a-jwt"); err == nil {
		t.Fatal("want error for non-JWT")
	}
	// No exp claim => fallback ~10m in the future.
	noExp := makeJWT(t, "at+jwt", map[string]any{"sub": "x"})
	if d := time.Until(jwtExpiry(noExp)); d < 8*time.Minute || d > 12*time.Minute {
		t.Fatalf("fallback expiry delta = %s", d)
	}
}
