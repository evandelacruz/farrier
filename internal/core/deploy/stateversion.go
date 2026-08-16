package deploy

import (
	"context"
	"fmt"
	"path"
	"strings"
)

// stateVersionFile is the file under stateDir recording which Forgejo image
// the state in that directory has been started under (tech-spec.md "Host
// state layout"). It lives beside the state it describes, not in the bundle
// directory, because it is a property of this host's database — two hosts
// restored from the same bundle can legitimately carry different values.
const stateVersionFile = "forgejo-version"

// StateVersionPath returns the host-side path of that record. It is the
// single spelling of this layout decision, the same way GitStatePath and
// GiteaStatePath are for the directories they name.
func StateVersionPath(remoteDir string) string {
	return path.Join(remoteDir, stateDir, stateVersionFile)
}

// ReadStateVersion returns the Forgejo image recorded at
// StateVersionPath(remoteDir), or "" when no record exists there.
//
// An empty result means *unknown*, never "no version" — it is what both a
// fresh host and an instance deployed before this record existed return, and
// the two are indistinguishable from here. checkStateVersion treats unknown
// as permission to proceed for exactly that reason: refusing would break
// every already-deployed instance on its next `up` (UP-003).
// A read that fails is an error rather than an absence: reading a transport
// or shell failure as "no record" is how a migration would be waved through
// (readOptionalFile).
func ReadStateVersion(ctx context.Context, host Host, remoteDir string) (string, error) {
	out, err := readOptionalFile(ctx, host, StateVersionPath(remoteDir))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// RecordStateVersion writes image to StateVersionPath(remoteDir).
//
// Up calls this immediately before converging, not after: converge is what
// starts Forgejo on that image, and Forgejo migrates the database as part of
// starting. Recording first means a converge that fails partway still leaves
// the pessimistic record — "this state may already have been opened by this
// version" — which is the honest thing to tell the next `up`. Recording
// afterward would leave a migrated database labelled with the version that
// no longer wrote it, and the next `up` would wave the mismatch through.
//
// restore.Restore calls it too, for the database it places: it stamps the
// version the snapshot recorded, which is the version restore then boots
// (RSTR-002), so the check below passes by construction rather than by
// exemption.
func RecordStateVersion(ctx context.Context, host Host, remoteDir, image string) error {
	if err := host.WriteFile(ctx, StateVersionPath(remoteDir), []byte(image+"\n"), 0o644); err != nil {
		return fmt.Errorf("record %s: %w", StateVersionPath(remoteDir), err)
	}
	return nil
}

// checkStateVersion is UPGR-003's enforcement point: schema migrations run
// during `upgrade` and at no other time.
//
// Forgejo migrates its own database whenever it starts on a version newer
// than the one that wrote it, so "when does a migration happen" is entirely
// decided by which image is started against which state. Every other path in
// this codebase already answers that safely — restore, and therefore promote
// and drill, boot the exact version the snapshot recorded (spec.md "Version
// pinning") — which leaves `up`: it converges the host to whatever forge
// image the bundle pins, and a bundle's pin can change without `upgrade`
// ever running, by hand-editing farrier.yaml. That `up` would migrate a live
// database with no pre-upgrade backup, no health gate, and no path back —
// exactly what UPGR-001 and UPGR-002 exist to provide.
//
// So a deployment that would start a different Forgejo image than the host's
// state was last started under is refused unless the caller declares itself
// the migration path (Options.Migrate, set only by internal/core/upgrade).
// The refusal names both versions and the command that is allowed to change
// them, the same "name the reason and the way out" posture RSTR-003 and
// UPGR-002 take.
func checkStateVersion(ctx context.Context, host Host, pinned, remoteDir string, migrate bool) (string, error) {
	recorded, err := ReadStateVersion(ctx, host, remoteDir)
	if err != nil {
		return "", err
	}

	switch {
	case migrate:
		// upgrade has already taken the pre-upgrade backup and gated on
		// instance health by the time it gets here (UPGR-001).
	case recorded == "" || recorded == pinned:
		// Nothing to migrate: either the host has no record to compare
		// against, or it is already running what the bundle pins.
	default:
		return "", fmt.Errorf(
			"forge state on this host was last started under forgejo %s, but the bundle pins %s: "+
				"starting a different version against an existing database runs schema migrations, which only `farrier upgrade` may do. "+
				"Run `farrier upgrade -image %s`, which backs the instance up first and leaves a path back, or restore the bundle's forgejo pin to %s",
			recorded, pinned, pinned, recorded)
	}

	if err := RecordStateVersion(ctx, host, remoteDir, pinned); err != nil {
		return "", err
	}

	switch {
	case recorded == "":
		return fmt.Sprintf("no prior record on this host; state now pinned to forgejo %s", pinned), nil
	case recorded == pinned:
		return fmt.Sprintf("state already started under forgejo %s; no migration", pinned), nil
	default:
		return fmt.Sprintf("migrating state from forgejo %s to %s", recorded, pinned), nil
	}
}
