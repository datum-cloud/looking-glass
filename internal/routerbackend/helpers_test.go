package routerbackend

import (
	"context"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/timestamppb"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	backendv1 "go.datum.net/looking-glass/api/lookingglass/backend/v1"
	"go.datum.net/looking-glass/internal/backendtest"
	"go.datum.net/looking-glass/internal/config"
	"go.datum.net/looking-glass/pkg/driver"
)

const (
	testCell     = "cell-a"
	testLocation = "example-location"
)

// testConfig returns a three-router location with budgets large enough that
// only tests that lower them hit one. The destination policy allows the
// documentation ranges the tests probe, through its exception list.
func testConfig() *config.Backend {
	cfg := &config.Backend{
		Cell: testCell,
		Budgets: config.BackendBudgets{
			RequestsCell:           config.TokenRate{PerSecond: 1000, Burst: 1000},
			RequestsProject:        config.TokenRate{PerSecond: 1000, Burst: 1000},
			LiveDiagnosticsProject: 100,
			RouterConcurrency:      100,
		},
		DestinationPolicy: config.DestinationPolicyConfig{
			Deny:  []string{"203.0.113.128/25"},
			Allow: []string{"198.51.100.0/24", "2001:db8::/32"},
		},
		Redaction: config.RedactionConfig{
			Communities:      []string{"64497:100", "65535:65281"},
			LargeCommunities: []string{"64497:1:*"},
		},
		Locations: []config.LocationConfig{{Name: testLocation, Routers: []config.RouterConfig{
			{Name: "router-a", Platform: "stub", ASN: 64496},
			{Name: "router-b", Platform: "stub", ASN: 64496},
			{Name: "router-c", Platform: "stub", ASN: 64496},
		}}},
	}
	cfg.SetDefaults()
	return cfg
}

// stub is a driver whose every diagnostic calls fn. A nil fn returns a small
// synthetic result.
type stub struct {
	caps  *driver.Capabilities
	fn    func(ctx context.Context, diag driver.Diagnostic) (any, error)
	calls atomic.Int32
}

func allCapabilities() driver.Capabilities {
	both := []driver.Family{driver.FamilyIPv4, driver.FamilyIPv6}
	var c driver.Capabilities
	for _, d := range []driver.Diagnostic{driver.DiagnosticRouteLookup, driver.DiagnosticBGPSummary, driver.DiagnosticPing, driver.DiagnosticTraceroute} {
		c.Diagnostics = append(c.Diagnostics, driver.DiagnosticCapability{Type: d, Families: both})
	}
	return c
}

func (s *stub) Capabilities() driver.Capabilities {
	if s.caps != nil {
		return *s.caps
	}
	return allCapabilities()
}

func (s *stub) do(ctx context.Context, d driver.Diagnostic, def any) (any, error) {
	s.calls.Add(1)
	if s.fn == nil {
		return def, nil
	}
	return s.fn(ctx, d)
}

func (s *stub) RouteLookup(ctx context.Context, req driver.RouteLookupRequest) (*driver.RouteResult, error) {
	v, err := s.do(ctx, driver.DiagnosticRouteLookup, &driver.RouteResult{Prefixes: []driver.PrefixRoutes{{
		Prefix: netip.MustParsePrefix("198.51.100.0/24"), TotalPaths: 1,
		Paths: []driver.Path{{Protocol: driver.ProtocolStatic, Active: true, NextHops: []driver.NextHop{{Address: netip.MustParseAddr("192.0.2.1"), Selected: true}}}},
	}}})
	r, _ := v.(*driver.RouteResult)
	return r, err
}

func (s *stub) BGPSummary(ctx context.Context, _ driver.BGPSummaryRequest) (*driver.BGPSummary, error) {
	v, err := s.do(ctx, driver.DiagnosticBGPSummary, &driver.BGPSummary{LocalAS: 64496})
	r, _ := v.(*driver.BGPSummary)
	return r, err
}

func (s *stub) Ping(ctx context.Context, req driver.PingRequest) (*driver.PingResult, error) {
	v, err := s.do(ctx, driver.DiagnosticPing, &driver.PingResult{Destination: req.Destination, Sent: req.Count})
	r, _ := v.(*driver.PingResult)
	return r, err
}

func (s *stub) Traceroute(ctx context.Context, req driver.TracerouteRequest) (*driver.TracerouteResult, error) {
	v, err := s.do(ctx, driver.DiagnosticTraceroute, &driver.TracerouteResult{Destination: req.Destination})
	r, _ := v.(*driver.TracerouteResult)
	return r, err
}

func (s *stub) Close() error { return nil }

// stubs builds a stub per router name on demand and remembers them.
type stubs struct {
	mu sync.Mutex
	m  map[string]*stub
}

func newStubs(preset map[string]*stub) *stubs {
	s := &stubs{m: map[string]*stub{}}
	for k, v := range preset {
		s.m[k] = v
	}
	return s
}

func (s *stubs) newDriver(r driver.Router) (driver.Driver, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.m[r.Name]
	if !ok {
		d = &stub{}
		s.m[r.Name] = d
	}
	return d, nil
}

func (s *stubs) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, d := range s.m {
		n += int(d.calls.Load())
	}
	return n
}

// serve starts a Server for cfg on an in-memory listener and returns a client.
func serve(t *testing.T, cfg *config.Backend, newDriver func(driver.Router) (driver.Driver, error)) backendv1.DiagnosticServiceClient {
	t.Helper()
	s, err := New(cfg, Options{NewDriver: newDriver})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	s.Register(srv)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return backendv1.NewDiagnosticServiceClient(conn)
}

func request(project string, q *backendv1.Query) *backendv1.RunDiagnosticRequest {
	return &backendv1.RunDiagnosticRequest{
		ApiMajor: backendv1.APIMajor, Cell: testCell, Location: testLocation,
		ProjectId: project, RequestId: uuid.NewString(),
		ExpiresAt: timestamppb.New(time.Now().Add(30 * time.Second)),
		Query:     q,
	}
}

func pingQuery(target string) *backendv1.Query {
	family := backendv1.AddressFamily_ADDRESS_FAMILY_IPV4
	if netip.MustParseAddr(target).Is6() {
		family = backendv1.AddressFamily_ADDRESS_FAMILY_IPV6
	}
	return &backendv1.Query{Type: backendv1.DiagnosticType_DIAGNOSTIC_TYPE_PING, AddressFamily: family, Target: target}
}

func collect(t *testing.T, client backendv1.DiagnosticServiceClient, req *backendv1.RunDiagnosticRequest) ([]*backendv1.DiagnosticEvent, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	return backendtest.Collect(ctx, client, req)
}

// outcomes returns each router's outcome and the complete event of a stream.
func outcomes(t *testing.T, events []*backendv1.DiagnosticEvent) (map[string]*backendv1.RouterOutcome, *backendv1.Complete) {
	t.Helper()
	m := map[string]*backendv1.RouterOutcome{}
	var complete *backendv1.Complete
	for _, ev := range events {
		if o := ev.GetOutcome(); o != nil {
			m[o.GetRouter()] = o
		}
		if c := ev.GetComplete(); c != nil {
			complete = c
		}
	}
	if complete == nil {
		t.Fatalf("stream has no complete event: %v", events)
	}
	return m, complete
}

func duration(d time.Duration) metav1.Duration { return metav1.Duration{Duration: d} }
