package frr

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/netip"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.datum.net/looking-glass/pkg/driver"
)

// zebraRoute is one protocol's entry in `show ip route <target> json`.
type zebraRoute struct {
	Protocol string         `json:"protocol"`
	Selected bool           `json:"selected"`
	Distance uint32         `json:"distance"`
	Metric   uint32         `json:"metric"`
	Nexthops []zebraNexthop `json:"nexthops"`
}

type zebraNexthop struct {
	IP            string `json:"ip"`
	InterfaceName string `json:"interfaceName"`
	FIB           bool   `json:"fib"`
	Duplicate     bool   `json:"duplicate"`
}

type rib map[netip.Prefix][]zebraRoute

func parseRIB(out []byte) (rib, error) {
	var raw map[string][]zebraRoute
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, parseError("route table", err)
	}
	r := rib{}
	for k, v := range raw {
		p, err := netip.ParsePrefix(k)
		if err != nil {
			return nil, parseError("route table", err)
		}
		r[p] = v
	}
	return r, nil
}

// prefixes returns the matched prefixes, most specific first.
func (r rib) prefixes() []netip.Prefix {
	ps := make([]netip.Prefix, 0, len(r))
	for p := range r {
		ps = append(ps, p)
	}
	slices.SortFunc(ps, func(a, b netip.Prefix) int {
		if a.Bits() != b.Bits() {
			return b.Bits() - a.Bits()
		}
		return a.Addr().Compare(b.Addr())
	})
	return ps
}

// routes returns prefix's paths, at most maxPaths of them. A BGP entry is
// replaced by the BGP table's paths for the prefix, which bgp reads.
func (r rib) routes(prefix netip.Prefix, maxPaths int, bgp func() ([]bgpPath, error)) (driver.PrefixRoutes, error) {
	pr := driver.PrefixRoutes{Prefix: prefix}
	var paths []driver.Path
	for _, z := range r[prefix] {
		proto := protocol(z.Protocol)
		if proto != driver.ProtocolBGP {
			metric := z.Metric
			paths = append(paths, driver.Path{
				Protocol:   proto,
				Active:     z.Selected,
				Preference: z.Distance,
				Metric:     &metric,
				NextHops:   zebraNextHops(z.Nexthops),
			})
			continue
		}
		bps, err := bgp()
		if err != nil {
			return pr, err
		}
		for _, bp := range bps {
			p, err := bp.path(z)
			if err != nil {
				return pr, err
			}
			paths = append(paths, p)
		}
	}
	// Active paths first, so a bound keeps the ones forwarding uses.
	slices.SortStableFunc(paths, func(a, b driver.Path) int {
		switch {
		case a.Active == b.Active:
			return 0
		case a.Active:
			return -1
		default:
			return 1
		}
	})
	pr.TotalPaths = len(paths)
	if maxPaths > 0 && len(paths) > maxPaths {
		paths = paths[:maxPaths]
	}
	pr.Paths = paths
	return pr, nil
}

func zebraNextHops(nhs []zebraNexthop) []driver.NextHop {
	var out []driver.NextHop
	for _, nh := range nhs {
		if nh.Duplicate {
			continue
		}
		hop := driver.NextHop{Interface: nh.InterfaceName, Selected: nh.FIB}
		if a, err := netip.ParseAddr(nh.IP); err == nil {
			hop.Address = a
		}
		out = append(out, hop)
	}
	return out
}

// protocol maps a zebra protocol name onto the API's protocols.
func protocol(name string) driver.Protocol {
	switch name {
	case "bgp":
		return driver.ProtocolBGP
	case "static":
		return driver.ProtocolStatic
	case "connected":
		return driver.ProtocolConnected
	case "local":
		return driver.ProtocolLocal
	case "ospf", "ospf6", "isis", "rip", "ripng", "eigrp", "babel", "openfabric":
		return driver.ProtocolIGP
	default:
		return driver.ProtocolOther
	}
}

// bgpPath is one path in `show bgp <afi> unicast <prefix> json`.
type bgpPath struct {
	ASPath struct {
		Segments []struct {
			Type string   `json:"type"`
			List []uint32 `json:"list"`
		} `json:"segments"`
	} `json:"aspath"`
	Origin    string  `json:"origin"`
	Metric    *uint32 `json:"metric"`
	LocPrf    *uint32 `json:"locPrf"`
	Multipath bool    `json:"multipath"`
	Bestpath  *struct {
		Overall bool `json:"overall"`
	} `json:"bestpath"`
	Community *struct {
		List []string `json:"list"`
	} `json:"community"`
	LargeCommunity *struct {
		List []string `json:"list"`
	} `json:"largeCommunity"`
	Nexthops []struct {
		IP   string `json:"ip"`
		Used bool   `json:"used"`
	} `json:"nexthops"`
}

func parseBGPPrefix(out []byte) ([]bgpPath, error) {
	var raw struct {
		Paths []bgpPath `json:"paths"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, parseError("BGP table", err)
	}
	return raw.Paths, nil
}

var segmentTypes = map[string]driver.ASPathSegmentType{
	"as-sequence":        driver.ASPathSequence,
	"as-set":             driver.ASPathSet,
	"as-confed-sequence": driver.ASPathConfedSequence,
	"as-confed-set":      driver.ASPathConfedSet,
}

// path converts a BGP path. z is the zebra entry the BGP routes were
// installed as: it gives the preference, and whether any BGP path forwards.
func (bp bgpPath) path(z zebraRoute) (driver.Path, error) {
	attrs := &driver.BGPAttributes{
		Origin:    origin(bp.Origin),
		LocalPref: bp.LocPrf,
		MED:       bp.Metric,
		Multipath: bp.Multipath,
	}
	for _, s := range bp.ASPath.Segments {
		t, ok := segmentTypes[s.Type]
		if !ok {
			return driver.Path{}, driver.Errorf(driver.ReasonParseError, "unknown AS path segment type")
		}
		attrs.ASPath = append(attrs.ASPath, driver.ASPathSegment{Type: t, ASNs: slices.Clone(s.List)})
	}
	if n := len(attrs.ASPath); n > 0 && attrs.ASPath[n-1].Type == driver.ASPathSequence {
		if asns := attrs.ASPath[n-1].ASNs; len(asns) > 0 {
			attrs.OriginASN = asns[len(asns)-1]
		}
	}
	if bp.Community != nil {
		for _, c := range bp.Community.List {
			attrs.Communities = append(attrs.Communities, community(c))
		}
	}
	if bp.LargeCommunity != nil {
		attrs.LargeCommunities = slices.Clone(bp.LargeCommunity.List)
	}
	best := bp.Bestpath != nil && bp.Bestpath.Overall
	p := driver.Path{
		Protocol:   driver.ProtocolBGP,
		Active:     z.Selected && (best || bp.Multipath),
		Preference: z.Distance,
		BGP:        attrs,
	}
	for _, nh := range bp.Nexthops {
		a, err := netip.ParseAddr(nh.IP)
		if err != nil {
			return driver.Path{}, parseError("BGP next hop", err)
		}
		p.NextHops = append(p.NextHops, driver.NextHop{Address: a, Selected: p.Active && nh.Used})
	}
	return p, nil
}

func origin(s string) string {
	switch strings.ToLower(s) {
	case "igp":
		return "IGP"
	case "egp":
		return "EGP"
	default:
		return "Incomplete"
	}
}

// wellKnownCommunities maps the names FRR prints for well-known communities
// (RFC 1997 and the IANA registry) to their numeric form, so an allowlist
// matches one spelling.
var wellKnownCommunities = map[string]string{
	"gshut":                   "65535:0",
	"acceptOwn":               "65535:1",
	"routeFilterTranslatedV4": "65535:2",
	"routeFilterV4":           "65535:3",
	"routeFilterTranslatedV6": "65535:4",
	"routeFilterV6":           "65535:5",
	"llgrStale":               "65535:6",
	"noLlgr":                  "65535:7",
	"acceptownNexthop":        "65535:8",
	"blackhole":               "65535:666",
	"noExport":                "65535:65281",
	"noAdvertise":             "65535:65282",
	"localAs":                 "65535:65283",
	"noPeer":                  "65535:65284",
	"internet":                "0:0",
}

func community(c string) string {
	if n, ok := wellKnownCommunities[c]; ok {
		return n
	}
	return c
}

func parseBGPSummary(out []byte) (*driver.BGPSummary, error) {
	var raw struct {
		RouterID string `json:"routerId"`
		AS       uint32 `json:"as"`
		Peers    map[string]struct {
			RemoteAS         uint32 `json:"remoteAs"`
			LocalAS          uint32 `json:"localAs"`
			Desc             string `json:"desc"`
			State            string `json:"state"`
			PeerUptimeMsec   int64  `json:"peerUptimeMsec"`
			PrefixesReceived uint64 `json:"pfxRcd"`
			PrefixesSent     uint64 `json:"pfxSnt"`
		} `json:"peers"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, parseError("BGP summary", err)
	}
	s := &driver.BGPSummary{LocalAS: raw.AS}
	// A family with no sessions prints {}: no router ID and no peers.
	if raw.RouterID != "" {
		id, err := netip.ParseAddr(raw.RouterID)
		if err != nil {
			return nil, parseError("BGP summary", err)
		}
		s.RouterID = id
	}
	for addr, p := range raw.Peers {
		a, err := netip.ParseAddr(addr)
		if err != nil {
			return nil, parseError("BGP summary", err)
		}
		peer := driver.BGPPeer{
			Address:          a,
			Internal:         p.RemoteAS == p.LocalAS,
			RemoteAS:         p.RemoteAS,
			Description:      p.Desc,
			State:            p.State,
			Established:      p.State == "Established",
			PrefixesReceived: p.PrefixesReceived,
			PrefixesSent:     p.PrefixesSent,
		}
		if peer.Established {
			peer.Uptime = time.Duration(p.PeerUptimeMsec) * time.Millisecond
		}
		s.Peers = append(s.Peers, peer)
	}
	slices.SortFunc(s.Peers, func(a, b driver.BGPPeer) int { return a.Address.Compare(b.Address) })
	return s, nil
}

var (
	pingReply  = regexp.MustCompile(`^\d+ bytes from ([0-9a-fA-F.:]+): icmp_seq=(\d+) .*time=([0-9.]+) ms`)
	pingError  = regexp.MustCompile(`^From ([0-9a-fA-F.:]+) icmp_seq=(\d+) (.+)$`)
	pingTotals = regexp.MustCompile(`^(\d+) packets transmitted, (\d+) received`)
)

// pingErrors maps iputils' text for an ICMP error onto its type and code.
var pingErrors = map[string]struct {
	icmpType string
	code     uint32
}{
	"Destination Net Unreachable":                          {"destination-unreachable", 0},
	"Destination Host Unreachable":                         {"destination-unreachable", 1},
	"Destination Protocol Unreachable":                     {"destination-unreachable", 2},
	"Destination Port Unreachable":                         {"destination-unreachable", 3},
	"Packet filtered":                                      {"destination-unreachable", 13},
	"Destination unreachable: No route":                    {"destination-unreachable", 0},
	"Destination unreachable: Administratively prohibited": {"destination-unreachable", 1},
	"Destination unreachable: Address unreachable":         {"destination-unreachable", 3},
	"Destination unreachable: Port unreachable":            {"destination-unreachable", 4},
	"Time to live exceeded":                                {"time-exceeded", 0},
	"Time exceeded: Hop limit":                             {"time-exceeded", 0},
}

// parsePing parses iputils ping output for count echo requests. A sequence
// number with no reply is a timeout.
func parsePing(out []byte, count int) (*driver.PingResult, error) {
	res := &driver.PingResult{}
	replies := map[uint32]driver.ProbeReply{}
	sawTotals := false
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if m := pingReply.FindStringSubmatch(line); m != nil {
			addr, seq, rtt, err := replyFields(m[1], m[2], m[3])
			if err != nil {
				return nil, err
			}
			if _, dup := replies[seq]; !dup {
				replies[seq] = driver.ProbeReply{Sequence: seq, Address: addr, RTT: rtt, ICMPType: "echo-reply"}
			}
			continue
		}
		if m := pingError.FindStringSubmatch(line); m != nil {
			addr, seq, _, err := replyFields(m[1], m[2], "0")
			if err != nil {
				return nil, err
			}
			e, ok := pingErrors[strings.TrimSpace(m[3])]
			if !ok {
				e.icmpType = "other"
			}
			replies[seq] = driver.ProbeReply{Sequence: seq, Address: addr, ICMPType: e.icmpType, ICMPCode: e.code}
			continue
		}
		if m := pingTotals.FindStringSubmatch(line); m != nil {
			sent, _ := strconv.Atoi(m[1])
			res.Sent = sent
			sawTotals = true
		}
	}
	if err := sc.Err(); err != nil {
		return nil, parseError("ping", err)
	}
	if !sawTotals {
		return nil, driver.Errorf(driver.ReasonParseError, "ping output has no statistics")
	}
	if res.Sent > count {
		return nil, driver.Errorf(driver.ReasonParseError, "ping sent %d requests, want at most %d", res.Sent, count)
	}
	for seq := uint32(1); seq <= uint32(res.Sent); seq++ {
		r, ok := replies[seq]
		if !ok {
			r = driver.ProbeReply{Sequence: seq, Timeout: true}
		}
		res.Replies = append(res.Replies, r)
	}
	return res, nil
}

func replyFields(addr, seq, rtt string) (netip.Addr, uint32, time.Duration, error) {
	a, err := netip.ParseAddr(addr)
	if err != nil {
		return netip.Addr{}, 0, 0, parseError("ping reply", err)
	}
	n, err := strconv.ParseUint(seq, 10, 32)
	if err != nil {
		return netip.Addr{}, 0, 0, parseError("ping reply", err)
	}
	d, err := millis(rtt)
	if err != nil {
		return netip.Addr{}, 0, 0, err
	}
	return a, uint32(n), d, nil
}

func millis(s string) (time.Duration, error) {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f < 0 {
		return 0, driver.Errorf(driver.ReasonParseError, "malformed round-trip time")
	}
	return time.Duration(f * float64(time.Millisecond)), nil
}

// tracerouteFlags maps traceroute's annotations onto ICMP unreachable codes.
var tracerouteFlags = map[string]uint32{
	"!N": 0, "!H": 1, "!P": 2, "!F": 4, "!S": 5, "!X": 13, "!V": 14, "!C": 15,
}

// parseTraceroute parses Linux traceroute output (-n). Each hop line is a TTL
// followed by probes: "*" for no reply, or an optional address, a time in ms
// and an optional "!" annotation.
func parseTraceroute(out []byte, dst netip.Addr) (*driver.TracerouteResult, error) {
	res := &driver.TracerouteResult{Destination: dst}
	sc := bufio.NewScanner(bytes.NewReader(out))
	header := false
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 0 {
			continue
		}
		if fields[0] == "traceroute" {
			header = true
			continue
		}
		ttl, err := strconv.Atoi(fields[0])
		if err != nil || ttl != len(res.Hops)+1 {
			return nil, driver.Errorf(driver.ReasonParseError, "malformed traceroute hop line")
		}
		hop, err := parseHop(ttl, fields[1:], dst)
		if err != nil {
			return nil, err
		}
		res.Hops = append(res.Hops, hop)
		for _, p := range hop.Probes {
			res.Reached = res.Reached || (!p.Timeout && p.Address == dst)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, parseError("traceroute", err)
	}
	if !header {
		return nil, driver.Errorf(driver.ReasonParseError, "traceroute output has no header")
	}
	return res, nil
}

func parseHop(ttl int, fields []string, dst netip.Addr) (driver.Hop, error) {
	hop := driver.Hop{TTL: ttl}
	var addr netip.Addr
	seq := uint32(0)
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		switch {
		case f == "*":
			seq++
			hop.Probes = append(hop.Probes, driver.ProbeReply{Sequence: seq, Timeout: true})
		case strings.HasPrefix(f, "!"):
			n := len(hop.Probes)
			if n == 0 || hop.Probes[n-1].Timeout {
				return hop, driver.Errorf(driver.ReasonParseError, "traceroute annotation without a reply")
			}
			hop.Probes[n-1].ICMPType = "destination-unreachable"
			hop.Probes[n-1].ICMPCode = tracerouteFlags[f]
		case i+1 < len(fields) && fields[i+1] == "ms":
			rtt, err := millis(f)
			if err != nil {
				return hop, err
			}
			if !addr.IsValid() {
				return hop, driver.Errorf(driver.ReasonParseError, "traceroute time without an address")
			}
			seq++
			probe := driver.ProbeReply{Sequence: seq, Address: addr, RTT: rtt, ICMPType: "time-exceeded"}
			if addr == dst {
				// A UDP probe that reaches the destination gets port unreachable.
				probe.ICMPType, probe.ICMPCode = "destination-unreachable", 3
				if dst.Is6() {
					probe.ICMPCode = 4
				}
			}
			hop.Probes = append(hop.Probes, probe)
			i++
		default:
			a, err := netip.ParseAddr(f)
			if err != nil {
				return hop, driver.Errorf(driver.ReasonParseError, "malformed traceroute probe")
			}
			addr = a
		}
	}
	return hop, nil
}

func parseError(what string, err error) error {
	return &driver.Error{Reason: driver.ReasonParseError, Message: fmt.Sprintf("malformed %s output", what), Err: err}
}
