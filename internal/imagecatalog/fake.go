package imagecatalog

import (
	"context"
	"fmt"
)

// Fake is a deterministic Resolver for tests: no network, no registry.
type Fake struct {
	// Images maps a source ref to what Resolve should return for it.
	Images map[string]Resolved
	// Err, when set, maps a source ref to an error Resolve should return
	// instead of a result.
	Err map[string]error
}

func NewFake() *Fake {
	return &Fake{Images: map[string]Resolved{}, Err: map[string]error{}}
}

func (f *Fake) Resolve(_ context.Context, ref string) (Resolved, error) {
	if err, ok := f.Err[ref]; ok {
		return Resolved{}, err
	}
	r, ok := f.Images[ref]
	if !ok {
		return Resolved{}, fmt.Errorf("fake resolver: no image registered for %q", ref)
	}
	return r, nil
}
