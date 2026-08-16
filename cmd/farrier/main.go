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

// command is one subcommand: its runner, and the one line describing it in
// the command list. Each runner owns its own flag parsing and returns the
// process exit code.
type command struct {
	run     func(args []string) int
	summary string
}

// commands is keyed by subcommand name. The order the list prints in comes
// from commandOrder, not from ranging this map — map iteration is randomized,
// so ranging it printed the commands in a different order on every run.
var commands = map[string]command{
	"init":    {runInit, "make a project folder into a forge definition"},
	"up":      {runUp, "deploy a bundle onto a host"},
	"publish": {runPublish, "create the repository on the instance and point origin at it"},
	"import":  {runImport, "bring repositories in from GitHub or GitLab"},
	"attach":  {runAttach, "give a nameless instance a domain, in place"},
	"status":  {runStatus, "instance health, certificate expiry, and last-backup age"},
	"backup":  {runBackup, "produce a verified, encrypted snapshot"},
	"restore": {runRestore, "rebuild an instance from a snapshot onto a fresh host"},
	"promote": {runPromote, "fail over: restore, verify, start, reconcile CI, flip DNS"},
	"upgrade": {runUpgrade, "back up, bump the pinned version, migrate, verify"},
	"drill":   {runDrill, "rehearse a restore on a scratch target and report"},
	"ui":      {runUI, "serve the dashboard on loopback and open a browser"},
}

// commandOrder is lifecycle order — the sequence an operator meets these in —
// rather than alphabetical, so the list doubles as a table of contents.
var commandOrder = []string{
	"init", "up", "publish", "import", "attach",
	"status", "backup", "restore", "promote", "upgrade", "drill", "ui",
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		printUsage(os.Stderr)
		return 2
	}

	// Asking for help is not a usage error, so it prints to stdout and exits
	// 0 — otherwise `farrier --help | less` shows nothing and any wrapper
	// script treats a successful help request as a failure.
	switch args[0] {
	case "help", "-h", "--help":
		if len(args) > 1 {
			return helpFor(args[1])
		}
		printUsage(os.Stdout)
		return 0
	}

	cmd, ok := commands[args[0]]
	if !ok {
		fmt.Fprintf(os.Stderr, "farrier: unknown command %q\n", args[0])
		printUsage(os.Stderr)
		return 2
	}
	return cmd.run(args[1:])
}

// helpFor prints one command's flags by handing it -h, which every runner's
// flag set already understands. The runner returns 2 for that, since it
// cannot tell a help request from a parse error — but this call site can, so
// the exit code is 0.
//
// The runner writes that usage to os.Stderr — no flag set in this package
// calls SetOutput, and flag.FlagSet.Output() falls back to os.Stderr when it
// has none. Sending it there would reproduce the bug this function exists to
// fix, one level down: `farrier help up | less` would show nothing while
// exiting 0. So os.Stderr is pointed at stdout for the duration of the call.
//
// It reads as a blunt instrument and it is, but the alternative is calling
// SetOutput in all twelve runners and keeping every future one in line — a
// rule that holds only as long as everyone remembers it. flag.FlagSet.Output()
// resolves os.Stderr on each call rather than capturing it, which is what
// makes the swap work at all.
func helpFor(name string) int {
	cmd, ok := commands[name]
	if !ok {
		fmt.Fprintf(os.Stderr, "farrier: unknown command %q\n", name)
		printUsage(os.Stderr)
		return 2
	}

	saved := os.Stderr
	os.Stderr = os.Stdout
	defer func() { os.Stderr = saved }()

	cmd.run([]string{"-h"})
	return 0
}

func printUsage(w *os.File) {
	fmt.Fprintln(w, "usage: farrier <command> [flags]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "commands:")
	for _, name := range commandOrder {
		fmt.Fprintf(w, "  %-8s %s\n", name, commands[name].summary)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "run `farrier help <command>` for a command's flags")
}

// runUp implements the `up` command (UP-001): it connects to the target
// over SSH and calls deploy.Up, printing the same job event stream a
// dashboard would render over SSE (spec.md "one core, thin frontends").
//
// -address is how a nameless bundle (INIT-005) learns where it is reached
// (UP-006). It is not validated here: whether it is required, forbidden, or
// well-formed is deploy.Up's call, so the CLI and the API report the same
// thing.
func runUp(args []string) int {
	fs := flag.NewFlagSet("up", flag.ContinueOnError)
	bundleDir := fs.String("bundle", "", "path to the bundle directory (required)")
	target := fs.String("target", "", "ssh://user@host[:port] of the deployment target (required)")
	remoteDir := fs.String("remote-dir", orchestrate.DefaultRemoteDir, "directory on the host to deploy into")
	address := fs.String("address", "", "IP or hostname to serve a nameless bundle's web UI at over plain HTTP (nameless bundles only); must be reachable from a container when the bundle deploys CI, so not a loopback address")
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
		return deploy.Up(ctx, job, client, b, deploy.Options{RemoteDir: *remoteDir, Address: *address})
	})

	if err != nil {
		fmt.Fprintf(os.Stderr, "farrier: up: %v\n", err)
		return 1
	}
	return 0
}
