package diagnostics

import (
	"github.com/prometheus/client_golang/prometheus"
	"go.miloapis.com/instrumentation/labels"
	"go.miloapis.com/instrumentation/metrics"

	"go.datum.net/looking-glass/api/v1alpha"
)

// labelDiagnostic is the diagnostic type label. The instrumentation library
// has no constant for it.
const labelDiagnostic = "diagnostic"

var (
	// sessionsTotal counts the sessions Run ended, by terminal reason.
	sessionsTotal = metrics.NewCounterVec(prometheus.CounterOpts{
		Namespace: "looking_glass",
		Subsystem: "gateway",
		Name:      "sessions_total",
		Help:      "Claimed sessions the gateway ran, by diagnostic type and terminal reason.",
	}, []string{labelDiagnostic, labels.Reason})

	// backendCallSeconds measures RunDiagnostic calls, from the call to the
	// end of the stream, including failed ones.
	backendCallSeconds = metrics.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "looking_glass",
		Subsystem: "gateway",
		Name:      "backend_call_duration_seconds",
		Help:      "Duration of backend RunDiagnostic calls by diagnostic type.",
		Buckets:   []float64{0.25, 0.5, 1, 2.5, 5, 10, 20, 40, 60, 90},
	}, []string{labelDiagnostic})

	// routerOutcomesTotal counts router outcomes. reason is Succeeded for an
	// observation, or the failure frame's reason.
	routerOutcomesTotal = metrics.NewCounterVec(prometheus.CounterOpts{
		Namespace: "looking_glass",
		Subsystem: "gateway",
		Name:      "router_outcomes_total",
		Help:      "Router outcomes the gateway streamed, by diagnostic type and reason.",
	}, []string{labelDiagnostic, labels.Reason})

	// streamViolationsTotal counts backend streams that broke the contract.
	streamViolationsTotal = metrics.NewCounter(prometheus.CounterOpts{
		Namespace: "looking_glass",
		Subsystem: "gateway",
		Name:      "backend_stream_violations_total",
		Help:      "Backend streams that broke the contract's grammar or counts.",
	})

	// slowClientsTotal counts clients disconnected for not reading frames.
	slowClientsTotal = metrics.NewCounter(prometheus.CounterOpts{
		Namespace: "looking_glass",
		Subsystem: "gateway",
		Name:      "slow_client_disconnects_total",
		Help:      "Clients the gateway disconnected because they read frames too slowly.",
	})
)

// diagnosticLabel bounds the diagnostic label to the four known types.
func diagnosticLabel(t v1alpha.DiagnosticType) string {
	switch t {
	case v1alpha.DiagnosticRouteLookup, v1alpha.DiagnosticBGPSummary, v1alpha.DiagnosticPing, v1alpha.DiagnosticTraceroute:
		return string(t)
	default:
		return "Unknown"
	}
}
