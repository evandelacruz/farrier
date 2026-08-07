// Package dns implements the DNS driver abstraction bundle records are
// changed through (spec.md "DNS drivers"): Set upserts a record, Delete
// removes it. Two drivers ship in-tree — cloudflare (DNS-001) and rfc2136
// (DNS-002) — plus ExecDriver, which reaches a third-party driver through
// the CORE-003 exec protocol instead of an in-tree implementation, the same
// plugin posture used by the keystore and blob packages.
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

// Driver sets and deletes DNS records for a bundle's domain. Every bundle
// record is created with a 60-second TTL (DNS-004), though ttl is a
// parameter here rather than a constant so callers stay free to use a
// different value for records outside that policy.
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
