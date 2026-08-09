package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/evandelacruz/farrier/internal/api"
	"github.com/evandelacruz/farrier/internal/core/ui"
)

// runUI implements the `ui` command (UI-001): it serves the dashboard and
// the loopback API on one address and opens the operator's browser there,
// then serves until interrupted. The command itself is flag parsing and
// signal wiring — the serving, the routing, and the browser are
// internal/core/ui, and the API handler is the same api.Server the
// dashboard's future views will call.
func runUI(args []string) int {
	fs := flag.NewFlagSet("ui", flag.ContinueOnError)
	addr := fs.String("addr", api.DefaultAddr, "address to serve the dashboard and API on (loopback by default; a wider bind is your own topology)")
	noOpen := fs.Bool("no-open", false, "serve without opening a browser")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	err := ui.Serve(ctx, ui.Options{
		Addr:   *addr,
		API:    api.New().Handler(),
		NoOpen: *noOpen,
		Out:    os.Stdout,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "farrier: ui: %v\n", err)
		return 1
	}
	return 0
}
