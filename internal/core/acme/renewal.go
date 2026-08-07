package acme

import "time"

// RenewalFraction is the point in a certificate's lifetime at which it
// becomes due for renewal: lego's own convention of renewing at two-thirds
// of lifetime (tech-spec "Operational targets").
const RenewalFraction = 2.0 / 3.0

// ExpiryWarningWindow is how close to expiry status must start warning
// (tech-spec: "status warns when a cert is inside 14 days of expiry").
const ExpiryWarningWindow = 14 * 24 * time.Hour

// NeedsRenewal reports whether cert is due for renewal at now: two-thirds
// of the way through its validity window (ACME-002).
func NeedsRenewal(cert *Certificate, now time.Time) bool {
	lifetime := cert.NotAfter.Sub(cert.NotBefore)
	renewAt := cert.NotBefore.Add(time.Duration(float64(lifetime) * RenewalFraction))
	return !now.Before(renewAt)
}

// ExpiryWarning reports whether cert is within ExpiryWarningWindow of
// expiring at now, and how long remains — what `status` uses to warn an
// operator a certificate is approaching expiry (ACME-002).
func ExpiryWarning(cert *Certificate, now time.Time) (warn bool, remaining time.Duration) {
	remaining = cert.NotAfter.Sub(now)
	return remaining <= ExpiryWarningWindow, remaining
}

// EnsureValid returns a certificate valid for cfg.Domain: existing
// unchanged if it isn't yet due for renewal, or a freshly issued one
// otherwise (existing nil counts as due). renewed reports whether Issue
// was called. This is the automatic-renewal entry point (ACME-002): a
// caller invokes it idempotently — e.g. from the same cron entry that
// drives backup's golden path — and it only reaches the ACME server when
// issuance or renewal is actually due.
func EnsureValid(cfg Config, existing *Certificate, now time.Time) (cert *Certificate, renewed bool, err error) {
	if existing != nil && !NeedsRenewal(existing, now) {
		return existing, false, nil
	}
	cert, err = Issue(cfg)
	return cert, true, err
}
