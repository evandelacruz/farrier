// Command farrier is the CLI frontend: flag parsing only, every real
// decision lives in internal/core (CLAUDE.md "House patterns").
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/deploy"
	"github.com/evandelacruz/farrier/internal/core/events"
	"github.com/evandelacruz/farrier/internal/core/orchestrate"
)

// commands maps a subcommand name to its runner. Each runner owns its own
// flag parsing and returns the process exit code.
var commands = map[string]func(args []string) int{
	"init":    runInit,
	"up":      runUp,
	"status":  runStatus,
	"import":  runImport,
	"backup":  runBackup,
	"restore": runRestore,
	"promote": runPromote,
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: farrier <command> [flags]")
		printCommands()
		return 2
	}

	cmd, ok := commands[args[0]]
	if !ok {
		fmt.Fprintf(os.Stderr, "farrier: unknown command %q\n", args[0])
		printCommands()
		return 2
	}
	return cmd(args[1:])
}

func printCommands() {
	fmt.Fprintln(os.Stderr, "commands:")
	for name := range commands {
		fmt.Fprintf(os.Stderr, "  %s\n", name)
	}
}

// runUp implements the `up` command (UP-001): it connects to the target
// over SSH and calls deploy.Up, printing the same job event stream a
// dashboard would render over SSE (spec.md "one core, thin frontends").
func runUp(args []string) int {
	fs := flag.NewFlagSet("up", flag.ContinueOnError)
	bundleDir := fs.String("bundle", "", "path to the bundle directory (required)")
	target := fs.String("target", "", "ssh://user@host[:port] of the deployment target (required)")
	remoteDir := fs.String("remote-dir", "/opt/farrier", "directory on the host to deploy into")
	keyFile := fs.String("ssh-key", "", "SSH private key file (default: the operator's SSH agent)")
	knownHosts := fs.String("known-hosts", "", "known_hosts file (default: ~/.ssh/known_hosts)")
	timeout := fs.Duration("ssh-timeout", 0, "SSH dial and handshake timeout (default: orchestrate.DefaultTimeout)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *bundleDir == "" || *target == "" {
		fs.Usage()
		fmt.Fprintln(os.Stderr, "farrier: up: -bundle and -target are required")
		return 2
	}

	b, err := bundle.Load(*bundleDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "farrier: up: load bundle: %v\n", err)
		return 1
	}

	ctx := context.Background()
	client, err := orchestrate.Connect(ctx, *target, orchestrate.Options{
		KeyFile:        *keyFile,
		KnownHostsFile: *knownHosts,
		Timeout:        *timeout,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "farrier: up: connect: %v\n", err)
		return 1
	}
	defer client.Close()

	job := events.NewJob()
	err = runJob(job, func() error {
		return deploy.Up(ctx, job, client, b, deploy.Options{RemoteDir: *remoteDir})
	})

	if err != nil {
		fmt.Fprintf(os.Stderr, "farrier: up: %v\n", err)
		return 1
	}
	return 0
}
