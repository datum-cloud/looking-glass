// Package frr is the reference driver for routers that run FRRouting
// (https://frrouting.org) on Linux. It reads the routing table and BGP state
// as vtysh JSON, and probes with iputils ping and Linux traceroute. It runs
// each command over SSH.
//
// The driver registers itself as platform "frr". A router's settings are:
//
//   - user: the SSH user. It must be able to run vtysh, ping and traceroute.
//   - privateKeyFile: the PEM private key the driver authenticates with.
//   - knownHostsFile: an OpenSSH known_hosts file holding the router's host
//     key. The driver never connects to a host whose key it cannot verify.
//   - probeSourceIPv4, probeSourceIPv6: the address each family's probes are
//     sent from. Ping and traceroute are offered only for a family that has
//     one.
//
// The router's Address is host or host:port; the port defaults to 22.
package frr

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/netip"
	"strconv"
	"strings"

	"go.datum.net/looking-glass/pkg/driver"
)

// Platform is the platform name the driver registers.
const Platform = "frr"

// Settings keys.
const (
	SettingUser            = "user"
	SettingPrivateKeyFile  = "privateKeyFile"
	SettingKnownHostsFile  = "knownHostsFile"
	SettingProbeSourceIPv4 = "probeSourceIPv4"
	SettingProbeSourceIPv6 = "probeSourceIPv6"
)

// MaxOutputBytes bounds the output the driver reads from one command. A
// longer output fails with ReasonResponseTooLarge.
const MaxOutputBytes = 4 << 20

// IPv4 and IPv6 header sizes, which traceroute counts in its packet length
// and ping does not.
const (
	ipv4ICMPHeaderBytes = 28
	ipv6UDPHeaderBytes  = 48
)

func init() {
	driver.Register(Platform, Factory)
}

// Runner runs one command on a router and returns its standard output.
// A command that exits non-zero returns its output and an *ExitError. A
// Runner must be safe for concurrent use.
type Runner interface {
	Run(ctx context.Context, command string, limit int) ([]byte, error)
	Close() error
}

// ExitError reports a command that exited with a non-zero status.
type ExitError struct {
	Status int
}

func (e *ExitError) Error() string { return fmt.Sprintf("exit status %d", e.Status) }

// Factory validates r's settings and returns a driver that reaches the router
// over SSH. It reads the key and known_hosts files and performs no network
// I/O.
func Factory(r driver.Router) (driver.Driver, error) {
	runner, err := NewSSHRunner(r.Address, r.Settings)
	if err != nil {
		return nil, err
	}
	return New(r, runner)
}

// New returns a driver for r that runs its commands with runner.
func New(r driver.Router, runner Runner) (driver.Driver, error) {
	d := &Driver{runner: runner}
	for key, dst := range map[string]*netip.Addr{SettingProbeSourceIPv4: &d.sourceV4, SettingProbeSourceIPv6: &d.sourceV6} {
		s, ok := r.Settings[key]
		if !ok {
			continue
		}
		a, err := netip.ParseAddr(s)
		if err != nil || a.Zone() != "" || (key == SettingProbeSourceIPv4) != a.Is4() {
			return nil, fmt.Errorf("setting %s: %q is not a %s address", key, s, strings.TrimPrefix(key, "probeSource"))
		}
		*dst = a
	}
	for key := range r.Settings {
		switch key {
		case SettingUser, SettingPrivateKeyFile, SettingKnownHostsFile, SettingProbeSourceIPv4, SettingProbeSourceIPv6:
		default:
			return nil, fmt.Errorf("unknown setting %q", key)
		}
	}
	return d, nil
}

// Driver runs diagnostics on one FRR router.
type Driver struct {
	runner   Runner
	sourceV4 netip.Addr
	sourceV6 netip.Addr
}

// Capabilities reports route lookups and BGP summaries for both families,
// and ping and traceroute for each family with a probe source.
func (d *Driver) Capabilities() driver.Capabilities {
	both := []driver.Family{driver.FamilyIPv4, driver.FamilyIPv6}
	var probe []driver.Family
	if d.sourceV4.IsValid() {
		probe = append(probe, driver.FamilyIPv4)
	}
	if d.sourceV6.IsValid() {
		probe = append(probe, driver.FamilyIPv6)
	}
	caps := driver.Capabilities{Diagnostics: []driver.DiagnosticCapability{
		{Type: driver.DiagnosticRouteLookup, Families: both},
		{Type: driver.DiagnosticBGPSummary, Families: both},
	}}
	if len(probe) > 0 {
		caps.Diagnostics = append(caps.Diagnostics,
			driver.DiagnosticCapability{Type: driver.DiagnosticPing, Families: probe},
			driver.DiagnosticCapability{Type: driver.DiagnosticTraceroute, Families: probe})
	}
	return caps
}

// RouteLookup reads the zebra RIB entry for the target and, for BGP routes,
// the BGP table's paths for the matched prefix.
func (d *Driver) RouteLookup(ctx context.Context, req driver.RouteLookupRequest) (*driver.RouteResult, error) {
	target := req.Target.Masked()
	ipv := "ip"
	if target.Addr().Is6() {
		ipv = "ipv6"
	}
	arg := target.String()
	exact := target.Bits() != target.Addr().BitLen()
	if !exact {
		arg = target.Addr().String()
	}
	out, err := d.vtysh(ctx, fmt.Sprintf("show %s route %s json", ipv, arg))
	if err != nil {
		return nil, err
	}
	rib, err := parseRIB(out)
	if err != nil {
		return nil, err
	}
	res := &driver.RouteResult{Exact: exact}
	for _, prefix := range rib.prefixes() {
		bgp := func() ([]bgpPath, error) {
			out, err := d.vtysh(ctx, fmt.Sprintf("show bgp %s unicast %s json", afi(prefix.Addr()), prefix))
			if err != nil {
				return nil, err
			}
			return parseBGPPrefix(out)
		}
		pr, err := rib.routes(prefix, req.MaxPaths, bgp)
		if err != nil {
			return nil, err
		}
		res.Prefixes = append(res.Prefixes, pr)
	}
	return res, nil
}

// BGPSummary reads one family's BGP unicast summary.
func (d *Driver) BGPSummary(ctx context.Context, req driver.BGPSummaryRequest) (*driver.BGPSummary, error) {
	family := "ipv4"
	if req.Family == driver.FamilyIPv6 {
		family = "ipv6"
	}
	out, err := d.vtysh(ctx, fmt.Sprintf("show bgp %s unicast summary json", family))
	if err != nil {
		return nil, err
	}
	return parseBGPSummary(out)
}

// Ping runs iputils ping from the family's probe source.
func (d *Driver) Ping(ctx context.Context, req driver.PingRequest) (*driver.PingResult, error) {
	src, flag, err := d.source(req.Destination)
	if err != nil {
		return nil, err
	}
	cmd := fmt.Sprintf("ping %s -n -c %d -W %d -s %d -I %s %s",
		flag, req.Count, waitSeconds(req.Wait.Seconds()), req.PayloadBytes, src, req.Destination)
	out, err := d.runner.Run(ctx, cmd, MaxOutputBytes)
	var exit *ExitError
	// ping exits 1 when replies are missing, which is a result, not a failure.
	if err != nil && (!errors.As(err, &exit) || exit.Status != 1) {
		return nil, d.fail(ctx, err, "ping")
	}
	res, err := parsePing(out, req.Count)
	if err != nil {
		return nil, err
	}
	res.Source = src
	res.Destination = req.Destination
	return res, nil
}

// Traceroute runs Linux traceroute (UDP probes) from the family's probe
// source.
func (d *Driver) Traceroute(ctx context.Context, req driver.TracerouteRequest) (*driver.TracerouteResult, error) {
	src, flag, err := d.source(req.Destination)
	if err != nil {
		return nil, err
	}
	header := ipv4ICMPHeaderBytes
	if req.Destination.Is6() {
		header = ipv6UDPHeaderBytes
	}
	cmd := fmt.Sprintf("traceroute %s -n -q %d -m %d -w %d -s %s %s %d",
		flag, req.ProbesPerHop, req.MaxHops, waitSeconds(req.Wait.Seconds()), src, req.Destination, req.PayloadBytes+header)
	out, err := d.runner.Run(ctx, cmd, MaxOutputBytes)
	if err != nil {
		return nil, d.fail(ctx, err, "traceroute")
	}
	res, err := parseTraceroute(out, req.Destination)
	if err != nil {
		return nil, err
	}
	res.Source = src
	return res, nil
}

// Close closes the runner.
func (d *Driver) Close() error { return d.runner.Close() }

func (d *Driver) vtysh(ctx context.Context, command string) ([]byte, error) {
	out, err := d.runner.Run(ctx, "vtysh -c "+strconv.Quote(command), MaxOutputBytes)
	if err != nil {
		return nil, d.fail(ctx, err, "vtysh")
	}
	return out, nil
}

// fail classifies a runner error. The context's state wins, so a command cut
// short by a deadline reports Timeout rather than the broken session.
func (d *Driver) fail(ctx context.Context, err error, what string) error {
	var de *driver.Error
	var exit *ExitError
	switch {
	case ctx.Err() != nil:
		return &driver.Error{Reason: driver.ReasonOf(ctx.Err()), Message: what + " did not finish", Err: ctx.Err()}
	case errors.As(err, &de):
		return err
	case errors.As(err, &exit):
		return &driver.Error{Reason: driver.ReasonRouterUnavailable, Message: fmt.Sprintf("%s exited with status %d", what, exit.Status), Err: err}
	default:
		return &driver.Error{Reason: driver.ReasonInternal, Message: what + " failed", Err: err}
	}
}

func (d *Driver) source(dst netip.Addr) (netip.Addr, string, error) {
	src, flag := d.sourceV4, "-4"
	if dst.Is6() {
		src, flag = d.sourceV6, "-6"
	}
	if !src.IsValid() {
		return netip.Addr{}, "", driver.Errorf(driver.ReasonProbeSourceUnavailable, "no probe source for the destination's family")
	}
	return src, flag, nil
}

func afi(a netip.Addr) string {
	if a.Is4() {
		return "ipv4"
	}
	return "ipv6"
}

// waitSeconds rounds a wait up to whole seconds, at least one, which every
// ping and traceroute version accepts.
func waitSeconds(s float64) int {
	return max(int(math.Ceil(s)), 1)
}
