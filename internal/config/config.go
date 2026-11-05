// Package config loads, defaults and validates the manager configuration file.
package config

import (
	"errors"
	"fmt"
	"net"
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
	return errors.Join(errs...)
}

func validateBindAddress(addr string) error {
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return fmt.Errorf("must be host:port: %w", err)
	}
	return nil
}
