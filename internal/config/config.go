// Package config loads, defaults and validates the manager configuration file.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

const (
	// DefaultMetricsBindAddress is where the manager serves Prometheus metrics.
	DefaultMetricsBindAddress = ":8080"
	// DefaultHealthProbeBindAddress is where the manager serves /healthz and
	// /readyz.
	DefaultHealthProbeBindAddress = ":8081"
	// DefaultLeaderElectionID names the leader election lease.
	DefaultLeaderElectionID = "looking-glass-manager.lookingglass.datumapis.com"
	// DefaultGracefulShutdownTimeout bounds how long runnables may take to stop
	// after the manager is asked to shut down. It stays below the default
	// 30 s pod termination grace period so the manager releases its lease
	// before the kubelet kills it.
	DefaultGracefulShutdownTimeout = 20 * time.Second
	// DefaultWebhookPort is the port the admission webhook server listens on.
	DefaultWebhookPort = 9443
	// DefaultWebhookCertDir holds the webhook server's tls.crt and tls.key.
	DefaultWebhookCertDir = "/tmp/k8s-webhook-server/serving-certs"
	// DefaultCreateQPS and DefaultCreateBurst bound how fast one project may
	// create sessions, per webhook replica.
	DefaultCreateQPS   = 0.5
	DefaultCreateBurst = 5
)

// Manager is the manager configuration file.
type Manager struct {
	// Metrics configures the metrics endpoint.
	Metrics MetricsConfig `json:"metrics"`

	// Health configures the liveness and readiness endpoints.
	Health HealthConfig `json:"health"`

	// LeaderElection configures leader election between manager replicas.
	LeaderElection LeaderElectionConfig `json:"leaderElection"`

	// GracefulShutdownTimeout bounds how long runnables may take to stop. A
	// zero value selects DefaultGracefulShutdownTimeout.
	GracefulShutdownTimeout metav1.Duration `json:"gracefulShutdownTimeout"`

	// Webhook configures the admission webhook server.
	Webhook WebhookConfig `json:"webhook"`

	// Admission configures session admission policy.
	Admission AdmissionConfig `json:"admission"`
}

// WebhookConfig configures the admission webhook server.
type WebhookConfig struct {
	// Enabled serves the session admission webhooks. It needs a serving
	// certificate in CertDir. The webhook configurations fail closed, so a
	// deployment that registers them without serving refuses every create.
	Enabled bool `json:"enabled"`

	// Host is the address the webhook server binds. Empty binds every address.
	Host string `json:"host"`

	// Port is the port the webhook server listens on.
	Port int `json:"port"`

	// CertDir holds tls.crt and tls.key.
	CertDir string `json:"certDir"`
}

// AdmissionConfig configures session admission policy.
type AdmissionConfig struct {
	// CreateRateLimit bounds session creates per project.
	CreateRateLimit RateLimitConfig `json:"createRateLimit"`

	// DestinationPolicy adds to the public destination policy for probes.
	DestinationPolicy DestinationPolicyConfig `json:"destinationPolicy"`
}

// RateLimitConfig is a per-project token bucket.
type RateLimitConfig struct {
	// QPS is the sustained rate, in creates per second.
	QPS float64 `json:"qps"`

	// Burst is the most creates allowed at once.
	Burst int `json:"burst"`
}

// DestinationPolicyConfig lists CIDR prefixes on top of the special-use ranges,
// which are always denied.
type DestinationPolicyConfig struct {
	// Deny lists platform and management ranges no probe may target. Keep it
	// equal to the cells' platform deny list.
	Deny []string `json:"deny"`

	// Allow lists explicitly authorized exceptions, which bypass every other
	// rule. A cell applies its own policy too, so an exception works only if
	// the cell allows the same range.
	Allow []string `json:"allow"`
}

// MetricsConfig configures the metrics endpoint.
type MetricsConfig struct {
	// BindAddress is the address the metrics endpoint listens on. "0"
	// disables the endpoint.
	BindAddress string `json:"bindAddress"`
}

// HealthConfig configures the liveness and readiness endpoints.
type HealthConfig struct {
	// BindAddress is the address /healthz and /readyz listen on.
	BindAddress string `json:"bindAddress"`
}

// LeaderElectionConfig configures leader election.
type LeaderElectionConfig struct {
	// Enabled turns on leader election. Run more than one replica only with
	// leader election enabled.
	Enabled bool `json:"enabled"`

	// ID names the lease. Defaults to DefaultLeaderElectionID.
	ID string `json:"id"`

	// Namespace holds the lease. When empty, the manager uses the namespace
	// it runs in.
	Namespace string `json:"namespace"`
}

// Default returns a configuration with every default applied.
func Default() *Manager {
	cfg := &Manager{}
	cfg.SetDefaults()
	return cfg
}

// Load reads the configuration file at path, rejecting unknown fields, then
// applies defaults and validates the result. An empty path returns Default().
func Load(path string) (*Manager, error) {
	if path == "" {
		cfg := Default()
		return cfg, cfg.Validate()
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}
	return Parse(data)
}

// Parse decodes a configuration document, rejecting unknown fields, then
// applies defaults and validates the result.
func Parse(data []byte) (*Manager, error) {
	cfg := &Manager{}
	if err := yaml.UnmarshalStrict(data, cfg); err != nil {
		return nil, fmt.Errorf("decoding config: %w", err)
	}
	cfg.SetDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// SetDefaults fills every unset field with its default.
func (c *Manager) SetDefaults() {
	if c.Metrics.BindAddress == "" {
		c.Metrics.BindAddress = DefaultMetricsBindAddress
	}
	if c.Health.BindAddress == "" {
		c.Health.BindAddress = DefaultHealthProbeBindAddress
	}
	if c.LeaderElection.ID == "" {
		c.LeaderElection.ID = DefaultLeaderElectionID
	}
	if c.GracefulShutdownTimeout.Duration == 0 {
		c.GracefulShutdownTimeout.Duration = DefaultGracefulShutdownTimeout
	}
	if c.Webhook.Port == 0 {
		c.Webhook.Port = DefaultWebhookPort
	}
	if c.Webhook.CertDir == "" {
		c.Webhook.CertDir = DefaultWebhookCertDir
	}
	if c.Admission.CreateRateLimit.QPS == 0 {
		c.Admission.CreateRateLimit.QPS = DefaultCreateQPS
	}
	if c.Admission.CreateRateLimit.Burst == 0 {
		c.Admission.CreateRateLimit.Burst = DefaultCreateBurst
	}
}

// Validate reports every invalid field.
func (c *Manager) Validate() error {
	var errs []error
	if c.Metrics.BindAddress != "0" {
		if err := validateBindAddress(c.Metrics.BindAddress); err != nil {
			errs = append(errs, fmt.Errorf("metrics.bindAddress: %w", err))
		}
	}
	if err := validateBindAddress(c.Health.BindAddress); err != nil {
		errs = append(errs, fmt.Errorf("health.bindAddress: %w", err))
	}
	if c.Metrics.BindAddress != "0" && c.Metrics.BindAddress == c.Health.BindAddress {
		errs = append(errs, errors.New("metrics.bindAddress and health.bindAddress must differ"))
	}
	if c.GracefulShutdownTimeout.Duration < 0 {
		errs = append(errs, errors.New("gracefulShutdownTimeout must not be negative"))
	}
	if c.Webhook.Port < 1 || c.Webhook.Port > 65535 {
		errs = append(errs, errors.New("webhook.port must be between 1 and 65535"))
	}
	if rl := c.Admission.CreateRateLimit; rl.QPS < 0 || rl.Burst < 0 {
		errs = append(errs, errors.New("admission.createRateLimit.qps and burst must not be negative"))
	}
	errs = append(errs, validatePrefixes("admission.destinationPolicy.deny", c.Admission.DestinationPolicy.Deny)...)
	errs = append(errs, validatePrefixes("admission.destinationPolicy.allow", c.Admission.DestinationPolicy.Allow)...)
	return errors.Join(errs...)
}

func validatePrefixes(field string, prefixes []string) []error {
	var errs []error
	for i, s := range prefixes {
		if _, err := netip.ParsePrefix(s); err != nil {
			errs = append(errs, fmt.Errorf("%s[%d]: %w", field, i, err))
		}
	}
	return errs
}

func validateBindAddress(addr string) error {
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return fmt.Errorf("must be host:port: %w", err)
	}
	return nil
}
