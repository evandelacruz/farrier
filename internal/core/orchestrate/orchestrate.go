// Package orchestrate reaches a host over SSH (ORCH-001) and converges it to
// a bundle's rendered Compose definition (ORCH-002): render Compose from the
// manifest, ship the files to the host, and run `docker compose up -d
// --remove-orphans` so the host always ends up exactly matching the bundle —
// anything the host has that the bundle doesn't gets torn down.
//
// Host state is disposable (spec.md "Stateless vs. stateful"): Converge
// never reads back what's already running before deciding what to do: it
// always writes the full Compose definition and lets `docker compose`
// reconcile, so a converge is the same operation whether the host is fresh,
// already matches, or has drifted.
package orchestrate

import "context"

// Transport reaches a single host and runs commands or writes files on it.
// SSHTransport (ORCH-001) is the only implementation that talks to a real
// host; tests use a fake.
type Transport interface {
	// Run executes command in a shell on the host and returns its combined
	// stdout. A nonzero exit status is an error that includes stderr.
	Run(ctx context.Context, command string) ([]byte, error)

	// WriteFile writes content to path on the host, creating any parent
	// directories, with mode as the final file permissions. It replaces
	// path atomically: a reader on the host never observes a partial
	// write.
	WriteFile(ctx context.Context, path string, content []byte, mode uint32) error

	// Close releases the transport's underlying connection.
	Close() error
}
