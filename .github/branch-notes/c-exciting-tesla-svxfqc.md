# Branch: c/exciting-tesla-svxfqc

Tracking branch for a destructive defect in `drill` (DRIL-003): every
Farrier deployment used the single hardcoded Compose project name
`farrier`, so a drill's teardown on a host that also runs a live instance
stopped and removed the live instance's containers.

The Compose project name is now a per-deployment identity, pinned on the
host at first converge and read back by every command that addresses the
deployment. Deployments already on disk under the old constant keep it.

See `docs/functional-requirements.md` § DRIL and `docs/tech-spec.md`
"Host state layout".
