package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// expvar registers /debug/vars on http.DefaultServeMux at init time, and the
// Prometheus client imports expvar. The public listener must never serve that
// default mux, and debug/metrics paths must not fall through to the index handler.
func TestPublicMuxDoesNotExposeDebugOrMetrics(t *testing.T) {
	mux := newPublicMux(time.Now())

	for _, path := range []string{"/debug/vars", "/debug", "/debug/pprof/", "/debug/pprof/goroutine", "/metrics"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, rec.Code)
		}
	}

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/config", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("GET /api/config = %d, want 200", rec.Code)
	}
}
