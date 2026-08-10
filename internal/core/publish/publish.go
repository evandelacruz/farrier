// Package publish implements IMPT-004: putting the local project folder on
// its own instance. It creates the repository on the forge, pushes the
// folder's existing history, and sets `origin` to the instance's
// git-over-SSH URL, so a project that has never had a forge behind it
// reaches a working remote without passing through GitHub or GitLab first.
//
// This is a different on-ramp from `import` (IMPT-001..003), not a variant
// of it. `import` hands Forgejo a source URL and lets Forgejo's own
// migration API do the transport; `publish` has no source forge to name —
// the history is already on the operator's disk, so the transport is the
// operator's own `git push` over the endpoint UP-005 publishes. Nothing in
// internal/core/importer is reshaped to accommodate it.
//
// # The sequence
//
// Five steps, in this order, each an event on the CORE-002 job stream:
//
//   - inspect   — the folder is a git work tree, has at least one commit,
//     has HEAD on a branch, and has no remote under the target
//     name. Nothing has been touched yet when any of these fail.
//   - authorize — resolve who the target token belongs to, and confirm that
//     account can be pushed to at all (see "SSH keys" below).
//   - create    — the repository must not already exist on the instance;
//     create it empty, with the local branch as its default branch.
//   - remote    — set the project's `origin` to the instance's SSH URL.
//   - push      — push every branch and every tag.
//
// The two failure modes worth designing against both land in `inspect` and
// `create`, before anything is written: a folder that is not a repository
// or carries no commits fails with an explicit message rather than an
// incidental git error, and neither an existing `origin` nor an existing
// repository on the instance is ever silently overwritten.
//
// Past that point the same posture IMPT-003 requires of `import` applies:
// a failure leaves no partially-published project behind. If the push
// fails after the repository was created, cleanup deletes the repository
// it created and removes the remote it set, so a re-run starts from the
// state the first run found. Cleanup never deletes a repository this run
// did not create.
//
// # SSH keys
//
// A push is rejected unless the operator's public key is registered with
// the forge account, so `authorize` checks for one rather than letting the
// operator discover it as a permission-denied error four steps later.
// Options.PublicKeyPath registers a key; without it, an account that has
// no key at all fails the step before anything is created.
//
// The instance's own host key is not left to trust-on-first-use: its public
// half is in the bundle manifest (bundle.Manifest.SSHHostKeyPublic, written
// by `init` from the same keystore entry deploy.Up installs on the host), so
// push runs against a known_hosts file rendered from it, under
// StrictHostKeyChecking. Reading the pin from the manifest is what lets
// someone publish to an instance whose secrets they do not hold — the whole
// point of a fingerprint is that it is public. A manifest written before
// that field existed falls back to the keystore, so no bundle already on
// disk loses its pin. That file is a temporary file
// outside the bundle directory, holds only the public half, and is removed
// when the push returns — the operator's own ~/.ssh/known_hosts is never
// edited, so their first manual `git push` prompts to accept the host key
// exactly as it would for any other new remote.
package publish

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/events"
	"github.com/evandelacruz/farrier/internal/core/keystore"
)

// The steps `publish` reports on its job event stream (CORE-002).
const (
	StepInspect   = "inspect"
	StepAuthorize = "authorize"
	StepCreate    = "create"
	StepRemote    = "remote"
	StepPush      = "push"
)

// DefaultRemoteName is the remote `publish` configures when Options names
// none — the name spec.md "The unit: one forge per project" commits to.
const DefaultRemoteName = "origin"

// Options configures one publish.
type Options struct {
	// Dir is the project folder to publish. Empty means the process's
	// working directory. It need not be the work tree's root: git resolves
	// upward, and the root it reports is what gets published.
	Dir string

	// Manifest is the project's bundle manifest — the instance's identity.
	// The SSH URL written into the remote comes from it
	// (bundle.Manifest.GitSSHCloneURL), as does the host key publish pins
	// the push against, via the manifest's keystore driver. Required.
	Manifest *bundle.Manifest

	// TargetBaseURL is the instance's API base URL. Empty derives
	// https://<manifest domain>, which is the address a named bundle
	// serves; it exists as an override for reaching an instance by some
	// other address.
	TargetBaseURL string
	// TargetToken authenticates to the instance's API. Required.
	TargetToken keystore.Secret

	// Owner is the user or organization the repository lands under on the
	// instance. Empty means the account TargetToken belongs to.
	Owner string
	// Name is the repository's name on the instance. Empty derives it from
	// the base name of the project folder's git work tree root.
	Name string
	// Private marks the created repository private. The zero value is
	// false, so callers set it explicitly; the CLI and the API both
	// default it to true.
	Private bool

	// RemoteName is the git remote to configure. Empty means
	// DefaultRemoteName.
	RemoteName string

	// PublicKeyPath is an SSH public key file to register with the forge
	// account so the push — and every later push by the operator — is
	// authorized. Empty registers nothing, and then an account with no key
	// already registered fails the authorize step.
	PublicKeyPath string

	// HTTPClient overrides the client used for the instance's API; nil
	// uses http.DefaultClient. Tests point this at an httptest.Server.
	HTTPClient *http.Client
	// Git overrides how git subprocesses run; nil runs the real git.
	Git Git
}

// Result reports what publish created and configured.
type Result struct {
	// Root is the git work tree that was published.
	Root string
	// FullName is the repository on the instance, "owner/name".
	FullName string
	// RemoteName and RemoteURL are the remote publish configured and the
	// SSH URL it points at.
	RemoteName string
	RemoteURL  string
	// Branch is the local branch that became the repository's default.
	Branch string
}

// Run publishes opts.Dir to the instance opts.Manifest identifies,
// reporting progress through job (CORE-002) and owning the job's terminal
// event — job.Succeeded or job.Failed — the way importer.Run owns it for
// `import`.
func Run(ctx context.Context, job *events.Job, opts Options) (Result, error) {
	result, err := run(ctx, job, opts)
	if err != nil {
		job.Failed(err.Error())
		return Result{}, err
	}
	job.Succeeded(successDetail(result))
	return result, nil
}

func successDetail(r Result) string {
	return fmt.Sprintf("published %s as %s; %s set to %s (default branch %s)",
		r.Root, r.FullName, r.RemoteName, r.RemoteURL, r.Branch)
}

// run is Run without the terminal event: every failure it returns has
// already emitted the failing step's own event, and has already undone
// whatever this run had done by then.
func run(ctx context.Context, job *events.Job, opts Options) (Result, error) {
	settings, err := resolve(opts)
	if err != nil {
		job.Emit(StepInspect, events.StateFailed, err.Error())
		return Result{}, fmt.Errorf("publish: %w", err)
	}

	// inspect — everything that can disqualify the folder, before any
	// call reaches the instance.
	job.Started(StepInspect, fmt.Sprintf("inspecting %s", settings.dir))
	local, err := inspectRepo(ctx, settings.git, settings.dir, settings.remoteName)
	if err != nil {
		job.Emit(StepInspect, events.StateFailed, err.Error())
		return Result{}, fmt.Errorf("publish: %w", err)
	}
	name := settings.name
	if name == "" {
		name = filepath.Base(local.Root)
	}
	if err := validateRepoName(name); err != nil {
		job.Emit(StepInspect, events.StateFailed, err.Error())
		return Result{}, fmt.Errorf("publish: %w", err)
	}
	job.Emit(StepInspect, events.StateSucceeded, fmt.Sprintf(
		"%s is a git repository on branch %s, with no %s remote",
		local.Root, local.Branch, settings.remoteName))

	// authorize — who the token is, and whether a push from this operator
	// can be authorized at all.
	job.Started(StepAuthorize, "resolving the instance account")
	client := settings.client
	user, err := client.whoami(ctx)
	if err != nil {
		job.Emit(StepAuthorize, events.StateFailed, err.Error())
		return Result{}, fmt.Errorf("publish: %w", err)
	}
	owner := settings.owner
	if owner == "" {
		owner = user
	}
	keyDetail, err := ensurePushKey(ctx, client, user, settings.publicKeyPath)
	if err != nil {
		job.Emit(StepAuthorize, events.StateFailed, err.Error())
		return Result{}, fmt.Errorf("publish: %w", err)
	}
	job.Emit(StepAuthorize, events.StateSucceeded, fmt.Sprintf("authenticated as %s; %s", user, keyDetail))

	// create — refuse to touch a repository that already exists, then
	// create an empty one with the local branch as its default.
	job.Started(StepCreate, fmt.Sprintf("creating %s/%s on %s", owner, name, settings.manifest.Domain))
	exists, err := client.repoExists(ctx, owner, name)
	if err != nil {
		job.Emit(StepCreate, events.StateFailed, err.Error())
		return Result{}, fmt.Errorf("publish: %w", err)
	}
	if exists {
		err := fmt.Errorf("repository %s/%s already exists on %s: publish will not overwrite it — publish under a different name with -name, or push to it by hand",
			owner, name, settings.manifest.Domain)
		job.Emit(StepCreate, events.StateFailed, err.Error())
		return Result{}, fmt.Errorf("publish: %w", err)
	}
	fullName, err := client.createRepo(ctx, createRepoRequest{
		owner:         owner,
		ownerIsSelf:   owner == user,
		name:          name,
		private:       settings.private,
		defaultBranch: local.Branch,
	})
	if err != nil {
		job.Emit(StepCreate, events.StateFailed, err.Error())
		return Result{}, fmt.Errorf("publish: %w", err)
	}
	job.Emit(StepCreate, events.StateSucceeded, fmt.Sprintf("created %s (private=%t, default branch %s)", fullName, settings.private, local.Branch))

	// Past this point the instance holds a repository this run created, so
	// every failure undoes it: no partially-published project is left
	// behind, the posture IMPT-003 fixed for `import`.
	result := Result{
		Root:       local.Root,
		FullName:   fullName,
		RemoteName: settings.remoteName,
		RemoteURL:  settings.manifest.GitSSHCloneURL(owner, name),
		Branch:     local.Branch,
	}

	fail := func(step string, err error) (Result, error) {
		err = rollback(settings, client, owner, name, result, err)
		job.Emit(step, events.StateFailed, err.Error())
		return Result{}, fmt.Errorf("publish: %w", err)
	}

	// remote — point the folder at the instance.
	job.Started(StepRemote, fmt.Sprintf("setting %s to %s", result.RemoteName, result.RemoteURL))
	if _, err := settings.git.Run(ctx, local.Root, nil, "remote", "add", result.RemoteName, result.RemoteURL); err != nil {
		return fail(StepRemote, fmt.Errorf("set remote %s: %w", result.RemoteName, err))
	}
	settings.remoteAdded = true
	job.Emit(StepRemote, events.StateSucceeded, fmt.Sprintf("%s set to %s", result.RemoteName, result.RemoteURL))

	// push — the folder's existing history, every branch and every tag.
	job.Started(StepPush, fmt.Sprintf("pushing %s history to %s", local.Root, result.RemoteURL))
	if err := pushHistory(ctx, settings, local.Root); err != nil {
		return fail(StepPush, err)
	}
	job.Emit(StepPush, events.StateSucceeded, fmt.Sprintf("pushed every branch and tag to %s", result.FullName))

	return result, nil
}

// rollback undoes what this run created after cause failed: the remote it
// set and the repository it created, in that order. Both are best-effort
// and neither replaces cause — a cleanup failure is reported alongside it,
// naming what the operator has to remove by hand.
func rollback(s *settings, client *client, owner, name string, result Result, cause error) error {
	if s.remoteAdded {
		if _, err := s.git.Run(context.Background(), result.Root, nil, "remote", "remove", result.RemoteName); err != nil {
			cause = fmt.Errorf("%w (cleanup also failed, remote %s may need removing by hand: %v)", cause, result.RemoteName, err)
		}
	}
	if err := client.deleteRepo(owner, name); err != nil {
		cause = fmt.Errorf("%w (cleanup also failed, %s/%s may need removing from the instance by hand: %v)", cause, owner, name, err)
	}
	return cause
}

// settings is Options resolved: defaults filled in, nothing validated
// against the network or the disk yet.
type settings struct {
	dir           string
	manifest      *bundle.Manifest
	owner         string
	name          string
	private       bool
	remoteName    string
	publicKeyPath string
	git           Git
	client        *client

	// remoteAdded records that this run configured the remote, so
	// rollback removes only a remote publish itself created.
	remoteAdded bool
}

func resolve(opts Options) (*settings, error) {
	if opts.Manifest == nil {
		return nil, fmt.Errorf("bundle manifest is required")
	}
	if strings.TrimSpace(opts.Manifest.Domain) == "" {
		return nil, fmt.Errorf("bundle manifest has no domain: publish needs the instance's address to build the remote URL")
	}
	if opts.TargetToken.Reveal() == "" {
		return nil, fmt.Errorf("target token is required")
	}

	dir := strings.TrimSpace(opts.Dir)
	if dir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("resolve working directory: %w", err)
		}
		dir = wd
	}

	baseURL := strings.TrimRight(strings.TrimSpace(opts.TargetBaseURL), "/")
	if baseURL == "" {
		baseURL = "https://" + opts.Manifest.Domain
	}

	remoteName := strings.TrimSpace(opts.RemoteName)
	if remoteName == "" {
		remoteName = DefaultRemoteName
	}

	git := opts.Git
	if git == nil {
		git = ExecGit{}
	}

	return &settings{
		dir:           dir,
		manifest:      opts.Manifest,
		owner:         strings.TrimSpace(opts.Owner),
		name:          strings.TrimSpace(opts.Name),
		private:       opts.Private,
		remoteName:    remoteName,
		publicKeyPath: strings.TrimSpace(opts.PublicKeyPath),
		git:           git,
		client: &client{
			baseURL: baseURL,
			token:   opts.TargetToken,
			http:    opts.HTTPClient,
		},
	}, nil
}

// validateRepoName rejects a derived or given repository name Forgejo
// would refuse, so the failure names the folder rather than arriving as an
// opaque 422 from the instance. Publishing a folder called "my project" or
// "." is a real case: the name is derived from the folder by default.
func validateRepoName(name string) error {
	if name == "" || name == "." || name == string(filepath.Separator) {
		return fmt.Errorf("could not derive a repository name from the project folder: name one with -name")
	}
	if strings.ContainsAny(name, " /\\:*?\"<>|") {
		return fmt.Errorf("repository name %q is not usable on the instance: name one with -name", name)
	}
	return nil
}

// pushHistory pushes every branch and every tag to the configured remote,
// pinning the instance's host key for the duration (see the package
// comment). Branches carry --set-upstream so the operator's next bare
// `git push` from the folder works with no arguments, which is the whole
// point of IMPT-004.
func pushHistory(ctx context.Context, s *settings, root string) error {
	env, cleanup, err := s.sshEnv(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	if _, err := s.git.Run(ctx, root, env, "push", "--set-upstream", "--all", s.remoteName); err != nil {
		return fmt.Errorf("push branches: %w", err)
	}
	if _, err := s.git.Run(ctx, root, env, "push", "--tags", s.remoteName); err != nil {
		return fmt.Errorf("push tags: %w", err)
	}
	return nil
}
