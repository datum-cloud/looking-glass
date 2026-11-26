package admission

import (
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

func TestDestinationPolicy(t *testing.T) {
	public := DestinationPolicy{Deny: mustPrefixes("2606:4700:10::/48")}
	operator := DestinationPolicy{Allow: mustPrefixes("10.255.255.0/24")}
	tests := []struct {
		policy DestinationPolicy
		addr   string
		ok     bool
	}{
		{public, "1.1.1.1", true},
		{public, "2606:4700::1111", true},
		{public, "10.1.2.3", false},
		{public, "100.64.0.1", false},
		{public, "127.0.0.1", false},
		{public, "169.254.1.1", false},
		{public, "172.16.0.1", false},
		{public, "192.168.1.1", false},
		{public, "192.0.2.1", false},
		{public, "198.18.0.1", false},
		{public, "198.51.100.7", false},
		{public, "203.0.113.9", false},
		{public, "224.0.0.1", false},
		{public, "255.255.255.255", false},
		{public, "0.0.0.0", false},
		{public, "::1", false},
		{public, "::", false},
		{public, "fd00::1", false},
		{public, "fe80::1", false},
		{public, "ff02::1", false},
		{public, "2001:db8::1", false},
		{public, "3fff::1", false},
		{public, "5f00::1", false},
		{public, "64:ff9b::808:808", false},
		{public, "2002:c000:204::1", false},
		{public, "2001:0::1", false},
		{public, "4000::1", false},
		{public, "2606:4700:10::1", false},
		{public, "10.255.255.1", false},
		{operator, "10.255.255.1", true},
		{operator, "10.255.254.1", false},
		{operator, "::ffff:10.255.255.1", true},
	}
	for _, tt := range tests {
		err := tt.policy.Check(netip.MustParseAddr(tt.addr))
		if (err == nil) != tt.ok {
			t.Errorf("Check(%s) = %v, want ok=%v", tt.addr, err, tt.ok)
		}
	}
	if err := public.Check(netip.Addr{}); err == nil {
		t.Error("Check accepted the zero address")
	}
	if err := public.Check(netip.MustParseAddr("2606:4700::1111%eth0")); err == nil {
		t.Error("Check accepted a zoned address")
	}
}

func TestCheckRouteTarget(t *testing.T) {
	policy := DestinationPolicy{
		Deny:  mustPrefixes("203.0.113.0/24", "2001:db8:ff::/48"),
		Allow: mustPrefixes("203.0.113.128/25"),
	}
	tests := []struct {
		target string
		ok     bool
	}{
		// Special-use ranges send no traffic, so route lookup may read them.
		{"192.0.2.0/24", true},
		{"10.0.0.0/8", true},
		{"198.51.100.7/32", true},
		// A target inside, containing or equal to a platform range is refused.
		{"203.0.113.9/32", false},
		{"203.0.113.0/24", false},
		{"203.0.0.0/16", false},
		{"0.0.0.0/0", false},
		{"2001:db8:ff:1::/64", false},
		{"2001:db8::/32", false},
		{"::ffff:203.0.113.9/128", false},
		// A target inside an exception is allowed.
		{"203.0.113.200/32", true},
		{"203.0.113.128/25", true},
		{"::ffff:203.0.113.200/128", true},
	}
	for _, tt := range tests {
		err := policy.CheckRouteTarget(netip.MustParsePrefix(tt.target))
		if (err == nil) != tt.ok {
			t.Errorf("CheckRouteTarget(%s) = %v, want ok=%v", tt.target, err, tt.ok)
		}
	}
	if err := policy.CheckRouteTarget(netip.Prefix{}); err == nil {
		t.Error("CheckRouteTarget accepted the zero prefix")
	}
}

// TestSpecialUseMatchesGalactic fails when admission's special-use list drifts
// from the one in the Galactic module go.mod pins, so a Galactic bump that
// changes the public destination policy also changes admission.
func TestSpecialUseMatchesGalactic(t *testing.T) {
	out, err := exec.CommandContext(t.Context(), "go", "list", "-m", "-f", "{{.Dir}}", "go.datum.net/galactic").Output()
	if err != nil {
		t.Fatalf("locating the galactic module: %v", err)
	}
	dir := strings.TrimSpace(string(out))
	if dir == "" {
		t.Fatal("the galactic module is not downloaded; run `go mod download go.datum.net/galactic`")
	}
	src, err := os.ReadFile(filepath.Join(dir, "internal", "fabric", "query", "destination.go"))
	if err != nil {
		t.Fatal(err)
	}
	block := regexp.MustCompile(`(?s)var specialUse = mustPrefixes\((.*?)\n\)`).FindSubmatch(src)
	if block == nil {
		t.Fatal("galactic destination.go no longer declares specialUse; update this test and the mirror")
	}
	var want []netip.Prefix
	// One prefix literal per line; trailing comments may quote words too.
	for _, m := range regexp.MustCompile(`(?m)^\s*"([^"]+)",`).FindAllSubmatch(block[1], -1) {
		want = append(want, netip.MustParsePrefix(string(m[1])).Masked())
	}
	if len(want) == 0 {
		t.Fatal("parsed no prefixes from galactic's specialUse")
	}
	if !slices.Equal(specialUse, want) {
		t.Errorf("specialUse drifted from galactic:\n got  %v\n want %v", specialUse, want)
	}
	if !regexp.MustCompile(`globalUnicastV6 = netip.MustParsePrefix\("2000::/3"\)`).Match(src) {
		t.Error("galactic's IPv6 global unicast range changed")
	}
}
