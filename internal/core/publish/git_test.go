package publish

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// These tests run the real git rather than the fake, because what they
// check is whether publish asked git the right questions — that
// `symbolic-ref` is how you catch a detached HEAD, that `remote get-url`
// fails on an absent remote, that the push flags publish uses really do
// move every branch and every tag. A fake can only confirm the calls were
// made; only git can confirm they mean what publish assumes.

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
}

// newRepo makes a work tree with one commit on branch "main" and one tag.
func newRepo(t *testing.T) string {
	t.Helper()
	requireGit(t)
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		if _, err := (ExecGit{}).Run(context.Background(), dir, gitIdentity(), args...); err != nil {
			t.Fatalf("git %s: %v", strings.Join(args, " "), err)
		}
	}
	run("init", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	run("add", "README.md")
	run("commit", "-m", "first")
	run("tag", "v0.1.0")
	return dir
}

// gitIdentity keeps commits from depending on the machine's git config.
func gitIdentity() []string {
	return []string{
		"GIT_AUTHOR_NAME=Farrier Test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Farrier Test", "GIT_COMMITTER_EMAIL=test@example.com",
	}
}

func TestInspectRepoAgainstRealGit(t *testing.T) {
	requireGit(t)
	git := ExecGit{}
	ctx := context.Background()

	t.Run("a folder that is not a repository", func(t *testing.T) {
		if _, err := inspectRepo(ctx, git, t.TempDir(), "origin"); err == nil || !strings.Contains(err.Error(), "is not a git repository") {
			t.Errorf("inspectRepo = %v, want a not-a-repository error", err)
		}
	})

	t.Run("a repository with no commits", func(t *testing.T) {
		dir := t.TempDir()
		if _, err := git.Run(ctx, dir, nil, "init", "-b", "main"); err != nil {
			t.Fatalf("git init: %v", err)
		}
		if _, err := inspectRepo(ctx, git, dir, "origin"); err == nil || !strings.Contains(err.Error(), "has no commits") {
			t.Errorf("inspectRepo = %v, want a no-commits error", err)
		}
	})

	t.Run("a healthy repository, from a subdirectory", func(t *testing.T) {
		root := newRepo(t)
		sub := filepath.Join(root, "pkg")
		if err := os.Mkdir(sub, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		local, err := inspectRepo(ctx, git, sub, "origin")
		if err != nil {
			t.Fatalf("inspectRepo: %v", err)
		}
		if local.Branch != "main" {
			t.Errorf("branch = %q, want main", local.Branch)
		}
		// The work tree root is what gets published, not the folder
		// publish was pointed at. macOS resolves /var through a symlink,
		// so compare resolved paths.
		wantRoot, _ := filepath.EvalSymlinks(root)
		gotRoot, _ := filepath.EvalSymlinks(local.Root)
		if gotRoot != wantRoot {
			t.Errorf("root = %q, want %q", gotRoot, wantRoot)
		}
	})

	t.Run("a detached HEAD", func(t *testing.T) {
		dir := newRepo(t)
		head, err := git.Run(ctx, dir, nil, "rev-parse", "HEAD")
		if err != nil {
			t.Fatalf("rev-parse: %v", err)
		}
		if _, err := git.Run(ctx, dir, nil, "checkout", "--detach", head); err != nil {
			t.Fatalf("checkout --detach: %v", err)
		}
		if _, err := inspectRepo(ctx, git, dir, "origin"); err == nil || !strings.Contains(err.Error(), "detached HEAD") {
			t.Errorf("inspectRepo = %v, want a detached-HEAD error", err)
		}
	})

	t.Run("a repository that already has the remote", func(t *testing.T) {
		dir := newRepo(t)
		if _, err := git.Run(ctx, dir, nil, "remote", "add", "origin", "ssh://git@elsewhere.example.com/evan/thing.git"); err != nil {
			t.Fatalf("remote add: %v", err)
		}
		_, err := inspectRepo(ctx, git, dir, "origin")
		if err == nil || !strings.Contains(err.Error(), "already has a remote named origin") {
			t.Fatalf("inspectRepo = %v, want an existing-remote error", err)
		}
		if !strings.Contains(err.Error(), "ssh://git@elsewhere.example.com/evan/thing.git") {
			t.Errorf("error = %v, want it to name the remote it refused to overwrite", err)
		}
		// A remote under another name is not in the way.
		if _, err := inspectRepo(ctx, git, dir, "farrier"); err != nil {
			t.Errorf("inspectRepo with a free remote name: %v", err)
		}
	})
}

// The flags pushHistory uses really do move every branch and every tag,
// and leave the pushed branches tracking — which is what makes a bare
// `git push` from the folder work afterwards, IMPT-004's actual promise.
func TestPushFlagsMoveEveryBranchAndTag(t *testing.T) {
	requireGit(t)
	git := ExecGit{}
	ctx := context.Background()

	source := newRepo(t)
	if _, err := git.Run(ctx, source, gitIdentity(), "branch", "release"); err != nil {
		t.Fatalf("git branch: %v", err)
	}

	remote := t.TempDir()
	if _, err := git.Run(ctx, remote, nil, "init", "--bare"); err != nil {
		t.Fatalf("git init --bare: %v", err)
	}
	if _, err := git.Run(ctx, source, nil, "remote", "add", "origin", remote); err != nil {
		t.Fatalf("remote add: %v", err)
	}

	if _, err := git.Run(ctx, source, nil, "push", "--set-upstream", "--all", "origin"); err != nil {
		t.Fatalf("push branches: %v", err)
	}
	if _, err := git.Run(ctx, source, nil, "push", "--tags", "origin"); err != nil {
		t.Fatalf("push tags: %v", err)
	}

	refs, err := git.Run(ctx, remote, nil, "for-each-ref", "--format=%(refname)")
	if err != nil {
		t.Fatalf("for-each-ref: %v", err)
	}
	for _, want := range []string{"refs/heads/main", "refs/heads/release", "refs/tags/v0.1.0"} {
		if !strings.Contains(refs, want) {
			t.Errorf("remote is missing %s; has:\n%s", want, refs)
		}
	}

	upstream, err := git.Run(ctx, source, nil, "rev-parse", "--abbrev-ref", "main@{upstream}")
	if err != nil {
		t.Fatalf("main has no upstream after the push: %v", err)
	}
	if upstream != "origin/main" {
		t.Errorf("upstream = %q, want origin/main", upstream)
	}
}

func TestExecGitCarriesGitsOwnError(t *testing.T) {
	requireGit(t)
	_, err := (ExecGit{}).Run(context.Background(), t.TempDir(), nil, "rev-parse", "--show-toplevel")
	if err == nil {
		t.Fatal("Run succeeded outside a repository, want an error")
	}
	if !strings.Contains(err.Error(), "not a git repository") {
		t.Errorf("error = %v, want git's own message", err)
	}
}
