// Package dns implements the DNS driver abstraction bundle records are
// changed through (spec.md "DNS drivers"): Set upserts a record, Delete
// removes it. Two drivers ship in-tree — cloudflare (DNS-001) and rfc2136
// (DNS-002) — plus ExecDriver, which reaches a third-party driver through
// the CORE-003 exec protocol instead of an in-tree implementation, the same
// plugin posture used by the keystore and blob packages. PrintDriver
// (DNS-003) satisfies Driver for a bundle with no DNS driver configured,
// reporting the exact record change through the shared event stream instead
// of failing.
//
// A driver's config (Cloudflare's API token, RFC 2136's TSIG secret) is
// secret, so — like blob's S3Config — it is never read from a bundle
// manifest's non-secret DriverRef.Config directly. A caller resolves the
// secret through a keystore driver first, then builds the typed Config a
// driver's constructor takes.
//
// ACME DNS-01 challenge proof (INIT-002) uses lego's own provider set and
// does not go through this interface (tech-spec.md "Driver interfaces").
package dns

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"
)

// BundleTTL is the TTL every bundle DNS record is created with (DNS-004):
// short enough that a DNS flip during promotion (spec.md "Failover")
// propagates within the promotion downtime window. Callers that create or
// update a bundle's own records — the domain at init, the flip at
// promotion — use SetBundleRecord rather than calling a Driver's Set
// directly, so the policy is enforced once instead of at every call site.
const BundleTTL = 60 * time.Second

// Driver sets and deletes DNS records for a bundle's domain. Set takes ttl
// as a parameter rather than hard-coding BundleTTL so the interface also
// serves records outside that policy; SetBundleRecord is the enforced path
// for bundle records themselves.
type Driver interface {
	// Set upserts record: if a record by that name already exists it is
	// replaced (value, type, and ttl), regardless of its previous type;
	// otherwise a new record is created.
	Set(ctx context.Context, record, value string, ttl time.Duration) error

	// Delete removes every record at name, of any type. Deleting a record
	// that does not exist is not an error — callers (e.g. FAIL-004's DNS
	// flip) call Delete idempotently during failover without first
	// checking what, if anything, is there.
	Delete(ctx context.Context, record string) error
}

// SetBundleRecord upserts record on d at BundleTTL (DNS-004). This is the
// path every caller creating or updating a bundle's own DNS record — the
// domain at init, the standby's record during a promotion DNS flip — must
// use instead of d.Set directly, so the 60-second policy holds regardless
// of which driver is configured.
func SetBundleRecord(ctx context.Context, d Driver, record, value string) error {
	return d.Set(ctx, record, value, BundleTTL)
}

// recordType infers the DNS record type Set should write for value: an A
// record for an IPv4 address, AAAA for IPv6, and CNAME for anything else
// (a hostname target). The Driver interface takes no explicit type — a
// bundle only ever points its domain at an IP or another hostname — so
// inferring it from value's shape keeps the interface small without losing
// the ability to write either kind of record.
func recordType(value string) string {
	if ip := net.ParseIP(value); ip != nil {
		if ip.To4() != nil {
			return "A"
		}
		return "AAAA"
	}
	return "CNAME"
}

func validateSetArgs(record, value string, ttl time.Duration) error {
	if strings.TrimSpace(record) == "" {
		return fmt.Errorf("dns: record name is required")
	}
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("dns: record value is required")
	}
	if ttl <= 0 {
		return fmt.Errorf("dns: ttl must be positive")
	}
	return nil
}

func validateDeleteArgs(record string) error {
	if strings.TrimSpace(record) == "" {
		return fmt.Errorf("dns: record name is required")
	}
	return nil
}
