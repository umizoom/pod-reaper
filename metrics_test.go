package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHealthz(t *testing.T) {
	m := newMetrics()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := start
	h := m.handler(10*time.Second, func() time.Time { return now }, start)

	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}

	if rec := get("/healthz"); rec.Code != http.StatusOK {
		t.Fatalf("startup grace: code = %d", rec.Code)
	}
	now = start.Add(3 * time.Minute)
	if rec := get("/healthz"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no scan ever completed: code = %d", rec.Code)
	}
	m.lastCompleted.Store(now.Add(-30 * time.Second).UnixNano())
	if rec := get("/healthz"); rec.Code != http.StatusOK {
		t.Fatalf("recent scan: code = %d", rec.Code)
	}

	m.reaped.WithLabelValues("default", "Deployment/x", "CrashLoopBackOff").Inc()
	rec := get("/metrics")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `pod_reaper_reaped_total{namespace="default",reason="CrashLoopBackOff",workload="Deployment/x"} 1`) {
		t.Fatalf("metrics output missing reaped counter:\n%s", rec.Body.String())
	}
}
