package diagnostics

import (
	"testing"

	"go.miloapis.com/instrumentation/instrumenttest"
	"go.miloapis.com/instrumentation/metrics"
)

// TestInstrumentationAdopted fails on a metric registered where no endpoint
// serves it, or a logger built outside the instrumentation library, anywhere
// in the module.
func TestInstrumentationAdopted(t *testing.T) {
	instrumenttest.AssertAdopted(t)
}

// TestMetricsLint checks the gateway's metrics against Prometheus naming
// conventions.
func TestMetricsLint(t *testing.T) {
	instrumenttest.Lint(t)
}

// TestMetricsScrape scrapes the instrumentation library's handler with the
// gateway's metrics linked.
func TestMetricsScrape(t *testing.T) {
	instrumenttest.AssertScrapes(t, metrics.Handler())
}
