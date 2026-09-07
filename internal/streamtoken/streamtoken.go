// Package streamtoken signs and verifies the short-lived exec-stream tokens
// that ignition-api mints and ignition-gateway validates. They are a distinct
// credential class from API access JWTs: separate audience, separate secret
// (HMAC), and a narrow claim set bound to one process attach.
package streamtoken

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	tokenType = "stream+jwt"
	issuer    = "ignition-api"
	// ActionAttach is the only action in the first slice.
	ActionAttach = "attach"
)

// Claims is the exec-stream token payload.
type Claims struct {
	Subject     string
	Audience    string
	ProjectID   string
	SandboxID   string
	ProcessID   string
	Generation  int64
	StreamEpoch int64
	Action      string
	IssuedAt    time.Time
	NotBefore   time.Time
	ExpiresAt   time.Time
}

// Sign returns an HS256 token for c using secret.
func Sign(secret string, c Claims) (string, error) {
	if secret == "" {
		return "", errors.New("streamtoken: empty secret")
	}
	if c.Action == "" {
		c.Action = ActionAttach
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"iss":          issuer,
		"aud":          c.Audience,
		"sub":          c.Subject,
		"iat":          c.IssuedAt.Unix(),
		"nbf":          c.NotBefore.Unix(),
		"exp":          c.ExpiresAt.Unix(),
		"project_id":   c.ProjectID,
		"sandbox_id":   c.SandboxID,
		"process_id":   c.ProcessID,
		"generation":   c.Generation,
		"stream_epoch": c.StreamEpoch,
		"action":       c.Action,
	})
	tok.Header["typ"] = tokenType
	return tok.SignedString([]byte(secret))
}

// Verify checks the signature, type, issuer, audience, and time bounds, and
// returns the claims. audience must match exactly.
func Verify(secret, raw, audience string) (Claims, error) {
	if secret == "" {
		return Claims{}, errors.New("streamtoken: empty secret")
	}
	parsed, err := jwt.Parse(raw, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method %v", t.Header["alg"])
		}
		if typ, _ := t.Header["typ"].(string); typ != tokenType {
			return nil, fmt.Errorf("unexpected token type %q", typ)
		}
		return []byte(secret), nil
	}, jwt.WithValidMethods([]string{"HS256"}),
		jwt.WithIssuer(issuer),
		jwt.WithAudience(audience),
		jwt.WithExpirationRequired(),
		jwt.WithLeeway(30*time.Second))
	if err != nil {
		return Claims{}, err
	}
	mc, ok := parsed.Claims.(jwt.MapClaims)
	if !ok || !parsed.Valid {
		return Claims{}, errors.New("streamtoken: invalid claims")
	}
	out := Claims{
		Audience:    audience,
		Action:      stringClaim(mc, "action"),
		Subject:     stringClaim(mc, "sub"),
		ProjectID:   stringClaim(mc, "project_id"),
		SandboxID:   stringClaim(mc, "sandbox_id"),
		ProcessID:   stringClaim(mc, "process_id"),
		Generation:  intClaim(mc, "generation"),
		StreamEpoch: intClaim(mc, "stream_epoch"),
	}
	if out.SandboxID == "" || out.ProcessID == "" {
		return Claims{}, errors.New("streamtoken: missing sandbox_id/process_id")
	}
	if out.Action != ActionAttach {
		return Claims{}, fmt.Errorf("streamtoken: unsupported action %q", out.Action)
	}
	return out, nil
}

func stringClaim(m jwt.MapClaims, k string) string {
	s, _ := m[k].(string)
	return s
}

func intClaim(m jwt.MapClaims, k string) int64 {
	switch v := m[k].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	default:
		return 0
	}
}
