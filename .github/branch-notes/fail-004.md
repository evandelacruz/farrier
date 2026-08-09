# Branch: claude/fail-004

Implements **FAIL-004**: `promote` must apply the DNS record change through
a configured driver, or print the exact change.

FAIL-001 (PR #65) built `internal/core/promote`'s DNS flip as part of its
four-step sequence: `flipDNS` calls `dns.SetBundleRecord` against a
`dns.Driver` resolved by `promote.ResolveDNSDriver`, which returns a real
driver (`cloudflare`/`rfc2136`/exec, DNS-001/002) for a configured bundle or
`dns.NewPrint(job)` (DNS-003) otherwise, reporting through the CORE-002
event stream either way. That PR deliberately left FAIL-004 as a separate
ID — "promote's sequence reuses their underlying mechanisms without adding
their own acceptance bar" — but the acceptance bar for FAIL-004 was, in
fact, already built alongside it: `TestPromoteEndToEnd` (configured driver,
DNS-004 TTL), `TestPromoteDNSPrintFallback` (print path), and
`TestPromoteCustomDNSRecord`, plus `dns_test.go`'s full
`TestResolveDNSDriver*` suite covering the driver switch and its error
paths, at the core, CLI (`cmd/farrier promote`), and API (`POST /promote`)
layers.

This PR verifies that coverage against the FAIL-004 text, confirms nothing
is missing, and flips `docs/status.json`.

See `docs/functional-requirements.md` § FAIL and `docs/status.json`.

This file exists for conductor tracking and can be deleted once the PR merges.
