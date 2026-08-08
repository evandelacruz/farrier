# Branch: claude/bkup-004

Implements **BKUP-004**: `backup` must verify the snapshot at creation and
exit nonzero, naming the specific defect, when verification fails.

New `internal/core/backup.Verify(ctx, dir, manifest, keyNames)` runs the
three checks tech-spec.md "What the system owns: verified restores"
defines — the same code path restore (RSTR-003) will reuse once it exists:

- **Completeness:** a known checksum algorithm, exactly one database
  component, every expected key name captured.
- **Checksums:** every component's file on disk, recomputed, matches what
  the manifest recorded at capture time.
- **Cross-consistency:** opens the snapshot's own captured `db.sqlite` and
  checks Forgejo's `repository` rows and `lfs_meta_object`/`attachment`
  references each resolve to a component this same snapshot captured. CI
  artifacts and avatars aren't cross-checked yet — their storage-key
  convention isn't established anywhere in this codebase, so this is a
  documented scope limit rather than a guess dressed up as a check.

`Verify` aggregates every defect into one `*VerifyError` instead of
stopping at the first, so one verify pass reports everything wrong.
`backup.Run` calls it right after writing `snapshot-manifest.json`
(`StepVerify`) and fails the job — naming every defect — if it returns an
error.

Encryption (BKUP-003) is still unbuilt, so `Verify` runs today against the
plain snapshot `Run` produces, not the encrypted archive tech-spec.md's
capture order (`checksum → encrypt → verify → write`) ultimately calls
for. Flagged in tech-spec.md: whoever lands BKUP-003 needs to move the
`Verify` call to run against the encrypted archive's decrypted form, not
leave it checking the pre-encryption snapshot alone.

Docs updated: `docs/tech-spec.md` (new "Snapshot verification" section,
capture-order note), `docs/status.json` (BKUP-004 landed).

Core-only: no CLI/API wiring yet (API-001 still tracks that separately,
gated on BKUP-003/BKUP-005 too).

This file exists for conductor tracking and can be deleted once the PR
merges.
