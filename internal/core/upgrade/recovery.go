// Package upgrade — this file implements UPGR-002: a failed `upgrade`
// leaves the operator with the pre-upgrade backup and a working path back
// to the pre-upgrade version.
//
// Most of that path already exists and is not rebuilt here. The
// pre-upgrade backup is taken before the pinned version is bumped, so the
// snapshot records the pre-upgrade Forgejo version (upgrade.go's ordering);
// every snapshot embeds the version that wrote it and `restore` boots that
// exact version every time (RSTR-002, spec.md "Version pinning"); and
// restore refuses on a failed verification, naming the specific defect
// (RSTR-003). Restoring the pre-upgrade snapshot therefore *is* the path
// back, and it works today.
//
// What UPGR-002 adds is that a failure neither destroys that snapshot nor
// obscures it:
//
//   - Nothing on any exit path removes it. Upgrade's only cleanup is
//     `defer os.RemoveAll(opts.WorkDir)`, which wipes local scratch, never
//     the destination — and Options.validate now refuses a destination
//     nested inside that work directory, which is the one configuration
//     where those two would be the same directory.
//   - Recovery names where the snapshot is, what version it pins, and the
//     exact command that puts that version back. It rides both the
//     returned error and the CORE-002 event stream, so the dashboard shows
//     the way back as well as the terminal does — a snapshot key that only
//     ever existed in terminal scrollback is not a path an operator can
//     rely on finding.
//
// Recovery deliberately does not revert the bumped pin in the bundle
// directory. A failure after deploy.Up has already restarted Forgejo on
// the new image means Forgejo has already migrated its own schema, and an
// older Forgejo refuses to start against a newer schema: re-pinning the
// bundle would invite `up` to boot straight into that. Restore is the one
// path back that returns the version *and* the pre-migration state
// together, and it re-pins from the snapshot regardless of what the bundle
// currently says (restore.pinnedBundle), so a bumped bundle costs the
// operator nothing there. Recovery states the bundle's pin instead of
// quietly changing it.
package upgrade

import "fmt"

// StepRecoveryPath identifies the event a failed upgrade emits to report
// the path back to the pre-upgrade version (UPGR-002). It is emitted with
// events.StateSucceeded — the step itself completed; the upgrade's own
// failure is the job's terminal event, emitted immediately after.
const StepRecoveryPath = "recovery-path"

// Recovery is the path back to the pre-upgrade version after an upgrade
// fails past the point where the pre-upgrade backup was taken: which
// snapshot, where it lives, which Forgejo version it pins, and the command
// that restores it.
//
// It holds no key material (KEY-003): the snapshot is age-encrypted, and
// the identity that decrypts it resolves through the bundle's keystore
// driver at restore time, exactly as it does for any other restore.
type Recovery struct {
	// Destination is where the pre-upgrade snapshot was written — the
	// same `-to` value the upgrade was given (BKUP-005).
	Destination string

	// SnapshotKey is the key naming the pre-upgrade snapshot at
	// Destination, as returned by backup.Backup. Naming the exact key
	// matters more than "the latest snapshot": anything else writing to
	// the same destination — a cron backup on the golden path — would
	// make "latest" resolve to a different, post-upgrade snapshot.
	SnapshotKey string

	// ForgejoVersion is the pre-upgrade pin the snapshot records, which is
	// the version restoring it boots (RSTR-002).
	ForgejoVersion string

	// Target is the forge host the snapshot restores onto, in
	// ssh://user@host:port form.
	Target string

	// BundleDir is the bundle directory the restore runs against.
	BundleDir string
}

// Command returns the exact `restore` invocation that puts the pre-upgrade
// version back. Deriving it should not be left to the operator: the
// snapshot key is the one argument they cannot reconstruct from anything
// they already have in front of them.
func (r Recovery) Command() string {
	return fmt.Sprintf("farrier restore -bundle %s -target %s -from %s -snapshot %s",
		r.BundleDir, r.Target, r.Destination, r.SnapshotKey)
}

// Detail renders Recovery as the one operator-facing sentence that goes
// into both the failure and the event stream, on a single line so it reads
// the same in a terminal and in the dashboard's event list.
func (r Recovery) Detail() string {
	return fmt.Sprintf(
		"the pre-upgrade backup is intact: snapshot %s at %s, pinning forgejo %s. "+
			"restore it to return to that version (restore boots the version the snapshot pins, "+
			"whatever the bundle now pins): %s",
		r.SnapshotKey, r.Destination, r.ForgejoVersion, r.Command())
}
