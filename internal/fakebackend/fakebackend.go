// Package fakebackend is a fake of Galactic's fabric.service.v1
// LookingGlassService for local development and tests. It validates requests
// the way the contract describes, then streams a snapshot, one synthetic
// outcome per selected router and a complete event. It never touches a
// network: every observation is generated from documentation-range addresses
// and documentation ASNs.
package fakebackend

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"slices"
	"time"

	"github.com/google/uuid"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	servicev1 "go.datum.net/galactic/api/fabric/service/v1"
	fabricv1 "go.datum.net/galactic/api/fabric/v1"
)

const (
	// APIMajor is the only service API major the fake serves.
	APIMajor = 1
	// ErrorDomain is the google.rpc.ErrorInfo domain of every refusal.
	ErrorDomain = "fabric-api.datumapis.com"
	// MaxRequestLifetime bounds expires_at, measured from receipt.
	MaxRequestLifetime = 120 * time.Second
	// MaxSelectedRouters bounds the nodes a request may name.
	MaxSelectedRouters = 32
	// MaxNodeExecution bounds one router's work.
	MaxNodeExecution = 10 * time.Second
	// ContractRevision is reported as the build the fake implements.
	ContractRevision = "fake"
	// PingProbes is the number of echo attempts per ping, as the service
	// sends.
	PingProbes = 3
)

// MaxBudgets are the advertised response budgets. A request may lower them,
// and zero selects these values.
var MaxBudgets = &fabricv1.Budgets{
	MaxResponseBytes:      1 << 20,
	MaxPrefixes:           64,
	MaxPathsPerPrefix:     16,
	MaxCommunitiesPerPath: 64,
}

// DeniedPrefixes stand in for the cell's destination policy: ping and
// traceroute toward them are refused with DestinationNotAllowed. The real
// policy also denies the documentation ranges; the fake allows them so that
// tests and examples can use synthetic addresses.
var DeniedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("fc00::/7"),
}

// Features are the capability names the fake advertises. A request may
// require only these.
var Features = []string{"router-outcomes", "terminal-coverage", "aggregate-probe-budgets"}

// Operations are the alpha diagnostics, each for both address families.
var Operations = []fabricv1.QueryType{
	fabricv1.QueryType_QUERY_TYPE_ROUTE_LOOKUP,
	fabricv1.QueryType_QUERY_TYPE_BGP_SUMMARY,
	fabricv1.QueryType_QUERY_TYPE_PING,
	fabricv1.QueryType_QUERY_TYPE_TRACEROUTE,
}

var projectIDPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// Options configures the fake.
type Options struct {
	// Cell is the cell the fake serves. Requests for another cell are refused.
	Cell string
	// Routers are the fabric routers in the cell's inventory, in order.
	Routers []string
	// Unreachable names routers whose outcome is a NodeUnreachable failure,
	// to exercise partial coverage.
	Unreachable []string
	// OutcomeDelay is the pause before each router's outcome.
	OutcomeDelay time.Duration
	// Now returns the current time. Defaults to time.Now.
	Now func() time.Time
}

// DefaultOptions returns a three-router cell with no failures.
func DefaultOptions() Options {
	return Options{
		Cell:         "cell-a",
		Routers:      []string{"router-a", "router-b", "router-c"},
		OutcomeDelay: 250 * time.Millisecond,
	}
}

// Server implements servicev1.LookingGlassServiceServer.
type Server struct {
	servicev1.UnimplementedLookingGlassServiceServer
	opts Options
}

// New returns a fake service, or an error when the options name no cell, no
// routers, a duplicate router or an unreachable router outside the inventory.
func New(opts Options) (*Server, error) {
	if opts.Cell == "" {
		return nil, errors.New("a cell is required")
	}
	if len(opts.Routers) == 0 {
		return nil, errors.New("at least one router is required")
	}
	seen := map[string]bool{}
	for _, r := range opts.Routers {
		if r == "" || seen[r] {
			return nil, fmt.Errorf("router names must be unique and non-empty: %q", r)
		}
		seen[r] = true
	}
	for _, r := range opts.Unreachable {
		if !seen[r] {
			return nil, fmt.Errorf("unreachable router %q is not in the inventory", r)
		}
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Server{opts: opts}, nil
}

// Register adds the fake to a gRPC server.
func (s *Server) Register(srv *grpc.Server) {
	servicev1.RegisterLookingGlassServiceServer(srv, s)
}

// GetCapabilities reports the fake's policy.
func (s *Server) GetCapabilities(_ context.Context, req *servicev1.GetCapabilitiesRequest) (*servicev1.GetCapabilitiesResponse, error) {
	if req.GetApiMajor() != APIMajor {
		return nil, refuse(codes.FailedPrecondition, "VersionUnsupported", "unsupported api_major")
	}
	if req.GetCell() != s.opts.Cell {
		return nil, refuse(codes.PermissionDenied, "Unauthorized", "cell is not served here")
	}
	families := []fabricv1.AddressFamily{fabricv1.AddressFamily_ADDRESS_FAMILY_IPV4, fabricv1.AddressFamily_ADDRESS_FAMILY_IPV6}
	ops := make([]*servicev1.Operation, 0, len(Operations))
	for _, t := range Operations {
		ops = append(ops, &servicev1.Operation{Type: t, AddressFamilies: families})
	}
	return &servicev1.GetCapabilitiesResponse{
		Cell:             s.opts.Cell,
		ApiMajor:         APIMajor,
		Enabled:          true,
		ContractRevision: ContractRevision,
		PolicyRevision:   ContractRevision,
		Operations:       ops,
		Features:         slices.Clone(Features),
		Limits: &servicev1.Limits{
			MaxRequestLifetime:  durationpb.New(MaxRequestLifetime),
			MaxNodeExecution:    durationpb.New(MaxNodeExecution),
			MaxSelectedRouters:  MaxSelectedRouters,
			MaxInventoryRouters: uint32(len(s.opts.Routers)),
			MaxBudgets:          proto.CloneOf(MaxBudgets),
			// The project rate and stream limits are the contract's; the
			// rest are plausible values for a small cell.
			Admission: &servicev1.AdmissionLimits{
				RequestCell:       &servicev1.TokenRate{PerSecond: 2, Burst: 4},
				RequestProject:    &servicev1.TokenRate{PerSecond: 0.2, Burst: 2},
				ActiveJobs:        8,
				ProjectActiveJobs: 2,
				Streams:           16,
				ProjectStreams:    2,
				QueueWait:         durationpb.New(5 * time.Second),
				PingPackets:       PingProbes,
				TraceroutePackets: 20,
			},
		},
		GeneratedAt: timestamppb.New(s.opts.Now()),
	}, nil
}

// RunDiagnostic validates the request and streams its events.
func (s *Server) RunDiagnostic(req *servicev1.RunDiagnosticRequest, stream grpc.ServerStreamingServer[servicev1.DiagnosticEvent]) error {
	selected, err := s.validate(req)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithDeadline(stream.Context(), req.GetExpiresAt().AsTime())
	defer cancel()

	r := &run{req: req, stream: stream}
	snap := &servicev1.Snapshot{
		SnapshotId:      "snap-" + req.GetRequestId(),
		CapturedAt:      timestamppb.New(s.opts.Now()),
		ResourceVersion: "1",
	}
	complete := &servicev1.Complete{SnapshotId: snap.GetSnapshotId()}
	for _, node := range s.opts.Routers {
		router := &servicev1.SnapshotRouter{Node: node, PodUid: podUID(node)}
		complete.Expected++
		if selected[node] {
			router.Selected = true
		} else {
			router.OmittedReason = "NotRequested"
			complete.Omitted++
		}
		snap.Routers = append(snap.Routers, router)
	}
	if err := r.send(&servicev1.DiagnosticEvent{Event: &servicev1.DiagnosticEvent_Snapshot{Snapshot: snap}}); err != nil {
		return err
	}

	var stopped string
	for _, router := range snap.GetRouters() {
		if !router.GetSelected() {
			continue
		}
		if stopped == "" {
			stopped = wait(ctx, stream.Context(), s.opts.OutcomeDelay)
		}
		outcome := &servicev1.RouterOutcome{Node: router.GetNode(), PodUid: router.GetPodUid()}
		switch {
		case stopped != "":
			outcome.Result = failure(stopped, "the request ended before the router ran")
		case slices.Contains(s.opts.Unreachable, router.GetNode()):
			outcome.Result = failure("NodeUnreachable", "router did not answer")
		default:
			outcome.Result = &servicev1.RouterOutcome_Observation{Observation: s.observe(router.GetNode(), req.GetQuery())}
		}
		outcome.CompletedAt = timestamppb.New(s.opts.Now())
		if outcome.GetFailure() != nil {
			complete.Failed++
		} else {
			complete.Successful++
		}
		if stopped == "Cancelled" {
			// The caller is gone; nothing more can be delivered.
			return status.Error(codes.Canceled, "caller cancelled the request")
		}
		if err := r.send(&servicev1.DiagnosticEvent{Event: &servicev1.DiagnosticEvent_Outcome{Outcome: outcome}}); err != nil {
			return err
		}
	}

	complete.FinishedAt = timestamppb.New(s.opts.Now())
	complete.Reason = terminalReason(stopped, complete)
	return r.send(&servicev1.DiagnosticEvent{Event: &servicev1.DiagnosticEvent_Complete{Complete: complete}})
}

// validate applies the contract's request rules and returns the selected
// routers.
func (s *Server) validate(req *servicev1.RunDiagnosticRequest) (map[string]bool, error) {
	if req.GetApiMajor() != APIMajor {
		return nil, refuse(codes.FailedPrecondition, "VersionUnsupported", "unsupported api_major")
	}
	if req.GetCell() != s.opts.Cell {
		return nil, refuse(codes.PermissionDenied, "Unauthorized", "cell is not served here")
	}
	if !projectIDPattern.MatchString(req.GetProjectId()) {
		return nil, invalid("project_id must be 1 to 128 characters of [A-Za-z0-9._:-]")
	}
	if id, err := uuid.Parse(req.GetRequestId()); err != nil || id.String() != req.GetRequestId() {
		return nil, invalid("request_id must be a lowercase UUID")
	}
	if req.GetExpiresAt() == nil {
		return nil, invalid("expires_at is required")
	}
	now := s.opts.Now()
	expires := req.GetExpiresAt().AsTime()
	if !expires.After(now) || expires.Sub(now) > MaxRequestLifetime {
		return nil, invalid(fmt.Sprintf("expires_at must be in the future and at most %s away", MaxRequestLifetime))
	}
	for _, f := range req.GetRequiredFeatures() {
		if !slices.Contains(Features, f) {
			return nil, refuse(codes.FailedPrecondition, "VersionUnsupported", "a required feature is not supported")
		}
	}
	if err := validateQuery(req.GetQuery()); err != nil {
		return nil, err
	}
	canonicalizeTarget(req.GetQuery())
	if b := req.GetBudgets(); b.GetMaxResponseBytes() > MaxBudgets.GetMaxResponseBytes() ||
		b.GetMaxPrefixes() > MaxBudgets.GetMaxPrefixes() ||
		b.GetMaxPathsPerPrefix() > MaxBudgets.GetMaxPathsPerPrefix() ||
		b.GetMaxCommunitiesPerPath() > MaxBudgets.GetMaxCommunitiesPerPath() {
		return nil, invalid("budgets must not exceed the advertised limits")
	}
	if len(req.GetNodes()) > MaxSelectedRouters {
		return nil, invalid(fmt.Sprintf("at most %d nodes may be named", MaxSelectedRouters))
	}
	selected := map[string]bool{}
	for _, n := range req.GetNodes() {
		if !slices.Contains(s.opts.Routers, n) || selected[n] {
			return nil, invalid("nodes must be unique names of routers in the cell")
		}
		selected[n] = true
	}
	if len(selected) == 0 {
		for _, n := range s.opts.Routers {
			selected[n] = true
		}
	}
	return selected, nil
}

func validateQuery(q *fabricv1.Query) error {
	if q == nil || !slices.Contains(Operations, q.GetType()) {
		return refuse(codes.FailedPrecondition, "QueryTypeUnavailable", "query type is not offered")
	}
	var wantV4 bool
	switch q.GetAddressFamily() {
	case fabricv1.AddressFamily_ADDRESS_FAMILY_IPV4:
		wantV4 = true
	case fabricv1.AddressFamily_ADDRESS_FAMILY_IPV6:
	default:
		return invalid("address_family is required")
	}
	if q.GetType() == fabricv1.QueryType_QUERY_TYPE_BGP_SUMMARY {
		if q.GetTarget() != "" {
			return invalid("bgp summary takes no target")
		}
		return nil
	}
	if q.GetType() == fabricv1.QueryType_QUERY_TYPE_ROUTE_LOOKUP {
		if p, err := netip.ParsePrefix(q.GetTarget()); err == nil {
			if p != p.Masked() || p.Addr().Is4In6() || p.Addr().Is4() != wantV4 {
				return invalid("a prefix target must have no host bits and match the requested family")
			}
			return nil
		}
	}
	addr, err := netip.ParseAddr(q.GetTarget())
	if err != nil || addr.Zone() != "" || addr.Is4In6() || addr.Is4() != wantV4 {
		return invalid("target must be a numeric address of the requested family, without a zone")
	}
	if q.GetType() == fabricv1.QueryType_QUERY_TYPE_ROUTE_LOOKUP {
		return nil
	}
	if !addr.IsGlobalUnicast() {
		return invalid("ping and traceroute targets must be unicast addresses")
	}
	for _, p := range DeniedPrefixes {
		if p.Contains(addr) {
			return refuse(codes.InvalidArgument, "DestinationNotAllowed", "destination is not allowed")
		}
	}
	return nil
}

// canonicalizeTarget rewrites a validated target to its canonical text form,
// so results echo one spelling of each address.
func canonicalizeTarget(q *fabricv1.Query) {
	if p, err := netip.ParsePrefix(q.GetTarget()); err == nil {
		q.Target = p.String()
	} else if a, err := netip.ParseAddr(q.GetTarget()); err == nil {
		q.Target = a.String()
	}
}

func (s *Server) observe(node string, q *fabricv1.Query) *fabricv1.Observation {
	now := s.opts.Now()
	obs := &fabricv1.Observation{
		Node:       node,
		RouterId:   routerID(node),
		Asn:        64496,
		FrrVersion: "fake",
		SampleTime: timestamppb.New(now),
		Duration:   durationpb.New(s.opts.OutcomeDelay),
	}
	v4 := q.GetAddressFamily() == fabricv1.AddressFamily_ADDRESS_FAMILY_IPV4
	source, hop := "2001:db8:ffff::1", "2001:db8:ffff::2"
	if v4 {
		source, hop = "192.0.2.1", "192.0.2.2"
	}
	switch q.GetType() {
	case fabricv1.QueryType_QUERY_TYPE_ROUTE_LOOKUP:
		prefix := "2001:db8::/32"
		if v4 {
			prefix = "198.51.100.0/24"
		}
		one := uint32(1)
		obs.Matched, obs.MatchedIsExact = &one, false
		obs.Result = &fabricv1.Observation_Routes{Routes: &fabricv1.RouteResult{
			LookupKind: fabricv1.LookupKind_LOOKUP_KIND_LONGEST_MATCH,
			Prefixes: []*fabricv1.PrefixObservation{{
				Prefix:     prefix,
				TotalPaths: 1,
				Paths: []*fabricv1.Path{{
					Best: true, Valid: true, AsPath: "64497", Origin: "IGP", OriginAsn: 64497,
					NextHops: []*fabricv1.NextHop{{Address: hop, Accessible: true, Used: true}},
				}},
			}},
		}}
	case fabricv1.QueryType_QUERY_TYPE_BGP_SUMMARY:
		obs.Result = &fabricv1.Observation_Summary{Summary: &fabricv1.SummaryResult{
			RouterId: obs.GetRouterId(), Asn: obs.GetAsn(), TotalPeers: 1, EstablishedPeers: 1,
			Peers: []*fabricv1.Peer{{
				Address: hop, RemoteAs: 64497, LocalAs: 64496, State: "Established", Established: true,
				Uptime: durationpb.New(time.Hour), PrefixesReceived: 1, PrefixesSent: 1,
			}},
		}}
	case fabricv1.QueryType_QUERY_TYPE_PING:
		replies := make([]*fabricv1.ProbeReply, 0, PingProbes)
		for i := range PingProbes {
			replies = append(replies, &fabricv1.ProbeReply{
				Sequence: uint32(i + 1), Address: q.GetTarget(),
				Rtt: durationpb.New(time.Duration(i+1) * time.Millisecond), IcmpType: "echo-reply",
			})
		}
		obs.Result = &fabricv1.Observation_Ping{Ping: &fabricv1.PingResult{
			Source: source, Destination: q.GetTarget(), Sent: PingProbes, Received: PingProbes,
			Replies: replies,
			RttMin:  durationpb.New(time.Millisecond), RttAvg: durationpb.New(2 * time.Millisecond),
			RttMax: durationpb.New(PingProbes * time.Millisecond),
		}}
	case fabricv1.QueryType_QUERY_TYPE_TRACEROUTE:
		obs.Result = &fabricv1.Observation_Traceroute{Traceroute: &fabricv1.TracerouteResult{
			Source: source, Destination: q.GetTarget(), Reached: true,
			Hops: []*fabricv1.Hop{
				{Ttl: 1, Probes: []*fabricv1.ProbeReply{{Sequence: 1, Address: hop, Rtt: durationpb.New(time.Millisecond), IcmpType: "time-exceeded"}}},
				{Ttl: 2, Probes: []*fabricv1.ProbeReply{{Sequence: 2, Address: q.GetTarget(), Rtt: durationpb.New(2 * time.Millisecond), IcmpType: "echo-reply"}}},
			},
		}}
	}
	return obs
}

// wait pauses for d. It returns "" when the pause completed, "Cancelled" when
// the caller went away and "DeadlineExceeded" when expires_at passed.
func wait(ctx, caller context.Context, d time.Duration) string {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return ""
	case <-ctx.Done():
		if caller.Err() != nil {
			return "Cancelled"
		}
		return "DeadlineExceeded"
	}
}

// terminalReason follows the contract's precedence: a stop reason wins, then
// coverage decides.
func terminalReason(stopped string, c *servicev1.Complete) string {
	switch {
	case stopped != "":
		return stopped
	case c.GetExpected()-c.GetOmitted() == 0:
		return "NoNodesAvailable"
	case c.GetSuccessful() == 0:
		return "ExecutionFailed"
	case c.GetFailed() > 0 || c.GetTruncatedRouters() > 0:
		// NotRequested omissions alone don't make a result partial.
		return "PartialResults"
	default:
		return "Succeeded"
	}
}

type run struct {
	req    *servicev1.RunDiagnosticRequest
	stream grpc.ServerStreamingServer[servicev1.DiagnosticEvent]
	seq    uint64
}

func (r *run) send(ev *servicev1.DiagnosticEvent) error {
	r.seq++
	ev.RequestId = r.req.GetRequestId()
	ev.Sequence = r.seq
	return r.stream.Send(ev)
}

func failure(reason, message string) *servicev1.RouterOutcome_Failure {
	return &servicev1.RouterOutcome_Failure{Failure: &servicev1.Failure{Reason: reason, Message: message}}
}

func invalid(message string) error {
	return refuse(codes.InvalidArgument, "InvalidQuery", message)
}

// refuse returns a gRPC status carrying a google.rpc.ErrorInfo, as the
// service does for every refusal before the snapshot.
func refuse(code codes.Code, reason, message string) error {
	st, err := status.New(code, message).WithDetails(&errdetails.ErrorInfo{Reason: reason, Domain: ErrorDomain})
	if err != nil {
		return status.Error(codes.Internal, "encoding error details")
	}
	return st.Err()
}

func podUID(node string) string {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("pod/"+node)).String()
}

// routerID derives a stable documentation-range router ID from the node name.
func routerID(node string) string {
	sum := uuid.NewSHA1(uuid.NameSpaceOID, []byte(node))
	return netip.AddrFrom4([4]byte{203, 0, 113, sum[0]}).String()
}
