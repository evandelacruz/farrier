package orchestrate

import "context"

// Transport reaches a single host and runs commands or writes files on it.
// *Client (ORCH-001) is the only implementation that talks to a real host;
// tests use a fake.
//
// Converge takes this rather than *Client so the sequence of commands and
// writes it drives can be asserted without an SSH server.
type Transport interface {
	// Output executes command in a shell on the host and returns its
	// stdout. A nonzero exit status is an error that includes stderr.
	Output(ctx context.Context, command string) ([]byte, error)

	// WriteFile writes content to path on the host, creating any parent
	// directories, with mode as the final file permissions. It replaces
	// path atomically: a reader on the host never observes a partial
	// write.
	WriteFile(ctx context.Context, path string, content []byte, mode uint32) error

	// Close releases the transport's underlying connection.
	Close() error
}
