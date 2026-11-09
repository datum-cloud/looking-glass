package fakebackend

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/timestamppb"

	servicev1 "go.datum.net/galactic/api/fabric/service/v1"
	fabricv1 "go.datum.net/galactic/api/fabric/v1"
)

const requestID = "b74e19ee-926f-43dc-b03d-c29550a827ab"

func dial(t *testing.T, opts Options) servicev1.LookingGlassServiceClient {
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
	return servicev1.NewLookingGlassServiceClient(conn)
}

func testOptions() Options {
	opts := DefaultOptions()
	opts.OutcomeDelay = time.Millisecond
	return opts
}

func request(q *fabricv1.Query) *servicev1.RunDiagnosticRequest {
	return &servicev1.RunDiagnosticRequest{
		ApiMajor:         APIMajor,
		Cell:             "cell-a",
		ProjectId:        "project-uid-1",
		RequestId:        requestID,
		ExpiresAt:        timestamppb.New(time.Now().Add(45 * time.Second)),
		Query:            q,
		RequiredFeatures: []string{"router-outcomes", "terminal-coverage", "aggregate-probe-budgets"},
	}
}

func pingQuery() *fabricv1.Query {
	return &fabricv1.Query{
		Type:          fabricv1.QueryType_QUERY_TYPE_PING,
		Target:        "198.51.100.10",
		AddressFamily: fabricv1.AddressFamily_ADDRESS_FAMILY_IPV4,
	}
}

func collect(t *testing.T, client servicev1.LookingGlassServiceClient, req *servicev1.RunDiagnosticRequest) ([]*servicev1.DiagnosticEvent, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream, err := client.RunDiagnostic(ctx, req)
	if err != nil {
		return nil, err
	}
	var events []*servicev1.DiagnosticEvent
	for {
		ev, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return events, nil
		}
		if err != nil {
			return events, err
		}
		events = append(events, ev)
	}
}

func TestGetCapabilities(t *testing.T) {
	client := dial(t, testOptions())
	resp, err := client.GetCapabilities(context.Background(), &servicev1.GetCapabilitiesRequest{Cell: "cell-a", ApiMajor: APIMajor})
	if err != nil {
		t.Fatalf("GetCapabilities: %v", err)
	}
	if !resp.GetEnabled() || resp.GetCell() != "cell-a" || resp.GetApiMajor() != APIMajor {
		t.Errorf("unexpected response header: %+v", resp)
	}
	if len(resp.GetOperations()) != len(Operations) {
		t.Errorf("operations = %d, want %d", len(resp.GetOperations()), len(Operations))
	}
	for _, op := range resp.GetOperations() {
		if len(op.GetAddressFamilies()) != 2 {
			t.Errorf("%v offers %v, want both families", op.GetType(), op.GetAddressFamilies())
		}
	}
	limits := resp.GetLimits()
	if limits.GetMaxBudgets().GetMaxPrefixes() == 0 || limits.GetMaxNodeExecution() == nil {
		t.Errorf("limits lack budgets or node execution: %+v", limits)
	}
	// aggregate-probe-budgets is advertised, so admission limits must be.
	if a := limits.GetAdmission(); a.GetRequestProject().GetPerSecond() != 0.2 || a.GetProjectStreams() != 2 {
		t.Errorf("admission limits = %+v", a)
	}
	if len(resp.GetFeatures()) != len(Features) {
		t.Errorf("features = %v, want %v", resp.GetFeatures(), Features)
	}

	_, err = client.GetCapabilities(context.Background(), &servicev1.GetCapabilitiesRequest{Cell: "cell-a", ApiMajor: 2})
	assertRefusal(t, err, codes.FailedPrecondition, "VersionUnsupported")
}

func TestRunDiagnosticStreamsEveryDiagnostic(t *testing.T) {
	client := dial(t, testOptions())
	queries := map[string]*fabricv1.Query{
		"route lookup v4 prefix": {Type: fabricv1.QueryType_QUERY_TYPE_ROUTE_LOOKUP, Target: "198.51.100.0/24", AddressFamily: fabricv1.AddressFamily_ADDRESS_FAMILY_IPV4},
		"route lookup v6":        {Type: fabricv1.QueryType_QUERY_TYPE_ROUTE_LOOKUP, Target: "2001:db8::1", AddressFamily: fabricv1.AddressFamily_ADDRESS_FAMILY_IPV6},
		"bgp summary":            {Type: fabricv1.QueryType_QUERY_TYPE_BGP_SUMMARY, AddressFamily: fabricv1.AddressFamily_ADDRESS_FAMILY_IPV4},
		"ping":                   pingQuery(),
		"traceroute v6":          {Type: fabricv1.QueryType_QUERY_TYPE_TRACEROUTE, Target: "2001:db8::10", AddressFamily: fabricv1.AddressFamily_ADDRESS_FAMILY_IPV6},
	}
	for name, q := range queries {
		t.Run(name, func(t *testing.T) {
			events, err := collect(t, client, request(q))
			if err != nil {
				t.Fatalf("RunDiagnostic: %v", err)
			}
			// snapshot, three outcomes, complete
			if len(events) != 5 {
				t.Fatalf("got %d events, want 5", len(events))
			}
			for i, ev := range events {
				if ev.GetSequence() != uint64(i+1) || ev.GetRequestId() != requestID {
					t.Errorf("event %d: sequence %d request %q", i, ev.GetSequence(), ev.GetRequestId())
				}
			}
			if events[0].GetSnapshot() == nil {
				t.Fatal("first event is not a snapshot")
			}
			for _, ev := range events[1:4] {
				obs := ev.GetOutcome().GetObservation()
				if obs == nil || obs.GetSampleTime() == nil || obs.GetRouterId() == "" {
					t.Fatalf("outcome without a sampled observation: %v", ev)
				}
				if obs.GetResult() == nil {
					t.Fatalf("observation has no result: %v", obs)
				}
			}
			c := events[4].GetComplete()
			if c.GetReason() != "Succeeded" || c.GetExpected() != 3 || c.GetSuccessful() != 3 || c.GetSnapshotId() != events[0].GetSnapshot().GetSnapshotId() {
				t.Errorf("complete = %+v", c)
			}
		})
	}
}

func TestRunDiagnosticPartialCoverage(t *testing.T) {
	opts := testOptions()
	opts.Unreachable = []string{"router-b"}
	client := dial(t, opts)

	req := request(pingQuery())
	req.Nodes = []string{"router-a", "router-b"}
	events, err := collect(t, client, req)
	if err != nil {
		t.Fatalf("RunDiagnostic: %v", err)
	}
	if len(events) != 4 {
		t.Fatalf("got %d events, want 4", len(events))
	}
	snap := events[0].GetSnapshot()
	if r := snap.GetRouters()[2]; r.GetSelected() || r.GetOmittedReason() != "NotRequested" {
		t.Errorf("router-c = %+v, want omitted NotRequested", r)
	}
	if f := events[2].GetOutcome().GetFailure(); f.GetReason() != "NodeUnreachable" {
		t.Errorf("router-b outcome = %v, want NodeUnreachable", events[2].GetOutcome())
	}
	c := events[3].GetComplete()
	if c.GetReason() != "PartialResults" || c.GetExpected() != 3 || c.GetSuccessful() != 1 || c.GetFailed() != 1 || c.GetOmitted() != 1 {
		t.Errorf("complete = %+v", c)
	}
}

func TestRunDiagnosticAllFailed(t *testing.T) {
	opts := testOptions()
	opts.Unreachable = opts.Routers
	events, err := collect(t, dial(t, opts), request(pingQuery()))
	if err != nil {
		t.Fatal(err)
	}
	if got := events[len(events)-1].GetComplete().GetReason(); got != "ExecutionFailed" {
		t.Errorf("reason = %q, want ExecutionFailed", got)
	}
}

func TestRunDiagnosticDeadline(t *testing.T) {
	opts := testOptions()
	opts.OutcomeDelay = time.Second
	client := dial(t, opts)

	req := request(pingQuery())
	req.ExpiresAt = timestamppb.New(time.Now().Add(1500 * time.Millisecond))
	events, err := collect(t, client, req)
	if err != nil {
		t.Fatalf("RunDiagnostic: %v", err)
	}
	c := events[len(events)-1].GetComplete()
	if c.GetReason() != "DeadlineExceeded" || c.GetSuccessful() != 1 || c.GetFailed() != 2 {
		t.Errorf("complete = %+v, want DeadlineExceeded with 1 successful and 2 failed", c)
	}
	for _, ev := range events[2:4] {
		if ev.GetOutcome().GetFailure().GetReason() != "DeadlineExceeded" {
			t.Errorf("outcome after deadline = %v", ev.GetOutcome())
		}
	}
}

func TestRunDiagnosticCallerCancel(t *testing.T) {
	opts := testOptions()
	opts.OutcomeDelay = time.Minute
	client := dial(t, opts)

	ctx, cancel := context.WithCancel(context.Background())
	stream, err := client.RunDiagnostic(ctx, request(pingQuery()))
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

func TestRunDiagnosticRefusals(t *testing.T) {
	client := dial(t, testOptions())
	tests := map[string]struct {
		mutate func(*servicev1.RunDiagnosticRequest)
		code   codes.Code
		reason string
	}{
		"api major":       {func(r *servicev1.RunDiagnosticRequest) { r.ApiMajor = 2 }, codes.FailedPrecondition, "VersionUnsupported"},
		"other cell":      {func(r *servicev1.RunDiagnosticRequest) { r.Cell = "cell-b" }, codes.PermissionDenied, "Unauthorized"},
		"project id":      {func(r *servicev1.RunDiagnosticRequest) { r.ProjectId = "has space" }, codes.InvalidArgument, "InvalidQuery"},
		"uppercase uuid":  {func(r *servicev1.RunDiagnosticRequest) { r.RequestId = "B74E19EE-926F-43DC-B03D-C29550A827AB" }, codes.InvalidArgument, "InvalidQuery"},
		"missing expiry":  {func(r *servicev1.RunDiagnosticRequest) { r.ExpiresAt = nil }, codes.InvalidArgument, "InvalidQuery"},
		"expired":         {func(r *servicev1.RunDiagnosticRequest) { r.ExpiresAt = timestamppb.New(time.Now().Add(-time.Second)) }, codes.InvalidArgument, "InvalidQuery"},
		"expiry too far":  {func(r *servicev1.RunDiagnosticRequest) { r.ExpiresAt = timestamppb.New(time.Now().Add(time.Hour)) }, codes.InvalidArgument, "InvalidQuery"},
		"unknown feature": {func(r *servicev1.RunDiagnosticRequest) { r.RequiredFeatures = []string{"live-hops"} }, codes.FailedPrecondition, "VersionUnsupported"},
		"unoffered query": {func(r *servicev1.RunDiagnosticRequest) { r.Query.Type = fabricv1.QueryType_QUERY_TYPE_AS_PATH }, codes.FailedPrecondition, "QueryTypeUnavailable"},
		"hostname target": {func(r *servicev1.RunDiagnosticRequest) { r.Query.Target = "router.example" }, codes.InvalidArgument, "InvalidQuery"},
		"family mismatch": {func(r *servicev1.RunDiagnosticRequest) { r.Query.Target = "2001:db8::1" }, codes.InvalidArgument, "InvalidQuery"},
		"ping prefix":     {func(r *servicev1.RunDiagnosticRequest) { r.Query.Target = "198.51.100.0/24" }, codes.InvalidArgument, "InvalidQuery"},
		"unknown node":    {func(r *servicev1.RunDiagnosticRequest) { r.Nodes = []string{"router-z"} }, codes.InvalidArgument, "InvalidQuery"},
		"duplicate node":  {func(r *servicev1.RunDiagnosticRequest) { r.Nodes = []string{"router-a", "router-a"} }, codes.InvalidArgument, "InvalidQuery"},
		"missing family": {func(r *servicev1.RunDiagnosticRequest) {
			r.Query.AddressFamily = fabricv1.AddressFamily_ADDRESS_FAMILY_UNSPECIFIED
		}, codes.InvalidArgument, "InvalidQuery"},
		"zoned target":       {func(r *servicev1.RunDiagnosticRequest) { setTarget(r, "fe80::1%eth0", false) }, codes.InvalidArgument, "InvalidQuery"},
		"ipv4-mapped target": {func(r *servicev1.RunDiagnosticRequest) { setTarget(r, "::ffff:198.51.100.10", false) }, codes.InvalidArgument, "InvalidQuery"},
		"multicast target":   {func(r *servicev1.RunDiagnosticRequest) { r.Query.Target = "224.0.0.1" }, codes.InvalidArgument, "InvalidQuery"},
		"loopback target":    {func(r *servicev1.RunDiagnosticRequest) { r.Query.Target = "127.0.0.1" }, codes.InvalidArgument, "InvalidQuery"},
		"denied destination": {func(r *servicev1.RunDiagnosticRequest) { r.Query.Target = "10.0.0.1" }, codes.InvalidArgument, "DestinationNotAllowed"},
		"prefix host bits": {func(r *servicev1.RunDiagnosticRequest) {
			r.Query.Type = fabricv1.QueryType_QUERY_TYPE_ROUTE_LOOKUP
			r.Query.Target = "198.51.100.1/24"
		}, codes.InvalidArgument, "InvalidQuery"},
		"budget over limit": {func(r *servicev1.RunDiagnosticRequest) {
			r.Budgets = &fabricv1.Budgets{MaxPrefixes: MaxBudgets.GetMaxPrefixes() + 1}
		}, codes.InvalidArgument, "InvalidQuery"},
		"summary with arg": {func(r *servicev1.RunDiagnosticRequest) { r.Query.Type = fabricv1.QueryType_QUERY_TYPE_BGP_SUMMARY }, codes.InvalidArgument, "InvalidQuery"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			req := request(pingQuery())
			tc.mutate(req)
			events, err := collect(t, client, req)
			if len(events) != 0 {
				t.Errorf("refused request streamed %d events", len(events))
			}
			assertRefusal(t, err, tc.code, tc.reason)
		})
	}
}

func setTarget(r *servicev1.RunDiagnosticRequest, target string, v4 bool) {
	r.Query.Target = target
	r.Query.AddressFamily = fabricv1.AddressFamily_ADDRESS_FAMILY_IPV6
	if v4 {
		r.Query.AddressFamily = fabricv1.AddressFamily_ADDRESS_FAMILY_IPV4
	}
}

func TestRunDiagnosticCanonicalizesTarget(t *testing.T) {
	req := request(&fabricv1.Query{
		Type:          fabricv1.QueryType_QUERY_TYPE_PING,
		Target:        "2001:DB8:0::10",
		AddressFamily: fabricv1.AddressFamily_ADDRESS_FAMILY_IPV6,
	})
	req.Nodes = []string{"router-a"}
	events, err := collect(t, dial(t, testOptions()), req)
	if err != nil {
		t.Fatal(err)
	}
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
		"no routers":          func(o *Options) { o.Routers = nil },
		"duplicate router":    func(o *Options) { o.Routers = []string{"router-a", "router-a"} },
		"unknown unreachable": func(o *Options) { o.Unreachable = []string{"router-z"} },
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

func assertRefusal(t *testing.T, err error, code codes.Code, reason string) {
	t.Helper()
	st, ok := status.FromError(err)
	if !ok || st.Code() != code {
		t.Fatalf("error = %v, want code %v", err, code)
	}
	for _, d := range st.Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok {
			if info.GetReason() != reason || info.GetDomain() != ErrorDomain {
				t.Errorf("ErrorInfo = %+v, want reason %q in %q", info, reason, ErrorDomain)
			}
			return
		}
	}
	t.Errorf("status %v carries no ErrorInfo", st)
}
