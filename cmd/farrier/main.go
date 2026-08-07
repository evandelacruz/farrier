// Command farrier is the CLI entrypoint: flag parsing that calls straight
// into internal/core (spec.md "Interfaces: one core, thin frontends"). It
// carries no logic of its own.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/deploy"
	"github.com/evandelacruz/farrier/internal/core/events"
	"github.com/evandelacruz/farrier/internal/core/orchestrate"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	var err error
	switch os.Args[1] {
	case "up":
		err = runUp(os.Args[2:])
	case "-h", "--help", "help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "farrier: unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "farrier: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: farrier <command> [flags]")
	fmt.Fprintln(os.Stderr, "\ncommands:")
	fmt.Fprintln(os.Stderr, "  up    deploy the stateless layer to a target host (UP-001)")
}

// runUp implements the `up` command (UP-001): it connects to the target
// over SSH and calls deploy.Up, printing the same job event stream a
// dashboard would render over SSE (spec.md "one core, thin frontends").
func runUp(args []string) error {
	fs := flag.NewFlagSet("up", flag.ExitOnError)
	bundleDir := fs.String("bundle", "", "path to the bundle directory (required)")
	target := fs.String("target", "", "ssh://user@host[:port] of the deployment target (required)")
	remoteDir := fs.String("remote-dir", "/opt/farrier", "directory on the host to deploy into")
	keyFile := fs.String("ssh-key", "", "SSH private key file (default: the operator's SSH agent)")
	knownHosts := fs.String("known-hosts", "", "known_hosts file (default: ~/.ssh/known_hosts)")
	timeout := fs.Duration("ssh-timeout", 0, "SSH dial and handshake timeout (default: orchestrate.DefaultTimeout)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *bundleDir == "" || *target == "" {
		fs.Usage()
		return fmt.Errorf("up: -bundle and -target are required")
	}

	b, err := bundle.Load(*bundleDir)
	if err != nil {
		return fmt.Errorf("load bundle: %w", err)
	}

	ctx := context.Background()
	client, err := orchestrate.Connect(ctx, *target, orchestrate.Options{
		KeyFile:        *keyFile,
		KnownHostsFile: *knownHosts,
		Timeout:        *timeout,
	})
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer client.Close()

	job := events.NewJob()
	stream, cancel := job.Subscribe()
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ev := range stream {
			printEvent(ev)
		}
	}()

	deployErr := deploy.Up(ctx, job, client, b, deploy.Options{RemoteDir: *remoteDir})
	<-done
	return deployErr
}

// printEvent renders one CORE-002 event the way both frontends are meant
// to: as it arrives, in the terminal for the CLI and over SSE for the
// dashboard. A step's own event is prefixed with the step name; the
// job-terminal event (empty step) is not.
func printEvent(ev events.Event) {
	ts := ev.Timestamp.Format(time.RFC3339)
	if ev.Step == "" {
		fmt.Printf("[%s] %s: %s\n", ts, ev.State, ev.Detail)
		return
	}
	fmt.Printf("[%s] %s %s: %s\n", ts, ev.Step, ev.State, ev.Detail)
}
