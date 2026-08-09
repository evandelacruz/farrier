// Package ui serves the dashboard on loopback and points the operator's
// browser at it (UI-001).
//
// The dashboard and the local control API share one listener and therefore
// one origin. That is the whole reason this package exists: the dashboard
// holds no logic of its own (CLAUDE.md "one core, thin skins"), so every
// view it will ever render is an internal/api call, and same-origin means
// those calls need no CORS handling, no second port, and no address for the
// page to be configured with. Routing is deliberately lopsided — the
// dashboard claims exactly two routes and the API handler takes everything
// else — so the RPC verbs keep the paths spec.md gives them and a new verb
// needs no change here.
//
// Serving comes before opening the browser, and a browser that will not
// open never stops the server: an operator on a headless box, over SSH, or
// with no desktop session still gets a working dashboard and a URL to point
// something at. The reverse — killing the server because the browser failed
// — would make the command useless in exactly the situations where a
// forge's control plane matters most.
package ui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os/exec"
	"runtime"
	"time"

	"github.com/evandelacruz/farrier/web"
)

// shutdownTimeout bounds the graceful shutdown after the context is
// cancelled. The dashboard's only long-lived requests are SSE job streams,
// which end as soon as their connections are closed, so this is a backstop
// against a wedged connection rather than a real drain window.
const shutdownTimeout = 5 * time.Second

// Options configures Serve.
type Options struct {
	// Addr is the TCP address to bind, "host:port". Required, and
	// loopback by default from the caller's side: the CLI defaults it to
	// api.DefaultAddr, and binding wider than loopback is the operator's
	// own topology (spec.md "Interfaces"). A zero port binds a free one,
	// which is what the announced URL then reports.
	Addr string

	// API handles every request the dashboard does not claim — the RPC
	// verbs and the SSE job stream of internal/api. Required.
	API http.Handler

	// Assets is the dashboard's file tree, which must contain
	// index.html. Nil uses web.Assets, the copy embedded in the binary;
	// tests substitute their own.
	Assets fs.FS

	// NoOpen serves without opening a browser, for an operator who wants
	// the URL and nothing else.
	NoOpen bool

	// Out receives the served URL and any browser-open failure. Nil
	// discards both — announcing to a terminal is the CLI's business,
	// not the core's.
	Out io.Writer

	// openURL opens url in the operator's browser. Nil uses
	// openBrowser; tests substitute their own.
	openURL func(url string) error
}

// Serve binds opts.Addr, serves the dashboard and the API on it, opens the
// operator's browser at the served URL, and blocks until ctx is cancelled
// or serving fails (UI-001).
//
// The listener is bound before the browser is opened so the URL is a real,
// already-listening address: a browser launched against a port that is
// still coming up races the first request. Cancelling ctx shuts the server
// down gracefully and returns nil — a Ctrl-C on the dashboard is the
// operator finishing, not a failure.
func Serve(ctx context.Context, opts Options) error {
	handler, err := Handler(opts.API, opts.Assets)
	if err != nil {
		return err
	}
	if opts.Addr == "" {
		return errors.New("ui: serve: an address to bind is required")
	}
	out := opts.Out
	if out == nil {
		out = io.Discard
	}

	ln, err := net.Listen("tcp", opts.Addr)
	if err != nil {
		return fmt.Errorf("ui: serve: listen on %s: %w", opts.Addr, err)
	}

	url := "http://" + ln.Addr().String() + "/"
	fmt.Fprintf(out, "dashboard: %s\n", url)

	srv := &http.Server{Handler: handler}
	serveErr := make(chan error, 1)
	go func() {
		err := srv.Serve(ln)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serveErr <- err
	}()

	if !opts.NoOpen {
		open := opts.openURL
		if open == nil {
			open = openBrowser
		}
		if err := open(url); err != nil {
			fmt.Fprintf(out, "could not open a browser (%v) — open %s yourself\n", err, url)
		}
	}

	select {
	case err := <-serveErr:
		if err != nil {
			return fmt.Errorf("ui: serve: %w", err)
		}
		return nil
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("ui: serve: shut down: %w", err)
	}
	<-serveErr
	return nil
}

// Handler routes the dashboard and the API onto one mux: GET / renders the
// dashboard's index.html, GET /ui/... serves its remaining static assets,
// and every other request — including a POST to / — goes to api.
//
// Only those two dashboard patterns are registered, and both are more
// specific than the "/" the API handler is registered under, so the API
// keeps the exact paths spec.md assigns it (POST /promote, GET /status,
// GET /jobs/{id}/events) and gaining a verb requires no change here. The
// static assets sit under a /ui/ prefix for the same reason: it is the one
// path segment the API is guaranteed never to want.
func Handler(api http.Handler, assets fs.FS) (http.Handler, error) {
	if api == nil {
		return nil, errors.New("ui: an API handler is required")
	}
	if assets == nil {
		assets = web.Assets
	}
	if _, err := fs.Stat(assets, "index.html"); err != nil {
		return nil, fmt.Errorf("ui: dashboard assets: %w", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFileFS(w, r, assets, "index.html")
	})
	mux.Handle("GET /ui/", http.StripPrefix("/ui/", http.FileServerFS(assets)))
	mux.Handle("/", api)
	return mux, nil
}

// openBrowser opens url in whatever the operator's desktop session treats
// as the browser, via the platform's own opener — macOS `open`, and
// `xdg-open` elsewhere, which is the freedesktop entry point every Linux
// desktop implements. Farrier's control plane targets macOS and Linux
// (tech-spec.md "Operational targets"); anywhere else, Serve reports that
// it could not open a browser and keeps serving.
//
// The command is started and reaped rather than waited on: `xdg-open` may
// hand off to a browser that outlives the dashboard, and waiting for it
// would stall Serve until the operator closed their browser.
func openBrowser(url string) error {
	var opener string
	switch runtime.GOOS {
	case "darwin":
		opener = "open"
	case "linux", "freebsd", "openbsd", "netbsd":
		opener = "xdg-open"
	default:
		return fmt.Errorf("no known browser opener for %s", runtime.GOOS)
	}

	cmd := exec.Command(opener, url)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("%s: %w", opener, err)
	}
	go cmd.Wait()
	return nil
}
