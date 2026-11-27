package routerbackend

import (
	"flag"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	backendv1 "go.datum.net/looking-glass/api/lookingglass/backend/v1"
	"go.datum.net/looking-glass/internal/config"
	"go.datum.net/looking-glass/pkg/driver"
	"go.datum.net/looking-glass/pkg/driver/frr/frrtest"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata/golden")

// goldenRouter is the recorded router's inventory name. It must never appear
// in an observation.
const goldenRouter = "edge-router-1"

type goldenCase struct {
	name   string
	family backendv1.AddressFamily
	diag   backendv1.DiagnosticType
	target string
}

var (
	v4 = backendv1.AddressFamily_ADDRESS_FAMILY_IPV4
	v6 = backendv1.AddressFamily_ADDRESS_FAMILY_IPV6
)

const (
	routeLookup = backendv1.DiagnosticType_DIAGNOSTIC_TYPE_ROUTE_LOOKUP
	bgpSum      = backendv1.DiagnosticType_DIAGNOSTIC_TYPE_BGP_SUMMARY
	pingDiag    = backendv1.DiagnosticType_DIAGNOSTIC_TYPE_PING
	traceDiag   = backendv1.DiagnosticType_DIAGNOSTIC_TYPE_TRACEROUTE
)

// goldenCases cover every recorded diagnostic the backend runs with the
// contract's fixed parameters.
var goldenCases = []goldenCase{
	{"route-v4-bgp-external", v4, routeLookup, "198.51.100.7"},
	{"route-v4-bgp-internal", v4, routeLookup, "203.0.113.0/24"},
	{"route-v4-static", v4, routeLookup, "198.18.0.0/15"},
	{"route-v4-connected", v4, routeLookup, "192.0.2.9"},
	{"route-v4-exact-no-match", v4, routeLookup, "198.51.100.0/25"},
	{"route-v6-bgp-external", v6, routeLookup, "2001:db8:100::7"},
	{"bgp-summary-v4", v4, bgpSum, ""},
	{"bgp-summary-v6", v6, bgpSum, ""},
	{"ping-v4", v4, pingDiag, "198.51.100.3"},
	{"ping-v4-no-reply", v4, pingDiag, "198.51.100.200"},
	{"ping-v6", v6, pingDiag, "2001:db8:100::3"},
	{"traceroute-v4", v4, traceDiag, "198.51.100.3"},
	{"traceroute-v6", v6, traceDiag, "2001:db8:100::3"},
	{"traceroute-v4-private-hop", v4, traceDiag, "198.51.100.4"},
}

func goldenConfig() *config.Backend {
	cfg := testConfig()
	cfg.DestinationPolicy.Deny = []string{"100.127.0.0/16"}
	cfg.Locations[0].Routers = []config.RouterConfig{{Name: goldenRouter, Platform: "frr", ASN: 64496}}
	return cfg
}

// TestGolden runs each recorded diagnostic through the backend and compares
// the observation with testdata/golden/<case>.json. Run with -update to
// rewrite the files after a deliberate change, and review the diff.
func TestGolden(t *testing.T) {
	client := serve(t, goldenConfig(), func(r driver.Router) (driver.Driver, error) { return frrtest.New(t, r.Name), nil })
	for _, tc := range goldenCases {
		t.Run(tc.name, func(t *testing.T) {
			obs := observeGolden(t, client, tc)
			path := filepath.Join("testdata", "golden", tc.name+".json")
			if *update {
				out, err := protojson.MarshalOptions{Multiline: true, Indent: "  "}.Marshal(obs)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, append(out, '\n'), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v (run with -update to create it)", err)
			}
			want := &backendv1.Observation{}
			if err := protojson.Unmarshal(data, want); err != nil {
				t.Fatal(err)
			}
			if !proto.Equal(obs, want) {
				t.Errorf("observation differs from %s:\n%s", path, protojson.Format(obs))
			}
		})
	}
}

// observeGolden runs one case and returns its observation without the
// timing fields, which change with every run.
func observeGolden(t *testing.T, client backendv1.DiagnosticServiceClient, tc goldenCase) *backendv1.Observation {
	t.Helper()
	events, err := collect(t, client, request("project-golden", &backendv1.Query{Type: tc.diag, AddressFamily: tc.family, Target: tc.target}))
	if err != nil {
		t.Fatal(err)
	}
	got, complete := outcomes(t, events)
	obs := got[goldenRouter].GetObservation()
	if obs == nil || complete.GetSuccessful() != 1 {
		t.Fatalf("outcome = %v, complete = %v; want one observation", got[goldenRouter], complete)
	}
	obs.SampleTime, obs.Duration = nil, nil
	return obs
}

// TestGoldenRedaction checks every golden observation for the values the
// contract withholds. The recordings contain each of them.
func TestGoldenRedaction(t *testing.T) {
	withheld := map[string]string{
		goldenRouter:  "router name",
		"192.0.2.254": "router ID",
		"10.240.40.3": "internal next hop and peer address",
		"fd00:40::3":  "internal peer address",
		"fe80:":       "link-local next hop",
		"10.250.0.2":  "private traceroute hop",
		"eth0":        "interface name",
		"eth1":        "interface name",
		"transit-a":   "peer description",
		"core-r3":     "peer description",
		`"r2"`:        "peer hostname",
		`"r3"`:        "peer hostname",
		"65535:666":   "community off the allowlist",
		"64497:200":   "community off the allowlist",
		"64497:3:4":   "large community off the allowlist",
	}
	// Peer addresses may appear as route next hops, never in a BGP summary.
	peerAddresses := []string{"192.0.2.3", "2001:db8:2::3"}

	client := serve(t, goldenConfig(), func(r driver.Router) (driver.Driver, error) { return frrtest.New(t, r.Name), nil })
	for _, tc := range goldenCases {
		out, err := protojson.Marshal(observeGolden(t, client, tc))
		if err != nil {
			t.Fatal(err)
		}
		for value, what := range withheld {
			if strings.Contains(string(out), value) {
				t.Errorf("%s: observation contains a withheld value (%s)", tc.name, what)
			}
		}
		if tc.diag == bgpSum {
			for _, a := range peerAddresses {
				if strings.Contains(string(out), a) {
					t.Errorf("%s: BGP summary contains peer address %s", tc.name, a)
				}
			}
		}
	}
}

func TestInternalNextHops(t *testing.T) {
	r := &redactor{platform: []netip.Prefix{netip.MustParsePrefix("198.18.0.0/15")}}
	for addr, want := range map[string]bool{
		"198.18.4.1":      true,  // platform range
		"10.1.2.3":        true,  // private
		"100.64.0.1":      true,  // shared address space
		"fe80::1":         true,  // link local
		"fd00::1":         true,  // unique local
		"::ffff:10.1.2.3": true,  // IPv4-mapped private
		"192.0.2.3":       false, // documentation, used by external peers in tests
		"2001:db8:2::3":   false,
		"198.51.100.1":    false,
	} {
		if got := r.internal(netip.MustParseAddr(addr)); got != want {
			t.Errorf("internal(%s) = %v, want %v", addr, got, want)
		}
	}
}

func TestProbeRedaction(t *testing.T) {
	r := &redactor{}
	got := r.ping(&driver.PingResult{
		Source:      netip.MustParseAddr("10.0.0.5"),
		Destination: netip.MustParseAddr("198.51.100.3"),
		Sent:        2,
		Replies: []driver.ProbeReply{
			{Sequence: 1, Address: netip.MustParseAddr("10.9.9.9"), ICMPType: "destination-unreachable", ICMPCode: 1},
			{Sequence: 2, Address: netip.MustParseAddr("198.51.100.3"), ICMPType: "echo-reply"},
		},
	})
	if got.GetSource() != "" || !got.GetReplies()[0].GetInternal() || got.GetReplies()[0].GetAddress() != "" ||
		got.GetReplies()[1].GetInternal() || got.GetReplies()[1].GetAddress() != "198.51.100.3" {
		t.Errorf("ping = %v, want the private source empty, the private reply internal and the public reply kept", got)
	}
}
