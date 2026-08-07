package dns

import (
	"context"
	"fmt"
	"time"

	"github.com/evandelacruz/farrier/internal/core/driver"
)

// ExecDriver satisfies Driver for an out-of-tree DNS driver reached
// through the CORE-003 exec protocol: one process per call, methods "set"
// and "delete", the same plugin posture used by keystore's execDriver and
// blob's ExecAdapter.
type ExecDriver struct {
	Invoker driver.Invoker
}

// NewExec returns an ExecDriver that calls through invoker.
func NewExec(invoker driver.Invoker) *ExecDriver {
	return &ExecDriver{Invoker: invoker}
}

type execSetParams struct {
	Record string `json:"record"`
	Value  string `json:"value"`
	TTL    int    `json:"ttl"`
}

// Set invokes the "set" method with ttl in whole seconds.
func (d *ExecDriver) Set(ctx context.Context, record, value string, ttl time.Duration) error {
	if err := validateSetArgs(record, value, ttl); err != nil {
		return err
	}
	params := execSetParams{Record: record, Value: value, TTL: int(ttl.Seconds())}
	if err := d.Invoker.Invoke(ctx, "set", params, nil); err != nil {
		return fmt.Errorf("dns: exec: set %s: %w", record, err)
	}
	return nil
}

type execDeleteParams struct {
	Record string `json:"record"`
}

// Delete invokes the "delete" method.
func (d *ExecDriver) Delete(ctx context.Context, record string) error {
	if err := validateDeleteArgs(record); err != nil {
		return err
	}
	if err := d.Invoker.Invoke(ctx, "delete", execDeleteParams{Record: record}, nil); err != nil {
		return fmt.Errorf("dns: exec: delete %s: %w", record, err)
	}
	return nil
}
