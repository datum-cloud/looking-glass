package routerbackend

import (
	"net/netip"
	"slices"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"

	backendv1 "go.datum.net/looking-glass/api/lookingglass/backend/v1"
	"go.datum.net/looking-glass/pkg/driver"
)

// Output bounds the contract fixes.
const (
	// MaxPathsPerPrefix bounds the paths a route lookup returns per prefix.
	MaxPathsPerPrefix = 8
	// MaxCommunities bounds the communities, and separately the large
	// communities, returned per path.
	MaxCommunities = 64
)

// nonPublic are address ranges that are never a public next hop, on top of
// the platform deny list: shared address space (RFC 6598). Private, loopback,
// link-local and unspecified addresses are checked with netip's predicates.
var nonPublic = []netip.Prefix{netip.MustParsePrefix("100.64.0.0/10")}

// redactor converts driver results into the contract's observation types,
// keeping only what a customer may see.
type redactor struct {
	// platform are the platform's infrastructure ranges.
	platform         []netip.Prefix
	communities      []string
	largeCommunities []string
}

// internal reports whether a next hop is inside the platform's
// infrastructure: a platform range, or an address that is not public.
func (r *redactor) internal(a netip.Addr) bool {
	a = a.Unmap()
	if a.IsPrivate() || a.IsLoopback() || a.IsLinkLocalUnicast() || a.IsUnspecified() {
		return true
	}
	contains := func(p netip.Prefix) bool { return p.Contains(a) }
	return slices.ContainsFunc(r.platform, contains) || slices.ContainsFunc(nonPublic, contains)
}

// routes converts a route lookup. It reports whether the payload is
// incomplete. routerASN stands in for the origin AS of a path the router's
// own AS originated, which has an empty AS path.
func (r *redactor) routes(res *driver.RouteResult, routerASN uint32) (*backendv1.RouteResult, bool) {
	out := &backendv1.RouteResult{LookupKind: backendv1.LookupKind_LOOKUP_KIND_LONGEST_MATCH}
	if res.Exact {
		out.LookupKind = backendv1.LookupKind_LOOKUP_KIND_EXACT
	}
	truncated := res.Truncated
	for _, pr := range res.Prefixes {
		paths := pr.Paths
		if len(paths) > MaxPathsPerPrefix {
			paths = paths[:MaxPathsPerPrefix]
		}
		total := max(pr.TotalPaths, len(pr.Paths))
		truncated = truncated || total > len(paths)
		p := &backendv1.PrefixRoutes{Prefix: pr.Prefix.String(), TotalPaths: uint32(total)}
		for _, path := range paths {
			converted, cut := r.path(path, routerASN)
			truncated = truncated || cut
			p.Paths = append(p.Paths, converted)
		}
		out.Prefixes = append(out.Prefixes, p)
	}
	return out, truncated
}

var protocols = map[driver.Protocol]backendv1.RouteProtocol{
	driver.ProtocolBGP:       backendv1.RouteProtocol_ROUTE_PROTOCOL_BGP,
	driver.ProtocolStatic:    backendv1.RouteProtocol_ROUTE_PROTOCOL_STATIC,
	driver.ProtocolConnected: backendv1.RouteProtocol_ROUTE_PROTOCOL_CONNECTED,
	driver.ProtocolLocal:     backendv1.RouteProtocol_ROUTE_PROTOCOL_LOCAL,
	driver.ProtocolAggregate: backendv1.RouteProtocol_ROUTE_PROTOCOL_AGGREGATE,
	driver.ProtocolIGP:       backendv1.RouteProtocol_ROUTE_PROTOCOL_IGP,
}

var segmentTypes = map[driver.ASPathSegmentType]backendv1.ASPathSegmentType{
	driver.ASPathSequence:       backendv1.ASPathSegmentType_AS_PATH_SEGMENT_TYPE_SEQUENCE,
	driver.ASPathSet:            backendv1.ASPathSegmentType_AS_PATH_SEGMENT_TYPE_SET,
	driver.ASPathConfedSequence: backendv1.ASPathSegmentType_AS_PATH_SEGMENT_TYPE_CONFED_SEQUENCE,
	driver.ASPathConfedSet:      backendv1.ASPathSegmentType_AS_PATH_SEGMENT_TYPE_CONFED_SET,
}

func (r *redactor) path(p driver.Path, routerASN uint32) (*backendv1.Path, bool) {
	proto, ok := protocols[p.Protocol]
	if !ok {
		proto = backendv1.RouteProtocol_ROUTE_PROTOCOL_OTHER
	}
	out := &backendv1.Path{Protocol: proto, Active: p.Active, Preference: p.Preference, Metric: p.Metric}
	for _, nh := range p.NextHops {
		hop := &backendv1.NextHop{Selected: nh.Selected}
		switch {
		case !nh.Address.IsValid():
			// A directly connected or discard route has no next hop address.
		case r.internal(nh.Address):
			hop.Internal = true
		default:
			hop.Address = nh.Address.Unmap().String()
		}
		out.NextHops = append(out.NextHops, hop)
	}
	if p.BGP == nil || p.Protocol != driver.ProtocolBGP {
		return out, false
	}
	b := p.BGP
	attrs := &backendv1.BGPAttributes{
		Origin:                b.Origin,
		OriginAsn:             b.OriginASN,
		LocalPref:             b.LocalPref,
		Med:                   b.MED,
		Multipath:             b.Multipath,
		TotalCommunities:      uint32(len(b.Communities)),
		TotalLargeCommunities: uint32(len(b.LargeCommunities)),
	}
	if attrs.OriginAsn == 0 && len(b.ASPath) == 0 {
		attrs.OriginAsn = routerASN
	}
	for _, s := range b.ASPath {
		t, ok := segmentTypes[s.Type]
		if !ok {
			t = backendv1.ASPathSegmentType_AS_PATH_SEGMENT_TYPE_UNSPECIFIED
		}
		attrs.AsPath = append(attrs.AsPath, &backendv1.ASPathSegment{Type: t, Asns: s.ASNs})
	}
	var cut, cutLarge bool
	attrs.Communities, cut = allowed(b.Communities, r.communities)
	attrs.LargeCommunities, cutLarge = allowed(b.LargeCommunities, r.largeCommunities)
	out.Bgp = attrs
	return out, cut || cutLarge
}

// allowed returns the communities that match an allowlist entry, at most
// MaxCommunities of them, and whether the bound cut any.
func allowed(communities, allowlist []string) ([]string, bool) {
	var out []string
	for _, c := range communities {
		for _, pattern := range allowlist {
			if communityMatches(c, pattern) {
				out = append(out, c)
				break
			}
		}
	}
	if len(out) > MaxCommunities {
		return out[:MaxCommunities], true
	}
	return out, false
}

// communityMatches compares a community with an allowlist entry field by
// field; "*" matches any field.
func communityMatches(community, pattern string) bool {
	c, p := strings.Split(community, ":"), strings.Split(pattern, ":")
	if len(c) != len(p) {
		return false
	}
	for i := range c {
		if p[i] != "*" && p[i] != c[i] {
			return false
		}
	}
	return true
}

// bgpSummary counts every session and lists only the external ones, without
// their addresses or descriptions.
func bgpSummary(s *driver.BGPSummary) *backendv1.BGPSummary {
	out := &backendv1.BGPSummary{TotalPeers: uint32(len(s.Peers))}
	for _, p := range s.Peers {
		if p.Established {
			out.EstablishedPeers++
		}
		if p.Internal {
			continue
		}
		out.Peers = append(out.Peers, &backendv1.BGPPeer{
			RemoteAs:         p.RemoteAS,
			State:            p.State,
			Established:      p.Established,
			Uptime:           durationpb.New(p.Uptime),
			PrefixesReceived: p.PrefixesReceived,
			PrefixesSent:     p.PrefixesSent,
		})
	}
	return out
}

func probe(p driver.ProbeReply) *backendv1.ProbeReply {
	out := &backendv1.ProbeReply{Sequence: p.Sequence, Timeout: p.Timeout}
	if p.Timeout {
		return out
	}
	if p.Address.IsValid() {
		out.Address = p.Address.Unmap().String()
	}
	out.Rtt = durationpb.New(p.RTT)
	out.IcmpType, out.IcmpCode = p.ICMPType, p.ICMPCode
	return out
}

func addrString(a netip.Addr) string {
	if !a.IsValid() {
		return ""
	}
	return a.Unmap().String()
}

// ping converts a ping and computes its statistics from the echo replies.
func ping(p *driver.PingResult) *backendv1.PingResult {
	out := &backendv1.PingResult{Source: addrString(p.Source), Destination: addrString(p.Destination), Sent: uint32(p.Sent)}
	var lo, hi, sum time.Duration
	for _, r := range p.Replies {
		out.Replies = append(out.Replies, probe(r))
		if r.Timeout || r.ICMPType != "echo-reply" {
			continue
		}
		if out.Received == 0 || r.RTT < lo {
			lo = r.RTT
		}
		hi = max(hi, r.RTT)
		sum += r.RTT
		out.Received++
	}
	if out.Received > 0 {
		out.RttMin = durationpb.New(lo)
		out.RttMax = durationpb.New(hi)
		out.RttAvg = durationpb.New(sum / time.Duration(out.Received))
	}
	return out
}

func traceroute(t *driver.TracerouteResult) *backendv1.TracerouteResult {
	out := &backendv1.TracerouteResult{Source: addrString(t.Source), Destination: addrString(t.Destination), Reached: t.Reached}
	for _, h := range t.Hops {
		hop := &backendv1.Hop{Ttl: uint32(h.TTL)}
		for _, p := range h.Probes {
			hop.Probes = append(hop.Probes, probe(p))
		}
		out.Hops = append(out.Hops, hop)
	}
	return out
}
