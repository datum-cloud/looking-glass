package diagnostics

import (
	"encoding/json"
	"strings"
	"testing"

	backendv1 "go.datum.net/looking-glass/api/lookingglass/backend/v1"
)

// TestInternalProbeRepliesKeepNoAddress checks that a reply the backend marks
// internal reaches the client with the marker and without an address, even
// if the backend also sent one.
func TestInternalProbeRepliesKeepNoAddress(t *testing.T) {
	got := probes([]*backendv1.ProbeReply{
		{Sequence: 1, Internal: true, Address: "10.0.0.1", IcmpType: "time-exceeded"},
		{Sequence: 2, Address: "198.51.100.3", IcmpType: "echo-reply"},
	})
	if !got[0].Internal || got[0].Address != "" || got[1].Internal || got[1].Address != "198.51.100.3" {
		t.Fatalf("probes = %+v, want the first internal without an address and the second public", got)
	}
	out, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"internal":true`) || strings.Contains(string(out), "10.0.0.1") {
		t.Errorf("frame JSON = %s, want internal true and no private address", out)
	}
}
