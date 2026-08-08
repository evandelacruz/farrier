package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewWiresRealCore(t *testing.T) {
	s := New()
	if s.jobs == nil || s.initRun == nil || s.loadBundle == nil || s.dial == nil || s.deployUp == nil ||
		s.importRun == nil || s.importRunBatch == nil || s.statusCheck == nil || s.newKeystore == nil {
		t.Fatalf("New() left a field unset: %+v", s)
	}
}

func TestHandlerUnknownRouteNotFound(t *testing.T) {
	s := newTestServer()
	req := httptest.NewRequest(http.MethodGet, "/nope", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}
