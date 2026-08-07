# Branch: c/upbeat-brown-i4n8jv

Tracking branch for DNS-003 — with no DNS driver configured, `Set`/`Delete`
never fail: `PrintDriver` reports the exact record to apply or remove
through the CORE-002 event stream instead, so the operator applies the
change by hand.

See `docs/functional-requirements.md` § DNS and `docs/status.json`.
