package frr_test

import (
	"context"
	"errors"
	"maps"
	"net/netip"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"go.datum.net/looking-glass/pkg/driver"
	"go.datum.net/looking-glass/pkg/driver/frr"
	"go.datum.net/looking-glass/pkg/driver/frr/frrtest"
)

func router() driver.Router {
	return driver.Router{Name: "r1", Platform: frr.Platform, Address: "192.0.2.2", Settings: maps.Clone(frrtest.Settings)}
}

func newDriver(t *testing.T, runner frr.Runner) driver.Driver {
	t.Helper()
	d, err := frr.New(router(), runner)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func u32(v uint32) *uint32 { return new(v) }

func addr(s string) netip.Addr { return netip.MustParseAddr(s) }

func TestRouteLookupBGP(t *testing.T) {
	d := frrtest.New(t, "r1")
	got, err := d.RouteLookup(t.Context(), driver.RouteLookupRequest{Target: netip.MustParsePrefix("198.51.100.7/32"), MaxPaths: 8})
	if err != nil {
		t.Fatal(err)
	}
	attrs := &driver.BGPAttributes{
		ASPath:           []driver.ASPathSegment{{Type: driver.ASPathSequence, ASNs: []uint32{64497}}},
		Origin:           "IGP",
		OriginASN:        64497,
		MED:              u32(50),
		Multipath:        true,
		Communities:      []string{"64497:100", "64497:200", "65535:666", "65535:65281"},
		LargeCommunities: []string{"64497:1:2", "64497:3:4"},
	}
	want := &driver.RouteResult{Prefixes: []driver.PrefixRoutes{{
		Prefix:     netip.MustParsePrefix("198.51.100.0/24"),
		TotalPaths: 2,
		Paths: []driver.Path{
			{Protocol: driver.ProtocolBGP, Active: true, Preference: 20, BGP: attrs,
				NextHops: []driver.NextHop{{Address: addr("192.0.2.3"), Selected: true}}},
			{Protocol: driver.ProtocolBGP, Active: true, Preference: 20, BGP: attrs,
				NextHops: []driver.NextHop{{Address: addr("192.0.2.3"), Selected: true}}},
		},
	}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("RouteLookup =\n%+v\nwant\n%+v", got, want)
	}
}

func TestRouteLookupBoundsPaths(t *testing.T) {
	d := frrtest.New(t, "r1")
	got, err := d.RouteLookup(t.Context(), driver.RouteLookupRequest{Target: netip.MustParsePrefix("198.51.100.0/24"), MaxPaths: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Exact || len(got.Prefixes) != 1 || got.Prefixes[0].TotalPaths != 2 || len(got.Prefixes[0].Paths) != 1 {
		t.Errorf("RouteLookup = %+v, want an exact match with 1 of 2 paths", got)
	}
}

func TestRouteLookupOtherProtocols(t *testing.T) {
	d := frrtest.New(t, "r1")
	tests := []struct {
		target string
		want   driver.Path
	}{
		{"198.18.0.0/15", driver.Path{Protocol: driver.ProtocolStatic, Active: true, Preference: 1, Metric: u32(0),
			NextHops: []driver.NextHop{{Selected: true}}}},
		{"192.0.2.9/32", driver.Path{Protocol: driver.ProtocolConnected, Active: true, Preference: 0, Metric: u32(0),
			NextHops: []driver.NextHop{{Interface: "eth0", Selected: true}}}},
	}
	for _, tt := range tests {
		got, err := d.RouteLookup(t.Context(), driver.RouteLookupRequest{Target: netip.MustParsePrefix(tt.target), MaxPaths: 8})
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Prefixes) != 1 || !reflect.DeepEqual(got.Prefixes[0].Paths, []driver.Path{tt.want}) {
			t.Errorf("RouteLookup(%s) = %+v, want one path %+v", tt.target, got, tt.want)
		}
	}
}

func TestRouteLookupInternalBGPAndIPv6(t *testing.T) {
	d := frrtest.New(t, "r1")
	got, err := d.RouteLookup(t.Context(), driver.RouteLookupRequest{Target: netip.MustParsePrefix("203.0.113.0/24"), MaxPaths: 8})
	if err != nil {
		t.Fatal(err)
	}
	p := got.Prefixes[0].Paths[0]
	if p.Preference != 200 || p.BGP.LocalPref == nil || *p.BGP.LocalPref != 100 || len(p.BGP.ASPath) != 0 || p.NextHops[0].Address != addr("10.240.40.3") {
		t.Errorf("internal path = %+v", p)
	}

	got, err = d.RouteLookup(t.Context(), driver.RouteLookupRequest{Target: netip.MustParsePrefix("2001:db8:100::7/128"), MaxPaths: 8})
	if err != nil {
		t.Fatal(err)
	}
	// FRR reports the global next hop and the link-local one forwarding uses.
	// The link-local address changes with every recording.
	hops := got.Prefixes[0].Paths[0].NextHops
	if got.Prefixes[0].Prefix != netip.MustParsePrefix("2001:db8:100::/48") || len(hops) != 2 ||
		hops[0] != (driver.NextHop{Address: addr("2001:db8:2::3")}) || !hops[1].Address.IsLinkLocalUnicast() || !hops[1].Selected {
		t.Errorf("IPv6 lookup = %+v, want a global and a selected link-local next hop", got.Prefixes[0])
	}
}

func TestRouteLookupNoMatch(t *testing.T) {
	d := frrtest.New(t, "r1")
	got, err := d.RouteLookup(t.Context(), driver.RouteLookupRequest{Target: netip.MustParsePrefix("198.51.100.0/25"), MaxPaths: 8})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Exact || len(got.Prefixes) != 0 {
		t.Errorf("RouteLookup = %+v, want an empty exact result", got)
	}
}

func TestBGPSummary(t *testing.T) {
	d := frrtest.New(t, "r1")
	got, err := d.BGPSummary(t.Context(), driver.BGPSummaryRequest{Family: driver.FamilyIPv4})
	if err != nil {
		t.Fatal(err)
	}
	if got.RouterID != addr("192.0.2.254") || got.LocalAS != 64496 || len(got.Peers) != 4 {
		t.Fatalf("BGPSummary = %+v", got)
	}
	external := 0
	for _, p := range got.Peers {
		if !p.Established || p.Uptime <= 0 {
			t.Errorf("peer %s: established %v, uptime %s", p.Address, p.Established, p.Uptime)
		}
		if !p.Internal {
			external++
			if p.RemoteAS != 64497 || p.Description == "" {
				t.Errorf("external peer %+v", p)
			}
		}
	}
	if external != 2 {
		t.Errorf("got %d external peers, want 2", external)
	}
	if _, err := d.BGPSummary(t.Context(), driver.BGPSummaryRequest{Family: driver.FamilyIPv6}); err != nil {
		t.Errorf("IPv6 summary: %v", err)
	}
}

func TestPing(t *testing.T) {
	d := frrtest.New(t, "r1")
	req := driver.PingRequest{Destination: addr("198.51.100.3"), Count: 3, Wait: time.Second, PayloadBytes: 56}
	got, err := d.Ping(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if got.Source != addr("192.0.2.2") || got.Destination != req.Destination || got.Sent != 3 || len(got.Replies) != 3 {
		t.Fatalf("Ping = %+v", got)
	}
	for i, r := range got.Replies {
		if r.Sequence != uint32(i+1) || r.Timeout || r.Address != req.Destination || r.RTT <= 0 || r.ICMPType != "echo-reply" {
			t.Errorf("reply %d = %+v", i, r)
		}
	}

	req.Destination = addr("198.51.100.200")
	got, err = d.Ping(t.Context(), req)
	if err != nil {
		t.Fatalf("Ping with every reply lost: %v", err)
	}
	for _, r := range got.Replies {
		if !r.Timeout || r.Address.IsValid() {
			t.Errorf("lost reply = %+v, want a timeout", r)
		}
	}

	req.Destination = addr("2001:db8:100::3")
	got, err = d.Ping(t.Context(), req)
	if err != nil || got.Source != addr("2001:db8:2::2") || len(got.Replies) != 3 || got.Replies[2].Timeout {
		t.Errorf("IPv6 Ping = %+v, %v", got, err)
	}
}

func TestTraceroute(t *testing.T) {
	d := frrtest.New(t, "r1")
	req := driver.TracerouteRequest{Destination: addr("198.51.100.3"), MaxHops: 20, ProbesPerHop: 3, Wait: time.Second, PayloadBytes: 56}
	got, err := d.Traceroute(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Reached || len(got.Hops) != 2 || got.Source != addr("192.0.2.2") {
		t.Fatalf("Traceroute = %+v", got)
	}
	first, last := got.Hops[0].Probes[0], got.Hops[1].Probes[2]
	if first.Address != addr("192.0.2.3") || first.ICMPType != "time-exceeded" || last.Sequence != 3 ||
		last.Address != req.Destination || last.ICMPType != "destination-unreachable" || last.ICMPCode != 3 {
		t.Errorf("probes = %+v, %+v", first, last)
	}

	req.Destination, req.MaxHops = addr("198.51.100.200"), 4
	got, err = d.Traceroute(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if got.Reached || len(got.Hops) != 4 || !got.Hops[3].Probes[2].Timeout {
		t.Errorf("silent traceroute = %+v", got)
	}

	req.Destination, req.MaxHops = addr("2001:db8:100::3"), 20
	got, err = d.Traceroute(t.Context(), req)
	if err != nil || !got.Reached || got.Hops[1].Probes[0].ICMPCode != 4 {
		t.Errorf("IPv6 Traceroute = %+v, %v", got, err)
	}
}

// fixed is a Runner that returns one output and error for every command.
type fixed struct {
	out []byte
	err error

	mu       sync.Mutex
	commands []string
}

func (f *fixed) Run(_ context.Context, command string, _ int) ([]byte, error) {
	f.mu.Lock()
	f.commands = append(f.commands, command)
	f.mu.Unlock()
	return f.out, f.err
}

func (f *fixed) Close() error { return nil }

func TestFailures(t *testing.T) {
	ping := func(d driver.Driver, ctx context.Context) error {
		_, err := d.Ping(ctx, driver.PingRequest{Destination: addr("198.51.100.3"), Count: 3, Wait: time.Second})
		return err
	}
	route := func(d driver.Driver, ctx context.Context) error {
		_, err := d.RouteLookup(ctx, driver.RouteLookupRequest{Target: netip.MustParsePrefix("198.51.100.7/32")})
		return err
	}
	trace := func(d driver.Driver, ctx context.Context) error {
		_, err := d.Traceroute(ctx, driver.TracerouteRequest{Destination: addr("198.51.100.3"), MaxHops: 3, ProbesPerHop: 1, Wait: time.Second})
		return err
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	tests := []struct {
		name   string
		runner *fixed
		ctx    context.Context
		call   func(driver.Driver, context.Context) error
		want   driver.Reason
	}{
		{"unparseable route table", &fixed{out: []byte("% Unknown command")}, t.Context(), route, driver.ReasonParseError},
		{"unparseable ping", &fixed{out: []byte("garbage")}, t.Context(), ping, driver.ReasonParseError},
		{"unparseable traceroute", &fixed{out: []byte("traceroute to x\n 1  nonsense 1 ms\n")}, t.Context(), trace, driver.ReasonParseError},
		{"ping error exit", &fixed{err: &frr.ExitError{Status: 2}}, t.Context(), ping, driver.ReasonRouterUnavailable},
		{"vtysh error exit", &fixed{err: &frr.ExitError{Status: 1}}, t.Context(), route, driver.ReasonRouterUnavailable},
		{"unreachable", &fixed{err: driver.Errorf(driver.ReasonRouterUnreachable, "no route")}, t.Context(), route, driver.ReasonRouterUnreachable},
		{"cancelled", &fixed{err: context.Canceled}, cancelled, route, driver.ReasonCancelled},
		{"other error", &fixed{err: errors.New("boom")}, t.Context(), route, driver.ReasonInternal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.call(newDriver(t, tt.runner), tt.ctx)
			if got := driver.ReasonOf(err); got != tt.want {
				t.Errorf("reason = %s (%v), want %s", got, err, tt.want)
			}
		})
	}
}

func TestProbeSourceRequired(t *testing.T) {
	r := router()
	delete(r.Settings, frr.SettingProbeSourceIPv6)
	runner := &fixed{}
	d, err := frr.New(r, runner)
	if err != nil {
		t.Fatal(err)
	}
	if d.Capabilities().Serves(driver.DiagnosticPing, driver.FamilyIPv6) || !d.Capabilities().Serves(driver.DiagnosticPing, driver.FamilyIPv4) {
		t.Errorf("capabilities = %+v, want ping for IPv4 only", d.Capabilities())
	}
	_, err = d.Ping(t.Context(), driver.PingRequest{Destination: addr("2001:db8:100::3"), Count: 3})
	if driver.ReasonOf(err) != driver.ReasonProbeSourceUnavailable || len(runner.commands) != 0 {
		t.Errorf("IPv6 ping without a source: %v after %d commands", err, len(runner.commands))
	}
}

func TestNewRejectsSettings(t *testing.T) {
	for name, settings := range map[string]map[string]string{
		"unknown key":          {"color": "blue"},
		"IPv6 source for IPv4": {frr.SettingProbeSourceIPv4: "2001:db8::1"},
		"malformed source":     {frr.SettingProbeSourceIPv6: "not-an-address"},
	} {
		if _, err := frr.New(driver.Router{Name: "r1", Settings: settings}, &fixed{}); err == nil {
			t.Errorf("%s: New accepted %v", name, settings)
		}
	}
}

func TestFactoryRequiresSSHSettings(t *testing.T) {
	dir := t.TempDir()
	if _, err := frr.Factory(driver.Router{Name: "r1", Address: "192.0.2.2", Settings: map[string]string{}}); err == nil {
		t.Error("Factory accepted a router with no SSH settings")
	}
	_, err := frr.Factory(driver.Router{Name: "r1", Address: "192.0.2.2", Settings: map[string]string{
		frr.SettingUser: "lg", frr.SettingPrivateKeyFile: filepath.Join(dir, "missing"), frr.SettingKnownHostsFile: filepath.Join(dir, "missing"),
	}})
	if err == nil {
		t.Error("Factory accepted a missing key file")
	}
}

func TestRegistered(t *testing.T) {
	if _, err := driver.New(driver.Router{Name: "r1", Platform: frr.Platform}); err == nil {
		t.Error("driver.New accepted an frr router without settings")
	}
	found := false
	for _, p := range driver.Platforms() {
		found = found || p == frr.Platform
	}
	if !found {
		t.Errorf("platforms %v do not include %q", driver.Platforms(), frr.Platform)
	}
}

// TestRefusesZonedDestination checks that a destination netip accepts but
// that carries a zone, which is free text, never reaches a command line.
func TestRefusesZonedDestination(t *testing.T) {
	runner := &fixed{}
	d := newDriver(t, runner)
	for _, s := range []string{"fe80::1%eth0;id", "2001:db8::1%a$(id)`x`", "2001:db8::1%x y"} {
		dst, err := netip.ParseAddr(s)
		if err != nil {
			t.Fatalf("ParseAddr(%q): %v", s, err)
		}
		_, err = d.Ping(t.Context(), driver.PingRequest{Destination: dst, Count: 3, Wait: time.Second})
		if driver.ReasonOf(err) != driver.ReasonDestinationNotAllowed {
			t.Errorf("Ping(%q) = %v, want DestinationNotAllowed", s, err)
		}
		_, err = d.Traceroute(t.Context(), driver.TracerouteRequest{Destination: dst, MaxHops: 3, ProbesPerHop: 1, Wait: time.Second})
		if driver.ReasonOf(err) != driver.ReasonDestinationNotAllowed {
			t.Errorf("Traceroute(%q) = %v, want DestinationNotAllowed", s, err)
		}
	}
	for _, dst := range []netip.Addr{{}, netip.MustParseAddr("::ffff:198.51.100.3")} {
		if _, err := d.Ping(t.Context(), driver.PingRequest{Destination: dst, Count: 3}); driver.ReasonOf(err) != driver.ReasonDestinationNotAllowed {
			t.Errorf("Ping(%v) = %v, want DestinationNotAllowed", dst, err)
		}
	}
	if _, err := d.RouteLookup(t.Context(), driver.RouteLookupRequest{}); driver.ReasonOf(err) != driver.ReasonDestinationNotAllowed {
		t.Errorf("RouteLookup with no target = %v, want DestinationNotAllowed", err)
	}
	if len(runner.commands) != 0 {
		t.Errorf("ran %q, want no command", runner.commands)
	}
}

func TestOverlongOutputLine(t *testing.T) {
	long := []byte("PING 198.51.100.3\n" + strings.Repeat("x", 70*1024) + "\n3 packets transmitted, 3 received\n")
	_, err := newDriver(t, &fixed{out: long}).Ping(t.Context(), driver.PingRequest{Destination: addr("198.51.100.3"), Count: 3, Wait: time.Second})
	if driver.ReasonOf(err) != driver.ReasonParseError {
		t.Errorf("Ping with a 70 KiB line = %v, want ParseError", err)
	}
	trace := []byte("traceroute to 198.51.100.3\n" + strings.Repeat("x", 70*1024) + "\n")
	_, err = newDriver(t, &fixed{out: trace}).Traceroute(t.Context(), driver.TracerouteRequest{Destination: addr("198.51.100.3"), MaxHops: 3, ProbesPerHop: 1, Wait: time.Second})
	if driver.ReasonOf(err) != driver.ReasonParseError {
		t.Errorf("Traceroute with a 70 KiB line = %v, want ParseError", err)
	}
}

func TestTraceroutePrivateHop(t *testing.T) {
	got, err := frrtest.New(t, "r1").Traceroute(t.Context(), driver.TracerouteRequest{
		Destination: addr("198.51.100.4"), MaxHops: 20, ProbesPerHop: 3, Wait: time.Second, PayloadBytes: 56,
	})
	if err != nil {
		t.Fatal(err)
	}
	// The driver reports what the router saw; the backend redacts it.
	if !got.Reached || len(got.Hops) != 2 || got.Hops[0].Probes[0].Address != addr("10.250.0.2") || !got.Hops[1].Probes[1].Timeout {
		t.Errorf("Traceroute = %+v, want a private first hop, then the destination with lost probes", got)
	}
}
