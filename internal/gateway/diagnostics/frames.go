package diagnostics

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	backendv1 "go.datum.net/looking-glass/api/lookingglass/backend/v1"
	"go.datum.net/looking-glass/api/v1alpha"
)

// Frame types, in the order a stream sends them.
const (
	FrameHello    = "hello"
	FrameSnapshot = "snapshot"
	FrameOutcome  = "outcome"
	FrameEnd      = "end"
)

// RouterID is the opaque identifier a client sees for a router: "r-" and the
// first 16 hex characters of SHA-256("<location>/<router name>").
func RouterID(location, router string) string {
	sum := sha256.Sum256([]byte(location + "/" + router))
	return "r-" + hex.EncodeToString(sum[:])[:16]
}

// Duration encodes as a protobuf-JSON duration string, such as "0.012s".
type Duration time.Duration

// MarshalJSON implements json.Marshaler.
func (d Duration) MarshalJSON() ([]byte, error) {
	return protojson.Marshal(durationpb.New(time.Duration(d)))
}

func duration(d *durationpb.Duration) Duration {
	return Duration(d.AsDuration())
}

func timestamp(t *timestamppb.Timestamp) time.Time {
	return t.AsTime().UTC()
}

// sequenced is a frame that takes its seq when it is queued.
type sequenced interface{ setSeq(n uint64) }

func (f *Hello) setSeq(n uint64)    { f.Seq = n }
func (f *Snapshot) setSeq(n uint64) { f.Seq = n }
func (f *Outcome) setSeq(n uint64)  { f.Seq = n }

// Hello is the first frame.
type Hello struct {
	Type       string             `json:"type"`
	Seq        uint64             `json:"seq"`
	Session    string             `json:"session"`
	Location   string             `json:"location"`
	Cell       string             `json:"cell"`
	Diagnostic v1alpha.Diagnostic `json:"diagnostic"`
	Deadline   time.Time          `json:"deadline"`
}

// Snapshot is the router set the diagnostic runs against.
type Snapshot struct {
	Type    string           `json:"type"`
	Seq     uint64           `json:"seq"`
	Routers []SnapshotRouter `json:"routers"`
}

// SnapshotRouter is one router in a snapshot.
type SnapshotRouter struct {
	Router        string `json:"router"`
	Selected      bool   `json:"selected"`
	OmittedReason string `json:"omittedReason,omitempty"`
}

// Outcome is one selected router's result: exactly one of Observation or
// Failure.
type Outcome struct {
	Type        string       `json:"type"`
	Seq         uint64       `json:"seq"`
	Router      string       `json:"router"`
	CompletedAt time.Time    `json:"completedAt"`
	Observation *Observation `json:"observation,omitempty"`
	Failure     *Failure     `json:"failure,omitempty"`
}

// End is the last frame.
type End struct {
	Type       string            `json:"type"`
	Seq        uint64            `json:"seq"`
	Reason     string            `json:"reason"`
	Message    string            `json:"message"`
	Coverage   *v1alpha.Coverage `json:"coverage,omitempty"`
	FinishedAt time.Time         `json:"finishedAt"`
	RetryAfter *Duration         `json:"retryAfter,omitempty"`
}

// Failure is a router failure.
type Failure struct {
	Reason        string    `json:"reason"`
	Message       string    `json:"message"`
	BackendReason string    `json:"backendReason"`
	Scope         string    `json:"scope,omitempty"`
	RetryAfter    *Duration `json:"retryAfter,omitempty"`
}

// Observation is one router's result. Exactly one payload is set, the one
// for the session's diagnostic.
type Observation struct {
	ASN         uint32            `json:"asn"`
	SampleTime  time.Time         `json:"sampleTime"`
	Duration    Duration          `json:"duration"`
	Truncated   bool              `json:"truncated"`
	RouteLookup *RouteLookup      `json:"routeLookup,omitempty"`
	BGPSummary  *BGPSummary       `json:"bgpSummary,omitempty"`
	Ping        *PingResult       `json:"ping,omitempty"`
	Traceroute  *TracerouteResult `json:"traceroute,omitempty"`
}

// RouteLookup is a route lookup's result. An empty Prefixes is a successful
// lookup that matched nothing.
type RouteLookup struct {
	LookupKind string         `json:"lookupKind,omitempty"`
	Prefixes   []PrefixRoutes `json:"prefixes"`
}

// PrefixRoutes are the paths to one prefix.
type PrefixRoutes struct {
	Prefix     string `json:"prefix"`
	TotalPaths uint32 `json:"totalPaths"`
	Paths      []Path `json:"paths"`
}

// Path is one route path.
type Path struct {
	Protocol   string         `json:"protocol"`
	Active     bool           `json:"active"`
	Preference uint32         `json:"preference"`
	Metric     *uint32        `json:"metric,omitempty"`
	NextHops   []NextHop      `json:"nextHops"`
	BGP        *BGPAttributes `json:"bgp,omitempty"`
}

// NextHop is one next hop. An internal next hop has no address.
type NextHop struct {
	Address  string `json:"address,omitempty"`
	Internal bool   `json:"internal,omitempty"`
	Selected bool   `json:"selected"`
}

// BGPAttributes are a BGP path's attributes.
type BGPAttributes struct {
	ASPath                []ASPathSegment `json:"asPath"`
	Origin                string          `json:"origin"`
	OriginASN             uint32          `json:"originASN"`
	LocalPref             *uint32         `json:"localPref,omitempty"`
	MED                   *uint32         `json:"med,omitempty"`
	Multipath             bool            `json:"multipath"`
	Communities           []string        `json:"communities"`
	TotalCommunities      uint32          `json:"totalCommunities"`
	LargeCommunities      []string        `json:"largeCommunities"`
	TotalLargeCommunities uint32          `json:"totalLargeCommunities"`
}

// ASPathSegment is one AS path segment.
type ASPathSegment struct {
	Type string   `json:"type,omitempty"`
	ASNs []uint32 `json:"asns"`
}

// BGPSummary counts every peer and lists the external ones.
type BGPSummary struct {
	TotalPeers       uint32    `json:"totalPeers"`
	EstablishedPeers uint32    `json:"establishedPeers"`
	Peers            []BGPPeer `json:"peers"`
}

// BGPPeer is one external BGP peer.
type BGPPeer struct {
	RemoteAS         uint32   `json:"remoteAS"`
	State            string   `json:"state"`
	Established      bool     `json:"established"`
	Uptime           Duration `json:"uptime"`
	PrefixesReceived uint64   `json:"prefixesReceived"`
	PrefixesSent     uint64   `json:"prefixesSent"`
}

// ProbeReply is one ping reply or traceroute probe. A probe with no reply has
// Timeout set and no address.
type ProbeReply struct {
	Sequence uint32   `json:"sequence"`
	Address  string   `json:"address,omitempty"`
	RTT      Duration `json:"rtt"`
	Timeout  bool     `json:"timeout"`
	ICMPType string   `json:"icmpType,omitempty"`
	ICMPCode uint32   `json:"icmpCode"`
}

// PingResult is a ping's result.
type PingResult struct {
	Source      string       `json:"source"`
	Destination string       `json:"destination"`
	Sent        uint32       `json:"sent"`
	Received    uint32       `json:"received"`
	RTTMin      Duration     `json:"rttMin"`
	RTTAvg      Duration     `json:"rttAvg"`
	RTTMax      Duration     `json:"rttMax"`
	Replies     []ProbeReply `json:"replies"`
}

// Hop is one traceroute hop.
type Hop struct {
	TTL    uint32       `json:"ttl"`
	Probes []ProbeReply `json:"probes"`
}

// TracerouteResult is a traceroute's result.
type TracerouteResult struct {
	Source      string `json:"source"`
	Destination string `json:"destination"`
	Reached     bool   `json:"reached"`
	Hops        []Hop  `json:"hops"`
}

var (
	lookupKinds = map[backendv1.LookupKind]string{
		backendv1.LookupKind_LOOKUP_KIND_EXACT:         "Exact",
		backendv1.LookupKind_LOOKUP_KIND_LONGEST_MATCH: "LongestMatch",
	}
	protocols = map[backendv1.RouteProtocol]string{
		backendv1.RouteProtocol_ROUTE_PROTOCOL_BGP:       "BGP",
		backendv1.RouteProtocol_ROUTE_PROTOCOL_STATIC:    "Static",
		backendv1.RouteProtocol_ROUTE_PROTOCOL_CONNECTED: "Connected",
		backendv1.RouteProtocol_ROUTE_PROTOCOL_LOCAL:     "Local",
		backendv1.RouteProtocol_ROUTE_PROTOCOL_AGGREGATE: "Aggregate",
		backendv1.RouteProtocol_ROUTE_PROTOCOL_IGP:       "IGP",
		backendv1.RouteProtocol_ROUTE_PROTOCOL_OTHER:     "Other",
	}
	segmentTypes = map[backendv1.ASPathSegmentType]string{
		backendv1.ASPathSegmentType_AS_PATH_SEGMENT_TYPE_SEQUENCE:        "Sequence",
		backendv1.ASPathSegmentType_AS_PATH_SEGMENT_TYPE_SET:             "Set",
		backendv1.ASPathSegmentType_AS_PATH_SEGMENT_TYPE_CONFED_SEQUENCE: "ConfedSequence",
		backendv1.ASPathSegmentType_AS_PATH_SEGMENT_TYPE_CONFED_SET:      "ConfedSet",
	}
)

// observation converts a backend observation for a query of type t. It fails
// when the observation has no sample time or carries the payload of another
// diagnostic.
func observation(t backendv1.DiagnosticType, o *backendv1.Observation) (*Observation, error) {
	if o.GetSampleTime() == nil {
		return nil, errors.New("observation has no sample time")
	}
	out := &Observation{
		ASN:        o.GetAsn(),
		SampleTime: timestamp(o.GetSampleTime()),
		Duration:   duration(o.GetDuration()),
		Truncated:  o.GetTruncated(),
	}
	switch r := o.GetResult().(type) {
	case *backendv1.Observation_Routes:
		if t != backendv1.DiagnosticType_DIAGNOSTIC_TYPE_ROUTE_LOOKUP {
			break
		}
		out.RouteLookup = routeLookup(r.Routes)
		return out, nil
	case *backendv1.Observation_BgpSummary:
		if t != backendv1.DiagnosticType_DIAGNOSTIC_TYPE_BGP_SUMMARY {
			break
		}
		out.BGPSummary = bgpSummary(r.BgpSummary)
		return out, nil
	case *backendv1.Observation_Ping:
		if t != backendv1.DiagnosticType_DIAGNOSTIC_TYPE_PING {
			break
		}
		out.Ping = ping(r.Ping)
		return out, nil
	case *backendv1.Observation_Traceroute:
		if t != backendv1.DiagnosticType_DIAGNOSTIC_TYPE_TRACEROUTE {
			break
		}
		out.Traceroute = traceroute(r.Traceroute)
		return out, nil
	}
	return nil, errors.New("observation payload does not match the diagnostic")
}

func routeLookup(r *backendv1.RouteResult) *RouteLookup {
	out := &RouteLookup{LookupKind: lookupKinds[r.GetLookupKind()], Prefixes: []PrefixRoutes{}}
	for _, p := range r.GetPrefixes() {
		pr := PrefixRoutes{Prefix: p.GetPrefix(), TotalPaths: p.GetTotalPaths(), Paths: []Path{}}
		for _, path := range p.GetPaths() {
			proto, ok := protocols[path.GetProtocol()]
			if !ok {
				proto = "Other"
			}
			rp := Path{
				Protocol:   proto,
				Active:     path.GetActive(),
				Preference: path.GetPreference(),
				Metric:     path.Metric,
				NextHops:   []NextHop{},
			}
			for _, nh := range path.GetNextHops() {
				hop := NextHop{Address: nh.GetAddress(), Internal: nh.GetInternal(), Selected: nh.GetSelected()}
				if hop.Internal {
					hop.Address = ""
				}
				rp.NextHops = append(rp.NextHops, hop)
			}
			if b := path.GetBgp(); b != nil {
				rp.BGP = bgpAttributes(b)
			}
			pr.Paths = append(pr.Paths, rp)
		}
		out.Prefixes = append(out.Prefixes, pr)
	}
	return out
}

func bgpAttributes(b *backendv1.BGPAttributes) *BGPAttributes {
	out := &BGPAttributes{
		ASPath:                []ASPathSegment{},
		Origin:                b.GetOrigin(),
		OriginASN:             b.GetOriginAsn(),
		LocalPref:             b.LocalPref,
		MED:                   b.Med,
		Multipath:             b.GetMultipath(),
		Communities:           append([]string{}, b.GetCommunities()...),
		TotalCommunities:      b.GetTotalCommunities(),
		LargeCommunities:      append([]string{}, b.GetLargeCommunities()...),
		TotalLargeCommunities: b.GetTotalLargeCommunities(),
	}
	for _, s := range b.GetAsPath() {
		out.ASPath = append(out.ASPath, ASPathSegment{Type: segmentTypes[s.GetType()], ASNs: append([]uint32{}, s.GetAsns()...)})
	}
	return out
}

func bgpSummary(s *backendv1.BGPSummary) *BGPSummary {
	out := &BGPSummary{TotalPeers: s.GetTotalPeers(), EstablishedPeers: s.GetEstablishedPeers(), Peers: []BGPPeer{}}
	for _, p := range s.GetPeers() {
		out.Peers = append(out.Peers, BGPPeer{
			RemoteAS:         p.GetRemoteAs(),
			State:            p.GetState(),
			Established:      p.GetEstablished(),
			Uptime:           duration(p.GetUptime()),
			PrefixesReceived: p.GetPrefixesReceived(),
			PrefixesSent:     p.GetPrefixesSent(),
		})
	}
	return out
}

func probes(in []*backendv1.ProbeReply) []ProbeReply {
	out := make([]ProbeReply, 0, len(in))
	for _, p := range in {
		reply := ProbeReply{
			Sequence: p.GetSequence(),
			Address:  p.GetAddress(),
			RTT:      duration(p.GetRtt()),
			Timeout:  p.GetTimeout(),
			ICMPType: p.GetIcmpType(),
			ICMPCode: p.GetIcmpCode(),
		}
		if reply.Timeout {
			reply.Address = ""
		}
		out = append(out, reply)
	}
	return out
}

func ping(p *backendv1.PingResult) *PingResult {
	return &PingResult{
		Source:      p.GetSource(),
		Destination: p.GetDestination(),
		Sent:        p.GetSent(),
		Received:    p.GetReceived(),
		RTTMin:      duration(p.GetRttMin()),
		RTTAvg:      duration(p.GetRttAvg()),
		RTTMax:      duration(p.GetRttMax()),
		Replies:     probes(p.GetReplies()),
	}
}

func traceroute(t *backendv1.TracerouteResult) *TracerouteResult {
	out := &TracerouteResult{
		Source:      t.GetSource(),
		Destination: t.GetDestination(),
		Reached:     t.GetReached(),
		Hops:        []Hop{},
	}
	for _, h := range t.GetHops() {
		out.Hops = append(out.Hops, Hop{TTL: h.GetTtl(), Probes: probes(h.GetProbes())})
	}
	return out
}

// failure converts a backend router failure.
func failure(f *backendv1.Failure) *Failure {
	out := &Failure{
		Reason:        failureReason(f.GetReason()),
		Message:       truncate(f.GetMessage(), maxFailureMessage),
		BackendReason: f.GetReason(),
		Scope:         f.GetScope(),
	}
	if f.GetRetryAfter() != nil {
		d := duration(f.GetRetryAfter())
		out.RetryAfter = &d
	}
	return out
}

// maxFailureMessage is the contract's bound on a failure message, in bytes.
const maxFailureMessage = 256

// truncate cuts s to at most n bytes without splitting a UTF-8 sequence.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}
