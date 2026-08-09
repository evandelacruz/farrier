package ui

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"
)

// testAssets stands in for the embedded dashboard so routing assertions do
// not depend on the real page's wording.
func testAssets() fstest.MapFS {
	return fstest.MapFS{
		"index.html": {Data: []byte("<!DOCTYPE html><title>dashboard</title>")},
		"app.css":    {Data: []byte("body { color: red }")},
	}
}

// apiStub records what reached the API handler and answers with a marker.
type apiStub struct {
	method string
	path   string
	calls  int
}

func (a *apiStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.calls++
	a.method = r.Method
	a.path = r.URL.Path
	io.WriteString(w, "api")
}

func TestHandlerServesDashboardAtRoot(t *testing.T) {
	h, err := Handler(&apiStub{}, testAssets())
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "dashboard") {
		t.Errorf("body = %q, want the dashboard index", rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
}

func TestHandlerServesStaticAssetsUnderUIPrefix(t *testing.T) {
	h, err := Handler(&apiStub{}, testAssets())
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ui/app.css", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "color: red") {
		t.Errorf("body = %q, want the stylesheet", rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/css") {
		t.Errorf("Content-Type = %q, want text/css", ct)
	}
}

// The API keeps every path it is spec'd for: the dashboard claims only /
// and /ui/, so adding an RPC verb never needs a change in this package.
func TestHandlerRoutesEverythingElseToAPI(t *testing.T) {
	for _, tc := range []struct {
		name   string
		method string
		path   string
	}{
		{"status verb", http.MethodGet, "/status"},
		{"mutation verb", http.MethodPost, "/promote"},
		{"job event stream", http.MethodGet, "/jobs/abc/events"},
		{"non-GET on the dashboard's own path", http.MethodPost, "/"},
		{"unknown path", http.MethodGet, "/nope"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := &apiStub{}
			h, err := Handler(api, testAssets())
			if err != nil {
				t.Fatalf("Handler: %v", err)
			}

			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))

			if api.calls != 1 {
				t.Fatalf("API handler calls = %d, want 1 (body %q)", api.calls, rec.Body.String())
			}
			if api.method != tc.method || api.path != tc.path {
				t.Errorf("API saw %s %s, want %s %s", api.method, api.path, tc.method, tc.path)
			}
		})
	}
}

func TestHandlerDefaultsToEmbeddedDashboard(t *testing.T) {
	h, err := Handler(&apiStub{}, nil)
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Farrier") {
		t.Errorf("body = %q, want the embedded dashboard", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ui/app.css", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("stylesheet status = %d, want 200 — the embedded page links it", rec.Code)
	}
}

func TestHandlerRejectsMissingAPI(t *testing.T) {
	if _, err := Handler(nil, testAssets()); err == nil {
		t.Fatal("Handler(nil, ...) = nil error, want a rejection")
	}
}

func TestHandlerRejectsAssetsWithoutIndex(t *testing.T) {
	assets := fstest.MapFS{"app.css": {Data: []byte("body {}")}}
	if _, err := Handler(&apiStub{}, assets); err == nil {
		t.Fatal("Handler with no index.html = nil error, want a rejection")
	}
}

// syncBuffer is a writer the test goroutine can read while Serve writes to
// it, which a plain strings.Builder is not.
type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// serveInBackground runs Serve on a free loopback port and returns the URL
// it announced, everything it printed, and a func that cancels it and
// reports Serve's error.
func serveInBackground(t *testing.T, opts Options) (url string, out *syncBuffer, stop func() error) {
	t.Helper()

	if opts.openURL == nil {
		opts.openURL = func(string) error { return nil }
	}
	if opts.Addr == "" {
		opts.Addr = "127.0.0.1:0"
	}
	if opts.API == nil {
		opts.API = &apiStub{}
	}
	if opts.Assets == nil {
		opts.Assets = testAssets()
	}
	out = &syncBuffer{}
	opts.Out = out

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- Serve(ctx, opts) }()

	// The announced URL is written before the browser is opened and
	// after the listener is bound, so waiting for the line is also
	// waiting for the port to be live.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if line := out.String(); strings.HasPrefix(line, "dashboard: ") {
			url = strings.TrimSpace(strings.TrimPrefix(strings.SplitN(line, "\n", 2)[0], "dashboard: "))
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("Serve never announced a URL; output %q", out.String())
		}
		time.Sleep(5 * time.Millisecond)
	}

	return url, out, func() error {
		cancel()
		select {
		case err := <-errc:
			return err
		case <-time.After(5 * time.Second):
			t.Fatal("Serve did not return after its context was cancelled")
			return nil
		}
	}
}

func TestServeServesDashboardAndOpensBrowser(t *testing.T) {
	opened := make(chan string, 1)
	api := &apiStub{}
	url, _, stop := serveInBackground(t, Options{
		API: api,
		openURL: func(u string) error {
			opened <- u
			return nil
		},
	})

	select {
	case got := <-opened:
		if got != url {
			t.Errorf("opened %q, want the served URL %q", got, url)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve never opened a browser")
	}
	if !strings.HasPrefix(url, "http://127.0.0.1:") {
		t.Errorf("served URL = %q, want a loopback address", url)
	}

	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "dashboard") {
		t.Errorf("GET %s = %d %q, want the dashboard", url, resp.StatusCode, body)
	}

	resp, err = http.Get(url + "status")
	if err != nil {
		t.Fatalf("GET status: %v", err)
	}
	resp.Body.Close()
	if api.calls != 1 {
		t.Errorf("API handler calls = %d, want the API served on the same address", api.calls)
	}

	if err := stop(); err != nil {
		t.Errorf("Serve returned %v after a clean shutdown, want nil", err)
	}
}

// A browser that will not open is reported, not fatal: a headless host
// still gets a dashboard and a URL.
func TestServeKeepsServingWhenTheBrowserWillNotOpen(t *testing.T) {
	url, out, stop := serveInBackground(t, Options{
		openURL: func(string) error { return errNoBrowser },
	})

	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want the dashboard still served", resp.StatusCode)
	}
	if err := stop(); err != nil {
		t.Errorf("Serve = %v, want nil", err)
	}
	if !strings.Contains(out.String(), "could not open a browser") || !strings.Contains(out.String(), url) {
		t.Errorf("output = %q, want the failure and the URL to open by hand", out.String())
	}
}

func TestServeNoOpenSkipsTheBrowser(t *testing.T) {
	var opened atomic.Int32
	_, _, stop := serveInBackground(t, Options{
		NoOpen:  true,
		openURL: func(string) error { opened.Add(1); return nil },
	})
	if err := stop(); err != nil {
		t.Errorf("Serve = %v, want nil", err)
	}
	if got := opened.Load(); got != 0 {
		t.Errorf("browser opened %d times, want 0 with NoOpen set", got)
	}
}

func TestServeRejectsMissingAddr(t *testing.T) {
	err := Serve(context.Background(), Options{API: &apiStub{}, Assets: testAssets(), NoOpen: true})
	if err == nil {
		t.Fatal("Serve with no address = nil error, want a rejection")
	}
}

func TestServeReportsAnUnusableAddress(t *testing.T) {
	err := Serve(context.Background(), Options{
		Addr:   "127.0.0.1:not-a-port",
		API:    &apiStub{},
		Assets: testAssets(),
		NoOpen: true,
	})
	if err == nil {
		t.Fatal("Serve on an unusable address = nil error, want the bind failure")
	}
	if !strings.Contains(err.Error(), "listen") {
		t.Errorf("error = %v, want it to name the failed bind", err)
	}
}

var errNoBrowser = errorString("no display")

type errorString string

func (e errorString) Error() string { return string(e) }
