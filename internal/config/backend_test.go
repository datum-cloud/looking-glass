package config

import (
	"strings"
	"testing"
)

const minimalBackend = `cell: cell-a
locations:
  - name: example-location
    routers:
      - name: router-a
        platform: frr
        asn: 64496
`

func TestParseBackendDefaults(t *testing.T) {
	cfg, err := ParseBackend([]byte(minimalBackend))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != DefaultBackendListen || cfg.Metrics.BindAddress != DefaultBackendMetricsBindAddress {
		t.Errorf("listen, metrics = %q, %q", cfg.Listen, cfg.Metrics.BindAddress)
	}
	l, b := cfg.Limits, cfg.Budgets
	if l.MaxRequestLifetime.Duration != DefaultMaxRequestLifetime || l.MaxRouters != DefaultMaxRouters ||
		l.MaxInventoryRouters != DefaultMaxInventoryRouters || l.RouterTimeout.Duration != DefaultRouterTimeout {
		t.Errorf("limits = %+v", l)
	}
	if b.RequestsCell.Burst == 0 || b.RequestsProject.PerSecond == 0 || b.LiveDiagnosticsProject != DefaultLiveDiagnosticsProject ||
		b.RouterConcurrency != DefaultRouterConcurrency || b.RouterQueueDepth != DefaultRouterQueueDepth ||
		b.RouterQueueWait.Duration != DefaultRouterQueueWait {
		t.Errorf("budgets = %+v", b)
	}
}

func TestParseBackendRejects(t *testing.T) {
	tests := map[string]struct {
		doc  string
		want string
	}{
		"no cell":                {strings.Replace(minimalBackend, "cell: cell-a\n", "", 1), "cell is required"},
		"no locations":           {"cell: cell-a\n", "at least one location"},
		"unknown field":          {minimalBackend + "colour: blue\n", "colour"},
		"public plaintext":       {minimalBackend + "listen: 0.0.0.0:9090\n", "tls.certFile"},
		"bad community":          {minimalBackend + "redaction:\n  communities: [\"64497:*:1\"]\n", "redaction.communities[0]"},
		"community too wide":     {minimalBackend + "redaction:\n  communities: [\"70000:1\"]\n", "redaction.communities[0]"},
		"bad large":              {minimalBackend + "redaction:\n  largeCommunities: [\"64497:1\"]\n", "redaction.largeCommunities[0]"},
		"bad deny prefix":        {minimalBackend + "destinationPolicy:\n  deny: [\"not-a-prefix\"]\n", "destinationPolicy.deny[0]"},
		"routers over inventory": {minimalBackend + "limits:\n  maxRouters: 20\n  maxInventoryRouters: 10\n", "maxRouters must not exceed"},
		"zero burst":             {minimalBackend + "budgets:\n  requestsCell:\n    perSecond: 1\n    burst: -1\n", "budgets.requestsCell"},
		"duplicate router": {minimalBackend + "      - name: router-a\n        platform: frr\n        asn: 64496\n",
			"unique within the location"},
		"no asn": {strings.Replace(minimalBackend, "        asn: 64496\n", "", 1), "platform and asn are required"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := ParseBackend([]byte(tt.doc))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("ParseBackend = %v, want an error mentioning %q", err, tt.want)
			}
		})
	}
}

func TestParseBackendAcceptsTLSAndLoopback(t *testing.T) {
	for _, extra := range []string{
		"listen: 127.0.0.1:9090\n",
		"listen: '[::1]:9090'\n",
		"listen: localhost:9090\n",
		"listen: 0.0.0.0:9090\ntls:\n  certFile: tls.crt\n  keyFile: tls.key\n  clientCAFile: ca.crt\n",
		"redaction:\n  communities: [\"64497:*\", \"65535:65281\"]\n  largeCommunities: [\"64497:1:*\"]\n",
	} {
		if _, err := ParseBackend([]byte(minimalBackend + extra)); err != nil {
			t.Errorf("ParseBackend with %q: %v", extra, err)
		}
	}
}

func TestLoadBackendMissingFile(t *testing.T) {
	if _, err := LoadBackend("testdata/missing.yaml"); err == nil {
		t.Error("LoadBackend accepted a missing file")
	}
}
