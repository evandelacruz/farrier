package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/events"
	"github.com/evandelacruz/farrier/internal/core/publish"
)

func doPublish(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/publish", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func TestHandlePublishBadJSON(t *testing.T) {
	if rec := doPublish(t, newTestServer(), "{not json"); rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestHandlePublishMissingDir(t *testing.T) {
	t.Setenv("FARRIER_TARGET_TOKEN", "t")
	if rec := doPublish(t, newTestServer(), `{}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

// The token comes from the server's own environment, never the request
// body, so an unset one is the server's problem to report.
func TestHandlePublishMissingToken(t *testing.T) {
	t.Setenv("FARRIER_TARGET_TOKEN", "")
	if rec := doPublish(t, newTestServer(), `{"dir":"/src/thing"}`); rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
}

func TestHandlePublishBadBundle(t *testing.T) {
	t.Setenv("FARRIER_TARGET_TOKEN", "t")
	s := newTestServer()
	s.loadBundle = func(dir string) (*bundle.Bundle, error) {
		return nil, fmt.Errorf("no manifest at %s", dir)
	}
	if rec := doPublish(t, s, `{"dir":"/src/thing"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestHandlePublishStartsAJob(t *testing.T) {
	t.Setenv("FARRIER_TARGET_TOKEN", "instance-token")

	var gotBundleDir string
	var gotOpts publish.Options
	done := make(chan struct{})

	s := newTestServer()
	s.loadBundle = func(dir string) (*bundle.Bundle, error) {
		gotBundleDir = dir
		return &bundle.Bundle{Manifest: bundle.Manifest{Domain: "git.example.com"}}, nil
	}
	s.publishRun = func(ctx context.Context, job *events.Job, opts publish.Options) (publish.Result, error) {
		gotOpts = opts
		close(done)
		return publish.Result{}, nil
	}

	rec := doPublish(t, s, `{"dir":"/src/thing","owner":"acme","name":"widgets","remote":"farrier","publicKey":"/keys/id.pub","target":"http://192.168.1.5:8222","address":"box.local"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusAccepted)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["jobId"] == "" {
		t.Error("response carries no job ID")
	}
	<-done

	if want := bundle.DirFor("/src/thing"); gotBundleDir != want {
		t.Errorf("bundle dir = %q, want %q", gotBundleDir, want)
	}
	if gotOpts.Dir != "/src/thing" || gotOpts.Owner != "acme" || gotOpts.Name != "widgets" {
		t.Errorf("opts = %+v, want the request's dir, owner, and name", gotOpts)
	}
	if gotOpts.RemoteName != "farrier" || gotOpts.PublicKeyPath != "/keys/id.pub" {
		t.Errorf("opts = %+v, want the request's remote and public key", gotOpts)
	}
	// Where the instance is comes through the wire unchanged; the core
	// decides what to do with it (UP-006).
	if gotOpts.TargetBaseURL != "http://192.168.1.5:8222" || gotOpts.Address != "box.local" {
		t.Errorf("opts = %+v, want the request's target and address", gotOpts)
	}
	if !gotOpts.Private {
		t.Error("private defaulted to false, want true")
	}
	if gotOpts.TargetToken.Reveal() != "instance-token" {
		t.Error("the token did not come from the server environment")
	}
}

func TestHandlePublishHonorsExplicitPrivateFalse(t *testing.T) {
	t.Setenv("FARRIER_TARGET_TOKEN", "t")

	var gotOpts publish.Options
	done := make(chan struct{})

	s := newTestServer()
	s.loadBundle = func(string) (*bundle.Bundle, error) {
		return &bundle.Bundle{Manifest: bundle.Manifest{Domain: "git.example.com"}}, nil
	}
	s.publishRun = func(ctx context.Context, job *events.Job, opts publish.Options) (publish.Result, error) {
		gotOpts = opts
		close(done)
		return publish.Result{}, nil
	}

	if rec := doPublish(t, s, `{"dir":"/src/thing","private":false,"bundle":"/elsewhere/.farrier"}`); rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusAccepted)
	}
	<-done
	if gotOpts.Private {
		t.Error("private = true, want the request's explicit false")
	}
}
