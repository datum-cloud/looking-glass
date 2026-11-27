package cmd

import (
	"testing"

	"go.miloapis.com/instrumentation/instrumenttest"
	"go.miloapis.com/instrumentation/metrics"
)

// TestMetricsLint checks the metrics this binary links (the router backend's
// among them) against Prometheus naming conventions.
func TestMetricsLint(t *testing.T) {
	instrumenttest.Lint(t)
}

// TestMetricsScrape scrapes the handler the backend command serves.
func TestMetricsScrape(t *testing.T) {
	instrumenttest.AssertScrapes(t, metrics.Handler())
}
