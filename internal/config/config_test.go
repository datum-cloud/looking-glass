package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDefault(t *testing.T) {
	cfg := Default()
	if cfg.Metrics.BindAddress != DefaultMetricsBindAddress {
		t.Errorf("metrics.bindAddress = %q, want %q", cfg.Metrics.BindAddress, DefaultMetricsBindAddress)
	}
	if cfg.Health.BindAddress != DefaultHealthProbeBindAddress {
		t.Errorf("health.bindAddress = %q, want %q", cfg.Health.BindAddress, DefaultHealthProbeBindAddress)
	}
	if cfg.LeaderElection.Enabled {
		t.Error("leader election is enabled by default")
	}
	if cfg.LeaderElection.ID != DefaultLeaderElectionID {
		t.Errorf("leaderElection.id = %q, want %q", cfg.LeaderElection.ID, DefaultLeaderElectionID)
	}
	if cfg.GracefulShutdownTimeout.Duration != DefaultGracefulShutdownTimeout {
		t.Errorf("gracefulShutdownTimeout = %v, want %v", cfg.GracefulShutdownTimeout.Duration, DefaultGracefulShutdownTimeout)
	}
	if cfg.Webhook.Enabled {
		t.Error("webhook is enabled by default")
	}
	if cfg.Webhook.Port != DefaultWebhookPort || cfg.Webhook.CertDir != DefaultWebhookCertDir {
		t.Errorf("webhook = %+v", cfg.Webhook)
	}
	if rl := cfg.Admission.CreateRateLimit; rl.QPS != DefaultCreateQPS || rl.Burst != DefaultCreateBurst {
		t.Errorf("admission.createRateLimit = %+v", rl)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("default config is invalid: %v", err)
	}
}

func TestParseAdmission(t *testing.T) {
	cfg, err := Parse([]byte(`
webhook:
  enabled: true
  port: 9444
  certDir: /var/run/certs
admission:
  createRateLimit:
    qps: 2
    burst: 10
  destinationPolicy:
    deny: ["203.0.113.0/24"]
    allow: ["10.255.255.0/24"]
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !cfg.Webhook.Enabled || cfg.Webhook.Port != 9444 || cfg.Webhook.CertDir != "/var/run/certs" {
		t.Errorf("webhook = %+v", cfg.Webhook)
	}
	if rl := cfg.Admission.CreateRateLimit; rl.QPS != 2 || rl.Burst != 10 {
		t.Errorf("admission.createRateLimit = %+v", rl)
	}
	dp := cfg.Admission.DestinationPolicy
	if len(dp.Deny) != 1 || len(dp.Allow) != 1 {
		t.Errorf("admission.destinationPolicy = %+v", dp)
	}
}

func TestParse(t *testing.T) {
	cfg, err := Parse([]byte(`
metrics:
  bindAddress: "0"
health:
  bindAddress: 127.0.0.1:9440
leaderElection:
  enabled: true
  namespace: looking-glass-system
gracefulShutdownTimeout: 5s
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Metrics.BindAddress != "0" {
		t.Errorf("metrics.bindAddress = %q, want 0", cfg.Metrics.BindAddress)
	}
	if cfg.Health.BindAddress != "127.0.0.1:9440" {
		t.Errorf("health.bindAddress = %q", cfg.Health.BindAddress)
	}
	if !cfg.LeaderElection.Enabled || cfg.LeaderElection.Namespace != "looking-glass-system" {
		t.Errorf("leaderElection = %+v", cfg.LeaderElection)
	}
	if cfg.LeaderElection.ID != DefaultLeaderElectionID {
		t.Errorf("leaderElection.id was not defaulted: %q", cfg.LeaderElection.ID)
	}
	if cfg.GracefulShutdownTimeout.Duration != 5*time.Second {
		t.Errorf("gracefulShutdownTimeout = %v, want 5s", cfg.GracefulShutdownTimeout.Duration)
	}
}

func TestParseRejects(t *testing.T) {
	tests := map[string]struct {
		doc  string
		want string
	}{
		"unknown field": {
			doc:  "metrics:\n  bindAdress: :8080\n",
			want: "unknown field",
		},
		"invalid health address": {
			doc:  "health:\n  bindAddress: localhost\n",
			want: "health.bindAddress",
		},
		"invalid metrics address": {
			doc:  "metrics:\n  bindAddress: nowhere\n",
			want: "metrics.bindAddress",
		},
		"shared address": {
			doc:  "metrics:\n  bindAddress: :9000\nhealth:\n  bindAddress: :9000\n",
			want: "must differ",
		},
		"negative shutdown timeout": {
			doc:  "gracefulShutdownTimeout: -1s\n",
			want: "gracefulShutdownTimeout",
		},
		"webhook port out of range": {
			doc:  "webhook:\n  port: 70000\n",
			want: "webhook.port",
		},
		"negative create rate": {
			doc:  "admission:\n  createRateLimit:\n    qps: -1\n",
			want: "admission.createRateLimit",
		},
		"invalid deny prefix": {
			doc:  "admission:\n  destinationPolicy:\n    deny: [\"203.0.113.0\"]\n",
			want: "admission.destinationPolicy.deny[0]",
		},
		"invalid allow prefix": {
			doc:  "admission:\n  destinationPolicy:\n    allow: [\"10.0.0.0/33\"]\n",
			want: "admission.destinationPolicy.allow[0]",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(tc.doc))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Parse error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestLoad(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load(\"\"): %v", err)
	}
	if cfg.Health.BindAddress != DefaultHealthProbeBindAddress {
		t.Errorf("Load(\"\") did not apply defaults: %+v", cfg)
	}

	path := filepath.Join(t.TempDir(), "manager.yaml")
	if err := os.WriteFile(path, []byte("health:\n  bindAddress: :9440\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err = Load(path)
	if err != nil {
		t.Fatalf("Load(%q): %v", path, err)
	}
	if cfg.Health.BindAddress != ":9440" {
		t.Errorf("health.bindAddress = %q, want :9440", cfg.Health.BindAddress)
	}

	if _, err := Load(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Error("Load of a missing file succeeded")
	}
}
