// Package routerbackend is the Looking Glass router backend: the
// lookingglass.backend.v1.DiagnosticService a cell's gateway calls. For each
// diagnostic it refuses what budgets or destination policy forbid before any
// router is queried, takes a snapshot of the location's routers, runs the
// diagnostic on the selected ones through their drivers, and streams each
// router's redacted outcome. docs/api/v1alpha-contract.md ("Backend mapping")
// defines the behavior.
package routerbackend

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"regexp"
	"slices"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/google/uuid"
	"golang.org/x/time/rate"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	backendv1 "go.datum.net/looking-glass/api/lookingglass/backend/v1"
	"go.datum.net/looking-glass/internal/admission"
	"go.datum.net/looking-glass/internal/config"
	"go.datum.net/looking-glass/pkg/driver"
)

// The probe parameters the contract fixes.
const (
	PingCount         = 3
	ProbeWait         = time.Second
	ProbePayloadBytes = 56
	TracerouteMaxHops = 20
	TracerouteProbes  = 3
)

// maxFailureMessage bounds a failure's message, in bytes.
const maxFailureMessage = 256

var projectIDPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// errRequestExpired is the cause of a diagnostic's context ending at
// expires_at, which tells a deadline apart from a cancellation.
var errRequestExpired = errors.New("expires_at passed")

// diagnostics maps the API's diagnostic types onto the driver's.
var diagnostics = map[backendv1.DiagnosticType]driver.Diagnostic{
	backendv1.DiagnosticType_DIAGNOSTIC_TYPE_ROUTE_LOOKUP: driver.DiagnosticRouteLookup,
	backendv1.DiagnosticType_DIAGNOSTIC_TYPE_BGP_SUMMARY:  driver.DiagnosticBGPSummary,
	backendv1.DiagnosticType_DIAGNOSTIC_TYPE_PING:         driver.DiagnosticPing,
	backendv1.DiagnosticType_DIAGNOSTIC_TYPE_TRACEROUTE:   driver.DiagnosticTraceroute,
}

var families = map[backendv1.AddressFamily]driver.Family{
	backendv1.AddressFamily_ADDRESS_FAMILY_IPV4: driver.FamilyIPv4,
	backendv1.AddressFamily_ADDRESS_FAMILY_IPV6: driver.FamilyIPv6,
}

// Options configures a Server beyond its configuration file.
type Options struct {
	// NewDriver returns the driver for one router. Defaults to driver.New.
	NewDriver func(driver.Router) (driver.Driver, error)
	// Now returns the current time. Defaults to time.Now.
	Now func() time.Time
	// Log receives one line per diagnostic and per router failure.
	Log logr.Logger
}

// Server implements backendv1.DiagnosticServiceServer.
type Server struct {
	backendv1.UnimplementedDiagnosticServiceServer

	cfg       *config.Backend
	policy    admission.DestinationPolicy
	redact    *redactor
	locations map[string]*location
	budgets   *requestBudgets
	seen      *requestIDs
	now       func() time.Time
	log       logr.Logger
}

type location struct {
	name    string
	routers []*router
}

type router struct {
	name  string
	asn   uint32
	drv   driver.Driver
	slots *routerSlots
}

// New builds a driver for every router in cfg and returns a Server. It fails
// when cfg is invalid or a router's driver rejects it, so the backend never
// serves a location with a router it cannot query.
func New(cfg *config.Backend, opts Options) (*Server, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if opts.NewDriver == nil {
		opts.NewDriver = driver.New
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	deny, err := admission.ParsePrefixes(cfg.DestinationPolicy.Deny)
	if err != nil {
		return nil, err
	}
	allow, err := admission.ParsePrefixes(cfg.DestinationPolicy.Allow)
	if err != nil {
		return nil, err
	}
	b := cfg.Budgets
	s := &Server{
		cfg:    cfg,
		policy: admission.DestinationPolicy{Deny: deny, Allow: allow},
		redact: &redactor{platform: deny, communities: cfg.Redaction.Communities, largeCommunities: cfg.Redaction.LargeCommunities},
		budgets: &requestBudgets{
			cell:         rate.NewLimiter(rate.Limit(b.RequestsCell.PerSecond), b.RequestsCell.Burst),
			projectRate:  rate.Limit(b.RequestsProject.PerSecond),
			projectBurst: b.RequestsProject.Burst,
			liveMax:      b.LiveDiagnosticsProject,
			now:          opts.Now,
			projects:     map[string]*projectBudget{},
		},
		seen:      &requestIDs{ids: map[string]time.Time{}},
		locations: map[string]*location{},
		now:       opts.Now,
		log:       opts.Log,
	}
	for _, lc := range cfg.Locations {
		loc := &location{name: lc.Name}
		for _, rc := range lc.Routers {
			d, err := opts.NewDriver(driver.Router{Name: rc.Name, Platform: rc.Platform, Address: rc.Address, Settings: rc.Settings})
			if err != nil {
				s.Close()
				return nil, fmt.Errorf("location %q: %w", lc.Name, err)
			}
			loc.routers = append(loc.routers, &router{
				name: rc.Name, asn: rc.ASN, drv: d,
				slots: newRouterSlots(b.RouterConcurrency, b.RouterQueueDepth, b.RouterQueueWait.Duration),
			})
		}
		s.locations[lc.Name] = loc
	}
	return s, nil
}

// Register adds the server to a gRPC server.
func (s *Server) Register(srv *grpc.Server) {
	backendv1.RegisterDiagnosticServiceServer(srv, s)
}

// Close closes every router's driver.
func (s *Server) Close() {
	for _, loc := range s.locations {
		for _, r := range loc.routers {
			_ = r.drv.Close()
		}
	}
}

// GetCapabilities reports each location's routers and the diagnostics every
// one of them serves, with the configured limits.
func (s *Server) GetCapabilities(_ context.Context, req *backendv1.GetCapabilitiesRequest) (*backendv1.GetCapabilitiesResponse, error) {
	if err := s.checkCaller(req.GetApiMajor(), req.GetCell()); err != nil {
		return nil, err
	}
	l, b := s.cfg.Limits, s.cfg.Budgets
	resp := &backendv1.GetCapabilitiesResponse{
		Cell:     s.cfg.Cell,
		ApiMajor: backendv1.APIMajor,
		Enabled:  !s.cfg.Disabled,
		Limits: &backendv1.Limits{
			MaxRequestLifetime:  durationpb.New(l.MaxRequestLifetime.Duration),
			MaxRouters:          uint32(l.MaxRouters),
			MaxInventoryRouters: uint32(l.MaxInventoryRouters),
			Budgets: &backendv1.Budgets{
				RequestsCell:           &backendv1.TokenRate{PerSecond: b.RequestsCell.PerSecond, Burst: float64(b.RequestsCell.Burst)},
				RequestsProject:        &backendv1.TokenRate{PerSecond: b.RequestsProject.PerSecond, Burst: float64(b.RequestsProject.Burst)},
				LiveDiagnosticsProject: uint32(b.LiveDiagnosticsProject),
				RouterConcurrency:      uint32(b.RouterConcurrency),
				RouterQueueDepth:       uint32(b.RouterQueueDepth),
				RouterQueueWait:        durationpb.New(b.RouterQueueWait.Duration),
			},
		},
		GeneratedAt: timestamppb.New(s.now()),
	}
	for _, lc := range s.cfg.Locations {
		loc := s.locations[lc.Name]
		lcaps := &backendv1.LocationCapabilities{Location: loc.name, EligibleRouters: uint32(len(loc.routers))}
		for _, t := range slices.Sorted(maps.Keys(diagnostics)) {
			op := &backendv1.Operation{Type: t}
			for _, f := range []backendv1.AddressFamily{backendv1.AddressFamily_ADDRESS_FAMILY_IPV4, backendv1.AddressFamily_ADDRESS_FAMILY_IPV6} {
				every := !slices.ContainsFunc(loc.routers, func(r *router) bool {
					return !r.drv.Capabilities().Serves(diagnostics[t], families[f])
				})
				if every {
					op.AddressFamilies = append(op.AddressFamilies, f)
				}
			}
			if len(op.AddressFamilies) > 0 {
				lcaps.Operations = append(lcaps.Operations, op)
			}
		}
		resp.Locations = append(resp.Locations, lcaps)
	}
	return resp, nil
}

// RunDiagnostic refuses the request or runs it and streams its events.
func (s *Server) RunDiagnostic(req *backendv1.RunDiagnosticRequest, stream grpc.ServerStreamingServer[backendv1.DiagnosticEvent]) error {
	diag := diagnostics[req.GetQuery().GetType()]
	q, loc, err := s.admit(req)
	if err != nil {
		reason, _ := backendv1.RefusalReason(err)
		requestsTotal.WithLabelValues(string(diag), reason).Inc()
		s.log.V(1).Info("refused diagnostic", "requestID", req.GetRequestId(), "reason", reason)
		return err
	}
	defer q.release()
	liveDiagnostics.Inc()
	defer liveDiagnostics.Dec()

	ctx, cancel := context.WithDeadlineCause(stream.Context(), req.GetExpiresAt().AsTime(), errRequestExpired)
	defer cancel()
	r := &run{s: s, req: req, q: q, loc: loc, stream: stream}
	reason, err := r.execute(ctx)
	if reason != "" {
		requestsTotal.WithLabelValues(string(diag), reason).Inc()
	}
	return err
}

// query is an admitted, parsed diagnostic.
type query struct {
	diag    driver.Diagnostic
	family  driver.Family
	addr    netip.Addr
	prefix  netip.Prefix
	release func()
}

// admit applies the contract's request rules, the destination policy and the
// budgets, in that order. It touches no router.
func (s *Server) admit(req *backendv1.RunDiagnosticRequest) (*query, *location, error) {
	if err := s.checkCaller(req.GetApiMajor(), req.GetCell()); err != nil {
		return nil, nil, err
	}
	if s.cfg.Disabled {
		return nil, nil, backendv1.Refuse(codes.Unavailable, backendv1.ReasonServiceDisabled, "the backend is disabled")
	}
	loc, ok := s.locations[req.GetLocation()]
	if !ok {
		return nil, nil, backendv1.Refuse(codes.FailedPrecondition, backendv1.ReasonLocationNotServed, "location is not served here")
	}
	if !projectIDPattern.MatchString(req.GetProjectId()) {
		return nil, nil, invalid("project_id must be 1 to 128 characters of [A-Za-z0-9._:-]")
	}
	if id, err := uuid.Parse(req.GetRequestId()); err != nil || id.String() != req.GetRequestId() {
		return nil, nil, invalid("request_id must be a lowercase UUID")
	}
	if req.GetExpiresAt() == nil {
		return nil, nil, invalid("expires_at is required")
	}
	now := s.now()
	expires := req.GetExpiresAt().AsTime()
	if !expires.After(now) {
		return nil, nil, backendv1.Refuse(codes.DeadlineExceeded, backendv1.ReasonDeadlineExceeded, "expires_at has passed")
	}
	if lifetime := s.cfg.Limits.MaxRequestLifetime.Duration; expires.Sub(now) > lifetime {
		return nil, nil, invalid(fmt.Sprintf("expires_at must be at most %s away", lifetime))
	}
	q, err := s.parseQuery(req.GetQuery())
	if err != nil {
		return nil, nil, err
	}
	if len(loc.routers) > s.cfg.Limits.MaxInventoryRouters {
		return nil, nil, backendv1.Refuse(codes.FailedPrecondition, backendv1.ReasonInventoryTooLarge, "the location has more routers than the backend allows")
	}
	duplicate := backendv1.Refuse(codes.AlreadyExists, backendv1.ReasonDuplicateRequest, "request_id was already used")
	if s.seen.has(req.GetRequestId(), now) {
		return nil, nil, duplicate
	}
	release, refused := s.budgets.admit(req.GetProjectId())
	if refused != nil {
		return nil, nil, backendv1.RefuseAfter(codes.ResourceExhausted, backendv1.ReasonRateLimited,
			fmt.Sprintf("over the %s budget", refused.scope), refused.retryAfter)
	}
	// The ID is recorded only once the request is admitted, so a throttled
	// request's ID stays usable for a later attempt.
	if !s.seen.add(req.GetRequestId(), expires, now) {
		release()
		return nil, nil, duplicate
	}
	q.release = release
	return q, loc, nil
}

// parseQuery checks the query's shape and target, then the destination
// policy.
func (s *Server) parseQuery(q *backendv1.Query) (*query, error) {
	diag, ok := diagnostics[q.GetType()]
	if q == nil || !ok {
		return nil, backendv1.Refuse(codes.FailedPrecondition, backendv1.ReasonQueryTypeUnavailable, "diagnostic is not offered")
	}
	family, ok := families[q.GetAddressFamily()]
	if !ok {
		return nil, invalid("address_family is required")
	}
	out := &query{diag: diag, family: family}
	wantV4 := family == driver.FamilyIPv4
	switch diag {
	case driver.DiagnosticBGPSummary:
		if q.GetTarget() != "" {
			return nil, invalid("a BGP summary takes no target")
		}
		return out, nil
	case driver.DiagnosticRouteLookup:
		p, err := netip.ParsePrefix(q.GetTarget())
		if err != nil {
			a, aerr := netip.ParseAddr(q.GetTarget())
			if aerr != nil || a.Zone() != "" {
				return nil, invalid("target must be an address or a prefix of address_family")
			}
			p = netip.PrefixFrom(a, a.BitLen())
		}
		if p != p.Masked() || p.Addr().Is4In6() || p.Addr().Is4() != wantV4 {
			return nil, invalid("a prefix target must have no host bits and match address_family")
		}
		if err := s.policy.CheckRouteTarget(p); err != nil {
			return nil, backendv1.Refuse(codes.InvalidArgument, backendv1.ReasonDestinationNotAllowed, "route lookups may not target platform ranges")
		}
		out.prefix = p
		return out, nil
	}
	a, err := netip.ParseAddr(q.GetTarget())
	if err != nil || a.Zone() != "" || a.Is4In6() || a.Is4() != wantV4 {
		return nil, invalid("target must be a numeric address of address_family, without a zone")
	}
	if err := s.policy.Check(a); err != nil {
		return nil, backendv1.Refuse(codes.InvalidArgument, backendv1.ReasonDestinationNotAllowed, "destination is not allowed")
	}
	out.addr = a
	return out, nil
}

// checkCaller refuses a request for another API major or another cell.
func (s *Server) checkCaller(apiMajor uint32, cell string) error {
	if apiMajor != backendv1.APIMajor {
		return backendv1.Refuse(codes.FailedPrecondition, backendv1.ReasonVersionUnsupported, "unsupported api_major")
	}
	if cell != s.cfg.Cell {
		return backendv1.Refuse(codes.PermissionDenied, backendv1.ReasonUnauthorized, "cell is not served here")
	}
	return nil
}

func invalid(message string) error {
	return backendv1.Refuse(codes.InvalidArgument, backendv1.ReasonInvalidQuery, message)
}

// requestIDs remembers request IDs until their expires_at, so a repeated ID
// is refused as DuplicateRequest.
type requestIDs struct {
	mu  sync.Mutex
	ids map[string]time.Time
}

// has reports whether id is recorded and its expires_at has not passed.
func (r *requestIDs) has(id string, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	exp, ok := r.ids[id]
	return ok && exp.After(now)
}

// add records id until expires and reports whether it was new.
func (r *requestIDs) add(id string, expires, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, exp := range r.ids {
		if !exp.After(now) {
			delete(r.ids, k)
		}
	}
	if _, ok := r.ids[id]; ok {
		return false
	}
	r.ids[id] = expires
	return true
}
