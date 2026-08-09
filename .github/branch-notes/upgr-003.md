# Branch: claude/upgr-003

Implements **UPGR-003**: schema migrations must run during `upgrade` and at
no other time.

Half the requirement is already structural. Forgejo migrates only when it
starts on a newer version than the database it opens, and `restore` always
boots the exact Forgejo version the snapshot recorded (RSTR-002, spec.md
"Version pinning"). `promote` and `drill` both compose `restore.Restore`,
so they inherit that pin rather than choosing their own. Those paths never
migrate, and the work there is proving it, not adding machinery.

The gap is `up`. `deploy.Up` converges the host to whatever forge image the
bundle pins and never relates that image to the version that wrote the
database already on the host, so an edited `farrier.yaml` plus `farrier up`
starts Forgejo on a newer version over an existing database — a migration
outside `upgrade`, with no pre-upgrade backup, no health gate, and no
recovery path.

Work:

1. **Record the version the host's state was last started under**, on the
   host beside the state it describes.
2. **Refuse in `deploy.Up`** when the bundle pins a different forge image
   than that record, naming both versions and the `upgrade` command that is
   the sanctioned way to change it.
3. **`upgrade` is the one caller that may migrate**, and says so explicitly.
   `restore` re-stamps the record to the snapshot's version alongside the
   database it places, so its own deploy matches by construction.
4. **Prove it.** Tests that a restore, a promote, and a drill deploy the
   snapshot's pinned version and never migrate, and that `upgrade` is the
   one path that changes the recorded version.

Nothing about snapshot verification, the four-kind state model, or the
identity model changes.

See `docs/functional-requirements.md` § UPGR and `docs/status.json`.

This file exists for conductor tracking and can be deleted once the PR
merges.
