// Package frrtest replays the FRR output recorded in pkg/driver/frr/testdata,
// so tests can run the frr driver without a router.
package frrtest

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"go.datum.net/looking-glass/pkg/driver"
	"go.datum.net/looking-glass/pkg/driver/frr"
)

// Recorded maps each command line testdata/record.sh ran on the router under
// test to the file holding its output.
var Recorded = map[string]string{
	`vtysh -c 'show ip route 198.51.100.7 json'`:                            "route-v4-address.json",
	`vtysh -c 'show ip route 198.51.100.0/24 json'`:                         "route-v4-prefix.json",
	`vtysh -c 'show ip route 198.51.100.0/25 json'`:                         "route-v4-exact-missing.json",
	`vtysh -c 'show ip route 203.0.113.0/24 json'`:                          "route-v4-internal.json",
	`vtysh -c 'show ip route 198.18.0.0/15 json'`:                           "route-v4-static.json",
	`vtysh -c 'show ip route 192.0.2.9 json'`:                               "route-v4-connected.json",
	`vtysh -c 'show ipv6 route 2001:db8:100::7 json'`:                       "route-v6-address.json",
	`vtysh -c 'show bgp ipv4 unicast 198.51.100.0/24 json'`:                 "bgp-v4-prefix.json",
	`vtysh -c 'show bgp ipv4 unicast 203.0.113.0/24 json'`:                  "bgp-v4-internal.json",
	`vtysh -c 'show bgp ipv6 unicast 2001:db8:100::/48 json'`:               "bgp-v6-prefix.json",
	`vtysh -c 'show bgp ipv4 unicast summary json'`:                         "bgp-v4-summary.json",
	`vtysh -c 'show bgp ipv6 unicast summary json'`:                         "bgp-v6-summary.json",
	`ping -4 -n -c 3 -W 1 -s 56 -I 192.0.2.2 198.51.100.3`:                  "ping-v4.txt",
	`ping -4 -n -c 3 -W 1 -s 56 -I 192.0.2.2 198.51.100.200`:                "ping-v4-loss.txt",
	`ping -6 -n -c 3 -W 1 -s 56 -I 2001:db8:2::2 2001:db8:100::3`:           "ping-v6.txt",
	`traceroute -4 -n -q 3 -m 20 -w 1 -s 192.0.2.2 198.51.100.3 84`:         "trace-v4.txt",
	`traceroute -4 -n -q 3 -m 4 -w 1 -s 192.0.2.2 198.51.100.200 84`:        "trace-v4-loss.txt",
	`traceroute -6 -n -q 3 -m 20 -w 1 -s 2001:db8:2::2 2001:db8:100::3 104`: "trace-v6.txt",
	`traceroute -4 -n -q 3 -m 20 -w 1 -s 192.0.2.2 198.51.100.4 84`:         "trace-v4-private.txt",
}

// lossy are the recordings whose command exited 1: ping with replies missing.
var lossy = map[string]bool{"ping-v4-loss.txt": true}

// Settings are the recorded router's probe sources.
var Settings = map[string]string{
	frr.SettingProbeSourceIPv4: "192.0.2.2",
	frr.SettingProbeSourceIPv6: "2001:db8:2::2",
}

// Replay is an frr.Runner that answers from the recordings. A command that
// was not recorded fails the test.
type Replay struct {
	T testing.TB
}

// Run returns the recorded output of command.
func (r Replay) Run(_ context.Context, command string, _ int) ([]byte, error) {
	name, ok := Recorded[command]
	if !ok {
		r.T.Errorf("command %q was not recorded", command)
		return nil, errors.New("not recorded")
	}
	out, err := os.ReadFile(filepath.Join(dir(), name))
	if err != nil {
		r.T.Fatal(err)
	}
	if lossy[name] {
		return out, &frr.ExitError{Status: 1}
	}
	return out, nil
}

// Close does nothing.
func (Replay) Close() error { return nil }

// New returns an frr driver for the recorded router that replays its output.
func New(t testing.TB, name string) driver.Driver {
	d, err := frr.New(driver.Router{Name: name, Platform: frr.Platform, Settings: Settings}, Replay{T: t})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func dir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "testdata")
}
