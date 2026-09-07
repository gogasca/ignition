package streamtoken

import (
	"testing"
	"time"
)

func base() Claims {
	now := time.Now()
	return Claims{
		Subject: "alice", Audience: "https://gw.example",
		ProjectID: "prj_1", SandboxID: "sbx_1", ProcessID: "prc_1",
		Generation: 4, StreamEpoch: 99, Action: ActionAttach,
		IssuedAt: now, NotBefore: now, ExpiresAt: now.Add(5 * time.Minute),
	}
}

func TestSignVerifyRoundTrip(t *testing.T) {
	tok, err := Sign("secret", base())
	if err != nil {
		t.Fatal(err)
	}
	got, err := Verify("secret", tok, "https://gw.example")
	if err != nil {
		t.Fatal(err)
	}
	if got.SandboxID != "sbx_1" || got.ProcessID != "prc_1" || got.Generation != 4 || got.StreamEpoch != 99 {
		t.Fatalf("claims round-tripped wrong: %+v", got)
	}
}

func TestVerifyRejects(t *testing.T) {
	good, _ := Sign("secret", base())
	cases := []struct {
		name, secret, tok, aud string
	}{
		{"wrong secret", "other", good, "https://gw.example"},
		{"wrong audience", "secret", good, "https://other"},
		{"garbage", "secret", "not-a-jwt", "https://gw.example"},
	}
	for _, c := range cases {
		if _, err := Verify(c.secret, c.tok, c.aud); err == nil {
			t.Fatalf("%s: expected error", c.name)
		}
	}
}

func TestVerifyRejectsExpired(t *testing.T) {
	c := base()
	c.IssuedAt = time.Now().Add(-1 * time.Hour)
	c.NotBefore = c.IssuedAt
	c.ExpiresAt = time.Now().Add(-1 * time.Minute)
	tok, _ := Sign("secret", c)
	if _, err := Verify("secret", tok, "https://gw.example"); err == nil {
		t.Fatal("expected expiry error")
	}
}
