// Package fakebackend is a fake of the backend API
// (lookingglass.backend.v1.DiagnosticService) for local development and
// tests. It validates requests the way the contract describes, then streams a
// snapshot, one synthetic outcome per selected router and a complete event.
// It never touches a network: every observation is built from
// documentation-range addresses and documentation ASNs.
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
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	backendv1 "go.datum.net/looking-glass/api/lookingglass/backend/v1"
)

const (
	// MaxRequestLifetime bounds expires_at, measured from receipt.
	MaxRequestLifetime = 120 * time.Second
	// MaxRouters bounds the routers one diagnostic runs on.
	MaxRouters = 8
	// PingProbes is the number of echo requests per ping, as the contract
	// fixes it.
	PingProbes = 3
)

// DeniedPrefixes stand in for the destination policy: ping and traceroute
// toward them are refused with DestinationNotAllowed. The real policy also
// denies the documentation ranges; the fake allows them so that tests and
// examples can use synthetic addresses.
var DeniedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("fc00::/7"),
}

// Diagnostics are the diagnostics the fake serves, each for both families.
var Diagnostics = []backendv1.DiagnosticType{
	backendv1.DiagnosticType_DIAGNOSTIC_TYPE_ROUTE_LOOKUP,
	backendv1.DiagnosticType_DIAGNOSTIC_TYPE_BGP_SUMMARY,
	backendv1.DiagnosticType_DIAGNOSTIC_TYPE_PING,
	backendv1.DiagnosticType_DIAGNOSTIC_TYPE_TRACEROUTE,
}

var projectIDPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// Options configures the fake.
type Options struct {
	// Cell is the cell the fake serves. Requests for another cell are refused.
	Cell string
	// Location is the one location the fake serves.
	Location string
	// Routers are the location's routers, in order.
	Routers []string
	// Unreachable names routers whose outcome is a RouterUnreachable failure.
	Unreachable []string
	// Ineligible names routers the snapshot lists but omits as Ineligible.
	Ineligible []string
	// OutcomeDelay is the pause before each router's outcome. A long delay
	// makes every router slow.
	OutcomeDelay time.Duration
	// Now returns the current time. Defaults to time.Now.
	Now func() time.Time
}

// DefaultOptions returns a three-router location with no failures.
func DefaultOptions() Options {
	return Options{
		Cell:         "cell-a",
		Location:     "example-location",
		Routers:      []string{"router-a", "router-b", "router-c"},
		OutcomeDelay: 250 * time.Millisecond,
	}
}

// Server implements backendv1.DiagnosticServiceServer.
type Server struct {
	backendv1.UnimplementedDiagnosticServiceServer
	opts Options
}

// New returns a fake, or an error when the options name no cell, location or
// routers, more than MaxRouters routers, a duplicate router, or an
// unreachable or ineligible router outside the location.
func New(opts Options) (*Server, error) {
	if opts.Cell == "" || opts.Location == "" {
		return nil, errors.New("a cell and a location are required")
	}
	if len(opts.Routers) == 0 || len(opts.Routers) > MaxRouters {
		return nil, fmt.Errorf("between 1 and %d routers are required", MaxRouters)
	}
	seen := map[string]bool{}
	for _, r := range opts.Routers {
		if r == "" || seen[r] {
			return nil, fmt.Errorf("router names must be unique and non-empty: %q", r)
		}
		seen[r] = true
	}
	for _, r := range slices.Concat(opts.Unreachable, opts.Ineligible) {
		if !seen[r] {
			return nil, fmt.Errorf("router %q is not at the location", r)
		}
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Server{opts: opts}, nil
}

// Register adds the fake to a gRPC server.
func (s *Server) Register(srv *grpc.Server) {
	backendv1.RegisterDiagnosticServiceServer(srv, s)
}

// GetCapabilities reports the fake's location and limits.
func (s *Server) GetCapabilities(_ context.Context, req *backendv1.GetCapabilitiesRequest) (*backendv1.GetCapabilitiesResponse, error) {
	if err := s.checkCaller(req.GetApiMajor(), req.GetCell()); err != nil {
		return nil, err
	}
	families := []backendv1.AddressFamily{backendv1.AddressFamily_ADDRESS_FAMILY_IPV4, backendv1.AddressFamily_ADDRESS_FAMILY_IPV6}
	ops := make([]*backendv1.Operation, 0, len(Diagnostics))
	for _, d := range Diagnostics {
		ops = append(ops, &backendv1.Operation{Type: d, AddressFamilies: families})
	}
	return &backendv1.GetCapabilitiesResponse{
		Cell:     s.opts.Cell,
		ApiMajor: backendv1.APIMajor,
		Enabled:  true,
		Locations: []*backendv1.LocationCapabilities{{
			Location:        s.opts.Location,
			EligibleRouters: uint32(len(s.opts.Routers) - len(s.opts.Ineligible)),
			Operations:      ops,
		}},
		Limits: &backendv1.Limits{
			MaxRequestLifetime:  durationpb.New(MaxRequestLifetime),
			MaxRouters:          MaxRouters,
			MaxInventoryRouters: MaxRouters,
			// Plausible values for a small cell.
			Budgets: &backendv1.Budgets{
				RequestsCell:           &backendv1.TokenRate{PerSecond: 2, Burst: 4},
				RequestsProject:        &backendv1.TokenRate{PerSecond: 0.2, Burst: 2},
				LiveDiagnosticsProject: 2,
				RouterConcurrency:      1,
				RouterQueueDepth:       4,
				RouterQueueWait:        durationpb.New(10 * time.Second),
			},
		},
		GeneratedAt: timestamppb.New(s.opts.Now()),
	}, nil
}

// RunDiagnostic validates the request and streams its events.
func (s *Server) RunDiagnostic(req *backendv1.RunDiagnosticRequest, stream grpc.ServerStreamingServer[backendv1.DiagnosticEvent]) error {
	if err := s.validate(req); err != nil {
		return err
	}
	ctx, cancel := context.WithDeadline(stream.Context(), req.GetExpiresAt().AsTime())
	defer cancel()

	r := &run{req: req, stream: stream}
	snap := &backendv1.Snapshot{
		SnapshotId: "snap-" + req.GetRequestId(),
		CapturedAt: timestamppb.New(s.opts.Now()),
	}
	complete := &backendv1.Complete{SnapshotId: snap.GetSnapshotId()}
	var ineligible bool
	for _, name := range s.opts.Routers {
		router := &backendv1.SnapshotRouter{Router: name, Selected: true}
		complete.Expected++
		if slices.Contains(s.opts.Ineligible, name) {
			router.Selected, router.OmittedReason = false, backendv1.OmittedIneligible
			complete.Omitted++
			ineligible = true
		}
		snap.Routers = append(snap.Routers, router)
	}
	if err := r.send(&backendv1.DiagnosticEvent{Event: &backendv1.DiagnosticEvent_Snapshot{Snapshot: snap}}); err != nil {
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
		if stopped == backendv1.ReasonCancelled {
			// The caller is gone; nothing more can be delivered.
			return status.Error(codes.Canceled, "caller cancelled the request")
		}
		outcome := &backendv1.RouterOutcome{Router: router.GetRouter()}
		switch {
		case stopped != "":
			outcome.Result = failure(stopped, "the request ended before the router ran")
		case slices.Contains(s.opts.Unreachable, router.GetRouter()):
			outcome.Result = failure(backendv1.ReasonRouterUnreachable, "router did not answer")
		default:
			outcome.Result = &backendv1.RouterOutcome_Observation{Observation: s.observe(req.GetQuery())}
		}
		outcome.CompletedAt = timestamppb.New(s.opts.Now())
		if outcome.GetFailure() != nil {
			complete.Failed++
		} else {
			complete.Successful++
		}
		if err := r.send(&backendv1.DiagnosticEvent{Event: &backendv1.DiagnosticEvent_Outcome{Outcome: outcome}}); err != nil {
			return err
		}
	}

	complete.FinishedAt = timestamppb.New(s.opts.Now())
	complete.Reason = terminalReason(stopped, ineligible, complete)
	return r.send(&backendv1.DiagnosticEvent{Event: &backendv1.DiagnosticEvent_Complete{Complete: complete}})
}

// checkCaller refuses a request for another API major or another cell.
func (s *Server) checkCaller(apiMajor uint32, cell string) error {
	if apiMajor != backendv1.APIMajor {
		return backendv1.Refuse(codes.FailedPrecondition, backendv1.ReasonVersionUnsupported, "unsupported api_major")
	}
	if cell != s.opts.Cell {
		return backendv1.Refuse(codes.PermissionDenied, backendv1.ReasonUnauthorized, "cell is not served here")
	}
	return nil
}

// validate applies the contract's request rules and canonicalizes the
// query's target.
func (s *Server) validate(req *backendv1.RunDiagnosticRequest) error {
	if err := s.checkCaller(req.GetApiMajor(), req.GetCell()); err != nil {
		return err
	}
	if req.GetLocation() != s.opts.Location {
		return backendv1.Refuse(codes.FailedPrecondition, backendv1.ReasonLocationNotServed, "location is not served here")
	}
	if !projectIDPattern.MatchString(req.GetProjectId()) {
		return invalid("project_id must be 1 to 128 characters of [A-Za-z0-9._:-]")
	}
	if id, err := uuid.Parse(req.GetRequestId()); err != nil || id.String() != req.GetRequestId() {
		return invalid("request_id must be a lowercase UUID")
	}
	if req.GetExpiresAt() == nil {
		return invalid("expires_at is required")
	}
	now := s.opts.Now()
	expires := req.GetExpiresAt().AsTime()
	if !expires.After(now) {
		return backendv1.Refuse(codes.DeadlineExceeded, backendv1.ReasonDeadlineExceeded, "expires_at has passed")
	}
	if expires.Sub(now) > MaxRequestLifetime {
		return invalid(fmt.Sprintf("expires_at must be at most %s away", MaxRequestLifetime))
	}
	if err := validateQuery(req.GetQuery()); err != nil {
		return err
	}
	canonicalizeTarget(req.GetQuery())
	return nil
}

func validateQuery(q *backendv1.Query) error {
	if q == nil || !slices.Contains(Diagnostics, q.GetType()) {
		return backendv1.Refuse(codes.FailedPrecondition, backendv1.ReasonQueryTypeUnavailable, "diagnostic is not offered")
	}
	var wantV4 bool
	switch q.GetAddressFamily() {
	case backendv1.AddressFamily_ADDRESS_FAMILY_IPV4:
		wantV4 = true
	case backendv1.AddressFamily_ADDRESS_FAMILY_IPV6:
	default:
		return invalid("address_family is required")
	}
	switch q.GetType() {
	case backendv1.DiagnosticType_DIAGNOSTIC_TYPE_BGP_SUMMARY:
		if q.GetTarget() != "" {
			return invalid("a BGP summary takes no target")
		}
		return nil
	case backendv1.DiagnosticType_DIAGNOSTIC_TYPE_ROUTE_LOOKUP:
		if p, err := netip.ParsePrefix(q.GetTarget()); err == nil {
			if p != p.Masked() || p.Addr().Is4In6() || p.Addr().Is4() != wantV4 {
				return invalid("a prefix target must have no host bits and match address_family")
			}
			return nil
		}
	}
	addr, err := netip.ParseAddr(q.GetTarget())
	if err != nil || addr.Zone() != "" || addr.Is4In6() || addr.Is4() != wantV4 {
		return invalid("target must be a numeric address of address_family, without a zone")
	}
	if q.GetType() == backendv1.DiagnosticType_DIAGNOSTIC_TYPE_ROUTE_LOOKUP {
		return nil
	}
	if !addr.IsGlobalUnicast() {
		return invalid("ping and traceroute targets must be unicast addresses")
	}
	for _, p := range DeniedPrefixes {
		if p.Contains(addr) {
			return backendv1.Refuse(codes.InvalidArgument, backendv1.ReasonDestinationNotAllowed, "destination is not allowed")
		}
	}
	return nil
}

// canonicalizeTarget rewrites a validated target to its canonical text form,
// so results echo one spelling of each address.
func canonicalizeTarget(q *backendv1.Query) {
	if p, err := netip.ParsePrefix(q.GetTarget()); err == nil {
		q.Target = p.String()
	} else if a, err := netip.ParseAddr(q.GetTarget()); err == nil {
		q.Target = a.String()
	}
}

// observe builds a synthetic, already redacted observation for q.
func (s *Server) observe(q *backendv1.Query) *backendv1.Observation {
	obs := &backendv1.Observation{
		Asn:        64496,
		SampleTime: timestamppb.New(s.opts.Now()),
		Duration:   durationpb.New(s.opts.OutcomeDelay),
	}
	v4 := q.GetAddressFamily() == backendv1.AddressFamily_ADDRESS_FAMILY_IPV4
	source, hop := "2001:db8:ffff::1", "2001:db8:ffff::2"
	if v4 {
		source, hop = "192.0.2.1", "192.0.2.2"
	}
	switch q.GetType() {
	case backendv1.DiagnosticType_DIAGNOSTIC_TYPE_ROUTE_LOOKUP:
		prefix := "2001:db8::/32"
		if v4 {
			prefix = "198.51.100.0/24"
		}
		localPref := uint32(100)
		obs.Result = &backendv1.Observation_Routes{Routes: &backendv1.RouteResult{
			LookupKind: backendv1.LookupKind_LOOKUP_KIND_LONGEST_MATCH,
			Prefixes: []*backendv1.PrefixRoutes{{
				Prefix:     prefix,
				TotalPaths: 1,
				Paths: []*backendv1.Path{{
					Protocol:   backendv1.RouteProtocol_ROUTE_PROTOCOL_BGP,
					Active:     true,
					Preference: 170,
					NextHops:   []*backendv1.NextHop{{Address: hop, Selected: true}},
					Bgp: &backendv1.BGPAttributes{
						AsPath: []*backendv1.ASPathSegment{{
							Type: backendv1.ASPathSegmentType_AS_PATH_SEGMENT_TYPE_SEQUENCE,
							Asns: []uint32{64497},
						}},
						Origin:    "IGP",
						OriginAsn: 64497,
						LocalPref: &localPref,
					},
				}},
			}},
		}}
	case backendv1.DiagnosticType_DIAGNOSTIC_TYPE_BGP_SUMMARY:
		obs.Result = &backendv1.Observation_BgpSummary{BgpSummary: &backendv1.BGPSummary{
			TotalPeers: 2, EstablishedPeers: 2,
			Peers: []*backendv1.BGPPeer{{
				RemoteAs: 64497, State: "Established", Established: true,
				Uptime: durationpb.New(time.Hour), PrefixesReceived: 1, PrefixesSent: 1,
			}},
		}}
	case backendv1.DiagnosticType_DIAGNOSTIC_TYPE_PING:
		replies := make([]*backendv1.ProbeReply, 0, PingProbes)
		for i := range PingProbes {
			replies = append(replies, &backendv1.ProbeReply{
				Sequence: uint32(i + 1), Address: q.GetTarget(),
				Rtt: durationpb.New(time.Duration(i+1) * time.Millisecond), IcmpType: "echo-reply",
			})
		}
		obs.Result = &backendv1.Observation_Ping{Ping: &backendv1.PingResult{
			Source: source, Destination: q.GetTarget(), Sent: PingProbes, Received: PingProbes,
			Replies: replies,
			RttMin:  durationpb.New(time.Millisecond), RttAvg: durationpb.New(2 * time.Millisecond),
			RttMax: durationpb.New(PingProbes * time.Millisecond),
		}}
	case backendv1.DiagnosticType_DIAGNOSTIC_TYPE_TRACEROUTE:
		obs.Result = &backendv1.Observation_Traceroute{Traceroute: &backendv1.TracerouteResult{
			Source: source, Destination: q.GetTarget(), Reached: true,
			Hops: []*backendv1.Hop{
				{Ttl: 1, Probes: []*backendv1.ProbeReply{{Sequence: 1, Address: hop, Rtt: durationpb.New(time.Millisecond), IcmpType: "time-exceeded"}}},
				{Ttl: 2, Probes: []*backendv1.ProbeReply{{Sequence: 2, Address: q.GetTarget(), Rtt: durationpb.New(2 * time.Millisecond), IcmpType: "echo-reply"}}},
			},
		}}
	}
	return obs
}

// wait pauses for d. It returns "" when the pause completed, ReasonCancelled
// when the caller went away and ReasonDeadlineExceeded when expires_at
// passed.
func wait(ctx, caller context.Context, d time.Duration) string {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return ""
	case <-ctx.Done():
		if caller.Err() != nil {
			return backendv1.ReasonCancelled
		}
		return backendv1.ReasonDeadlineExceeded
	}
}

// terminalReason follows the contract's precedence: a stop reason wins, then
// coverage decides. An ineligible router makes a result partial.
func terminalReason(stopped string, ineligible bool, c *backendv1.Complete) string {
	switch {
	case stopped == backendv1.ReasonDeadlineExceeded:
		return backendv1.CompleteDeadlineExceeded
	case c.GetSuccessful()+c.GetFailed() == 0:
		return backendv1.CompleteNoNodesAvailable
	case c.GetSuccessful() == 0:
		return backendv1.CompleteExecutionFailed
	case c.GetFailed() > 0 || c.GetTruncated() > 0 || ineligible:
		return backendv1.CompletePartialResults
	default:
		return backendv1.CompleteSucceeded
	}
}

type run struct {
	req    *backendv1.RunDiagnosticRequest
	stream grpc.ServerStreamingServer[backendv1.DiagnosticEvent]
	seq    uint64
}

func (r *run) send(ev *backendv1.DiagnosticEvent) error {
	r.seq++
	ev.RequestId = r.req.GetRequestId()
	ev.Sequence = r.seq
	return r.stream.Send(ev)
}

func failure(reason, message string) *backendv1.RouterOutcome_Failure {
	return &backendv1.RouterOutcome_Failure{Failure: &backendv1.Failure{Reason: reason, Message: message}}
}

func invalid(message string) error {
	return backendv1.Refuse(codes.InvalidArgument, backendv1.ReasonInvalidQuery, message)
}
