package config

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

// Backend defaults. The budgets are sized for a small cell; deployments set
// their own.
const (
	DefaultBackendListen             = "127.0.0.1:9090"
	DefaultBackendMetricsBindAddress = ":8080"
	DefaultMaxRequestLifetime        = 120 * time.Second
	DefaultMaxRouters                = 4
	DefaultMaxInventoryRouters       = 16
	// DefaultRouterTimeout bounds one router's diagnostic. It covers a
	// worst-case traceroute (60 s) with margin.
	DefaultRouterTimeout          = 75 * time.Second
	DefaultLiveDiagnosticsProject = 2
	DefaultRouterConcurrency      = 1
	DefaultRouterQueueDepth       = 4
	DefaultRouterQueueWait        = 10 * time.Second
)

// Backend is the router backend's configuration file.
type Backend struct {
	// Cell is the cell the backend serves. Requests for another cell are
	// refused as Unauthorized.
	Cell string `json:"cell"`

	// Disabled refuses every diagnostic with ServiceDisabled and reports
	// enabled: false in capabilities.
	Disabled bool `json:"disabled"`

	// Listen is the address the gRPC service listens on.
	Listen string `json:"listen"`

	// TLS secures the gRPC service with mutual TLS. It is required unless
	// Listen is a loopback address.
	TLS TLSConfig `json:"tls"`

	// Metrics configures the Prometheus metrics endpoint.
	Metrics MetricsConfig `json:"metrics"`

	// Limits bound each diagnostic.
	Limits BackendLimits `json:"limits"`

	// Budgets bound the work the backend accepts.
	Budgets BackendBudgets `json:"budgets"`

	// DestinationPolicy adds platform ranges and exceptions to the public
	// destination policy, as admission does. Keep it equal to admission's.
	// Its deny list also marks next hops as internal.
	DestinationPolicy DestinationPolicyConfig `json:"destinationPolicy"`

	// Redaction lists the communities results may show.
	Redaction RedactionConfig `json:"redaction"`

	// Locations lists the locations the backend serves and their routers.
	Locations []LocationConfig `json:"locations"`
}

// TLSConfig names PEM files for mutual TLS.
type TLSConfig struct {
	// CertFile and KeyFile are the server's certificate and key.
	CertFile string `json:"certFile"`
	KeyFile  string `json:"keyFile"`
	// ClientCAFile holds the CAs a client certificate must chain to.
	ClientCAFile string `json:"clientCAFile"`
}

// BackendLimits bound each diagnostic.
type BackendLimits struct {
	// MaxRequestLifetime bounds expires_at, measured from receipt.
	MaxRequestLifetime metav1.Duration `json:"maxRequestLifetime"`
	// MaxRouters bounds the routers one diagnostic runs on.
	MaxRouters int `json:"maxRouters"`
	// MaxInventoryRouters bounds a location's router set; a diagnostic at a
	// larger location is refused with InventoryTooLarge.
	MaxInventoryRouters int `json:"maxInventoryRouters"`
	// RouterTimeout bounds one router's diagnostic, from the moment the
	// router starts it.
	RouterTimeout metav1.Duration `json:"routerTimeout"`
}

// BackendBudgets bound the work the backend accepts.
type BackendBudgets struct {
	// RequestsCell and RequestsProject are token buckets for diagnostics
	// across the cell and per project.
	RequestsCell    TokenRate `json:"requestsCell"`
	RequestsProject TokenRate `json:"requestsProject"`
	// LiveDiagnosticsProject bounds one project's concurrent diagnostics.
	LiveDiagnosticsProject int `json:"liveDiagnosticsProject"`
	// RouterConcurrency bounds the diagnostics one router runs at once.
	RouterConcurrency int `json:"routerConcurrency"`
	// RouterQueueDepth bounds the diagnostics waiting for one router.
	RouterQueueDepth int `json:"routerQueueDepth"`
	// RouterQueueWait bounds a diagnostic's wait for a router.
	RouterQueueWait metav1.Duration `json:"routerQueueWait"`
}

// TokenRate is a token bucket: a sustained rate per second and a burst.
type TokenRate struct {
	PerSecond float64 `json:"perSecond"`
	Burst     int     `json:"burst"`
}

// RedactionConfig lists the communities results may show. An entry is a
// community (ASN:value) or large community (ASN:value:value) in which any
// field may be "*". Every other community is withheld and only counted.
type RedactionConfig struct {
	Communities      []string `json:"communities"`
	LargeCommunities []string `json:"largeCommunities"`
}

// LocationConfig is one location and its routers.
type LocationConfig struct {
	// Name is the location name sessions use.
	Name    string         `json:"name"`
	Routers []RouterConfig `json:"routers"`
}

// RouterConfig is one router and how its driver reaches it.
type RouterConfig struct {
	// Name is the router's inventory name, unique within its location.
	Name string `json:"name"`
	// Platform selects the registered driver.
	Platform string `json:"platform"`
	// Address is where the driver connects.
	Address string `json:"address"`
	// ASN is the router's BGP autonomous system number, reported with every
	// observation.
	ASN uint32 `json:"asn"`
	// Settings holds the driver's platform-specific options.
	Settings map[string]string `json:"settings"`
}

// LoadBackend reads the backend configuration file at path, rejecting unknown
// fields, then applies defaults and validates the result.
func LoadBackend(path string) (*Backend, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}
	return ParseBackend(data)
}

// ParseBackend decodes a backend configuration document, rejecting unknown
// fields, then applies defaults and validates the result.
func ParseBackend(data []byte) (*Backend, error) {
	cfg := &Backend{}
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
func (c *Backend) SetDefaults() {
	setDefault(&c.Listen, DefaultBackendListen)
	setDefault(&c.Metrics.BindAddress, DefaultBackendMetricsBindAddress)
	setDefault(&c.Limits.MaxRequestLifetime.Duration, DefaultMaxRequestLifetime)
	setDefault(&c.Limits.MaxRouters, DefaultMaxRouters)
	setDefault(&c.Limits.MaxInventoryRouters, DefaultMaxInventoryRouters)
	setDefault(&c.Limits.RouterTimeout.Duration, DefaultRouterTimeout)
	setDefault(&c.Budgets.RequestsCell, TokenRate{PerSecond: 2, Burst: 4})
	setDefault(&c.Budgets.RequestsProject, TokenRate{PerSecond: 0.2, Burst: 2})
	setDefault(&c.Budgets.LiveDiagnosticsProject, DefaultLiveDiagnosticsProject)
	setDefault(&c.Budgets.RouterConcurrency, DefaultRouterConcurrency)
	setDefault(&c.Budgets.RouterQueueDepth, DefaultRouterQueueDepth)
	setDefault(&c.Budgets.RouterQueueWait.Duration, DefaultRouterQueueWait)
}

func setDefault[T comparable](field *T, value T) {
	var zero T
	if *field == zero {
		*field = value
	}
}

// Validate reports every invalid field.
func (c *Backend) Validate() error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	if c.Cell == "" {
		add("cell is required")
	}
	if err := validateBindAddress(c.Listen); err != nil {
		add("listen: %w", err)
	} else if !isLoopback(c.Listen) && (c.TLS.CertFile == "" || c.TLS.KeyFile == "" || c.TLS.ClientCAFile == "") {
		add("tls.certFile, tls.keyFile and tls.clientCAFile are required unless listen is a loopback address")
	}
	if c.Metrics.BindAddress != "0" {
		if err := validateBindAddress(c.Metrics.BindAddress); err != nil {
			add("metrics.bindAddress: %w", err)
		}
	}
	l, b := c.Limits, c.Budgets
	if l.MaxRequestLifetime.Duration < 0 || l.RouterTimeout.Duration < 0 || b.RouterQueueWait.Duration < 0 {
		add("durations must not be negative")
	}
	if l.MaxRouters < 0 || l.MaxInventoryRouters < 0 || b.LiveDiagnosticsProject < 0 || b.RouterConcurrency < 0 || b.RouterQueueDepth < 0 {
		add("limits and budgets must not be negative")
	}
	if l.MaxRouters > l.MaxInventoryRouters {
		add("limits.maxRouters must not exceed limits.maxInventoryRouters")
	}
	for name, r := range map[string]TokenRate{"budgets.requestsCell": b.RequestsCell, "budgets.requestsProject": b.RequestsProject} {
		if r.PerSecond <= 0 || r.Burst < 1 {
			add("%s: perSecond must be positive and burst at least 1", name)
		}
	}
	errs = append(errs, validatePrefixes("destinationPolicy.deny", c.DestinationPolicy.Deny)...)
	errs = append(errs, validatePrefixes("destinationPolicy.allow", c.DestinationPolicy.Allow)...)
	for i, p := range c.Redaction.Communities {
		if !ValidCommunityPattern(p, 2) {
			add("redaction.communities[%d]: %q is not ASN:value with optional * fields", i, p)
		}
	}
	for i, p := range c.Redaction.LargeCommunities {
		if !ValidCommunityPattern(p, 3) {
			add("redaction.largeCommunities[%d]: %q is not ASN:value:value with optional * fields", i, p)
		}
	}
	errs = append(errs, validateLocations(c.Locations)...)
	return errors.Join(errs...)
}

// validateLocations requires at least one location, unique location names,
// and routers with unique names, a platform and an ASN.
func validateLocations(locs []LocationConfig) []error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	if len(locs) == 0 {
		add("locations: at least one location is required")
	}
	locations := map[string]bool{}
	for i, loc := range locs {
		if loc.Name == "" || locations[loc.Name] {
			add("locations[%d]: name must be unique and non-empty", i)
		}
		locations[loc.Name] = true
		if len(loc.Routers) == 0 {
			add("locations[%d]: at least one router is required", i)
		}
		routers := map[string]bool{}
		for j, r := range loc.Routers {
			if r.Name == "" || routers[r.Name] {
				add("locations[%d].routers[%d]: name must be unique within the location and non-empty", i, j)
			}
			routers[r.Name] = true
			if r.Platform == "" || r.ASN == 0 {
				add("locations[%d].routers[%d]: platform and asn are required", i, j)
			}
		}
	}
	return errs
}

// ValidCommunityPattern reports whether p is an allowlist entry with fields
// colon-separated fields, each a number or "*". A community (2 fields) has
// 16-bit fields; a large community (3 fields) has 32-bit fields.
func ValidCommunityPattern(p string, fields int) bool {
	parts := strings.Split(p, ":")
	if len(parts) != fields {
		return false
	}
	bits := 32
	if fields == 2 {
		bits = 16
	}
	for _, f := range parts {
		if f == "*" {
			continue
		}
		if _, err := strconv.ParseUint(f, 10, bits); err != nil {
			return false
		}
	}
	return true
}

func isLoopback(hostport string) bool {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	a, err := netip.ParseAddr(host)
	return err == nil && a.IsLoopback()
}
