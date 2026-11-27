package fakebackend

import (
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/timestamppb"

	backendv1 "go.datum.net/looking-glass/api/lookingglass/backend/v1"
	"go.datum.net/looking-glass/internal/backendtest"
)

func dial(t *testing.T, opts Options) backendv1.DiagnosticServiceClient {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	fake, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	fake.Register(srv)
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

func testOptions() Options {
	opts := DefaultOptions()
	opts.OutcomeDelay = time.Millisecond
	return opts
}

func fixture(opts Options) backendtest.Fixture {
	return backendtest.Fixture{
		Cell:     opts.Cell,
		Location: opts.Location,
		Routers:  opts.Routers,
		Queries:  backendtest.DefaultQueries(),
	}
}

func pingQuery() *backendv1.Query {
	return &backendv1.Query{
		Type:          backendv1.DiagnosticType_DIAGNOSTIC_TYPE_PING,
		AddressFamily: backendv1.AddressFamily_ADDRESS_FAMILY_IPV4,
		Target:        "198.51.100.10",
	}
}

func collect(t *testing.T, client backendv1.DiagnosticServiceClient, req *backendv1.RunDiagnosticRequest) []*backendv1.DiagnosticEvent {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	events, err := backendtest.Collect(ctx, client, req)
	if err != nil {
		t.Fatalf("RunDiagnostic: %v", err)
	}
	return events
}

func TestConformance(t *testing.T) {
	opts := testOptions()
	backendtest.Run(t, dial(t, opts), fixture(opts))
}

func TestConformanceIPv6(t *testing.T) {
	opts := testOptions()
	f := fixture(opts)
	f.Queries = nil
	for _, q := range backendtest.DefaultQueries() {
		q.AddressFamily = backendv1.AddressFamily_ADDRESS_FAMILY_IPV6
		if q.GetTarget() != "" {
			q.Target = "2001:db8::7"
		}
		f.Queries = append(f.Queries, q)
	}
	backendtest.Run(t, dial(t, opts), f)
}

func TestPartialCoverage(t *testing.T) {
	opts := testOptions()
	opts.Unreachable = []string{"router-b"}
	f := fixture(opts)
	events := collect(t, dial(t, opts), f.Request(pingQuery()))
	backendtest.CheckStream(t, f, pingQuery(), events)

	if got := events[2].GetOutcome().GetFailure().GetReason(); got != backendv1.ReasonRouterUnreachable {
		t.Errorf("router-b failure reason = %q, want RouterUnreachable", got)
	}
	if c := events[len(events)-1].GetComplete(); c.GetReason() != backendv1.CompletePartialResults || c.GetFailed() != 1 {
		t.Errorf("complete = %+v, want PartialResults with one failure", c)
	}
}

func TestIneligibleRouter(t *testing.T) {
	opts := testOptions()
	opts.Ineligible = []string{"router-c"}
	client := dial(t, opts)
	f := fixture(opts)

	caps, err := client.GetCapabilities(t.Context(), &backendv1.GetCapabilitiesRequest{ApiMajor: backendv1.APIMajor, Cell: opts.Cell})
	if err != nil {
		t.Fatal(err)
	}
	if got := caps.GetLocations()[0].GetEligibleRouters(); got != 2 {
		t.Errorf("eligible_routers = %d, want 2", got)
	}

	events := collect(t, client, f.Request(pingQuery()))
	backendtest.CheckStream(t, f, pingQuery(), events)
	if r := events[0].GetSnapshot().GetRouters()[2]; r.GetSelected() || r.GetOmittedReason() != backendv1.OmittedIneligible {
		t.Errorf("router-c = %+v, want omitted as Ineligible", r)
	}
	if got := events[len(events)-1].GetComplete().GetReason(); got != backendv1.CompletePartialResults {
		t.Errorf("reason = %q, want PartialResults", got)
	}
}

func TestEveryRouterFailsOrIsIneligible(t *testing.T) {
	tests := map[string]struct {
		mutate func(*Options)
		reason string
	}{
		"all unreachable": {func(o *Options) { o.Unreachable = o.Routers }, backendv1.CompleteExecutionFailed},
		"all ineligible":  {func(o *Options) { o.Ineligible = o.Routers }, backendv1.CompleteNoNodesAvailable},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			opts := testOptions()
			tc.mutate(&opts)
			f := fixture(opts)
			events := collect(t, dial(t, opts), f.Request(pingQuery()))
			backendtest.CheckStream(t, f, pingQuery(), events)
			if got := events[len(events)-1].GetComplete().GetReason(); got != tc.reason {
				t.Errorf("reason = %q, want %q", got, tc.reason)
			}
		})
	}
}

func TestDeadlineDuringStream(t *testing.T) {
	opts := testOptions()
	opts.OutcomeDelay = time.Second
	f := fixture(opts)
	req := f.Request(pingQuery())
	req.ExpiresAt = timestamppb.New(time.Now().Add(1500 * time.Millisecond))
	events := collect(t, dial(t, opts), req)

	c := events[len(events)-1].GetComplete()
	if c.GetReason() != backendv1.CompleteDeadlineExceeded || c.GetSuccessful() != 1 || c.GetFailed() != 2 {
		t.Errorf("complete = %+v, want DeadlineExceeded with 1 successful and 2 failed", c)
	}
	for _, ev := range events[2:4] {
		if ev.GetOutcome().GetFailure().GetReason() != backendv1.ReasonDeadlineExceeded {
			t.Errorf("outcome after the deadline = %v, want a DeadlineExceeded failure", ev.GetOutcome())
		}
	}
}

func TestCallerCancel(t *testing.T) {
	opts := testOptions()
	opts.OutcomeDelay = time.Minute
	client := dial(t, opts)

	ctx, cancel := context.WithCancel(t.Context())
	stream, err := client.RunDiagnostic(ctx, fixture(opts).Request(pingQuery()))
	if err != nil {
		t.Fatal(err)
	}
	if ev, err := stream.Recv(); err != nil || ev.GetSnapshot() == nil {
		t.Fatalf("first event = %v, %v; want a snapshot", ev, err)
	}
	cancel()
	done := make(chan error, 1)
	go func() {
		_, err := stream.Recv()
		done <- err
	}()
	select {
	case err := <-done:
		if status.Code(err) != codes.Canceled {
			t.Errorf("Recv after cancel = %v, want Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stream did not end after cancel")
	}
}

// TestTargetRefusals covers target rules beyond the conformance suite's.
func TestTargetRefusals(t *testing.T) {
	opts := testOptions()
	client := dial(t, opts)
	f := fixture(opts)
	tests := map[string]struct {
		mutate func(*backendv1.Query)
		reason string
	}{
		"missing family": {func(q *backendv1.Query) { q.AddressFamily = backendv1.AddressFamily_ADDRESS_FAMILY_UNSPECIFIED }, backendv1.ReasonInvalidQuery},
		"zoned target": {func(q *backendv1.Query) {
			q.Target, q.AddressFamily = "fe80::1%eth0", backendv1.AddressFamily_ADDRESS_FAMILY_IPV6
		}, backendv1.ReasonInvalidQuery},
		"ipv4-mapped target": {func(q *backendv1.Query) {
			q.Target, q.AddressFamily = "::ffff:198.51.100.10", backendv1.AddressFamily_ADDRESS_FAMILY_IPV6
		}, backendv1.ReasonInvalidQuery},
		"multicast target":   {func(q *backendv1.Query) { q.Target = "224.0.0.1" }, backendv1.ReasonInvalidQuery},
		"loopback target":    {func(q *backendv1.Query) { q.Target = "127.0.0.1" }, backendv1.ReasonInvalidQuery},
		"ping a prefix":      {func(q *backendv1.Query) { q.Target = "198.51.100.0/24" }, backendv1.ReasonInvalidQuery},
		"denied destination": {func(q *backendv1.Query) { q.Target = "10.0.0.1" }, backendv1.ReasonDestinationNotAllowed},
		"prefix host bits": {func(q *backendv1.Query) {
			q.Type, q.Target = backendv1.DiagnosticType_DIAGNOSTIC_TYPE_ROUTE_LOOKUP, "198.51.100.1/24"
		}, backendv1.ReasonInvalidQuery},
		"summary with a target": {func(q *backendv1.Query) { q.Type = backendv1.DiagnosticType_DIAGNOSTIC_TYPE_BGP_SUMMARY }, backendv1.ReasonInvalidQuery},
		"unspecified diagnostic": {func(q *backendv1.Query) {
			q.Type = backendv1.DiagnosticType_DIAGNOSTIC_TYPE_UNSPECIFIED
		}, backendv1.ReasonQueryTypeUnavailable},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			q := pingQuery()
			tc.mutate(q)
			events, err := backendtest.Collect(t.Context(), client, f.Request(q))
			if len(events) != 0 {
				t.Errorf("refused request streamed %d events", len(events))
			}
			if got, _ := backendv1.RefusalReason(err); got != tc.reason {
				t.Errorf("reason = %q (%v), want %q", got, err, tc.reason)
			}
		})
	}
}

func TestRefusalCodes(t *testing.T) {
	opts := testOptions()
	client := dial(t, opts)
	f := fixture(opts)
	tests := map[string]struct {
		mutate func(*backendv1.RunDiagnosticRequest)
		code   codes.Code
	}{
		"api major":       {func(r *backendv1.RunDiagnosticRequest) { r.ApiMajor = 2 }, codes.FailedPrecondition},
		"other cell":      {func(r *backendv1.RunDiagnosticRequest) { r.Cell = "cell-b" }, codes.PermissionDenied},
		"other location":  {func(r *backendv1.RunDiagnosticRequest) { r.Location = "other" }, codes.FailedPrecondition},
		"expired":         {func(r *backendv1.RunDiagnosticRequest) { r.ExpiresAt = timestamppb.New(time.Now().Add(-time.Second)) }, codes.DeadlineExceeded},
		"uppercase uuid":  {func(r *backendv1.RunDiagnosticRequest) { r.RequestId = "B74E19EE-926F-43DC-B03D-C29550A827AB" }, codes.InvalidArgument},
		"project id char": {func(r *backendv1.RunDiagnosticRequest) { r.ProjectId = "has space" }, codes.InvalidArgument},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			req := f.Request(pingQuery())
			tc.mutate(req)
			_, err := backendtest.Collect(t.Context(), client, req)
			if got := status.Code(err); got != tc.code {
				t.Errorf("code = %s (%v), want %s", got, err, tc.code)
			}
		})
	}
}

func TestCanonicalizesTarget(t *testing.T) {
	opts := testOptions()
	f := fixture(opts)
	events := collect(t, dial(t, opts), f.Request(&backendv1.Query{
		Type:          backendv1.DiagnosticType_DIAGNOSTIC_TYPE_PING,
		AddressFamily: backendv1.AddressFamily_ADDRESS_FAMILY_IPV6,
		Target:        "2001:DB8:0::10",
	}))
	ping := events[1].GetOutcome().GetObservation().GetPing()
	if ping.GetDestination() != "2001:db8::10" {
		t.Errorf("destination = %q, want the canonical 2001:db8::10", ping.GetDestination())
	}
	if ping.GetSent() != PingProbes || len(ping.GetReplies()) != PingProbes {
		t.Errorf("ping sent %d with %d replies, want %d", ping.GetSent(), len(ping.GetReplies()), PingProbes)
	}
}

func TestNewRejectsInvalidOptions(t *testing.T) {
	tests := map[string]func(*Options){
		"no cell":             func(o *Options) { o.Cell = "" },
		"no location":         func(o *Options) { o.Location = "" },
		"no routers":          func(o *Options) { o.Routers = nil },
		"too many routers":    func(o *Options) { o.Routers = []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"} },
		"duplicate router":    func(o *Options) { o.Routers = []string{"router-a", "router-a"} },
		"unknown unreachable": func(o *Options) { o.Unreachable = []string{"router-z"} },
		"unknown ineligible":  func(o *Options) { o.Ineligible = []string{"router-z"} },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			opts := testOptions()
			mutate(&opts)
			if _, err := New(opts); err == nil {
				t.Fatal("New accepted invalid options")
			}
		})
	}
}
