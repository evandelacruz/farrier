# Branch: c/gracious-curie-wfbwes

Tracking branch for DNS-004 — enforce the 60-second TTL policy on every
bundle DNS record: an exported `dns.BundleTTL` constant and a
`dns.SetBundleRecord` helper that always creates records at that TTL,
regardless of the driver behind it.

See `docs/functional-requirements.md` § DNS and `docs/status.json`.
