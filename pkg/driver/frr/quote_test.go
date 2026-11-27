package frr

import (
	"os/exec"
	"testing"
)

// TestShellQuote runs each quoted string through a real shell and checks it
// comes back as one unchanged word, with nothing expanded or executed.
func TestShellQuote(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh on PATH")
	}
	for _, s := range []string{
		"show ip route 198.51.100.0/24 json",
		"it's",
		"$(id) `id` ${HOME} !1 \\ \" ; | & > <",
		"'; touch /tmp/pwned; '",
	} {
		out, err := exec.CommandContext(t.Context(), sh, "-c", "printf %s "+shellQuote(s)).Output()
		if err != nil {
			t.Fatalf("sh -c with %q: %v", s, err)
		}
		if string(out) != s {
			t.Errorf("shell saw %q, want %q", out, s)
		}
	}
}
