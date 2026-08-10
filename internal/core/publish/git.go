package publish

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Git runs one git subprocess. dir is the working directory; env entries
// are appended to the process environment for that one call.
//
// It is an interface for one reason: the sequencing in publish.go decides
// what git is asked to do, and that decision is what the tests need to
// pin. The shipped implementation is ExecGit, which runs the operator's
// own git — publish deliberately does not reimplement git transport, the
// same way `import` deliberately does not (see the importer package).
type Git interface {
	Run(ctx context.Context, dir string, env []string, args ...string) (string, error)
}

// ExecGit runs the git on the operator's PATH.
type ExecGit struct{}

// Run executes `git args...` in dir and returns its trimmed stdout. A
// non-zero exit becomes an error carrying git's own stderr: git already
// explains its failures better than a wrapper can, and a push rejected for
// a missing SSH key is exactly the message the operator needs to see.
func (ExecGit) Run(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = strings.TrimSpace(stdout.String())
		}
		if detail == "" {
			return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
		}
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, detail)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// localRepo is what inspect learned about the project folder.
type localRepo struct {
	// Root is the git work tree's root — the folder that gets published,
	// which is not necessarily the folder publish was pointed at.
	Root string
	// Branch is the branch HEAD is on, which becomes the repository's
	// default branch on the instance.
	Branch string
}

// inspectRepo answers the questions that disqualify a folder from being
// published, and answers them before anything is created on the instance.
//
// Each one is checked explicitly rather than being left to surface as a
// side effect of a later git command, because these are the two failure
// modes IMPT-004 has to be deliberate about: a folder that is not a
// repository or holds no commits has nothing to publish, and a folder that
// already has the target remote is one publish must not overwrite. In all
// four cases nothing has been touched, locally or on the instance.
func inspectRepo(ctx context.Context, git Git, dir, remoteName string) (localRepo, error) {
	root, err := git.Run(ctx, dir, nil, "rev-parse", "--show-toplevel")
	if err != nil {
		return localRepo{}, fmt.Errorf("%s is not a git repository: run `git init` and commit the project's history first", dir)
	}
	if _, err := git.Run(ctx, dir, nil, "rev-parse", "--verify", "HEAD"); err != nil {
		return localRepo{}, fmt.Errorf("%s has no commits: there is no history to publish — commit the project first", root)
	}
	branch, err := git.Run(ctx, dir, nil, "symbolic-ref", "--short", "HEAD")
	if err != nil || branch == "" {
		return localRepo{}, fmt.Errorf("%s has a detached HEAD: check out the branch you want published first", root)
	}
	if url, err := git.Run(ctx, dir, nil, "remote", "get-url", remoteName); err == nil {
		return localRepo{}, fmt.Errorf("%s already has a remote named %s, pointing at %s: publish will not overwrite it — remove it, or publish under a different remote name",
			root, remoteName, url)
	}
	return localRepo{Root: root, Branch: branch}, nil
}

// sshEnv renders the bundle's SSH host public key into a temporary
// known_hosts file and returns the environment that makes git's ssh use
// it, plus the func that removes it again.
//
// Pinning rather than trusting on first use is what makes the push
// non-interactive and what makes it verify: the instance's host key is
// bundle identity (state.KeySSHHostKeyPublic, installed on every deploy),
// so publish already knows the key the endpoint must present, and a host
// answering with a different one fails the push instead of being accepted
// silently. The file holds only the public half, lives outside the bundle
// directory, and is deleted when the push returns; the operator's own
// known_hosts is never edited (KEY-003).
//
// An operator who has set GIT_SSH_COMMAND keeps it — their ssh invocation
// is extended, not replaced, so a custom ssh binary or a jump host still
// applies.
func (s *settings) sshEnv(ctx context.Context) ([]string, func(), error) {
	line, err := knownHostsLine(ctx, s.manifest)
	if err != nil {
		return nil, func() {}, err
	}

	file, err := os.CreateTemp("", "farrier-known-hosts-*")
	if err != nil {
		return nil, func() {}, fmt.Errorf("create known_hosts file: %w", err)
	}
	cleanup := func() {
		file.Close()
		os.Remove(file.Name())
	}
	if _, err := file.WriteString(line); err != nil {
		cleanup()
		return nil, func() {}, fmt.Errorf("write known_hosts file: %w", err)
	}
	if err := file.Close(); err != nil {
		cleanup()
		return nil, func() {}, fmt.Errorf("write known_hosts file: %w", err)
	}

	command, err := sshCommand(os.Getenv("GIT_SSH_COMMAND"), file.Name())
	if err != nil {
		cleanup()
		return nil, func() {}, err
	}
	return []string{"GIT_SSH_COMMAND=" + command}, cleanup, nil
}

// sshCommand extends base (an operator's GIT_SSH_COMMAND, or plain ssh)
// with the options that pin the instance's host key. git splits
// GIT_SSH_COMMAND with shell-style single-quote rules, so the path is
// single-quoted; a path containing a single quote is rejected rather than
// mis-split, which cannot happen for os.CreateTemp's own names and is
// checked only so the quoting is safe by construction.
func sshCommand(base, knownHostsPath string) (string, error) {
	if strings.Contains(knownHostsPath, "'") {
		return "", fmt.Errorf("known_hosts path %q contains a quote", knownHostsPath)
	}
	if strings.TrimSpace(base) == "" {
		base = "ssh"
	}
	return fmt.Sprintf("%s -o UserKnownHostsFile='%s' -o StrictHostKeyChecking=yes", base, knownHostsPath), nil
}
