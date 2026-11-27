package routerbackend

import (
	"github.com/prometheus/client_golang/prometheus"
	"go.miloapis.com/instrumentation/labels"
	"go.miloapis.com/instrumentation/metrics"
)

// Metric label names that the instrumentation library has no constant for.
const (
	labelDiagnostic = "diagnostic"
	labelLocation   = "location"
	labelRouter     = "router"
)

var (
	// requestsTotal counts diagnostics by their end: a refusal reason before
	// the snapshot, or the complete event's reason.
	requestsTotal = metrics.NewCounterVec(prometheus.CounterOpts{
		Namespace: "looking_glass",
		Subsystem: "backend",
		Name:      "requests_total",
		Help:      "Diagnostics by type and by refusal or completion reason.",
	}, []string{labelDiagnostic, labels.Reason})

	// routerOutcomesTotal counts router outcomes. reason is Succeeded or the
	// failure reason.
	routerOutcomesTotal = metrics.NewCounterVec(prometheus.CounterOpts{
		Namespace: "looking_glass",
		Subsystem: "backend",
		Name:      "router_outcomes_total",
		Help:      "Router outcomes by location, router, diagnostic type and reason.",
	}, []string{labelLocation, labelRouter, labelDiagnostic, labels.Reason})

	// routerSeconds measures driver calls, including failed ones.
	routerSeconds = metrics.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "looking_glass",
		Subsystem: "backend",
		Name:      "router_call_duration_seconds",
		Help:      "Duration of driver calls by diagnostic type.",
		Buckets:   []float64{0.1, 0.25, 0.5, 1, 2.5, 5, 10, 20, 40, 80},
	}, []string{labelDiagnostic})

	// liveDiagnostics is the number of diagnostics past admission and not yet
	// finished.
	liveDiagnostics = metrics.NewGauge(prometheus.GaugeOpts{
		Namespace: "looking_glass",
		Subsystem: "backend",
		Name:      "live_diagnostics",
		Help:      "Diagnostics currently running.",
	})
)
