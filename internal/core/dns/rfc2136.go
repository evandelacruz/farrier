package dns

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"time"
)

// rfc2136WaitDelay bounds how long a Set/Delete call waits for nsupdate's
// stdout/stderr to close after the process exits or is killed, the same
// protection keystore's CommandDriver applies to its configured command.
const rfc2136WaitDelay = 2 * time.Second

// defaultTSIGAlgorithm is used when RFC2136Config.Algorithm is unset.
const defaultTSIGAlgorithm = "hmac-sha256"

// RFC2136Config configures the "rfc2136" DNS driver (DNS-002): dynamic DNS
// updates via nsupdate, authenticated with a TSIG key.
//
// Server, Zone, KeyName, and Algorithm are non-secret bundle driver config
// (bundle.DriverRef); KeySecret is not — a caller resolves it through a
// keystore driver before building RFC2136Config, the same split
// CloudflareConfig draws for its APIToken.
type RFC2136Config struct {
	// Server is host[:port] of the DNS server accepting updates; no
	// scheme, port defaults to 53.
	Server string
	Zone   string

	KeyName   string
	KeySecret string
	// Algorithm is the TSIG algorithm KeySecret is keyed under; empty
	// defaults to hmac-sha256.
	Algorithm string

	// Command overrides the nsupdate executable; empty resolves
	// "nsupdate" via PATH. Args are fixed arguments passed before the
	// update script, which always arrives on the process's stdin, never
	// as an argument, so the TSIG secret never appears in a process
	// listing.
	Command string
	Args    []string
}

// RFC2136Driver is the "rfc2136" DNS driver: Set and Delete via nsupdate
// dynamic updates, authenticated by a TSIG key.
type RFC2136Driver struct {
	server    string
	zone      string
	keyName   string
	keySecret string
	algorithm string
	command   string
	args      []string
}

// NewRFC2136 builds an RFC2136Driver from cfg.
func NewRFC2136(cfg RFC2136Config) (*RFC2136Driver, error) {
	if strings.TrimSpace(cfg.Server) == "" {
		return nil, fmt.Errorf("dns: rfc2136: server is required")
	}
	if strings.TrimSpace(cfg.Zone) == "" {
		return nil, fmt.Errorf("dns: rfc2136: zone is required")
	}
	if strings.TrimSpace(cfg.KeyName) == "" {
		return nil, fmt.Errorf("dns: rfc2136: tsig key name is required")
	}
	if strings.TrimSpace(cfg.KeySecret) == "" {
		return nil, fmt.Errorf("dns: rfc2136: tsig key secret is required")
	}

	algorithm := cfg.Algorithm
	if algorithm == "" {
		algorithm = defaultTSIGAlgorithm
	}
	command := cfg.Command
	if command == "" {
		command = "nsupdate"
	}

	return &RFC2136Driver{
		server:    cfg.Server,
		zone:      cfg.Zone,
		keyName:   cfg.KeyName,
		keySecret: cfg.KeySecret,
		algorithm: algorithm,
		command:   command,
		args:      cfg.Args,
	}, nil
}

// Set upserts record via nsupdate (DNS-002): a single update transaction
// deletes any existing RRset at record, of any type, then adds the new
// one, so switching a record's type (e.g. CNAME to A) leaves nothing
// stale.
func (d *RFC2136Driver) Set(ctx context.Context, record, value string, ttl time.Duration) error {
	if err := validateSetArgs(record, value, ttl); err != nil {
		return err
	}

	script := d.header()
	fmt.Fprintf(script, "update delete %s\n", record)
	fmt.Fprintf(script, "update add %s %d %s %s\n", record, int(ttl.Seconds()), recordType(value), value)
	script.WriteString("send\n")

	if err := d.run(ctx, script); err != nil {
		return fmt.Errorf("dns: rfc2136: set %s: %w", record, err)
	}
	return nil
}

// Delete removes every RRset at record via nsupdate (DNS-002).
func (d *RFC2136Driver) Delete(ctx context.Context, record string) error {
	if err := validateDeleteArgs(record); err != nil {
		return err
	}

	script := d.header()
	fmt.Fprintf(script, "update delete %s\n", record)
	script.WriteString("send\n")

	if err := d.run(ctx, script); err != nil {
		return fmt.Errorf("dns: rfc2136: delete %s: %w", record, err)
	}
	return nil
}

// header starts an nsupdate script with the TSIG key and target server and
// zone — the lines common to every Set/Delete transaction. The key line
// carries KeySecret, so this script travels only over the child process's
// stdin, never as a command-line argument.
func (d *RFC2136Driver) header() *bytes.Buffer {
	script := &bytes.Buffer{}
	fmt.Fprintf(script, "key %s:%s %s\n", d.algorithm, d.keyName, d.keySecret)
	host, port := splitServerAddr(d.server)
	fmt.Fprintf(script, "server %s %s\n", host, port)
	fmt.Fprintf(script, "zone %s\n", d.zone)
	return script
}

// splitServerAddr splits a host[:port] server address, defaulting to port
// 53 when none is given.
func splitServerAddr(server string) (host, port string) {
	host, port, err := net.SplitHostPort(server)
	if err != nil {
		return server, "53"
	}
	return host, port
}

func (d *RFC2136Driver) run(ctx context.Context, script *bytes.Buffer) error {
	cmd := exec.CommandContext(ctx, d.command, d.args...)
	cmd.Stdin = script
	cmd.WaitDelay = rfc2136WaitDelay
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("%w", ctx.Err())
		}
		return fmt.Errorf("%s: %w%s", d.command, err, rfc2136StderrSuffix(stderr.String()))
	}
	return nil
}

func rfc2136StderrSuffix(stderr string) string {
	stderr = strings.TrimSpace(stderr)
	if stderr == "" {
		return ""
	}
	return fmt.Sprintf(" (stderr: %s)", stderr)
}
