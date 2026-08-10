package publish

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/events"
	"github.com/evandelacruz/farrier/internal/core/keystore"
	"github.com/evandelacruz/farrier/internal/core/state"
)

const testHostKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIPublishTestHostKeyBlobAAAAAAAAAAAAAAAAAAAA farrier@instance\n"

// --- fake git -------------------------------------------------------------

type gitCall struct {
	dir  string
	env  []string
	args []string
}

// fakeGit answers the git commands publish runs, so a test can assert on
// what publish decided to do without a real repository or a real remote.
// Fail maps a command prefix ("push --set-upstream") onto the error that
// command returns.
type fakeGit struct {
	root   string
	branch string
	// hasRemote makes `remote get-url` succeed, i.e. the folder already
	// has the remote publish was going to configure.
	hasRemote  string
	noRepo     bool
	noCommits  bool
	detached   bool
	fail       map[string]error
	calls      []gitCall
	pushEnvSet []string
}

func (g *fakeGit) Run(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	g.calls = append(g.calls, gitCall{dir: dir, env: env, args: args})
	joined := strings.Join(args, " ")
	for prefix, err := range g.fail {
		if strings.HasPrefix(joined, prefix) {
			return "", err
		}
	}
	switch {
	case joined == "rev-parse --show-toplevel":
		if g.noRepo {
			return "", fmt.Errorf("not a git repository")
		}
		return g.root, nil
	case joined == "rev-parse --verify HEAD":
		if g.noCommits {
			return "", fmt.Errorf("needed a single revision")
		}
		return "0123456789abcdef", nil
	case joined == "symbolic-ref --short HEAD":
		if g.detached {
			return "", fmt.Errorf("ref HEAD is not a symbolic ref")
		}
		return g.branch, nil
	case strings.HasPrefix(joined, "remote get-url"):
		if g.hasRemote == "" {
			return "", fmt.Errorf("no such remote")
		}
		return g.hasRemote, nil
	case strings.HasPrefix(joined, "push"):
		g.pushEnvSet = env
		return "", nil
	default:
		return "", nil
	}
}

func (g *fakeGit) ran(prefix string) bool {
	for _, call := range g.calls {
		if strings.HasPrefix(strings.Join(call.args, " "), prefix) {
			return true
		}
	}
	return false
}

// --- fake forge -----------------------------------------------------------

// fakeForge is the instance's API reduced to what publish calls, recording
// what it was asked to create and delete.
type fakeForge struct {
	user       string
	existing   map[string]bool
	keys       []string
	created    []createRepoBody
	createdAt  []string // request paths, so org vs. user creation is visible
	deleted    []string
	addedKeys  []map[string]string
	createFail int
}

func (f *fakeForge) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/user", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"login": f.user})
	})
	mux.HandleFunc("GET /api/v1/user/keys", func(w http.ResponseWriter, r *http.Request) {
		out := make([]map[string]string, 0, len(f.keys))
		for _, k := range f.keys {
			out = append(out, map[string]string{"key": k})
		}
		json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("POST /api/v1/user/keys", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		f.addedKeys = append(f.addedKeys, body)
		f.keys = append(f.keys, body["key"])
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{}`))
	})
	mux.HandleFunc("GET /api/v1/repos/{owner}/{name}", func(w http.ResponseWriter, r *http.Request) {
		full := r.PathValue("owner") + "/" + r.PathValue("name")
		if f.existing[full] {
			json.NewEncoder(w).Encode(map[string]string{"full_name": full})
			return
		}
		http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
	})
	mux.HandleFunc("DELETE /api/v1/repos/{owner}/{name}", func(w http.ResponseWriter, r *http.Request) {
		f.deleted = append(f.deleted, r.PathValue("owner")+"/"+r.PathValue("name"))
		w.WriteHeader(http.StatusNoContent)
	})
	create := func(owner string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if f.createFail != 0 {
				http.Error(w, `{"message":"repository creation is disabled"}`, f.createFail)
				return
			}
			var body createRepoBody
			json.NewDecoder(r.Body).Decode(&body)
			f.created = append(f.created, body)
			f.createdAt = append(f.createdAt, r.URL.Path)
			if owner == "" {
				owner = r.PathValue("org")
			}
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]string{"full_name": owner + "/" + body.Name})
		}
	}
	mux.HandleFunc("POST /api/v1/user/repos", create(f.user))
	mux.HandleFunc("POST /api/v1/orgs/{org}/repos", create(""))

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// --- helpers --------------------------------------------------------------

// testManifest is a manifest carrying only what publish reads: the
// instance's domain and git-over-SSH port, and the instance's SSH host
// public key.
//
// Its keystore points at an empty directory on purpose. Publishing must
// not open the keystore at all — that is the whole point of the manifest
// carrying the key — so any test that starts doing so fails here rather
// than passing quietly on a key it should not have been able to read.
func testManifest(t *testing.T, port int) *bundle.Manifest {
	t.Helper()
	m := legacyManifest(t, port)
	m.SSHHostKeyPublic = strings.TrimSpace(testHostKey)
	m.Drivers.Keystore.Config = map[string]any{"path": t.TempDir()}
	return m
}

// legacyManifest is a manifest written before the host public key became a
// manifest field: the key is only in the bundle's keystore, which is where
// publish has to fall back to so bundles already on disk keep their pin.
func legacyManifest(t *testing.T, port int) *bundle.Manifest {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, state.KeySSHHostKeyPublic), []byte(testHostKey), 0o600); err != nil {
		t.Fatalf("write host key: %v", err)
	}
	return &bundle.Manifest{
		Domain:     "git.example.com",
		GitSSHPort: port,
		Drivers: bundle.DriverConfig{
			Keystore: bundle.DriverRef{Driver: "file", Config: map[string]any{"path": dir}},
		},
	}
}

// homeWithKeys builds a home directory holding the named public key files,
// for Options.homeDir. The default-key search then runs against a home this
// test owns: a suite that read the real ~/.ssh would pass on a laptop with
// a key and fail in CI without one, which is worse than no test at all.
func homeWithKeys(t *testing.T, names ...string) string {
	t.Helper()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
		t.Fatalf("make .ssh: %v", err)
	}
	for _, name := range names {
		stem := strings.TrimSuffix(name, ".pub")
		keyType := "ssh-ed25519"
		if strings.Contains(stem, "rsa") {
			keyType = "ssh-rsa"
		}
		line := fmt.Sprintf("%s AAAA%s evan@laptop\n", keyType, stem)
		if err := os.WriteFile(filepath.Join(home, ".ssh", name), []byte(line), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return home
}

func detailFor(job *events.Job, step string, state events.State) string {
	for _, ev := range job.Events() {
		if ev.Step == step && ev.State == state {
			return ev.Detail
		}
	}
	return ""
}

// --- tests ----------------------------------------------------------------

func TestRunPublishesTheFolder(t *testing.T) {
	forge := &fakeForge{user: "admin", keys: []string{"ssh-ed25519 AAAAoperator operator@laptop"}}
	srv := forge.server(t)
	git := &fakeGit{root: "/home/evan/my-project", branch: "trunk"}
	job := events.NewJob()

	result, err := Run(context.Background(), job, Options{
		Dir:           "/home/evan/my-project/sub",
		Manifest:      testManifest(t, 2222),
		TargetBaseURL: srv.URL,
		TargetToken:   keystore.NewSecret("token"),
		Private:       true,
		Git:           git,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if result.FullName != "admin/my-project" {
		t.Errorf("full name = %q, want admin/my-project", result.FullName)
	}
	if want := "ssh://git@git.example.com:2222/admin/my-project.git"; result.RemoteURL != want {
		t.Errorf("remote URL = %q, want %q", result.RemoteURL, want)
	}
	if result.Root != "/home/evan/my-project" {
		t.Errorf("root = %q, want the work tree root", result.Root)
	}

	// The repository is created empty, private, with the local branch as
	// its default — and under the token's own account, not an org.
	if len(forge.created) != 1 {
		t.Fatalf("created %d repositories, want 1", len(forge.created))
	}
	created := forge.created[0]
	if created.Name != "my-project" || created.DefaultBranch != "trunk" || !created.Private || created.AutoInit {
		t.Errorf("created = %+v, want name my-project, default branch trunk, private, no auto-init", created)
	}
	if forge.createdAt[0] != "/api/v1/user/repos" {
		t.Errorf("created at %q, want the authenticated user's repos endpoint", forge.createdAt[0])
	}

	// The remote is set, and the history — every branch, every tag — is
	// pushed, with branches tracking so a later bare `git push` works.
	if !git.ran("remote add origin ssh://git@git.example.com:2222/admin/my-project.git") {
		t.Errorf("origin was not set; calls: %v", git.calls)
	}
	if !git.ran("push --set-upstream --all origin") {
		t.Errorf("branches were not pushed with tracking; calls: %v", git.calls)
	}
	if !git.ran("push --tags origin") {
		t.Errorf("tags were not pushed; calls: %v", git.calls)
	}

	// Every step reported, and the job's terminal event names the remote.
	for _, step := range []string{StepInspect, StepAuthorize, StepCreate, StepRemote, StepPush} {
		if detailFor(job, step, events.StateSucceeded) == "" {
			t.Errorf("step %q reported no success event", step)
		}
	}
	terminal := detailFor(job, "", events.StateSucceeded)
	if !strings.Contains(terminal, result.RemoteURL) {
		t.Errorf("terminal detail %q does not name the remote URL", terminal)
	}
	if forge.deleted != nil {
		t.Errorf("a successful publish deleted %v", forge.deleted)
	}
}

func TestRunUsesPort22ScpStyleRemote(t *testing.T) {
	forge := &fakeForge{user: "admin", keys: []string{"ssh-ed25519 AAAAoperator"}}
	srv := forge.server(t)
	git := &fakeGit{root: "/src/thing", branch: "main"}

	result, err := Run(context.Background(), events.NewJob(), Options{
		Dir:           "/src/thing",
		Manifest:      testManifest(t, 22),
		TargetBaseURL: srv.URL,
		TargetToken:   keystore.NewSecret("token"),
		Git:           git,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if want := "git@git.example.com:admin/thing.git"; result.RemoteURL != want {
		t.Errorf("remote URL = %q, want %q", result.RemoteURL, want)
	}
}

func TestRunCreatesUnderAnOrganization(t *testing.T) {
	forge := &fakeForge{user: "admin", keys: []string{"ssh-ed25519 AAAAoperator"}}
	srv := forge.server(t)
	git := &fakeGit{root: "/src/thing", branch: "main"}

	result, err := Run(context.Background(), events.NewJob(), Options{
		Dir:           "/src/thing",
		Manifest:      testManifest(t, 2222),
		TargetBaseURL: srv.URL,
		TargetToken:   keystore.NewSecret("token"),
		Owner:         "acme",
		Name:          "renamed",
		Git:           git,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if forge.createdAt[0] != "/api/v1/orgs/acme/repos" {
		t.Errorf("created at %q, want the organization endpoint", forge.createdAt[0])
	}
	if result.FullName != "acme/renamed" {
		t.Errorf("full name = %q, want acme/renamed", result.FullName)
	}
}

// The two failure modes IMPT-004 has to be deliberate about, plus the
// states that make a push impossible. None of them may touch the instance.
func TestRunRefusesFoldersItCannotPublish(t *testing.T) {
	tests := []struct {
		name string
		git  *fakeGit
		want string
	}{
		{"not a repository", &fakeGit{noRepo: true}, "is not a git repository"},
		{"no commits", &fakeGit{root: "/src/thing", noCommits: true}, "has no commits"},
		{"detached HEAD", &fakeGit{root: "/src/thing", detached: true}, "detached HEAD"},
		{
			"remote already set",
			&fakeGit{root: "/src/thing", branch: "main", hasRemote: "git@github.com:evan/thing.git"},
			"already has a remote named origin",
		},
		{
			"unusable folder name",
			&fakeGit{root: "/src/my thing", branch: "main"},
			"not usable on the instance",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			forge := &fakeForge{user: "admin", keys: []string{"ssh-ed25519 AAAAoperator"}}
			srv := forge.server(t)
			job := events.NewJob()

			_, err := Run(context.Background(), job, Options{
				Dir:           "/src/thing",
				Manifest:      testManifest(t, 2222),
				TargetBaseURL: srv.URL,
				TargetToken:   keystore.NewSecret("token"),
				Git:           tc.git,
			})
			if err == nil {
				t.Fatal("Run succeeded, want an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
			if len(forge.created) != 0 || len(forge.deleted) != 0 {
				t.Errorf("the instance was touched: created %v, deleted %v", forge.created, forge.deleted)
			}
			if tc.git.ran("remote add") || tc.git.ran("push") {
				t.Errorf("the folder was modified; calls: %v", tc.git.calls)
			}
			if detailFor(job, StepInspect, events.StateFailed) == "" {
				t.Error("the inspect step reported no failure event")
			}
		})
	}
}

func TestRunRefusesARepositoryThatAlreadyExists(t *testing.T) {
	forge := &fakeForge{
		user:     "admin",
		keys:     []string{"ssh-ed25519 AAAAoperator"},
		existing: map[string]bool{"admin/thing": true},
	}
	srv := forge.server(t)
	git := &fakeGit{root: "/src/thing", branch: "main"}
	job := events.NewJob()

	_, err := Run(context.Background(), job, Options{
		Dir:           "/src/thing",
		Manifest:      testManifest(t, 2222),
		TargetBaseURL: srv.URL,
		TargetToken:   keystore.NewSecret("token"),
		Git:           git,
	})
	if err == nil {
		t.Fatal("Run succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("error = %v, want it to say the repository already exists", err)
	}
	// Nothing created, and above all nothing deleted: the repository that
	// was already there is not this run's to remove.
	if len(forge.created) != 0 || len(forge.deleted) != 0 {
		t.Errorf("created %v, deleted %v; want neither", forge.created, forge.deleted)
	}
	if git.ran("remote add") {
		t.Errorf("the folder's remote was set anyway; calls: %v", git.calls)
	}
}

// An account with no key and a machine with no key is the one case that
// still fails, and the operator has to be able to act on it: the message
// names every path that was tried, the override, and the command that
// makes a key.
func TestRunRefusesWhenNeitherTheAccountNorTheMachineHasAKey(t *testing.T) {
	forge := &fakeForge{user: "admin"}
	srv := forge.server(t)
	git := &fakeGit{root: "/src/thing", branch: "main"}
	home := t.TempDir() // no ~/.ssh at all

	_, err := Run(context.Background(), events.NewJob(), Options{
		Dir:           "/src/thing",
		Manifest:      testManifest(t, 2222),
		TargetBaseURL: srv.URL,
		TargetToken:   keystore.NewSecret("token"),
		Git:           git,
		homeDir:       home,
	})
	if err == nil {
		t.Fatal("Run succeeded, want an error")
	}
	for _, want := range []string{
		"no ssh public key",
		filepath.Join(home, ".ssh", "id_ed25519.pub"),
		filepath.Join(home, ".ssh", "id_rsa.pub"),
		"-ssh-key",
		"ssh-keygen",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to mention %q", err, want)
		}
	}
	if len(forge.created) != 0 {
		t.Errorf("created %v before failing authorize", forge.created)
	}
}

// The documented happy path: a fresh instance, an operator who typed no
// flags. The account has no key, so publish registers the operator's own —
// and says which file it took, because that is a change to their account.
func TestRunRegistersTheOperatorsOwnKeyWhenTheAccountHasNone(t *testing.T) {
	home := homeWithKeys(t, "id_ed25519.pub")
	forge := &fakeForge{user: "admin"}
	srv := forge.server(t)
	git := &fakeGit{root: "/src/thing", branch: "main"}
	job := events.NewJob()

	if _, err := Run(context.Background(), job, Options{
		Dir:           "/src/thing",
		Manifest:      testManifest(t, 2222),
		TargetBaseURL: srv.URL,
		TargetToken:   keystore.NewSecret("token"),
		Git:           git,
		homeDir:       home,
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(forge.addedKeys) != 1 {
		t.Fatalf("registered %d keys, want the operator's own", len(forge.addedKeys))
	}
	if got, want := forge.addedKeys[0]["key"], "ssh-ed25519 AAAAid_ed25519"; got != want {
		t.Errorf("registered key = %q, want %q", got, want)
	}
	detail := detailFor(job, StepAuthorize, events.StateSucceeded)
	if !strings.Contains(detail, filepath.Join(home, ".ssh", "id_ed25519.pub")) {
		t.Errorf("authorize detail = %q, want it to name the file that was registered", detail)
	}
}

// ed25519 before RSA: it is what the README's own ssh-keygen line produces
// and what a modern host prefers.
func TestRunPrefersEd25519OverRSA(t *testing.T) {
	home := homeWithKeys(t, "id_rsa.pub", "id_ed25519.pub")
	forge := &fakeForge{user: "admin"}
	srv := forge.server(t)

	if _, err := Run(context.Background(), events.NewJob(), Options{
		Dir:           "/src/thing",
		Manifest:      testManifest(t, 2222),
		TargetBaseURL: srv.URL,
		TargetToken:   keystore.NewSecret("token"),
		Git:           &fakeGit{root: "/src/thing", branch: "main"},
		homeDir:       home,
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(forge.addedKeys) != 1 || forge.addedKeys[0]["key"] != "ssh-ed25519 AAAAid_ed25519" {
		t.Errorf("registered %v, want the ed25519 key", forge.addedKeys)
	}
}

// An RSA-only machine is still publishable — the fallback is a list, not
// one path.
func TestRunFallsBackToRSA(t *testing.T) {
	home := homeWithKeys(t, "id_rsa.pub")
	forge := &fakeForge{user: "admin"}
	srv := forge.server(t)

	if _, err := Run(context.Background(), events.NewJob(), Options{
		Dir:           "/src/thing",
		Manifest:      testManifest(t, 2222),
		TargetBaseURL: srv.URL,
		TargetToken:   keystore.NewSecret("token"),
		Git:           &fakeGit{root: "/src/thing", branch: "main"},
		homeDir:       home,
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(forge.addedKeys) != 1 || forge.addedKeys[0]["key"] != "ssh-rsa AAAAid_rsa" {
		t.Errorf("registered %v, want the rsa key", forge.addedKeys)
	}
}

// The flag is an override, so a named key beats the default even when the
// default file is sitting right there.
func TestRunPrefersTheNamedKeyOverTheDefault(t *testing.T) {
	home := homeWithKeys(t, "id_ed25519.pub")
	named := filepath.Join(t.TempDir(), "work.pub")
	if err := os.WriteFile(named, []byte("ssh-ed25519 AAAAworkblob evan@work\n"), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}

	forge := &fakeForge{user: "admin"}
	srv := forge.server(t)

	if _, err := Run(context.Background(), events.NewJob(), Options{
		Dir:           "/src/thing",
		Manifest:      testManifest(t, 2222),
		TargetBaseURL: srv.URL,
		TargetToken:   keystore.NewSecret("token"),
		PublicKeyPath: named,
		Git:           &fakeGit{root: "/src/thing", branch: "main"},
		homeDir:       home,
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(forge.addedKeys) != 1 || forge.addedKeys[0]["key"] != "ssh-ed25519 AAAAworkblob" {
		t.Errorf("registered %v, want the key named with -ssh-key", forge.addedKeys)
	}
}

// A path from a flag is never shell-expanded, so publish expands the
// leading ~ itself rather than reading a directory named "~".
func TestRunExpandsATildeInTheNamedKeyPath(t *testing.T) {
	home := homeWithKeys(t, "id_ed25519.pub")
	forge := &fakeForge{user: "admin"}
	srv := forge.server(t)

	if _, err := Run(context.Background(), events.NewJob(), Options{
		Dir:           "/src/thing",
		Manifest:      testManifest(t, 2222),
		TargetBaseURL: srv.URL,
		TargetToken:   keystore.NewSecret("token"),
		PublicKeyPath: "~/.ssh/id_ed25519.pub",
		Git:           &fakeGit{root: "/src/thing", branch: "main"},
		homeDir:       home,
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(forge.addedKeys) != 1 || forge.addedKeys[0]["key"] != "ssh-ed25519 AAAAid_ed25519" {
		t.Errorf("registered %v, want the key under the expanded home directory", forge.addedKeys)
	}
}

// An account that already has a key is already publishable. A file on disk
// is not a reason to upload a second one — unchanged behavior, and the
// reason the fallback is safe to have at all.
func TestRunRegistersNothingWhenTheAccountAlreadyHasAKey(t *testing.T) {
	home := homeWithKeys(t, "id_ed25519.pub")
	forge := &fakeForge{user: "admin", keys: []string{"ssh-ed25519 AAAAsomeotherkey evan@desktop"}}
	srv := forge.server(t)
	job := events.NewJob()

	if _, err := Run(context.Background(), job, Options{
		Dir:           "/src/thing",
		Manifest:      testManifest(t, 2222),
		TargetBaseURL: srv.URL,
		TargetToken:   keystore.NewSecret("token"),
		Git:           &fakeGit{root: "/src/thing", branch: "main"},
		homeDir:       home,
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(forge.addedKeys) != 0 {
		t.Errorf("registered %v, want nothing added to an account that can already be pushed to", forge.addedKeys)
	}
	if detail := detailFor(job, StepAuthorize, events.StateSucceeded); !strings.Contains(detail, "already registered") {
		t.Errorf("authorize detail = %q, want it to report the key the account already has", detail)
	}
}

func TestRunRegistersTheGivenPublicKeyOnce(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "id_ed25519.pub")
	if err := os.WriteFile(keyFile, []byte("ssh-ed25519 AAAAoperatorblob evan@laptop\n"), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}

	forge := &fakeForge{user: "admin"}
	srv := forge.server(t)

	for i := range 2 {
		git := &fakeGit{root: fmt.Sprintf("/src/thing%d", i), branch: "main"}
		if _, err := Run(context.Background(), events.NewJob(), Options{
			Dir:           git.root,
			Manifest:      testManifest(t, 2222),
			TargetBaseURL: srv.URL,
			TargetToken:   keystore.NewSecret("token"),
			PublicKeyPath: keyFile,
			Git:           git,
		}); err != nil {
			t.Fatalf("Run %d: %v", i, err)
		}
	}

	if len(forge.addedKeys) != 1 {
		t.Fatalf("registered %d keys, want 1 (the second run must recognise its own key)", len(forge.addedKeys))
	}
	added := forge.addedKeys[0]
	if added["key"] != "ssh-ed25519 AAAAoperatorblob" {
		t.Errorf("registered key = %q, want the type and blob without the comment", added["key"])
	}
	if added["title"] != "farrier: evan@laptop" {
		t.Errorf("key title = %q, want it named for the key's comment", added["title"])
	}
}

// A push that fails after the repository was created leaves nothing
// behind: the repository is deleted and the remote is removed, the same
// posture IMPT-003 fixed for `import`.
func TestRunRollsBackWhenThePushFails(t *testing.T) {
	forge := &fakeForge{user: "admin", keys: []string{"ssh-ed25519 AAAAoperator"}}
	srv := forge.server(t)
	git := &fakeGit{
		root:   "/src/thing",
		branch: "main",
		fail:   map[string]error{"push --set-upstream": fmt.Errorf("permission denied (publickey)")},
	}
	job := events.NewJob()

	_, err := Run(context.Background(), job, Options{
		Dir:           "/src/thing",
		Manifest:      testManifest(t, 2222),
		TargetBaseURL: srv.URL,
		TargetToken:   keystore.NewSecret("token"),
		Git:           git,
	})
	if err == nil {
		t.Fatal("Run succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("error = %v, want it to carry git's own message", err)
	}
	if want := []string{"admin/thing"}; len(forge.deleted) != 1 || forge.deleted[0] != want[0] {
		t.Errorf("deleted = %v, want %v", forge.deleted, want)
	}
	if !git.ran("remote remove origin") {
		t.Errorf("the remote was left behind; calls: %v", git.calls)
	}
	if detailFor(job, StepPush, events.StateFailed) == "" {
		t.Error("the push step reported no failure event")
	}
}

func TestRunRollsBackWhenTheRemoteCannotBeSet(t *testing.T) {
	forge := &fakeForge{user: "admin", keys: []string{"ssh-ed25519 AAAAoperator"}}
	srv := forge.server(t)
	git := &fakeGit{
		root:   "/src/thing",
		branch: "main",
		fail:   map[string]error{"remote add": fmt.Errorf("remote origin already exists")},
	}

	_, err := Run(context.Background(), events.NewJob(), Options{
		Dir:           "/src/thing",
		Manifest:      testManifest(t, 2222),
		TargetBaseURL: srv.URL,
		TargetToken:   keystore.NewSecret("token"),
		Git:           git,
	})
	if err == nil {
		t.Fatal("Run succeeded, want an error")
	}
	if len(forge.deleted) != 1 {
		t.Errorf("deleted = %v, want the repository this run created", forge.deleted)
	}
	// The remote was never set, so cleanup must not try to remove one.
	if git.ran("remote remove") {
		t.Errorf("cleanup removed a remote it never set; calls: %v", git.calls)
	}
}

// The push runs against the instance's own host key rather than accepting
// whatever answers, and the file holding it is temporary.
func TestRunPinsTheInstanceHostKeyForThePush(t *testing.T) {
	forge := &fakeForge{user: "admin", keys: []string{"ssh-ed25519 AAAAoperator"}}
	srv := forge.server(t)
	git := &fakeGit{root: "/src/thing", branch: "main"}

	var knownHostsPath string
	var contents []byte
	git.fail = nil
	if _, err := Run(context.Background(), events.NewJob(), Options{
		Dir:           "/src/thing",
		Manifest:      testManifest(t, 2222),
		TargetBaseURL: srv.URL,
		TargetToken:   keystore.NewSecret("token"),
		Git: gitFunc(func(ctx context.Context, dir string, env []string, args ...string) (string, error) {
			if strings.HasPrefix(strings.Join(args, " "), "push") && knownHostsPath == "" {
				knownHostsPath = knownHostsFrom(env)
				contents, _ = os.ReadFile(knownHostsPath)
			}
			return git.Run(ctx, dir, env, args...)
		}),
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if knownHostsPath == "" {
		t.Fatal("push ran without a pinned known_hosts file")
	}
	want := "[git.example.com]:2222 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIPublishTestHostKeyBlobAAAAAAAAAAAAAAAAAAAA\n"
	if string(contents) != want {
		t.Errorf("known_hosts = %q, want %q", contents, want)
	}
	if !strings.Contains(strings.Join(git.pushEnvSet, " "), "StrictHostKeyChecking=yes") {
		t.Errorf("push env = %v, want strict host key checking", git.pushEnvSet)
	}
	if _, err := os.Stat(knownHostsPath); !os.IsNotExist(err) {
		t.Errorf("known_hosts file %s outlived the push", knownHostsPath)
	}
}

type gitFunc func(ctx context.Context, dir string, env []string, args ...string) (string, error)

func (f gitFunc) Run(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	return f(ctx, dir, env, args...)
}

func knownHostsFrom(env []string) string {
	for _, entry := range env {
		_, value, ok := strings.Cut(entry, "UserKnownHostsFile='")
		if !ok {
			continue
		}
		path, _, ok := strings.Cut(value, "'")
		if ok {
			return path
		}
	}
	return ""
}

func TestResolveRejectsIncompleteOptions(t *testing.T) {
	tests := []struct {
		name string
		opts Options
		want string
	}{
		{"no manifest", Options{TargetToken: keystore.NewSecret("t")}, "manifest is required"},
		{"no token", Options{Manifest: &bundle.Manifest{Domain: "git.example.com"}}, "token is required"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := resolve(tc.opts); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("resolve = %v, want an error mentioning %q", err, tc.want)
			}
		})
	}
}

func TestResolveDefaults(t *testing.T) {
	s, err := resolve(Options{
		Dir:         "/src/thing",
		Manifest:    &bundle.Manifest{Domain: "git.example.com"},
		TargetToken: keystore.NewSecret("t"),
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if s.remoteName != DefaultRemoteName {
		t.Errorf("remote name = %q, want %q", s.remoteName, DefaultRemoteName)
	}
	if s.client.baseURL != "https://git.example.com" {
		t.Errorf("base URL = %q, want the bundle's own domain", s.client.baseURL)
	}
	if _, ok := s.git.(ExecGit); !ok {
		t.Errorf("git = %T, want the real git", s.git)
	}
}

// --- the nameless tier (UP-006) -------------------------------------------

// namelessManifest is testManifest with the domain taken off: the bundle
// `init` produces by default, whose identity is the address the operator
// deployed it at rather than a name it owns.
func namelessManifest(t *testing.T, port int) *bundle.Manifest {
	t.Helper()
	m := testManifest(t, port)
	m.Domain = ""
	return m
}

// The command the README documents for a nameless instance, resolved: the
// operator names the API and nothing else, and the git-over-SSH endpoint
// follows from it, on the manifest's own port.
func TestResolveDerivesTheHostFromTheTarget(t *testing.T) {
	m := namelessManifest(t, 2222)
	s, err := resolve(Options{
		Dir:           "/src/thing",
		Manifest:      m,
		TargetBaseURL: "http://127.0.0.1:8222",
		TargetToken:   keystore.NewSecret("t"),
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if s.host != "127.0.0.1" {
		t.Errorf("host = %q, want the target's host", s.host)
	}
	if want := "ssh://git@127.0.0.1:2222/acme/widgets.git"; m.GitSSHCloneURLAt(s.host, "acme", "widgets") != want {
		t.Errorf("clone URL = %q, want %q", m.GitSSHCloneURLAt(s.host, "acme", "widgets"), want)
	}
	if want := "[127.0.0.1]:2222"; m.GitSSHKnownHostsHostAt(s.host) != want {
		t.Errorf("known_hosts host = %q, want %q", m.GitSSHKnownHostsHostAt(s.host), want)
	}
}

// End to end against the tier the quick start walks a first-time operator
// through, with no address given: both URLs publish writes — the remote and
// the pin — name the host the operator addressed the API at.
func TestRunPublishesToANamelessInstance(t *testing.T) {
	forge := &fakeForge{user: "admin", keys: []string{"ssh-ed25519 AAAAoperator"}}
	srv := forge.server(t)
	// httptest serves on a loopback literal, which is exactly the shape a
	// nameless instance is reached at.
	target, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	host := target.Hostname()
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}

	git := &fakeGit{root: "/home/evan/my-project", branch: "main"}
	job := events.NewJob()

	var knownHosts []byte
	result, err := Run(context.Background(), job, Options{
		Dir:           "/home/evan/my-project",
		Manifest:      namelessManifest(t, 2222),
		TargetBaseURL: srv.URL,
		TargetToken:   keystore.NewSecret("token"),
		Git: gitFunc(func(ctx context.Context, dir string, env []string, args ...string) (string, error) {
			if strings.HasPrefix(strings.Join(args, " "), "push") && knownHosts == nil {
				knownHosts, _ = os.ReadFile(knownHostsFrom(env))
			}
			return git.Run(ctx, dir, env, args...)
		}),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	wantURL := fmt.Sprintf("ssh://git@%s:2222/admin/my-project.git", host)
	if result.RemoteURL != wantURL {
		t.Errorf("remote URL = %q, want %q", result.RemoteURL, wantURL)
	}
	if !git.ran("remote add origin " + wantURL) {
		t.Errorf("origin was not set to the address; calls: %v", git.calls)
	}
	wantPin := fmt.Sprintf("[%s]:2222 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIPublishTestHostKeyBlobAAAAAAAAAAAAAAAAAAAA\n", target.Hostname())
	if string(knownHosts) != wantPin {
		t.Errorf("known_hosts = %q, want %q", knownHosts, wantPin)
	}
	// The operator is told where the repository is being created, which is
	// the address — a nameless bundle has no domain to name there.
	if detail := detailFor(job, StepCreate, events.StateStarted); !strings.Contains(detail, host) {
		t.Errorf("create detail = %q, want the address the publish is addressing", detail)
	}
}

// The API and git over SSH can answer at different hosts — a tunnel or a
// proxy in front of the API — so the derived default is overridable.
func TestResolveAddressOverridesTheTargetHost(t *testing.T) {
	s, err := resolve(Options{
		Dir:           "/src/thing",
		Manifest:      &bundle.Manifest{},
		Address:       "box.local",
		TargetBaseURL: "http://127.0.0.1:9999",
		TargetToken:   keystore.NewSecret("t"),
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if s.host != "box.local" {
		t.Errorf("host = %q, want the address rather than the target's host", s.host)
	}
	if s.client.baseURL != "http://127.0.0.1:9999" {
		t.Errorf("base URL = %q, want the target the operator named", s.client.baseURL)
	}
}

// An address with no target is a complete instruction on its own: the
// instance's API is reached at the same address, over plain HTTP, at the
// manifest's public web port.
func TestResolveDerivesTheAPIURLFromTheAddress(t *testing.T) {
	s, err := resolve(Options{
		Dir:         "/src/thing",
		Manifest:    &bundle.Manifest{},
		Address:     "192.168.1.5",
		TargetToken: keystore.NewSecret("t"),
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if s.host != "192.168.1.5" {
		t.Errorf("host = %q, want the address", s.host)
	}
	if s.client.baseURL != "http://192.168.1.5:8222" {
		t.Errorf("base URL = %q, want the nameless instance's own URL", s.client.baseURL)
	}
}

// An IPv6 address survives both spellings: bracketed in the URL authority
// the remote is built from, and bracketed once — around the port, not
// around the literal — in the known_hosts entry.
func TestResolveSpellsAnIPv6AddressForBothURLs(t *testing.T) {
	m := &bundle.Manifest{}
	s, err := resolve(Options{
		Dir:         "/src/thing",
		Manifest:    m,
		Address:     "fd00::1",
		TargetToken: keystore.NewSecret("t"),
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if want := "ssh://git@[fd00::1]:2222/acme/widgets.git"; m.GitSSHCloneURLAt(s.host, "acme", "widgets") != want {
		t.Errorf("clone URL = %q, want %q", m.GitSSHCloneURLAt(s.host, "acme", "widgets"), want)
	}
	if want := "[fd00::1]:2222"; m.GitSSHKnownHostsHostAt(s.host) != want {
		t.Errorf("known_hosts host = %q, want %q", m.GitSSHKnownHostsHostAt(s.host), want)
	}
}

// Neither flag is the one case publish cannot guess its way out of, so it
// names both ways out rather than failing on an empty host.
func TestResolveRejectsANamelessBundleWithNoAddressAndNoTarget(t *testing.T) {
	_, err := resolve(Options{
		Dir:         "/src/thing",
		Manifest:    &bundle.Manifest{},
		TargetToken: keystore.NewSecret("t"),
	})
	if err == nil {
		t.Fatal("resolve = nil error, want a refusal when nothing says where the instance is")
	}
	for _, flag := range []string{"-address", "-target"} {
		if !strings.Contains(err.Error(), flag) {
			t.Errorf("error = %q, want it to name %s", err, flag)
		}
	}
}

// A named bundle already answers where the instance is. A second answer is
// a disagreement, not an override — the pairing deploy and app.ini enforce.
func TestResolveRejectsAnAddressForANamedBundle(t *testing.T) {
	_, err := resolve(Options{
		Dir:         "/src/thing",
		Manifest:    &bundle.Manifest{Domain: "git.example.com"},
		Address:     "192.168.1.5",
		TargetToken: keystore.NewSecret("t"),
	})
	if err == nil || !strings.Contains(err.Error(), "nameless bundle only") {
		t.Errorf("resolve = %v, want an address rejected for a named bundle", err)
	}
}

// A target with no host cannot stand in for the address, and says so.
func TestResolveRejectsATargetWithNoHost(t *testing.T) {
	_, err := resolve(Options{
		Dir:           "/src/thing",
		Manifest:      &bundle.Manifest{},
		TargetBaseURL: "/api/v1",
		TargetToken:   keystore.NewSecret("t"),
	})
	if err == nil || !strings.Contains(err.Error(), "-address") {
		t.Errorf("resolve = %v, want a refusal naming the address flag", err)
	}
}

func TestKnownHostsLineOmitsThePortOn22(t *testing.T) {
	line, err := knownHostsLine(context.Background(), testManifest(t, 22), "git.example.com")
	if err != nil {
		t.Fatalf("knownHostsLine: %v", err)
	}
	if !strings.HasPrefix(line, "git.example.com ssh-ed25519 ") {
		t.Errorf("line = %q, want a bare hostname on port 22", line)
	}
	if strings.Contains(line, "farrier@instance") {
		t.Errorf("line = %q, want the comment dropped", line)
	}
}

// Pinning the endpoint is not a privileged act: the key is public, and
// requiring the keystore to read it meant requiring read access to
// SECRET_KEY, INTERNAL_TOKEN, and the age backup key alongside it. That is
// what stopped anyone but an instance's owner from publishing to a shared
// instance. Here the keystore names a driver that cannot even be built, so
// a line still coming out proves nothing consulted it.
func TestKnownHostsLineComesFromTheManifestWithoutTheKeystore(t *testing.T) {
	m := testManifest(t, 2222)
	m.Drivers.Keystore = bundle.DriverRef{}

	line, err := knownHostsLine(context.Background(), m, "git.example.com")
	if err != nil {
		t.Fatalf("knownHostsLine: %v", err)
	}
	if want := "[git.example.com]:2222 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIPublishTestHostKeyBlobAAAAAAAAAAAAAAAAAAAA\n"; line != want {
		t.Errorf("line = %q, want %q", line, want)
	}
}

// A bundle written before the manifest carried the key falls back to the
// keystore, so its push keeps the same pin it has always had rather than
// losing it to an upgrade.
func TestKnownHostsLineFallsBackToTheKeystore(t *testing.T) {
	fromKeystore, err := knownHostsLine(context.Background(), legacyManifest(t, 2222), "git.example.com")
	if err != nil {
		t.Fatalf("knownHostsLine: %v", err)
	}
	fromManifest, err := knownHostsLine(context.Background(), testManifest(t, 2222), "git.example.com")
	if err != nil {
		t.Fatalf("knownHostsLine: %v", err)
	}
	if fromKeystore != fromManifest {
		t.Errorf("keystore line = %q, manifest line = %q, want the same pin from either source", fromKeystore, fromManifest)
	}
}

// The fallback is a fallback, not a licence to give up: a bundle with the
// key in neither place fails the push rather than accepting whatever host
// answers.
func TestKnownHostsLineFailsWhenNeitherSourceHasTheKey(t *testing.T) {
	m := testManifest(t, 2222)
	m.SSHHostKeyPublic = ""

	if _, err := knownHostsLine(context.Background(), m, "git.example.com"); err == nil {
		t.Fatal("knownHostsLine = nil error, want a refusal when nothing holds the host key")
	}
}

// The pin goes in ahead of whatever the operator set, because ssh keeps
// the first value it obtains for a keyword: an operator wrapper that sets
// StrictHostKeyChecking or UserKnownHostsFile must not be able to defeat
// it. Everything else the operator set survives untouched.
func TestSSHCommandExtendsTheOperatorsOwn(t *testing.T) {
	const pins = "-o UserKnownHostsFile='/tmp/kh' -o StrictHostKeyChecking=yes"
	tests := []struct {
		name string
		base string
		want string
	}{
		{"unset", "", "ssh " + pins},
		{"identity file survives", "ssh -i /keys/id", "ssh " + pins + " -i /keys/id"},
		{"proxy command survives", `ssh -o ProxyCommand="nc -X connect %h %p"`, "ssh " + pins + ` -o ProxyCommand="nc -X connect %h %p"`},
		{"config file survives", "/usr/bin/ssh -F /home/op/.ssh/config -p 2022", "/usr/bin/ssh " + pins + " -F /home/op/.ssh/config -p 2022"},
		{"strict host key checking loses", "ssh -o StrictHostKeyChecking=accept-new", "ssh " + pins + " -o StrictHostKeyChecking=accept-new"},
		{"known hosts file loses", "ssh -o UserKnownHostsFile=/dev/null -o StrictHostKeyChecking=no", "ssh " + pins + " -o UserKnownHostsFile=/dev/null -o StrictHostKeyChecking=no"},
		{"glued form loses", "ssh -oStrictHostKeyChecking=no", "ssh " + pins + " -oStrictHostKeyChecking=no"},
		{"quoted program", `'/opt/my ssh/ssh' -o StrictHostKeyChecking=no`, `'/opt/my ssh/ssh' ` + pins + " -o StrictHostKeyChecking=no"},
		{"leading assignment", "SSH_AUTH_SOCK=/run/sock ssh -o StrictHostKeyChecking=no", "SSH_AUTH_SOCK=/run/sock ssh " + pins + " -o StrictHostKeyChecking=no"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := sshCommand(tc.base, "/tmp/kh")
			if err != nil {
				t.Fatalf("sshCommand: %v", err)
			}
			if got != tc.want {
				t.Errorf("sshCommand = %q, want %q", got, tc.want)
			}
		})
	}

	if _, err := sshCommand("", "/tmp/it's"); err == nil {
		t.Error("sshCommand accepted a quoted path, want an error")
	}
	if _, err := sshCommand(`'/opt/my ssh/ssh -o StrictHostKeyChecking=no`, "/tmp/kh"); err == nil {
		t.Error("sshCommand accepted an unterminated quote around the program, want an error")
	}
}
