package backup

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/evandelacruz/farrier/internal/core/state"
)

// GitCapturer captures one repository's bare git data as a tar stream, given
// the Remote a state.GitExporter enumerated. state.GitExporter itself only
// lists remotes (STATE-001, spec.md "Git data") — it doesn't stream
// repository content, since replication is ordinarily the operator's own
// mirroring tooling pointed at those remotes — so backup pairs it with a
// GitCapturer to actually read the bytes tar-captures into a snapshot's
// repos/ directory (tech-spec.md "Snapshot format").
type GitCapturer interface {
	// Archive returns a tar stream of the bare repository remote points at.
	// The caller must Close the returned reader.
	Archive(ctx context.Context, remote state.Remote) (io.ReadCloser, error)
}

// LocalGitCapturer tars a bare repository directly from disk, given a
// Remote whose URL is an absolute filesystem path — what
// state.LocalGitExporter returns (the drill path, DRIL-001, and anywhere
// else the repository root is reachable without SSH).
type LocalGitCapturer struct{}

// Archive walks the bare repository at remote.URL and streams it out as a
// tar archive, entry by entry, so the whole repository is never held in
// memory at once.
func (LocalGitCapturer) Archive(ctx context.Context, remote state.Remote) (io.ReadCloser, error) {
	root := remote.URL
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("backup: local git capturer: stat %s: %w", root, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("backup: local git capturer: %s is not a directory", root)
	}

	pr, pw := io.Pipe()
	go func() {
		pw.CloseWithError(tarDir(ctx, pw, root))
	}()
	return pr, nil
}

// tarDir writes every file under root into w as a tar archive, with entry
// names relative to root.
func tarDir(ctx context.Context, w io.Writer, root string) error {
	tw := tar.NewWriter(w)
	err := filepath.Walk(root, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		hdr, err := tar.FileInfoHeader(fi, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if fi.IsDir() {
			hdr.Name += "/"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if fi.IsDir() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
	if err != nil {
		return err
	}
	return tw.Close()
}

// SSHGitCapturer tars a bare repository on a remote forge host over an
// already-connected Runner (an *orchestrate.Client in production), running
// tar in the repository's directory — the same "shell out over an
// already-connected Runner" approach state.SSHDatabaseExporter takes with
// sqlite3.
type SSHGitCapturer struct {
	Runner state.Runner
}

// Archive parses remote.URL's path back out of the ssh:// address
// state.SSHGitExporter built it from, and streams `tar -C <path> -cf - .`
// run in that directory over Runner.
func (c SSHGitCapturer) Archive(ctx context.Context, remote state.Remote) (io.ReadCloser, error) {
	if c.Runner == nil {
		return nil, errors.New("backup: ssh git capturer: runner is required")
	}
	dir, err := repoPath(remote.URL)
	if err != nil {
		return nil, fmt.Errorf("backup: ssh git capturer: %w", err)
	}

	pr, pw := io.Pipe()
	go func() {
		var stderr strings.Builder
		command := fmt.Sprintf("tar -C %s -cf - .", gitShellQuote(dir))
		runErr := c.Runner.Run(ctx, command, pw, &stderr)
		if runErr != nil {
			runErr = fmt.Errorf("%w%s", runErr, gitCommandStderrSuffix(stderr.String()))
		}
		pw.CloseWithError(runErr)
	}()
	return pr, nil
}

// repoPath extracts the filesystem path a state.SSHGitExporter-produced
// ssh://user@host:port/path remote URL points at.
func repoPath(remoteURL string) (string, error) {
	u, err := url.Parse(remoteURL)
	if err != nil {
		return "", fmt.Errorf("parse remote url %q: %w", remoteURL, err)
	}
	if u.Scheme != "ssh" {
		return "", fmt.Errorf("remote url %q is not an ssh:// address", remoteURL)
	}
	if u.Path == "" {
		return "", fmt.Errorf("remote url %q has no path", remoteURL)
	}
	return u.Path, nil
}

func gitShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func gitCommandStderrSuffix(stderr string) string {
	stderr = strings.TrimSpace(stderr)
	if stderr == "" {
		return ""
	}
	return fmt.Sprintf(" (stderr: %s)", stderr)
}
