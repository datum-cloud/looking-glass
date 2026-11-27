package diagnostics

import (
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	backendv1 "go.datum.net/looking-glass/api/lookingglass/backend/v1"
	"go.datum.net/looking-glass/api/v1alpha"
)

// ExecutionTimeout is how long a claimed session may run, measured from its
// claim. The backend request expires at StartedAt plus this.
const ExecutionTimeout = 90 * time.Second

var projectIDPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// Session is a session the gateway has claimed. The gateway fills it from
// the cell-local session object after the claim succeeds, never from anything
// the client sent.
type Session struct {
	// UID is the session's metadata.uid.
	UID string
	// ProjectID is the UID of the project that owns the session. The backend
	// attributes the request, and its project budgets, to it.
	ProjectID string
	// Cell is status.cell.
	Cell string
	// Location is spec.location.
	Location string
	// Diagnostic is spec.diagnostic.
	Diagnostic v1alpha.Diagnostic
	// StartedAt is status.startedAt, the time of the claim.
	StartedAt time.Time
}

// Deadline is when the session's execution ends.
func (s Session) Deadline() time.Time {
	return s.StartedAt.Add(ExecutionTimeout)
}

// request builds the one RunDiagnostic request for the session.
func (s Session) request(requestID string) (*backendv1.RunDiagnosticRequest, error) {
	switch {
	case s.UID == "":
		return nil, errors.New("session UID is empty")
	case !projectIDPattern.MatchString(s.ProjectID):
		return nil, errors.New("project ID must be 1 to 128 characters of [A-Za-z0-9._:-]")
	case s.Cell == "" || s.Location == "":
		return nil, errors.New("cell and location are required")
	case s.StartedAt.IsZero():
		return nil, errors.New("startedAt is required")
	}
	q, err := Query(s.Diagnostic)
	if err != nil {
		return nil, err
	}
	return &backendv1.RunDiagnosticRequest{
		ApiMajor:  backendv1.APIMajor,
		Cell:      s.Cell,
		Location:  s.Location,
		ProjectId: s.ProjectID,
		RequestId: requestID,
		ExpiresAt: timestamppb.New(s.Deadline()),
		Query:     q,
	}, nil
}

// Query converts a session's diagnostic to a backend query, with the address
// family made explicit and the target in canonical form.
func Query(d v1alpha.Diagnostic) (*backendv1.Query, error) {
	switch d.Type {
	case v1alpha.DiagnosticRouteLookup:
		if d.RouteLookup == nil {
			return nil, errors.New("routeLookup is not set")
		}
		target, is4, err := routeTarget(string(d.RouteLookup.Destination))
		if err != nil {
			return nil, err
		}
		if d.RouteLookup.AddressFamily != "" && d.RouteLookup.AddressFamily != familyName(is4) {
			return nil, errors.New("routeLookup.addressFamily does not match the destination")
		}
		return &backendv1.Query{
			Type:          backendv1.DiagnosticType_DIAGNOSTIC_TYPE_ROUTE_LOOKUP,
			AddressFamily: family(is4),
			Target:        target,
		}, nil
	case v1alpha.DiagnosticBGPSummary:
		if d.BGPSummary == nil {
			return nil, errors.New("bgpSummary is not set")
		}
		var is4 bool
		switch d.BGPSummary.AddressFamily {
		case v1alpha.AddressFamilyIPv4:
			is4 = true
		case v1alpha.AddressFamilyIPv6:
		default:
			return nil, fmt.Errorf("unknown address family %q", d.BGPSummary.AddressFamily)
		}
		return &backendv1.Query{
			Type:          backendv1.DiagnosticType_DIAGNOSTIC_TYPE_BGP_SUMMARY,
			AddressFamily: family(is4),
		}, nil
	case v1alpha.DiagnosticPing:
		if d.Ping == nil {
			return nil, errors.New("ping is not set")
		}
		return probeQuery(backendv1.DiagnosticType_DIAGNOSTIC_TYPE_PING, string(d.Ping.Destination))
	case v1alpha.DiagnosticTraceroute:
		if d.Traceroute == nil {
			return nil, errors.New("traceroute is not set")
		}
		return probeQuery(backendv1.DiagnosticType_DIAGNOSTIC_TYPE_TRACEROUTE, string(d.Traceroute.Destination))
	default:
		return nil, fmt.Errorf("unknown diagnostic type %q", d.Type)
	}
}

// routeTarget canonicalizes a route lookup destination: an address, or a
// prefix with no host bits.
func routeTarget(dest string) (target string, is4 bool, err error) {
	if p, err := netip.ParsePrefix(dest); err == nil {
		if p != p.Masked() || p.Addr().Is4In6() {
			return "", false, errors.New("a prefix destination must have no host bits")
		}
		return p.String(), p.Addr().Is4(), nil
	}
	addr, err := address(dest)
	if err != nil {
		return "", false, err
	}
	return addr.String(), addr.Is4(), nil
}

func probeQuery(t backendv1.DiagnosticType, dest string) (*backendv1.Query, error) {
	addr, err := address(dest)
	if err != nil {
		return nil, err
	}
	return &backendv1.Query{Type: t, AddressFamily: family(addr.Is4()), Target: addr.String()}, nil
}

func address(s string) (netip.Addr, error) {
	addr, err := netip.ParseAddr(s)
	if err != nil || addr.Zone() != "" || addr.Is4In6() {
		return netip.Addr{}, errors.New("destination must be a numeric address without a zone")
	}
	return addr, nil
}

func family(is4 bool) backendv1.AddressFamily {
	if is4 {
		return backendv1.AddressFamily_ADDRESS_FAMILY_IPV4
	}
	return backendv1.AddressFamily_ADDRESS_FAMILY_IPV6
}

func familyName(is4 bool) v1alpha.AddressFamily {
	if is4 {
		return v1alpha.AddressFamilyIPv4
	}
	return v1alpha.AddressFamilyIPv6
}
