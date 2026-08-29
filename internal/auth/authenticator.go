package auth

import (
	"context"
	"errors"
	"strings"
)

var (
	ErrUnauthenticated = errors.New("unauthenticated")
)

// Authenticator validates a Bearer token and returns a principal.
type Authenticator interface {
	Authenticate(ctx context.Context, bearer string) (Principal, error)
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
