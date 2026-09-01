package auth

import (
	"context"
	"errors"
	"strings"
)

var (
	ErrUnauthenticated = errors.New("unauthenticated")
)

// Authenticator validates a credential and returns a principal. The credential
// is the raw header value: an `Authorization: Bearer <token>` value for token
// authenticators, or a signed-header assertion (e.g. IAP's
// `X-Goog-IAP-JWT-Assertion`) for header authenticators. A leading `Bearer `
// prefix is tolerated and stripped.
type Authenticator interface {
	Authenticate(ctx context.Context, bearer string) (Principal, error)
}

// Chain tries each authenticator in order and returns the first success. A
// later authenticator is tried only when the previous one returned
// ErrUnauthenticated; any other error (JWKS fetch failure, misconfiguration)
// is surfaced immediately so it is not masked by a fallback.
type Chain []Authenticator

func (c Chain) Authenticate(ctx context.Context, bearer string) (Principal, error) {
	err := error(ErrUnauthenticated)
	for _, a := range c {
		var p Principal
		p, err = a.Authenticate(ctx, bearer)
		if err == nil {
			return p, nil
		}
		if !errors.Is(err, ErrUnauthenticated) {
			return Principal{}, err
		}
	}
	return Principal{}, err
}

// Static maps opaque bearer tokens to principals (tests and local dev).
type Static struct {
	Tokens map[string]Principal
}

func (s Static) Authenticate(_ context.Context, bearer string) (Principal, error) {
	token := strings.TrimSpace(strings.TrimPrefix(bearer, "Bearer "))
	if token == "" {
		return Principal{}, ErrUnauthenticated
	}
	p, ok := s.Tokens[token]
	if !ok {
		return Principal{}, ErrUnauthenticated
	}
	return p, nil
}
