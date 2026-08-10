package initialize

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/keystore"
)

// IncompleteFile is the resume record's filename inside the bundle
// directory. Run writes it immediately before the first piece of key
// material is stored and removes it once the bundle is on disk, so its
// presence means exactly one thing: an `init` for this bundle directory
// got as far as writing key material and did not finish.
//
// It is not part of a bundle. bundle.Exists looks for the manifest and
// compose/ (bundle.go), neither of which this is, so a directory holding
// only this file is still "not initialized" as far as INIT-004 is
// concerned — which is what lets the next `init` run at all.
const IncompleteFile = "init-incomplete.json"

// incompleteSchema versions the record. A record written by a newer
// farrier than the one reading it is refused rather than guessed at: the
// whole point of the file is to decide whether key material may be
// reused, and reading it wrong is the one mistake with a live instance's
// identity on the other side of it.
const incompleteSchema = 1

// incompleteNote is written into the record so an operator who finds the
// file — and no farrier output explaining it, because the process was
// killed — can act on it without reading source.
const incompleteNote = "farrier init stored key material here and did not finish. " +
	"Re-run `farrier init` in this folder: it reuses the key material named below instead of " +
	"generating new material, and removes this file once the bundle is written. " +
	"This file holds no key material, only key names."

// incompleteRecord is what Run leaves behind between the first Store and
// the finished bundle: which pieces of key material this bundle's
// identity is made of, and which keystore target they went to.
//
// It records key *names* and a fingerprint of the keystore target, never
// key material and never the target's config values (KEY-003). The
// fingerprint is enough to answer the only question the record exists to
// answer — "is the keystore I am about to write to the same one the
// unfinished run wrote to?" — without putting a driver's configuration on
// disk a second time.
type incompleteRecord struct {
	Schema int    `json:"schema"`
	Note   string `json:"note"`
	// KeystoreDriver is the driver name, which is not a secret: it is the
	// same string the manifest carries in plain sight.
	KeystoreDriver string `json:"keystoreDriver"`
	// KeystoreFingerprint is a SHA-256 over the whole keystore DriverRef,
	// so a target that differs only in its config (a different directory,
	// a different command) does not match.
	KeystoreFingerprint string `json:"keystoreFingerprint"`
	// Keys are the non-rotating key names the unfinished run intended this
	// instance to own. A name here may or may not actually be in the
	// keystore — the run may have died between any two stores — so the
	// reader checks each one rather than trusting the list.
	Keys []string `json:"keys"`
}

// matches reports whether rec was written for the same keystore target
// params are about to use. A mismatch means the operator pointed init at
// a different keystore between the two runs, and nothing in the new
// target may be treated as this run's own work.
func (rec incompleteRecord) matches(ref bundle.DriverRef, fingerprint string) bool {
	return rec.KeystoreDriver == strings.TrimSpace(ref.Driver) && rec.KeystoreFingerprint == fingerprint
}

// fingerprintDriverRef hashes a driver reference — name and config
// together — into a stable hex digest. encoding/json sorts map keys, so
// the same target always produces the same digest regardless of the order
// the operator passed its config in.
func fingerprintDriverRef(ref bundle.DriverRef) (string, error) {
	raw, err := json.Marshal(struct {
		Driver string         `json:"driver"`
		Config map[string]any `json:"config"`
	}{Driver: strings.TrimSpace(ref.Driver), Config: ref.Config})
	if err != nil {
		return "", fmt.Errorf("initialize: fingerprint keystore target: %w", err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// readIncompleteRecord reads dir's resume record, returning nil when
// there is none. A record that exists but cannot be read or parsed is an
// error, not an absence: treating it as "no unfinished run" would send
// init on to generate a second identity over the top of the first one's
// key material, which is the exact outcome the record exists to prevent.
func readIncompleteRecord(dir string) (*incompleteRecord, error) {
	path := filepath.Join(dir, IncompleteFile)
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("initialize: read %s: %w", path, err)
	}
	var rec incompleteRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return nil, fmt.Errorf("initialize: %s is not readable (%v); it records which key material an unfinished init wrote, and init cannot tell what is safe to reuse without it. It holds no key material, so deleting it is safe once you know whether the keystore target holds key material for this bundle", path, err)
	}
	if rec.Schema != incompleteSchema {
		return nil, fmt.Errorf("initialize: %s has schema %d, and this farrier understands %d; upgrade farrier rather than guessing what an unfinished init wrote", path, rec.Schema, incompleteSchema)
	}
	return &rec, nil
}

// writeIncompleteRecord creates the bundle directory if it isn't there
// and writes the record into it, atomically, so a reader never sees a
// half-written one.
func writeIncompleteRecord(dir string, rec incompleteRecord) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("initialize: create %s: %w", dir, err)
	}
	raw, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("initialize: encode %s: %w", IncompleteFile, err)
	}
	path := filepath.Join(dir, IncompleteFile)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o644); err != nil {
		return fmt.Errorf("initialize: write %s: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("initialize: write %s: %w", path, err)
	}
	return nil
}

// removeIncompleteRecord deletes dir's resume record. An already-absent
// record is success — the record's job is done either way.
func removeIncompleteRecord(dir string) error {
	if err := os.Remove(filepath.Join(dir, IncompleteFile)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("initialize: remove %s: %w", filepath.Join(dir, IncompleteFile), err)
	}
	return nil
}

// identityKeys are the key names whose presence in a keystore means an
// instance's identity is already there: every piece of key material init
// generates that keystore.Rotates says must never be overwritten. The TLS
// certificate and its private key are excluded because they are the one
// declared rotating pair (spec.md "Key material") — a re-run reissues
// them and the keystore accepts the overwrite, so they say nothing about
// whether an identity already exists.
func identityKeys() []string {
	names := make([]string, 0, len(keyMaterialOrder))
	for _, name := range keyMaterialOrder {
		if !keystore.Rotates(name) {
			names = append(names, name)
		}
	}
	return names
}

// preflight is what the validate step learns about the keystore target
// before anything is generated: which key material a previous unfinished
// init already wrote and this run should reuse, and anything the operator
// should be told about the state it found.
type preflight struct {
	// Reuse holds the key names already in the keystore that belong to
	// this bundle directory's unfinished init. Run neither regenerates nor
	// re-stores them, so the rotation guard is never asked to overwrite
	// them and the instance keeps the identity it was already given.
	Reuse map[string]bool
	// Derived is key material this run must store even though it was not
	// generated: the public half of an SSH host key an unfinished init
	// stored without it. See repairSSHPair.
	Derived map[string]keystore.Secret
	// Notes are operator-facing sentences for the CORE-002 stream.
	Notes []string
	// Fingerprint identifies the keystore target these findings are about;
	// it goes into the record Run writes before it stores anything.
	Fingerprint string
}

// inspectKeystore decides, before init generates or spends anything,
// whether the keystore target is empty, holds this bundle directory's own
// unfinished work, or holds somebody else's identity.
//
// The three answers and what each means:
//
//   - Nothing there. The normal first run. Reuse is empty.
//   - Key material there, and dir's resume record says an unfinished init
//     wrote it to this same keystore target. Run reuses it. This is the
//     restart path: no manual deletion, no flag, and nothing is
//     overwritten — the material stays exactly as the first attempt wrote
//     it, so an instance that later comes up has one consistent identity.
//   - Key material there with no record accounting for it. Refused, by
//     name, in the validate step. That material may belong to a live
//     instance whose bundle lives somewhere else, and there is no way from
//     here to tell that case from an abandoned one — so init does not
//     reuse it (it would give two instances one identity) and does not
//     overwrite it (that is what the rotation guard exists to stop). This
//     is the same refusal the keystore guard would have produced at the
//     first Store, moved to before the ACME exchange and given a message
//     that names the recovery.
//
// A Resolve error that is not ErrNotFound stops the preflight rather than
// being read as absence — the same fail-closed reading guardedDriver.Store
// takes (keystore/guard.go).
func inspectKeystore(ctx context.Context, driver keystore.Driver, driverName, dir string, ref bundle.DriverRef) (preflight, error) {
	fingerprint, err := fingerprintDriverRef(ref)
	if err != nil {
		return preflight{}, err
	}
	out := preflight{Reuse: map[string]bool{}, Derived: map[string]keystore.Secret{}, Fingerprint: fingerprint}

	rec, err := readIncompleteRecord(dir)
	if err != nil {
		return preflight{}, err
	}
	recorded := map[string]bool{}
	switch {
	case rec == nil:
	case rec.matches(ref, fingerprint):
		for _, name := range rec.Keys {
			recorded[name] = true
		}
	default:
		out.Notes = append(out.Notes, fmt.Sprintf(
			"%s records an unfinished init that wrote key material to the %s keystore driver, which is not the keystore target this run was given; that key material belongs to no bundle and this run will not touch it",
			filepath.Join(dir, IncompleteFile), rec.KeystoreDriver))
	}

	var conflicts []string
	for _, name := range identityKeys() {
		present, err := keyExists(ctx, driver, name)
		if err != nil {
			return preflight{}, err
		}
		if !present {
			continue
		}
		if recorded[name] {
			out.Reuse[name] = true
			continue
		}
		conflicts = append(conflicts, name)
	}
	if len(conflicts) > 0 {
		return preflight{}, conflictError(driver, driverName, conflicts)
	}
	if err := repairSSHPair(ctx, driver, &out); err != nil {
		return preflight{}, err
	}
	if len(out.Reuse) > 0 {
		out.Notes = append(out.Notes, fmt.Sprintf(
			"resuming an unfinished init: %d piece(s) of key material are already in the %s keystore driver and will be kept as this instance's identity rather than regenerated (%s)",
			len(out.Reuse), driverName, strings.Join(sortedNames(out.Reuse), ", ")))
	}
	return out, nil
}

// repairSSHPair resolves the one way an unfinished init can leave the SSH
// host key torn. storeKeyMaterial writes KeySSHHostKey immediately before
// KeySSHHostKeyPublic (keys.go), so a run that dies between the two leaves
// a private key with no public half. Both are non-rotating, so a retry can
// neither replace the private key nor skip the public one — it derives the
// public half from the private key that is already there, which is the
// only answer that keeps the pair a pair.
//
// The reverse — a public half with no private key — cannot happen from
// that ordering, so it means something outside init wrote to the keystore.
// It is refused rather than guessed at: a fresh private key stored under a
// stale public half would be a host identity no client could verify.
func repairSSHPair(ctx context.Context, driver keystore.Driver, out *preflight) error {
	private, public := out.Reuse[KeySSHHostKey], out.Reuse[KeySSHHostKeyPublic]
	switch {
	case private == public:
		return nil
	case public && !private:
		return fmt.Errorf("initialize: the keystore holds %s with no %s; init cannot store a host key whose published public half belongs to a different key. Remove %s, once you have confirmed no live instance depends on it, and re-run init", KeySSHHostKeyPublic, KeySSHHostKey, KeySSHHostKeyPublic)
	}

	secret, err := driver.Resolve(ctx, KeySSHHostKey)
	if err != nil {
		return fmt.Errorf("initialize: read the stored %s to derive its public half: %w", KeySSHHostKey, err)
	}
	derived, err := sshPublicKeyFor(secret.Reveal())
	if err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	out.Derived[KeySSHHostKeyPublic] = keystore.NewSecret(derived)
	out.Notes = append(out.Notes, fmt.Sprintf(
		"the unfinished init stored %s without %s; deriving the public half from the stored host key rather than replacing the pair",
		KeySSHHostKey, KeySSHHostKeyPublic))
	return nil
}

// keyExists reports whether keyName already has key material behind
// driver. Only a positive ErrNotFound counts as absence; every other
// error is returned, because "I could not tell" must never become "go
// ahead and write" (keystore.ErrNotFound).
func keyExists(ctx context.Context, driver keystore.Driver, keyName string) (bool, error) {
	switch _, err := driver.Resolve(ctx, keyName); {
	case err == nil:
		return true, nil
	case errors.Is(err, keystore.ErrNotFound):
		return false, nil
	default:
		return false, fmt.Errorf("initialize: keystore: could not check whether %s already exists: %w", keyName, err)
	}
}

// conflictError is the message an operator gets when the keystore target
// already holds an instance's identity that this bundle directory has no
// claim on. It names the keys, names where they are, and gives both ways
// forward — the cheap safe one first.
func conflictError(driver keystore.Driver, driverName string, conflicts []string) error {
	where := driverName + " keystore driver"
	if target := keystore.Target(driver, conflicts[0]); target != "" {
		where = fmt.Sprintf("%s (%s)", where, target)
	}
	return fmt.Errorf(
		"initialize: the %s already holds key material that no bundle in this folder accounts for: %s. "+
			"Key material is an instance's identity and never rotates, so init will neither adopt it (two instances would share one identity) nor overwrite it. "+
			"Point this bundle at a keystore target of its own, or — only once you have confirmed no live instance depends on it — remove that key material and re-run init",
		where, strings.Join(conflicts, ", "))
}

// sortedNames renders a name set in a stable order for an operator-facing
// message.
func sortedNames(set map[string]bool) []string {
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// recoveryHint is the sentence appended to every failure that happens
// after key material has been written: the operator's next move, stated
// where they will actually read it.
func recoveryHint(dir string) string {
	return fmt.Sprintf(
		"key material was already written to the keystore, and %s now records it — re-run `farrier init` with the same keystore target and it will reuse that key material rather than generate new material; nothing needs to be deleted by hand",
		filepath.Join(dir, IncompleteFile))
}
