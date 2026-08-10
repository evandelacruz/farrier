package publish

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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
// instance's domain and git-over-SSH port, and a file keystore holding the
// instance's SSH host public key.
func testManifest(t *testing.T, port int) *bundle.Manifest {
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

func TestRunRefusesAnAccountWithNoSSHKey(t *testing.T) {
	forge := &fakeForge{user: "admin"}
	srv := forge.server(t)
	git := &fakeGit{root: "/src/thing", branch: "main"}

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
	if !strings.Contains(err.Error(), "no ssh public key") {
		t.Errorf("error = %v, want it to say the account has no ssh key", err)
	}
	if len(forge.created) != 0 {
		t.Errorf("created %v before failing authorize", forge.created)
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
		{"no domain", Options{Manifest: &bundle.Manifest{}, TargetToken: keystore.NewSecret("t")}, "no domain"},
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

func TestKnownHostsLineOmitsThePortOn22(t *testing.T) {
	line, err := knownHostsLine(context.Background(), testManifest(t, 22))
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
