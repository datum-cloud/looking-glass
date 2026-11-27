package routerbackend

import (
	"context"
	"errors"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	backendv1 "go.datum.net/looking-glass/api/lookingglass/backend/v1"
	"go.datum.net/looking-glass/internal/backendtest"
	"go.datum.net/looking-glass/internal/config"
	"go.datum.net/looking-glass/pkg/driver"
	"go.datum.net/looking-glass/pkg/driver/frr"
)

func TestConformance(t *testing.T) {
	client := serve(t, testConfig(), newStubs(nil).newDriver)
	backendtest.Run(t, client, backendtest.Fixture{
		Cell:     testCell,
		Location: testLocation,
		Routers:  []string{"router-a", "router-b", "router-c"},
		Queries:  backendtest.DefaultQueries(),
	})
}

// TestRefusedBeforeAnyDriverCall checks that budgets and destination policy
// refuse a request before the snapshot, without querying any router.
func TestRefusedBeforeAnyDriverCall(t *testing.T) {
	routeQuery := func(target string) *backendv1.Query {
		return &backendv1.Query{Type: backendv1.DiagnosticType_DIAGNOSTIC_TYPE_ROUTE_LOOKUP, AddressFamily: backendv1.AddressFamily_ADDRESS_FAMILY_IPV4, Target: target}
	}
	tests := []struct {
		name      string
		configure func(*config.Backend)
		first     *backendv1.Query // run once successfully before the refused request
		query     *backendv1.Query
		reason    string
		retry     bool
	}{
		{name: "project budget", configure: func(c *config.Backend) { c.Budgets.RequestsProject = config.TokenRate{PerSecond: 0.01, Burst: 1} },
			first: pingQuery("198.51.100.7"), query: pingQuery("198.51.100.7"), reason: backendv1.ReasonRateLimited, retry: true},
		{name: "cell budget", configure: func(c *config.Backend) { c.Budgets.RequestsCell = config.TokenRate{PerSecond: 0.01, Burst: 1} },
			first: pingQuery("198.51.100.7"), query: pingQuery("198.51.100.7"), reason: backendv1.ReasonRateLimited, retry: true},
		{name: "special-use destination", query: pingQuery("10.1.2.3"), reason: backendv1.ReasonDestinationNotAllowed},
		{name: "platform destination", query: pingQuery("203.0.113.200"), reason: backendv1.ReasonDestinationNotAllowed},
		{name: "route lookup overlapping a platform range", query: routeQuery("203.0.113.0/24"), reason: backendv1.ReasonDestinationNotAllowed},
		{name: "route lookup inside a platform range", query: routeQuery("203.0.113.130"), reason: backendv1.ReasonDestinationNotAllowed},
		// A zone is free text that netip keeps verbatim, and drivers run
		// commands in a router's shell, so a zoned target never reaches one.
		{name: "zoned ping target", query: pingQuery("fe80::1%eth0;id"), reason: backendv1.ReasonInvalidQuery},
		{name: "zoned global ping target", query: pingQuery("2001:db8::1%a$(id)"), reason: backendv1.ReasonInvalidQuery},
		{name: "zoned traceroute target", query: &backendv1.Query{Type: backendv1.DiagnosticType_DIAGNOSTIC_TYPE_TRACEROUTE,
			AddressFamily: backendv1.AddressFamily_ADDRESS_FAMILY_IPV6, Target: "2001:db8::1%x;id"}, reason: backendv1.ReasonInvalidQuery},
		{name: "zoned route lookup target", query: &backendv1.Query{Type: backendv1.DiagnosticType_DIAGNOSTIC_TYPE_ROUTE_LOOKUP,
			AddressFamily: backendv1.AddressFamily_ADDRESS_FAMILY_IPV6, Target: "2001:db8::1%x;id"}, reason: backendv1.ReasonInvalidQuery},
		{name: "IPv4-mapped ping target", query: &backendv1.Query{Type: backendv1.DiagnosticType_DIAGNOSTIC_TYPE_PING,
			AddressFamily: backendv1.AddressFamily_ADDRESS_FAMILY_IPV6, Target: "::ffff:198.51.100.7"}, reason: backendv1.ReasonInvalidQuery},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfig()
			if tt.configure != nil {
				tt.configure(cfg)
			}
			drivers := newStubs(nil)
			client := serve(t, cfg, drivers.newDriver)
			if tt.first != nil {
				// Another project, so only the cell budget is shared.
				project := "project-one"
				if tt.name == "project budget" {
					project = "project-two"
				}
				if _, err := collect(t, client, request(project, tt.first)); err != nil {
					t.Fatalf("first request: %v", err)
				}
			}
			before := drivers.calls()
			events, err := collect(t, client, request("project-two", tt.query))
			if got, _ := backendv1.RefusalReason(err); got != tt.reason || len(events) != 0 {
				t.Fatalf("refusal reason %q (%v) after %d events, want %q before the snapshot", got, err, len(events), tt.reason)
			}
			if _, ok := backendv1.RefusalRetryAfter(err); ok != tt.retry {
				t.Errorf("refusal carries retry advice: %v, want %v", ok, tt.retry)
			}
			if n := drivers.calls() - before; n != 0 {
				t.Errorf("%d driver calls for a refused request, want 0", n)
			}
		})
	}
}

func TestLiveDiagnosticsBudget(t *testing.T) {
	cfg := testConfig()
	cfg.Budgets.LiveDiagnosticsProject = 1
	release := make(chan struct{})
	var once sync.Once
	block := &stub{fn: func(ctx context.Context, _ driver.Diagnostic) (any, error) {
		first := false
		once.Do(func() { first = true })
		if first {
			select {
			case <-release:
			case <-ctx.Done():
			}
		}
		return &driver.PingResult{Sent: 3}, nil
	}}
	drivers := newStubs(map[string]*stub{"router-a": block})
	client := serve(t, cfg, drivers.newDriver)

	first := make(chan error, 1)
	go func() {
		_, err := collect(t, client, request("project-one", pingQuery("198.51.100.7")))
		first <- err
	}()
	waitFor(t, func() bool { return block.calls.Load() == 1 })
	before := drivers.calls()
	_, err := collect(t, client, request("project-one", pingQuery("198.51.100.7")))
	if got, _ := backendv1.RefusalReason(err); got != backendv1.ReasonRateLimited || drivers.calls() != before {
		t.Errorf("second live diagnostic: reason %q (%v), %d new driver calls; want RateLimited and none", got, err, drivers.calls()-before)
	}
	if _, err := collect(t, client, request("project-other", pingQuery("198.51.100.7"))); err != nil {
		t.Errorf("another project's diagnostic: %v", err)
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if _, err := collect(t, client, request("project-one", pingQuery("198.51.100.7"))); err != nil {
		t.Errorf("diagnostic after the first finished: %v", err)
	}
}

// TestBusyRouterFailsAlone holds one router's only slot and checks that a
// second diagnostic fails on that router alone, with the contract's reason.
func TestBusyRouterFailsAlone(t *testing.T) {
	for _, tt := range []struct {
		name   string
		depth  int
		reason string
	}{
		{"queue full", 0, backendv1.ReasonQueueFull},
		{"queue timeout", 1, backendv1.ReasonQueueTimeout},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfig()
			cfg.Budgets.RouterConcurrency = 1
			cfg.Budgets.RouterQueueDepth = tt.depth
			cfg.Budgets.RouterQueueWait = duration(50 * time.Millisecond)
			release := make(chan struct{})
			var once sync.Once
			busy := &stub{fn: func(ctx context.Context, _ driver.Diagnostic) (any, error) {
				first := false
				once.Do(func() { first = true })
				if first {
					select {
					case <-release:
					case <-ctx.Done():
					}
				}
				return &driver.PingResult{Sent: 3}, nil
			}}
			drivers := newStubs(map[string]*stub{"router-a": busy})
			client := serve(t, cfg, drivers.newDriver)
			done := make(chan struct{})
			go func() {
				defer close(done)
				_, _ = collect(t, client, request("project-one", pingQuery("198.51.100.7")))
			}()
			waitFor(t, func() bool { return busy.calls.Load() == 1 })

			events, err := collect(t, client, request("project-two", pingQuery("198.51.100.7")))
			if err != nil {
				t.Fatal(err)
			}
			got, complete := outcomes(t, events)
			f := got["router-a"].GetFailure()
			if f.GetReason() != tt.reason || f.GetScope() != "router" || f.GetRetryAfter() == nil {
				t.Errorf("busy router outcome = %v, want %s with scope router and retry advice", got["router-a"], tt.reason)
			}
			for _, name := range []string{"router-b", "router-c"} {
				if got[name].GetObservation() == nil {
					t.Errorf("%s outcome = %v, want an observation", name, got[name])
				}
			}
			if complete.GetReason() != backendv1.CompletePartialResults || complete.GetFailed() != 1 {
				t.Errorf("complete = %v, want PartialResults with one failure", complete)
			}
			close(release)
			<-done
		})
	}
}

// TestRouterFailuresAreIsolated checks that an unreachable router, a router
// that stalls past its timeout and unparseable router output each produce one
// router failure while the other routers succeed.
func TestRouterFailuresAreIsolated(t *testing.T) {
	garbage, err := frr.New(driver.Router{Name: "router-a", Settings: map[string]string{frr.SettingProbeSourceIPv4: "192.0.2.2"}},
		fixedRunner{out: []byte("this is not ping output")})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		driver driver.Driver
		reason string
	}{
		{"unreachable", &stub{fn: func(context.Context, driver.Diagnostic) (any, error) {
			return nil, driver.Errorf(driver.ReasonRouterUnreachable, "could not connect to the router")
		}}, backendv1.ReasonRouterUnreachable},
		{"stalled past its timeout", &stub{fn: func(context.Context, driver.Diagnostic) (any, error) {
			// Ignores its context, as a broken driver would.
			time.Sleep(5 * time.Second)
			return nil, nil
		}}, backendv1.ReasonTimeout},
		{"unparseable output", garbage, backendv1.ReasonParseError},
		{"unknown reason", &stub{fn: func(context.Context, driver.Diagnostic) (any, error) {
			return nil, driver.Errorf("SomethingNew", "a new failure")
		}}, backendv1.ReasonInternal},
		{"no result", &stub{fn: func(context.Context, driver.Diagnostic) (any, error) { return nil, nil }}, backendv1.ReasonInternal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfig()
			cfg.Limits.RouterTimeout = duration(200 * time.Millisecond)
			client := serve(t, cfg, func(r driver.Router) (driver.Driver, error) {
				if r.Name == "router-a" {
					return tt.driver, nil
				}
				return &stub{}, nil
			})
			start := time.Now()
			events, err := collect(t, client, request("project-one", pingQuery("198.51.100.7")))
			if err != nil {
				t.Fatal(err)
			}
			if elapsed := time.Since(start); elapsed > 3*time.Second {
				t.Errorf("diagnostic took %s, want it bounded by the router timeout", elapsed)
			}
			got, complete := outcomes(t, events)
			if f := got["router-a"].GetFailure(); f.GetReason() != tt.reason || f.GetMessage() == "" {
				t.Errorf("router-a outcome = %v, want a %s failure with a message", got["router-a"], tt.reason)
			}
			if got["router-b"].GetObservation() == nil || got["router-c"].GetObservation() == nil {
				t.Errorf("other routers = %v, %v; want observations", got["router-b"], got["router-c"])
			}
			if complete.GetReason() != backendv1.CompletePartialResults || complete.GetSuccessful() != 2 || complete.GetFailed() != 1 {
				t.Errorf("complete = %v, want PartialResults, 2 successful, 1 failed", complete)
			}
		})
	}
}

// fixedRunner is an frr.Runner that returns one output for every command.
type fixedRunner struct{ out []byte }

func (f fixedRunner) Run(context.Context, string, int) ([]byte, error) { return f.out, nil }
func (f fixedRunner) Close() error                                     { return nil }

// TestCancelStopsDriverCalls cancels a diagnostic while every router is
// running and checks that each driver call returns promptly.
func TestCancelStopsDriverCalls(t *testing.T) {
	var mu sync.Mutex
	stopped := map[string]time.Time{}
	started := make(chan struct{}, 3)
	blocking := func(name string) *stub {
		return &stub{fn: func(ctx context.Context, _ driver.Diagnostic) (any, error) {
			started <- struct{}{}
			<-ctx.Done()
			mu.Lock()
			stopped[name] = time.Now()
			mu.Unlock()
			return nil, ctx.Err()
		}}
	}
	drivers := newStubs(map[string]*stub{"router-a": blocking("router-a"), "router-b": blocking("router-b"), "router-c": blocking("router-c")})
	client := serve(t, testConfig(), drivers.newDriver)

	ctx, cancel := context.WithCancel(t.Context())
	stream, err := client.RunDiagnostic(ctx, request("project-one", pingQuery("198.51.100.7")))
	if err != nil {
		t.Fatal(err)
	}
	if ev, err := stream.Recv(); err != nil || ev.GetSnapshot() == nil {
		t.Fatalf("first event = %v, %v; want the snapshot", ev, err)
	}
	for range 3 {
		<-started
	}
	cancelled := time.Now()
	cancel()
	const bound = 2 * time.Second
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(stopped) == 3
	})
	mu.Lock()
	defer mu.Unlock()
	for name, at := range stopped {
		if d := at.Sub(cancelled); d > bound {
			t.Errorf("%s stopped %s after the cancel, want within %s", name, d, bound)
		}
	}
}

func TestRequestDeadline(t *testing.T) {
	drivers := newStubs(map[string]*stub{"router-a": {fn: func(ctx context.Context, _ driver.Diagnostic) (any, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}})
	client := serve(t, testConfig(), drivers.newDriver)
	req := request("project-one", pingQuery("198.51.100.7"))
	req.ExpiresAt.Seconds = time.Now().Add(time.Second).Unix() + 1
	events, err := collect(t, client, req)
	if err != nil {
		t.Fatal(err)
	}
	got, complete := outcomes(t, events)
	if got["router-a"].GetFailure().GetReason() != backendv1.ReasonDeadlineExceeded || got["router-b"].GetObservation() == nil {
		t.Errorf("outcomes = %v", got)
	}
	if complete.GetReason() != backendv1.CompleteDeadlineExceeded {
		t.Errorf("complete reason = %q, want DeadlineExceeded", complete.GetReason())
	}
}

func TestSnapshotSelection(t *testing.T) {
	cfg := testConfig()
	cfg.Limits.MaxRouters = 1
	v4only := driver.Capabilities{Diagnostics: []driver.DiagnosticCapability{{Type: driver.DiagnosticPing, Families: []driver.Family{driver.FamilyIPv4}}}}
	drivers := newStubs(map[string]*stub{"router-a": {caps: &v4only}})
	client := serve(t, cfg, drivers.newDriver)

	events, err := collect(t, client, request("project-one", pingQuery("2001:db8::7")))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"router-a": backendv1.OmittedIneligible, "router-b": "", "router-c": backendv1.OmittedNotRequested}
	for _, r := range events[0].GetSnapshot().GetRouters() {
		if r.GetOmittedReason() != want[r.GetRouter()] || r.GetSelected() != (want[r.GetRouter()] == "") {
			t.Errorf("snapshot router %v, want omitted reason %q", r, want[r.GetRouter()])
		}
	}
	_, complete := outcomes(t, events)
	if complete.GetReason() != backendv1.CompletePartialResults || complete.GetOmitted() != 2 {
		t.Errorf("complete = %v, want PartialResults with 2 omitted", complete)
	}

	caps, err := client.GetCapabilities(t.Context(), &backendv1.GetCapabilitiesRequest{ApiMajor: backendv1.APIMajor, Cell: testCell})
	if err != nil {
		t.Fatal(err)
	}
	ops := caps.GetLocations()[0].GetOperations()
	if len(ops) != 1 || ops[0].GetType() != backendv1.DiagnosticType_DIAGNOSTIC_TYPE_PING || len(ops[0].GetAddressFamilies()) != 1 {
		t.Errorf("operations = %v, want only what every router serves: IPv4 ping", ops)
	}
	if caps.GetLimits().GetMaxRouters() != 1 {
		t.Errorf("max_routers = %d, want the configured 1", caps.GetLimits().GetMaxRouters())
	}
}

func TestOtherRefusals(t *testing.T) {
	t.Run("disabled", func(t *testing.T) {
		cfg := testConfig()
		cfg.Disabled = true
		client := serve(t, cfg, newStubs(nil).newDriver)
		_, err := collect(t, client, request("project-one", pingQuery("198.51.100.7")))
		if got, _ := backendv1.RefusalReason(err); got != backendv1.ReasonServiceDisabled {
			t.Errorf("reason %q, want ServiceDisabled", got)
		}
		caps, err := client.GetCapabilities(t.Context(), &backendv1.GetCapabilitiesRequest{ApiMajor: backendv1.APIMajor, Cell: testCell})
		if err != nil || caps.GetEnabled() {
			t.Errorf("capabilities enabled = %v (%v), want false", caps.GetEnabled(), err)
		}
	})
	t.Run("duplicate request", func(t *testing.T) {
		client := serve(t, testConfig(), newStubs(nil).newDriver)
		req := request("project-one", pingQuery("198.51.100.7"))
		if _, err := collect(t, client, req); err != nil {
			t.Fatal(err)
		}
		_, err := collect(t, client, req)
		if got, _ := backendv1.RefusalReason(err); got != backendv1.ReasonDuplicateRequest {
			t.Errorf("reason %q, want DuplicateRequest", got)
		}
	})
	t.Run("throttled request keeps its ID", func(t *testing.T) {
		cfg := testConfig()
		cfg.Budgets.RequestsProject = config.TokenRate{PerSecond: 0.01, Burst: 1}
		client := serve(t, cfg, newStubs(nil).newDriver)
		if _, err := collect(t, client, request("project-one", pingQuery("198.51.100.7"))); err != nil {
			t.Fatal(err)
		}
		throttled := request("project-one", pingQuery("198.51.100.7"))
		_, err := collect(t, client, throttled)
		if got, _ := backendv1.RefusalReason(err); got != backendv1.ReasonRateLimited {
			t.Fatalf("reason %q, want RateLimited", got)
		}
		retry := request("project-two", pingQuery("198.51.100.7"))
		retry.RequestId = throttled.GetRequestId()
		if _, err := collect(t, client, retry); err != nil {
			t.Errorf("admitted request reusing a throttled request's ID: %v", err)
		}
	})
	t.Run("inventory too large", func(t *testing.T) {
		cfg := testConfig()
		cfg.Limits.MaxRouters, cfg.Limits.MaxInventoryRouters = 2, 2
		client := serve(t, cfg, newStubs(nil).newDriver)
		_, err := collect(t, client, request("project-one", pingQuery("198.51.100.7")))
		if got, _ := backendv1.RefusalReason(err); got != backendv1.ReasonInventoryTooLarge {
			t.Errorf("reason %q, want InventoryTooLarge", got)
		}
	})
}

func TestNewRejectsBadRouter(t *testing.T) {
	_, err := New(testConfig(), Options{NewDriver: func(r driver.Router) (driver.Driver, error) {
		if r.Name == "router-b" {
			return nil, errors.New("bad settings")
		}
		return &stub{}, nil
	}})
	if err == nil || !strings.Contains(err.Error(), "bad settings") {
		t.Errorf("New = %v, want the driver's error", err)
	}
}

func TestOutputBounds(t *testing.T) {
	var paths []driver.Path
	for range 10 {
		communities := slices.Repeat([]string{"64497:100"}, 70)
		paths = append(paths, driver.Path{Protocol: driver.ProtocolBGP, Active: true, BGP: &driver.BGPAttributes{Communities: communities}})
	}
	long := strings.Repeat("é", 200)
	drivers := newStubs(map[string]*stub{
		"router-a": {fn: func(context.Context, driver.Diagnostic) (any, error) {
			return &driver.RouteResult{Prefixes: []driver.PrefixRoutes{{Prefix: netip.MustParsePrefix("198.51.100.0/24"), TotalPaths: 10, Paths: paths}}}, nil
		}},
		"router-b": {fn: func(context.Context, driver.Diagnostic) (any, error) {
			return nil, driver.Errorf(driver.ReasonRouterUnavailable, "%s", long)
		}},
	})
	client := serve(t, testConfig(), drivers.newDriver)
	events, err := collect(t, client, request("project-one", &backendv1.Query{
		Type: backendv1.DiagnosticType_DIAGNOSTIC_TYPE_ROUTE_LOOKUP, AddressFamily: backendv1.AddressFamily_ADDRESS_FAMILY_IPV4, Target: "198.51.100.7",
	}))
	if err != nil {
		t.Fatal(err)
	}
	got, complete := outcomes(t, events)
	obs := got["router-a"].GetObservation()
	prefix := obs.GetRoutes().GetPrefixes()[0]
	bgp := prefix.GetPaths()[0].GetBgp()
	if !obs.GetTruncated() || len(prefix.GetPaths()) != MaxPathsPerPrefix || prefix.GetTotalPaths() != 10 ||
		len(bgp.GetCommunities()) != MaxCommunities || bgp.GetTotalCommunities() != 70 {
		t.Errorf("bounded observation: truncated %v, %d of %d paths, %d of %d communities",
			obs.GetTruncated(), len(prefix.GetPaths()), prefix.GetTotalPaths(), len(bgp.GetCommunities()), bgp.GetTotalCommunities())
	}
	if complete.GetTruncated() != 1 {
		t.Errorf("complete.truncated = %d, want 1", complete.GetTruncated())
	}
	if msg := got["router-b"].GetFailure().GetMessage(); len(msg) > 256 || len(msg) < 250 || !strings.HasPrefix(long, msg) {
		t.Errorf("failure message is %d bytes, want a 256-byte prefix cut on a rune boundary", len(msg))
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 10s")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestConcurrentDuplicateRequests sends one request ID many times at once and
// checks that exactly one is admitted and that the refused copies take no
// budget.
func TestConcurrentDuplicateRequests(t *testing.T) {
	const copies = 16
	cfg := testConfig()
	cfg.Budgets.RequestsProject = config.TokenRate{PerSecond: 0.001, Burst: copies}
	cfg.Budgets.LiveDiagnosticsProject = copies
	client := serve(t, cfg, newStubs(nil).newDriver)
	req := request("project-one", pingQuery("198.51.100.7"))

	reasons := make(chan string, copies)
	var wg sync.WaitGroup
	for range copies {
		wg.Go(func() {
			_, err := collect(t, client, proto.Clone(req).(*backendv1.RunDiagnosticRequest))
			reason, _ := backendv1.RefusalReason(err)
			if err != nil && reason == "" {
				reason = err.Error()
			}
			reasons <- reason
		})
	}
	wg.Wait()
	close(reasons)
	admitted := 0
	for r := range reasons {
		switch r {
		case "":
			admitted++
		case backendv1.ReasonDuplicateRequest:
		default:
			t.Errorf("copy refused with %q, want DuplicateRequest", r)
		}
	}
	if admitted != 1 {
		t.Fatalf("%d copies admitted, want 1", admitted)
	}
	// Only the admitted copy took a project token.
	for i := range copies - 1 {
		if _, err := collect(t, client, request("project-one", pingQuery("198.51.100.7"))); err != nil {
			t.Fatalf("fresh request %d: %v", i, err)
		}
	}
	_, err := collect(t, client, request("project-one", pingQuery("198.51.100.7")))
	if got, _ := backendv1.RefusalReason(err); got != backendv1.ReasonRateLimited {
		t.Errorf("request past the budget: reason %q, want RateLimited", got)
	}
}
