package deploy

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/keystore"
	"github.com/evandelacruz/farrier/internal/core/state"
)

// stateOwnerFile is the file under stateDir recording which instance the
// forge state in that directory belongs to (tech-spec.md "Host state
// layout"). It sits beside forgejo-version, and for the same reason: it
// describes this host's state rather than the bundle, and only the host can
// answer the question it exists to answer.
const stateOwnerFile = "owner"

// Field names in that record. It is a small text file of "<field>: <value>"
// lines rather than one bare value, because two things have to be in it:
// the key the comparison is actually made on, and a name for the operator
// to recognize the instance by. Unknown fields are ignored on read, so a
// later version may add one without a record written today becoming
// unreadable.
const (
	ownerKeyField    = "ssh-host-key"
	ownerDomainField = "domain"
)

// StateOwnerPath returns the host-side path of that record — the single
// spelling of this layout decision, the same way StateVersionPath is for
// the record beside it.
func StateOwnerPath(remoteDir string) string {
	return path.Join(remoteDir, stateDir, stateOwnerFile)
}

// StateOwner is the instance a host's forge state belongs to (UP-008).
//
// Identity is the instance's SSH host key, because that is the one piece of
// bundle identity that is present on every instance, never rotates, and
// never changes under the instance:
//
//   - A nameless bundle (INIT-005) has no domain, so a domain cannot be the
//     comparison — and a nameless instance is exactly the one an operator is
//     most likely to deploy a second time on the machine they are sitting at.
//   - `attach` fills a domain into a nameless bundle in place, on a live
//     host (UP-007), and explicitly keeps the SSH host key. Comparing on the
//     domain would make an attached instance a stranger to its own state on
//     the next `up`.
//   - It is public by definition — the string an operator would paste into
//     known_hosts (bundle.Manifest.SSHHostKeyPublic) — so recording it on the
//     host and naming it in a refusal keeps key material out of both
//     (KEY-003).
//
// Domain is a label, never the comparison: it is what lets a refusal name
// the instance whose state is already there rather than only a key. It is
// empty for a nameless bundle, and also for an owner recovered from a host
// that predates this record, which is why Describe calls an owner with no
// domain "an unnamed instance" rather than claiming it is nameless.
type StateOwner struct {
	// SSHHostKey is the public half of the instance's SSH host key,
	// normalized to "<type> <base64>" (normalizeSSHPublicKey). Empty means
	// *unknown* — a host with no record and no installed key — never "no
	// instance".
	SSHHostKey string

	// Domain is the bundle's domain when it has one, for display only.
	Domain string
}

// Known reports whether o identifies an instance at all. An unknown owner is
// what a fresh host and a host deployed before this record existed both
// return, and the two are indistinguishable from here.
func (o StateOwner) Known() bool { return o.SSHHostKey != "" }

// SameInstance reports whether o and other are the same instance. Two
// unknown owners are not the same instance — unknown is the absence of an
// answer, not an answer two hosts can agree on.
func (o StateOwner) SameInstance(other StateOwner) bool {
	return o.Known() && other.Known() && o.SSHHostKey == other.SSHHostKey
}

// Describe names o for an operator reading a refusal.
func (o StateOwner) Describe() string {
	if !o.Known() {
		return "an instance this host records nothing about"
	}
	name := "an unnamed instance"
	if o.Domain != "" {
		name = o.Domain
	}
	return fmt.Sprintf("%s, whose ssh host key is %q", name, o.SSHHostKey)
}

// record renders o as the bytes written to StateOwnerPath.
func (o StateOwner) record() []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s\n", ownerKeyField, o.SSHHostKey)
	if o.Domain != "" {
		fmt.Fprintf(&b, "%s: %s\n", ownerDomainField, o.Domain)
	}
	return []byte(b.String())
}

// parseStateOwner reads a record back. Anything it does not recognize is
// ignored rather than refused: this file is read on every `up`, and a record
// a future version wrote a field into must not be what stops a deployment.
func parseStateOwner(record string) StateOwner {
	var o StateOwner
	for _, line := range strings.Split(record, "\n") {
		field, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(field) {
		case ownerKeyField:
			o.SSHHostKey = normalizeSSHPublicKey(value)
		case ownerDomainField:
			o.Domain = strings.TrimSpace(value)
		}
	}
	return o
}

// normalizeSSHPublicKey reduces an OpenSSH public key line to the two fields
// that identify it — type and base64 blob — dropping the comment, which is
// a label an operator may change and which `up` itself does not ship
// consistently.
//
// A line it cannot split that way is trimmed and returned as it stands
// rather than rejected. What this compares is identity, not syntax: a
// bundle whose key does not parse is still that bundle, and refusing to
// deploy it because its key material is oddly shaped would be a new failure
// this requirement never asked for. bundle.SplitSSHPublicKey takes the same
// posture about what a public key is allowed to look like.
func normalizeSSHPublicKey(line string) string {
	keyType, blob, err := bundle.SplitSSHPublicKey(line)
	if err != nil {
		return strings.TrimSpace(line)
	}
	return keyType + " " + blob
}

// StateOwnerOf returns the owner that state deployed from m belongs to,
// reading the public half of m's SSH host key from driver.
//
// The keystore rather than m.SSHHostKeyPublic, because the keystore is what
// deploy.Up actually installs on the host (configureSSHHostKey): the record
// then names the key the instance really presents, on a bundle written
// before the manifest carried that field as much as on one written after.
// Reading it here also means a bundle whose keystore cannot produce it fails
// before Up touches the host rather than several steps in.
func StateOwnerOf(ctx context.Context, m *bundle.Manifest, driver keystore.Driver) (StateOwner, error) {
	public, err := driver.Resolve(ctx, state.KeySSHHostKeyPublic)
	if err != nil {
		return StateOwner{}, fmt.Errorf("resolve %s: %w", state.KeySSHHostKeyPublic, err)
	}
	key := normalizeSSHPublicKey(public.Reveal())
	if key == "" {
		return StateOwner{}, fmt.Errorf("%s in this bundle's keystore is empty", state.KeySSHHostKeyPublic)
	}
	return StateOwner{SSHHostKey: key, Domain: strings.TrimSpace(m.Domain)}, nil
}

// ReadStateOwner returns the instance the forge state under remoteDir
// belongs to, or an unknown owner when the host says nothing about it.
//
// It asks two questions, in order:
//
//  1. The record at StateOwnerPath, written by the deployment that owns this
//     state.
//  2. Failing that, the SSH host key installed under the gitea state
//     directory. `up` writes the bundle's own key there on every deployment
//     (configureSSHHostKey, RSTR-004), so a live instance deployed before
//     this record existed still says whose it is — which is the whole
//     population UP-008 has to protect, since every one of them was deployed
//     by a binary that wrote no record. Only the public half is read; the
//     private half beside it is never touched.
//
// An empty result means *unknown*, never "nobody owns this" — a fresh host
// and a host whose state predates both of the above return it alike.
// checkStateOwner treats unknown as permission to proceed for that reason:
// refusing on it would refuse every first deployment.
//
// A read that fails is an error, never an absence. Reading a transport
// failure as "no record" is how a deployment would end up converging over
// another instance's state anyway.
func ReadStateOwner(ctx context.Context, host Host, remoteDir string) (StateOwner, error) {
	record, err := readOptionalFile(ctx, host, StateOwnerPath(remoteDir))
	if err != nil {
		return StateOwner{}, err
	}
	if owner := parseStateOwner(record); owner.Known() {
		return owner, nil
	}

	installedKeyPath := path.Join(GiteaStatePath(remoteDir), sshHostKeyRelPath()) + ".pub"
	installed, err := readOptionalFile(ctx, host, installedKeyPath)
	if err != nil {
		return StateOwner{}, err
	}
	return StateOwner{SSHHostKey: normalizeSSHPublicKey(installed)}, nil
}

// RecordStateOwner writes owner to StateOwnerPath(remoteDir), claiming that
// state for the instance owner names.
//
// `up` claims the state as soon as its check passes, before it configures
// anything, rather than after a successful deployment. A deployment that
// fails partway has still put this bundle's app.ini, certificate, and SSH
// host key on that host, so "this directory is this bundle's" is the honest
// thing to tell the next `up` — including the next `up` of a different
// bundle, which is exactly the one that must be refused. Claiming only on
// success would leave a half-deployed directory looking free.
//
// restore.placeState calls it too, alongside the forgejo-version stamp it
// already writes: the state on that host is now the snapshot's, whatever it
// was before, so recovery onto a reused target keeps working rather than
// being refused by a record describing what that target used to hold.
//
// The record is world-readable (0644, like forgejo-version). It carries a
// public key and a domain, and nothing else — no key material ever reaches
// it (KEY-003).
func RecordStateOwner(ctx context.Context, host Host, remoteDir string, owner StateOwner) error {
	if !owner.Known() {
		return fmt.Errorf("record %s: the owner to record is unknown", StateOwnerPath(remoteDir))
	}
	if err := host.WriteFile(ctx, StateOwnerPath(remoteDir), owner.record(), 0o644); err != nil {
		return fmt.Errorf("record %s: %w", StateOwnerPath(remoteDir), err)
	}
	return nil
}

// checkStateOwner is UP-008's enforcement point: a deployment onto host
// state belonging to a different instance is refused, before anything on
// that host is touched.
//
// Every deployment lays its state out the same way under <RemoteDir>/state,
// so a second bundle pointed at a directory a first one is already using
// takes it over. Nothing about that fails at deploy time today: Forgejo
// starts, opens a database written by another instance, and finds its
// SECRET_KEY no longer matches what encrypted that database's contents. The
// forge comes up looking healthy while sessions, CSRF tokens, and every
// secret it holds are undecryptable, and the first instance's state is now
// underneath a second instance's configuration.
//
// So the check runs immediately after CheckHost — ahead of the first byte
// written to the host — and its refusal names what it found, what this
// bundle is, and the two ways out. State that belongs to this same instance,
// or that no host record can attribute to anyone, is what a re-run and a
// first run look like, and both proceed (UP-003).
func checkStateOwner(ctx context.Context, host Host, b *bundle.Bundle, remoteDir string) (string, error) {
	driver, err := keystore.New(b.Manifest.Drivers.Keystore.Driver, b.Manifest.Drivers.Keystore.Config)
	if err != nil {
		return "", fmt.Errorf("keystore driver: %w", err)
	}
	mine, err := StateOwnerOf(ctx, &b.Manifest, driver)
	if err != nil {
		return "", err
	}

	found, err := ReadStateOwner(ctx, host, remoteDir)
	if err != nil {
		return "", err
	}
	if found.Known() && !found.SameInstance(mine) {
		return "", fmt.Errorf(
			"the forge state in %s belongs to %s, and this deployment is %s: "+
				"deploying over it would start the forge against another instance's database, "+
				"whose contents this bundle's key material cannot decrypt — its sessions, its CSRF tokens, and every secret it holds. "+
				"Give this bundle a directory of its own on this host with -remote-dir, or run this command with the bundle that owns this state",
			path.Join(remoteDir, stateDir), found.Describe(), mine.Describe())
	}

	if err := RecordStateOwner(ctx, host, remoteDir, mine); err != nil {
		return "", err
	}

	switch {
	case !found.Known():
		return fmt.Sprintf("no forge state on this host is claimed by another instance; %s now owns %s", mine.Describe(), path.Join(remoteDir, stateDir)), nil
	default:
		return fmt.Sprintf("the forge state in %s belongs to this instance", path.Join(remoteDir, stateDir)), nil
	}
}

// readOptionalFile returns the contents of p on host, or "" when there is no
// file there.
//
// The `if` is what makes an absent file a zero exit with no output, so a
// nonzero exit is only ever a real transport or shell failure — which must
// not be read as "nothing there" by a caller whose whole job is deciding
// whether something is there.
func readOptionalFile(ctx context.Context, host Host, p string) (string, error) {
	quoted := stateShQuote(p)
	out, err := host.Output(ctx, fmt.Sprintf("if [ -f %s ]; then cat %s; fi", quoted, quoted))
	if err != nil {
		return "", fmt.Errorf("read %s: %w", p, err)
	}
	return string(out), nil
}
