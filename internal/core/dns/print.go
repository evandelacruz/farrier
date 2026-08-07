package dns

import (
	"context"
	"fmt"
	"time"

	"github.com/evandelacruz/farrier/internal/core/events"
)

// StepDNSChange identifies the DNS-change step PrintDriver emits onto a
// job's event stream.
const StepDNSChange = "dns-change"

// PrintDriver satisfies Driver for a bundle with no DNS driver configured
// (DNS-003; bundle.DriverConfig's DNS field is optional). Set and Delete
// never fail for lack of a driver: instead of applying the change, they
// emit the exact record the operator must apply by hand onto Job's CORE-002
// event stream — the same channel every other operation reports through, so
// the CLI and the dashboard render the instruction identically without
// either frontend containing DNS logic of its own.
type PrintDriver struct {
	Job *events.Job
}

// NewPrint returns a PrintDriver that reports through job.
func NewPrint(job *events.Job) *PrintDriver {
	return &PrintDriver{Job: job}
}

// Set validates record, value, and ttl exactly as every other driver does,
// then emits the record to create or update — formatted as an operator
// would write it into a zone file — instead of applying it.
func (d *PrintDriver) Set(ctx context.Context, record, value string, ttl time.Duration) error {
	if err := validateSetArgs(record, value, ttl); err != nil {
		return err
	}
	d.Job.Emit(StepDNSChange, events.StateSucceeded, fmt.Sprintf(
		"no DNS driver configured — apply this record manually: %s %d IN %s %s",
		record, int(ttl.Seconds()), recordType(value), value,
	))
	return nil
}

// Delete validates record exactly as every other driver does, then emits
// the record to remove instead of applying the change. Delete has no type
// or value to report — like every Driver's Delete, it targets every record
// at record, of any type.
func (d *PrintDriver) Delete(ctx context.Context, record string) error {
	if err := validateDeleteArgs(record); err != nil {
		return err
	}
	d.Job.Emit(StepDNSChange, events.StateSucceeded, fmt.Sprintf(
		"no DNS driver configured — remove this record manually: %s (any type)",
		record,
	))
	return nil
}
