package state

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// GitExporter exposes a forge's git data as a mirrorable set of remotes
// (STATE-001, spec.md "Git data"): content-addressed data replicated via
// push mirrors, not through Farrier itself. Implementations enumerate the
// bare repositories stored under a Forgejo repository root — laid out
// "<root>/<owner>/<repo>.git", Forgejo's own convention — and return each
// as a Remote the operator's replication tooling can point at directly.
type GitExporter interface {
	Remotes(ctx context.Context) ([]Remote, error)
}

// Runner executes one command against a host, streaming its stdout and
// stderr. *orchestrate.Client satisfies it; state does not import
// orchestrate so the two packages compose without a dependency cycle — the
// caller wires a connected Client in when it builds an SSHGitExporter.
type Runner interface {
	Run(ctx context.Context, command string, stdout, stderr io.Writer) error
}

// LocalGitExporter enumerates bare repositories directly on disk: the
// drill path (DRIL-001), and anywhere else the repository root is reachable
// without SSH.
type LocalGitExporter struct {
	// Root is the Forgejo repository root: a directory of
	// "<owner>/<repo>.git" bare repositories.
	Root string
}

// Remotes lists every "<owner>/<repo>.git" bare repository under Root,
// returning each as a Remote whose URL is its absolute filesystem path —
// usable directly with `git remote add --mirror`. A missing Root is treated
// as no repositories yet, not an error, since nothing has been pushed.
func (e *LocalGitExporter) Remotes(ctx context.Context) ([]Remote, error) {
	if strings.TrimSpace(e.Root) == "" {
		return nil, errors.New("state: local git exporter: root is required")
	}

	owners, err := os.ReadDir(e.Root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("state: local git exporter: read root %s: %w", e.Root, err)
	}

	var remotes []Remote
	for _, owner := range owners {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !owner.IsDir() {
			continue
		}
		ownerPath := filepath.Join(e.Root, owner.Name())
		repos, err := os.ReadDir(ownerPath)
		if err != nil {
			return nil, fmt.Errorf("state: local git exporter: read %s: %w", ownerPath, err)
		}
		for _, repo := range repos {
			if !repo.IsDir() || !strings.HasSuffix(repo.Name(), ".git") {
				continue
			}
			remotes = append(remotes, Remote{
				Name: owner.Name() + "/" + strings.TrimSuffix(repo.Name(), ".git"),
				URL:  filepath.Join(ownerPath, repo.Name()),
			})
		}
	}

	sort.Slice(remotes, func(i, j int) bool { return remotes[i].Name < remotes[j].Name })
	return remotes, nil
}

// SSHGitExporter enumerates bare repositories on a remote forge host over
// an already-connected Runner (an *orchestrate.Client in production).
type SSHGitExporter struct {
	Runner Runner

	// User, Host, and Port identify the host in the ssh:// URLs Remotes
	// returns — the same address the Runner is already connected to.
	User string
	Host string
	Port string

	// Root is the Forgejo repository root on the remote host: a directory
	// of "<owner>/<repo>.git" bare repositories.
	Root string
}

// Remotes lists every "<owner>/<repo>.git" bare repository under Root on
// the remote host, returning each as a Remote whose URL is an ssh:// address
// usable directly with `git remote add --mirror`.
func (e *SSHGitExporter) Remotes(ctx context.Context) ([]Remote, error) {
	if e.Runner == nil {
		return nil, errors.New("state: ssh git exporter: runner is required")
	}
	if strings.TrimSpace(e.User) == "" || strings.TrimSpace(e.Host) == "" {
		return nil, errors.New("state: ssh git exporter: user and host are required")
	}
	root := strings.TrimSpace(e.Root)
	if root == "" {
		return nil, errors.New("state: ssh git exporter: root is required")
	}
	root = strings.TrimRight(root, "/")

	command := fmt.Sprintf("find %s -mindepth 2 -maxdepth 2 -type d -name '*.git'", shellQuote(root))
	var stdout, stderr strings.Builder
	if err := e.Runner.Run(ctx, command, &stdout, &stderr); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, fmt.Errorf("state: ssh git exporter: list %s: %s: %w", root, msg, err)
		}
		return nil, fmt.Errorf("state: ssh git exporter: list %s: %w", root, err)
	}

	port := e.Port
	if port == "" {
		port = "22"
	}
	addr := net.JoinHostPort(e.Host, port)

	var remotes []Remote
	for _, line := range strings.Split(stdout.String(), "\n") {
		path := strings.TrimSpace(line)
		if path == "" {
			continue
		}
		rel := strings.TrimPrefix(path, root+"/")
		if rel == path || !strings.HasSuffix(rel, ".git") {
			// Not a well-formed "<owner>/<repo>.git" entry under root; skip
			// rather than surface a malformed remote.
			continue
		}
		remotes = append(remotes, Remote{
			Name: strings.TrimSuffix(rel, ".git"),
			URL:  fmt.Sprintf("ssh://%s@%s%s", e.User, addr, path),
		})
	}

	sort.Slice(remotes, func(i, j int) bool { return remotes[i].Name < remotes[j].Name })
	return remotes, nil
}

// shellQuote wraps s in single quotes for safe interpolation into a POSIX
// shell command, escaping any single quote s already contains.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
