package initialize

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/events"
	"github.com/evandelacruz/farrier/internal/core/forge"
	"github.com/evandelacruz/farrier/internal/core/keystore"
)

// scriptedKeystore is a command keystore (KEY-002) backed by a directory,
// whose store side fails for any key with a matching ".deny-<key>" marker
// file beside it. Two runs against it use byte-identical driver config —
// which matters, because the resume record only licenses reuse when the
// keystore target is the same one the unfinished run wrote to, and a
// target that differs by so much as a config value must not match.
//
// The command driver is used rather than the file driver precisely
// because its config carries shell, so a test can make one store call
// fail the way a real secret manager fails: mid-run, on one key, with the
// keys before it already written.
func scriptedKeystore(t *testing.T) (ref bundle.DriverRef, keysDir string) {
	t.Helper()
	keysDir = t.TempDir()
	return bundle.DriverRef{Driver: "command", Config: map[string]any{
		"command":      fmt.Sprintf(`cat %q/"$FARRIER_KEY_NAME" 2>/dev/null || true`, keysDir),
		"storeCommand": fmt.Sprintf(`if [ -f %q/".deny-$FARRIER_KEY_NAME" ]; then echo "the vault said no" >&2; exit 1; fi; cat > %q/"$FARRIER_KEY_NAME"`, keysDir, keysDir),
	}}, keysDir
}

func denyStore(t *testing.T, keysDir, keyName string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(keysDir, ".deny-"+keyName), nil, 0o644); err != nil {
		t.Fatalf("deny %s: %v", keyName, err)
	}
}

func allowStore(t *testing.T, keysDir, keyName string) {
	t.Helper()
	if err := os.Remove(filepath.Join(keysDir, ".deny-"+keyName)); err != nil {
		t.Fatalf("allow %s: %v", keyName, err)
	}
}

func storedKey(t *testing.T, keysDir, keyName string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(keysDir, keyName))
	if err != nil {
		t.Fatalf("read stored %s: %v", keyName, err)
	}
	return string(raw)
}

func readRecord(t *testing.T, dir string) incompleteRecord {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, IncompleteFile))
	if err != nil {
		t.Fatalf("read resume record: %v", err)
	}
	var rec incompleteRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatalf("decode resume record: %v", err)
	}
	return rec
}

// The defect this package was fixed for: init stored key material, failed
// on a later step, and left the operator with a keystore full of
// non-rotating key material, no bundle, and no way to re-run init. Now the
// second init reuses exactly what the first one wrote.
func TestRunResumesAfterAKeystoreFailureMidwayThroughStoring(t *testing.T) {
	ref, keysDir := scriptedKeystore(t)
	params := validParams(t, &fakeResolver{})
	params.Keystore = ref
	dir := BundleDir(params)
	denyStore(t, keysDir, KeyAgeBackupKey)

	_, err := Run(context.Background(), events.NewJob(), params)
	if err == nil {
		t.Fatal("Run: want the first attempt to fail on the denied store, got nil")
	}
	if !strings.Contains(err.Error(), "re-run `farrier init`") {
		t.Errorf("error = %v, want it to name re-running init as the recovery", err)
	}
	if !strings.Contains(err.Error(), IncompleteFile) {
		t.Errorf("error = %v, want it to name %s", err, IncompleteFile)
	}
	if exists, _ := bundle.Exists(dir); exists {
		t.Fatal("a failed init wrote a bundle")
	}
	firstSecretKey := storedKey(t, keysDir, forge.KeySecretKey)
	firstHostKey := storedKey(t, keysDir, KeySSHHostKey)

	rec := readRecord(t, dir)
	if rec.Schema != incompleteSchema || rec.KeystoreDriver != "command" {
		t.Errorf("record = %+v, want schema %d and the command driver", rec, incompleteSchema)
	}
	if len(rec.Keys) != len(identityKeys()) {
		t.Errorf("record keys = %v, want every non-rotating key name %v", rec.Keys, identityKeys())
	}

	// The operator's whole recovery: fix the keystore, run init again. No
	// deletion, no flag.
	allowStore(t, keysDir, KeyAgeBackupKey)
	job := events.NewJob()
	b, err := Run(context.Background(), job, params)
	if err != nil {
		t.Fatalf("Run (second attempt): %v", err)
	}
	if b == nil {
		t.Fatal("Run (second attempt) returned no bundle")
	}

	if got := storedKey(t, keysDir, forge.KeySecretKey); got != firstSecretKey {
		t.Error("the second init replaced SECRET_KEY; key material is non-rotating and must survive the retry")
	}
	if got := storedKey(t, keysDir, KeySSHHostKey); got != firstHostKey {
		t.Error("the second init replaced the SSH host key; existing known_hosts entries would break")
	}
	if got := storedKey(t, keysDir, KeyAgeBackupKey); strings.TrimSpace(got) == "" {
		t.Error("the second init did not store the age backup key the first attempt failed on")
	}
	if _, err := os.Stat(filepath.Join(dir, IncompleteFile)); !os.IsNotExist(err) {
		t.Errorf("stat resume record after success: %v, want it removed once the bundle is written", err)
	}

	var resumed bool
	for _, ev := range job.Events() {
		if strings.Contains(ev.Detail, "resuming an unfinished init") {
			resumed = true
		}
	}
	if !resumed {
		t.Error("the resumed run never said it was resuming; the operator should not have to infer it")
	}
}

// INIT-006 on the resumed run: key material an earlier attempt stored is
// still this instance's identity, so the report names all of it — the age
// key warning included. An operator who only sees the successful run must
// not be told about two of nine keys.
func TestResumedRunReportsReusedKeyMaterialAndTheAgeWarning(t *testing.T) {
	ref, keysDir := scriptedKeystore(t)
	params := validParams(t, &fakeResolver{})
	params.Keystore = ref
	denyStore(t, keysDir, KeyAgeBackupKey)
	if _, err := Run(context.Background(), events.NewJob(), params); err == nil {
		t.Fatal("Run: want the first attempt to fail")
	}
	allowStore(t, keysDir, KeyAgeBackupKey)

	job := events.NewJob()
	if _, err := Run(context.Background(), job, params); err != nil {
		t.Fatalf("Run (second attempt): %v", err)
	}

	details := reportDetails(job)
	for _, name := range allKeyNames {
		var found bool
		for _, detail := range details {
			if strings.HasPrefix(detail, name+" ") {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("no report event for %s on the resumed run; events = %q", name, details)
		}
	}
	if len(details) == 0 || !strings.Contains(details[len(details)-1], "unrecoverable") {
		t.Errorf("last report event = %q, want the age backup key warning last", details)
	}
	var kept bool
	for _, detail := range details {
		if strings.HasPrefix(detail, forge.KeySecretKey+" ") && strings.Contains(detail, "kept from an earlier unfinished init") {
			kept = true
		}
	}
	if !kept {
		t.Error("reused key material was reported as if this run had written it")
	}
}

// storeKeyMaterial writes the host key immediately before its public half,
// so a run that dies between the two leaves a private key with no public
// half. Both are non-rotating, so the retry can neither replace the
// private key nor leave the public half missing: it derives the public
// half from the key already in the keystore.
func TestRunDerivesTheSSHPublicHalfWhenAnUnfinishedRunStoredOnlyThePrivate(t *testing.T) {
	ref, keysDir := scriptedKeystore(t)
	params := validParams(t, &fakeResolver{})
	params.Keystore = ref
	denyStore(t, keysDir, KeySSHHostKeyPublic)

	if _, err := Run(context.Background(), events.NewJob(), params); err == nil {
		t.Fatal("Run: want the first attempt to fail on the denied store")
	}
	private := storedKey(t, keysDir, KeySSHHostKey)
	if _, err := os.Stat(filepath.Join(keysDir, KeySSHHostKeyPublic)); !os.IsNotExist(err) {
		t.Fatalf("stat %s: %v, want the first attempt to have failed before storing it", KeySSHHostKeyPublic, err)
	}

	allowStore(t, keysDir, KeySSHHostKeyPublic)
	if _, err := Run(context.Background(), events.NewJob(), params); err != nil {
		t.Fatalf("Run (second attempt): %v", err)
	}

	if got := storedKey(t, keysDir, KeySSHHostKey); got != private {
		t.Fatal("the second init replaced the host key instead of completing the pair")
	}
	signer, err := ssh.ParsePrivateKey([]byte(private))
	if err != nil {
		t.Fatalf("parse the stored host key: %v", err)
	}
	want := string(ssh.MarshalAuthorizedKey(signer.PublicKey()))
	if got := storedKey(t, keysDir, KeySSHHostKeyPublic); got != want {
		t.Errorf("stored public half = %q, want the public half of the stored private key %q", got, want)
	}
}

// The safety the whole design turns on: key material with no record
// accounting for it may belong to a live instance whose bundle lives
// elsewhere, so init neither adopts it nor overwrites it. The refusal is
// in the validate step, before an ACME exchange is spent on a run that
// was never going to finish.
func TestRunRefusesAKeystoreHoldingUnaccountedKeyMaterial(t *testing.T) {
	keysDir := t.TempDir()
	if err := (keystore.FileDriver{Path: keysDir}).Store(context.Background(), forge.KeySecretKey, keystore.NewSecret("a-live-instance")); err != nil {
		t.Fatalf("seed the keystore: %v", err)
	}
	prover := &fakeProver{}
	params := validParams(t, &fakeResolver{})
	params.Prover = prover
	params.Keystore = bundle.DriverRef{Driver: "file", Config: map[string]any{"path": keysDir}}
	job := events.NewJob()

	_, err := Run(context.Background(), job, params)
	if err == nil {
		t.Fatal("Run: want a refusal when the keystore already holds key material, got nil")
	}
	if !strings.Contains(err.Error(), forge.KeySecretKey) {
		t.Errorf("error = %v, want it to name the key material it found", err)
	}
	if !strings.Contains(err.Error(), "keystore target of its own") {
		t.Errorf("error = %v, want it to name a way forward", err)
	}
	if len(prover.calls) != 0 {
		t.Errorf("prover calls = %v, want the refusal before any ACME exchange is spent", prover.calls)
	}
	if got := storedKey(t, keysDir, forge.KeySecretKey); got != "a-live-instance" {
		t.Errorf("stored SECRET_KEY = %q, want the refused run to have left it alone", got)
	}
	assertJobFailed(t, job)
}

// A record only licenses reuse for the keystore target it was written
// for. Pointed at a different one, it proves nothing about what is in
// that target — and reusing on the strength of it would give two
// instances one identity.
func TestRunRefusesReuseWhenTheRecordNamesADifferentKeystoreTarget(t *testing.T) {
	keysDir := t.TempDir()
	if err := (keystore.FileDriver{Path: keysDir}).Store(context.Background(), forge.KeySecretKey, keystore.NewSecret("somebody-elses")); err != nil {
		t.Fatalf("seed the keystore: %v", err)
	}
	params := validParams(t, &fakeResolver{})
	params.Keystore = bundle.DriverRef{Driver: "file", Config: map[string]any{"path": keysDir}}
	dir := BundleDir(params)
	if err := writeIncompleteRecord(dir, incompleteRecord{
		Schema:              incompleteSchema,
		Note:                incompleteNote,
		KeystoreDriver:      "file",
		KeystoreFingerprint: strings.Repeat("0", 64),
		Keys:                identityKeys(),
	}); err != nil {
		t.Fatalf("write the record: %v", err)
	}

	_, err := Run(context.Background(), events.NewJob(), params)
	if err == nil {
		t.Fatal("Run: want a refusal when the record was written for another keystore target, got nil")
	}
	if !strings.Contains(err.Error(), forge.KeySecretKey) {
		t.Errorf("error = %v, want it to name the key material it would have had to overwrite", err)
	}
}

// A record farrier cannot read is not the same as no record: reading it as
// an absence would send init on to mint a second identity over the top of
// the first one's key material.
func TestRunRefusesAnUnreadableResumeRecord(t *testing.T) {
	params := validParams(t, &fakeResolver{})
	dir := BundleDir(params)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, IncompleteFile), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("write the record: %v", err)
	}

	_, err := Run(context.Background(), events.NewJob(), params)
	if err == nil {
		t.Fatal("Run: want a refusal for an unreadable resume record, got nil")
	}
	if !strings.Contains(err.Error(), IncompleteFile) {
		t.Errorf("error = %v, want it to name the file", err)
	}
}

// Cancellation is a failure like any other (the posture BKUP-002 and
// DRIL-003 already take): the record stays behind, and the error says the
// retry is just init again.
func TestRunLeavesTheRecordWhenTheContextIsCanceledMidStore(t *testing.T) {
	keysDir := t.TempDir()
	params := validParams(t, &fakeResolver{})
	params.Keystore = bundle.DriverRef{Driver: "command", Config: map[string]any{
		"command": fmt.Sprintf(`cat %q/"$FARRIER_KEY_NAME" 2>/dev/null || true`, keysDir),
		"storeCommand": fmt.Sprintf(
			`if [ "$FARRIER_KEY_NAME" = %q ]; then sleep 30; fi; cat > %q/"$FARRIER_KEY_NAME"`,
			KeyAgeBackupKey, keysDir),
	}}
	dir := BundleDir(params)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_, err := Run(ctx, events.NewJob(), params)
	if err == nil {
		t.Fatal("Run: want an error when the context is canceled mid-store, got nil")
	}
	if !strings.Contains(err.Error(), "re-run `farrier init`") {
		t.Errorf("error = %v, want it to name re-running init as the recovery", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, IncompleteFile)); statErr != nil {
		t.Errorf("stat resume record: %v, want a canceled run to leave it behind", statErr)
	}
}

// A clean first run leaves nothing behind: the record exists only for the
// window between the first Store and the finished bundle.
func TestRunLeavesNoRecordBehindOnSuccess(t *testing.T) {
	params := validParams(t, &fakeResolver{})
	if _, err := Run(context.Background(), events.NewJob(), params); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(BundleDir(params), IncompleteFile)); !os.IsNotExist(err) {
		t.Errorf("stat resume record: %v, want no record after a clean run", err)
	}
}

// The record is not a bundle, so INIT-004 does not mistake it for one —
// which is what lets the retry run at all.
func TestTheResumeRecordIsNotABundle(t *testing.T) {
	dir := t.TempDir()
	if err := writeIncompleteRecord(dir, incompleteRecord{Schema: incompleteSchema, Note: incompleteNote}); err != nil {
		t.Fatalf("write the record: %v", err)
	}
	exists, err := bundle.Exists(dir)
	if err != nil {
		t.Fatalf("bundle.Exists: %v", err)
	}
	if exists {
		t.Error("bundle.Exists treated a resume record as a bundle; INIT-004 would refuse the retry")
	}
}

// A Save that got part-way leaves a torn bundle, which INIT-004 counts as
// a bundle and refuses. The refusal has to stop telling the operator to
// remove the folder, because the folder now also holds the record their
// key material's recovery depends on.
func TestRunRefusingATornBundleTellsTheOperatorToKeepTheRecord(t *testing.T) {
	params := validParams(t, &fakeResolver{})
	dir := BundleDir(params)
	if err := os.MkdirAll(filepath.Join(dir, bundle.ComposeDir), 0o755); err != nil {
		t.Fatalf("stage a torn bundle: %v", err)
	}
	if err := writeIncompleteRecord(dir, incompleteRecord{Schema: incompleteSchema, Note: incompleteNote}); err != nil {
		t.Fatalf("write the record: %v", err)
	}

	_, err := Run(context.Background(), events.NewJob(), params)
	if err == nil {
		t.Fatal("Run: want INIT-004's refusal for a torn bundle, got nil")
	}
	if !strings.Contains(err.Error(), "keep "+IncompleteFile) {
		t.Errorf("error = %v, want it to say the resume record must be kept", err)
	}
	if !strings.Contains(err.Error(), bundle.ComposeDir) {
		t.Errorf("error = %v, want it to name what to remove", err)
	}
}

// KEY-003: the record is written to disk in the project folder, next to
// code that may well be committed. It carries key names, never key
// material, and never the keystore target's config values either.
func TestTheResumeRecordCarriesNoKeyMaterial(t *testing.T) {
	ref, keysDir := scriptedKeystore(t)
	params := validParams(t, &fakeResolver{})
	params.Keystore = ref
	denyStore(t, keysDir, KeyAgeBackupKey)
	if _, err := Run(context.Background(), events.NewJob(), params); err == nil {
		t.Fatal("Run: want the first attempt to fail")
	}

	raw, err := os.ReadFile(filepath.Join(BundleDir(params), IncompleteFile))
	if err != nil {
		t.Fatalf("read the record: %v", err)
	}
	for _, name := range identityKeys() {
		if name == KeyAgeBackupKey {
			continue // the run failed before storing it
		}
		value := strings.TrimSpace(storedKey(t, keysDir, name))
		if value == "" {
			t.Fatalf("%s stored empty, cannot check it does not leak", name)
		}
		if strings.Contains(string(raw), value) {
			t.Errorf("the resume record leaked the value of %s", name)
		}
	}
	if strings.Contains(string(raw), keysDir) {
		t.Error("the resume record wrote the keystore target's config into the project folder; a fingerprint is all it needs")
	}
}

// Two keystore targets that differ only in config are different targets.
// The fingerprint has to see that, or a record would license reuse of key
// material it knows nothing about.
func TestKeystoreFingerprintDistinguishesTargetsAndIgnoresConfigOrder(t *testing.T) {
	one, err := fingerprintDriverRef(bundle.DriverRef{Driver: "file", Config: map[string]any{"path": "/a", "mode": "x"}})
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	same, err := fingerprintDriverRef(bundle.DriverRef{Driver: "file", Config: map[string]any{"mode": "x", "path": "/a"}})
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	if one != same {
		t.Error("the same target fingerprinted differently depending on config order")
	}
	other, err := fingerprintDriverRef(bundle.DriverRef{Driver: "file", Config: map[string]any{"path": "/b", "mode": "x"}})
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	if one == other {
		t.Error("two different keystore directories fingerprinted the same")
	}
}

// The panic path. fail() covers every exit with a return value; a panic
// unwinding through Run has none, and would otherwise leave the operator
// with key material in a keystore and a stream that just stops.
func TestAnnounceIncompleteReportsARunThatEndedWithoutATerminalEvent(t *testing.T) {
	job := events.NewJob()
	announceIncomplete(job, nil, nil, ".farrier")

	evts := job.Events()
	if len(evts) != 1 || evts[0].State != events.StateFailed {
		t.Fatalf("events = %+v, want one failure event", evts)
	}
	if !strings.Contains(evts[0].Detail, IncompleteFile) {
		t.Errorf("detail = %q, want it to name the resume record", evts[0].Detail)
	}
	if !job.Done() {
		t.Error("the job is still open after announcing an unfinished init")
	}
}

// It must also stay out of the way: emitting after a terminal event
// panics, and both a successful run and a failed one have already closed
// the stream by the time the deferred call runs.
func TestAnnounceIncompleteStaysQuietOnRunsThatAlreadyReported(t *testing.T) {
	success := events.NewJob()
	success.Succeeded("done")
	announceIncomplete(success, &bundle.Bundle{}, nil, ".farrier")
	if len(success.Events()) != 1 {
		t.Errorf("events = %+v, want the successful run's stream untouched", success.Events())
	}

	failed := events.NewJob()
	failed.Failed("the vault said no")
	announceIncomplete(failed, nil, os.ErrPermission, ".farrier")
	if len(failed.Events()) != 1 {
		t.Errorf("events = %+v, want the failed run's stream untouched", failed.Events())
	}
}
