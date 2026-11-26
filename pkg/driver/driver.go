// Package driver defines how the Looking Glass backend queries one router,
// and a registry that maps router platforms to the drivers that serve them.
//
// A driver runs one diagnostic on one router and returns what the router
// reported, unredacted. The backend converts every result into the API's
// observation types and applies the redaction rules; a driver never decides
// what a customer may see. docs/backend-drivers.md describes how drivers are
// packaged and selected.
package driver

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// Router describes one router and how its driver reaches it.
type Router struct {
	// Name is the router's inventory name, unique within its location.
	Name string
	// Platform selects the registered driver.
	Platform string
	// Address is where the driver connects to the router.
	Address string
	// Settings holds platform-specific options from the backend's
	// configuration. The registered Factory validates them.
	Settings map[string]string
}

// Driver runs diagnostics on one router. A driver must be safe for concurrent
// use: the backend may run several diagnostics on one router at once, up to
// its configured router concurrency.
//
// Every method honors ctx: it returns promptly once ctx is done, with an error
// whose Reason is Timeout or Cancelled. A failure returns an *Error; any other
// error is reported as ReasonInternal.
type Driver interface {
	// Capabilities reports the diagnostics and address families this router
	// serves. It performs no I/O.
	Capabilities() Capabilities
	// RouteLookup reads the router's global routing table.
	RouteLookup(ctx context.Context, req RouteLookupRequest) (*RouteResult, error)
	// BGPSummary lists the router's BGP sessions for one address family.
	BGPSummary(ctx context.Context, req BGPSummaryRequest) (*BGPSummary, error)
	// Ping sends ICMP echo requests from the router.
	Ping(ctx context.Context, req PingRequest) (*PingResult, error)
	// Traceroute traces the path from the router.
	Traceroute(ctx context.Context, req TracerouteRequest) (*TracerouteResult, error)
	// Close releases the driver's connections. It is safe to call more than
	// once.
	Close() error
}

// Factory validates r and returns a Driver for it. It must perform no network
// I/O, so the backend can reject a bad inventory before contacting any
// router.
type Factory func(r Router) (Driver, error)

// Diagnostic names a diagnostic.
type Diagnostic string

// The diagnostics a driver may serve.
const (
	DiagnosticRouteLookup Diagnostic = "RouteLookup"
	DiagnosticBGPSummary  Diagnostic = "BGPSummary"
	DiagnosticPing        Diagnostic = "Ping"
	DiagnosticTraceroute  Diagnostic = "Traceroute"
)

// Family is an IP address family.
type Family string

// The address families.
const (
	FamilyIPv4 Family = "IPv4"
	FamilyIPv6 Family = "IPv6"
)

// Capabilities lists what a router serves.
type Capabilities struct {
	Diagnostics []DiagnosticCapability
}

// Serves reports whether the capabilities include d for f.
func (c Capabilities) Serves(d Diagnostic, f Family) bool {
	for _, dc := range c.Diagnostics {
		if dc.Type == d && slices.Contains(dc.Families, f) {
			return true
		}
	}
	return false
}

// DiagnosticCapability is one diagnostic and the families it serves.
type DiagnosticCapability struct {
	Type     Diagnostic
	Families []Family
}

// RouteLookupRequest looks up one target. A target with a full-length prefix
// is an address and gets the longest match; any other target gets an exact
// match.
type RouteLookupRequest struct {
	Target netip.Prefix
	// MaxPaths bounds the paths returned per prefix. RouteResult still counts
	// every path in TotalPaths.
	MaxPaths int
}

// BGPSummaryRequest selects the sessions of one address family.
type BGPSummaryRequest struct {
	Family Family
}

// PingRequest sends Count echo requests to Destination.
type PingRequest struct {
	Destination netip.Addr
	Count       int
	// Wait bounds the wait for each reply.
	Wait time.Duration
	// PayloadBytes is the echo payload size.
	PayloadBytes int
}

// TracerouteRequest traces the path to Destination.
type TracerouteRequest struct {
	Destination netip.Addr
	MaxHops     int
	// ProbesPerHop is an upper bound; a platform may send fewer.
	ProbesPerHop int
	// Wait bounds the wait for each probe's reply.
	Wait time.Duration
	// PayloadBytes is the probe payload size.
	PayloadBytes int
}

// RouteResult is a route lookup's result. An empty result is a successful
// lookup that matched nothing.
type RouteResult struct {
	// Exact reports an exact-match lookup.
	Exact    bool
	Prefixes []PrefixRoutes
	// Truncated reports that the router's output was incomplete.
	Truncated bool
}

// PrefixRoutes is one matched prefix and its paths.
type PrefixRoutes struct {
	Prefix     netip.Prefix
	TotalPaths int
	Paths      []Path
}

// Protocol is the protocol a path came from.
type Protocol string

// The route protocols.
const (
	ProtocolBGP       Protocol = "BGP"
	ProtocolStatic    Protocol = "Static"
	ProtocolConnected Protocol = "Connected"
	ProtocolLocal     Protocol = "Local"
	ProtocolAggregate Protocol = "Aggregate"
	ProtocolIGP       Protocol = "IGP"
	ProtocolOther     Protocol = "Other"
)

// Path is one path to a prefix.
type Path struct {
	Protocol Protocol
	// Active reports that the router selected this path for forwarding.
	Active bool
	// Preference orders protocols; lower wins.
	Preference uint32
	// Metric is nil when the protocol has none.
	Metric   *uint32
	NextHops []NextHop
	// BGP is set for BGP paths only.
	BGP *BGPAttributes
}

// NextHop is one next hop as the router reports it.
type NextHop struct {
	Address netip.Addr
	// Interface is the outgoing interface name, when the router reports one.
	Interface string
	// Selected reports that forwarding uses this next hop.
	Selected bool
}

// ASPathSegmentType is the type of an AS path segment.
type ASPathSegmentType string

// The AS path segment types.
const (
	ASPathSequence       ASPathSegmentType = "Sequence"
	ASPathSet            ASPathSegmentType = "Set"
	ASPathConfedSequence ASPathSegmentType = "ConfedSequence"
	ASPathConfedSet      ASPathSegmentType = "ConfedSet"
)

// ASPathSegment is one AS path segment.
type ASPathSegment struct {
	Type ASPathSegmentType
	ASNs []uint32
}

// BGPAttributes are a BGP path's attributes, with every community.
type BGPAttributes struct {
	ASPath []ASPathSegment
	// Origin is IGP, EGP or Incomplete.
	Origin           string
	OriginASN        uint32
	LocalPref        *uint32
	MED              *uint32
	Multipath        bool
	Communities      []string
	LargeCommunities []string
}

// BGPSummary is one address family's BGP sessions.
type BGPSummary struct {
	RouterID netip.Addr
	LocalAS  uint32
	Peers    []BGPPeer
}

// BGPPeer is one BGP session.
type BGPPeer struct {
	Address netip.Addr
	// Internal reports a session within the platform's own network, as
	// opposed to one with another network.
	Internal         bool
	RemoteAS         uint32
	Description      string
	State            string
	Established      bool
	Uptime           time.Duration
	PrefixesReceived uint64
	PrefixesSent     uint64
}

// ProbeReply is one ping reply or traceroute probe. A probe that got no reply
// has Timeout set and no Address.
type ProbeReply struct {
	Sequence uint32
	Address  netip.Addr
	RTT      time.Duration
	Timeout  bool
	ICMPType string
	ICMPCode uint32
}

// PingResult is a ping's result. Lost replies are part of the result, not a
// failure.
type PingResult struct {
	Source      netip.Addr
	Destination netip.Addr
	Sent        int
	Replies     []ProbeReply
}

// Hop is one traceroute hop.
type Hop struct {
	TTL    int
	Probes []ProbeReply
}

// TracerouteResult is a traceroute's result. Silent hops are part of the
// result, not a failure.
type TracerouteResult struct {
	Source      netip.Addr
	Destination netip.Addr
	Reached     bool
	Hops        []Hop
}

var (
	mu        sync.RWMutex
	factories = map[string]Factory{}
)

// Register makes a driver available for a router platform. Drivers call it
// from their package's init function. It panics on an empty or duplicate
// platform, which is a programming error.
func Register(platform string, f Factory) {
	mu.Lock()
	defer mu.Unlock()
	if platform == "" || f == nil {
		panic("driver: Register needs a platform and a factory")
	}
	if _, ok := factories[platform]; ok {
		panic(fmt.Sprintf("driver: platform %q registered twice", platform))
	}
	factories[platform] = f
}

// Platforms returns the registered platforms, sorted.
func Platforms() []string {
	mu.RLock()
	defer mu.RUnlock()
	names := make([]string, 0, len(factories))
	for n := range factories {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// New returns a Driver for r from the factory registered for its platform.
func New(r Router) (Driver, error) {
	mu.RLock()
	f, ok := factories[r.Platform]
	mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("router %q: no driver for platform %q (registered: %s)",
			r.Name, r.Platform, strings.Join(Platforms(), ", "))
	}
	d, err := f(r)
	if err != nil {
		return nil, fmt.Errorf("router %q: %w", r.Name, err)
	}
	return d, nil
}
