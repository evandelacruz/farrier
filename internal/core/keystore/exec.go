package keystore

import (
	"context"
	"fmt"

	"github.com/evandelacruz/farrier/internal/core/driver"
)

// ExecResolver is the out-of-tree keystore driver (CORE-003): it satisfies
// Resolver by invoking a driver executable through the exec protocol
// instead of resolving key material in-tree — the same posture used by
// dns and blob (tech-spec.md "Driver interfaces").
//
// A resolved secret is small enough to travel in the exec protocol's JSON
// envelope directly, unlike blob's Get/Put, which stage large content
// through a local temp file instead.
type ExecResolver struct {
	Invoker driver.Invoker
}

// NewExec returns an ExecResolver that calls through invoker.
func NewExec(invoker driver.Invoker) *ExecResolver {
	return &ExecResolver{Invoker: invoker}
}

type execResolveParams struct {
	Key string `json:"key"`
}

type execResolveResult struct {
	Secret string `json:"secret"`
}

// Resolve invokes the "resolve" method and wraps its result as a Secret
// immediately, so the raw value never exists outside of that wrapper.
func (r *ExecResolver) Resolve(ctx context.Context, keyName string) (Secret, error) {
	var res execResolveResult
	if err := r.Invoker.Invoke(ctx, "resolve", execResolveParams{Key: keyName}, &res); err != nil {
		return Secret{}, fmt.Errorf("keystore: exec: resolve %q: %w", keyName, err)
	}
	return NewSecret(res.Secret), nil
}
