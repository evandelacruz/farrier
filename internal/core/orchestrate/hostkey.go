package orchestrate

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// hostKeyCallback builds a HostKeyCallback backed by the operator's
// known_hosts file (Options.KnownHostsFile, defaulting to
// ~/.ssh/known_hosts). An unrecognized host fails closed with an
// actionable error rather than trusting whatever key the host presents —
// Farrier runs unattended, so there is no prompt to accept a new key the
// way an interactive ssh client offers one.
func hostKeyCallback(opts Options) (ssh.HostKeyCallback, error) {
	path := opts.KnownHostsFile
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("orchestrate: locate home directory for known_hosts: %w", err)
		}
		path = filepath.Join(home, ".ssh", "known_hosts")
	}

	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("orchestrate: known_hosts file %s: %w (connect once with the system ssh client to record the host key, or set KnownHostsFile)", path, err)
	}

	callback, err := knownhosts.New(path)
	if err != nil {
		return nil, fmt.Errorf("orchestrate: parse known_hosts file %s: %w", path, err)
	}
	return callback, nil
}
