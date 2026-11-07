package main

import (
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics holds the Prometheus collectors and liveness state.
type Metrics struct {
	registry *prometheus.Registry

	reaped        *prometheus.CounterVec // namespace, workload, reason
	skipped       *prometheus.CounterVec // reason: cooldown | rate_limit
	deleteErrors  prometheus.Counter
	scanErrors    prometheus.Counter
	notifyErrors  prometheus.Counter
	lastSuccess   prometheus.Gauge
	scanDuration  prometheus.Histogram
	lastCompleted atomic.Int64 // unix nanos of the last finished scan (success or error)
}

func newMetrics() *Metrics {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	f := promauto.With(reg)
	return &Metrics{
		registry: reg,
		reaped: f.NewCounterVec(prometheus.CounterOpts{
			Name: "pod_reaper_reaped_total",
			Help: "Pods deleted by pod-reaper.",
		}, []string{"namespace", "workload", "reason"}),
		skipped: f.NewCounterVec(prometheus.CounterOpts{
			Name: "pod_reaper_skipped_total",
			Help: "Eligible pods not deleted, counted once per cooldown or rate-limit window.",
		}, []string{"reason"}),
		deleteErrors: f.NewCounter(prometheus.CounterOpts{
			Name: "pod_reaper_delete_errors_total",
			Help: "Failed pod delete calls.",
		}),
		scanErrors: f.NewCounter(prometheus.CounterOpts{
			Name: "pod_reaper_scan_errors_total",
			Help: "Scans that failed to list pods.",
		}),
		notifyErrors: f.NewCounter(prometheus.CounterOpts{
			Name: "pod_reaper_notify_errors_total",
			Help: "Failed webhook notifications.",
		}),
		lastSuccess: f.NewGauge(prometheus.GaugeOpts{
			Name: "pod_reaper_last_successful_scan_timestamp_seconds",
			Help: "Unix time of the last scan that listed pods successfully.",
		}),
		scanDuration: f.NewHistogram(prometheus.HistogramOpts{
			Name:    "pod_reaper_scan_duration_seconds",
			Help:    "Duration of each scan.",
			Buckets: prometheus.DefBuckets,
		}),
	}
}

// handler serves /metrics and /healthz. /healthz fails when no scan has
// finished within the liveness window, i.e. the polling loop is stuck.
func (m *Metrics) handler(interval time.Duration, now func() time.Time, started time.Time) http.Handler {
	window := 3 * interval
	if window < 2*time.Minute {
		window = 2 * time.Minute
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		ref := started
		if last := m.lastCompleted.Load(); last != 0 {
			ref = time.Unix(0, last)
		}
		if age := now().Sub(ref); age > window {
			http.Error(w, fmt.Sprintf("no scan completed in %s", age.Round(time.Second)), http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok\n"))
	})
	return mux
}
