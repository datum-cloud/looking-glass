package manager

import (
	"os"
	"path/filepath"
	"testing"

	"go.datum.net/looking-glass/internal/config"
	"go.datum.net/looking-glass/internal/placement"
)

// syntheticKubeconfig is a kubeconfig whose server is never dialed: building a
// cluster.Cluster does not connect until it starts.
const syntheticKubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: hub
  cluster:
    server: https://127.0.0.1:1
contexts:
- name: hub
  context:
    cluster: hub
    user: hub
current-context: hub
users:
- name: hub
  user:
    token: synthetic-token
`

func TestNewPlacerStatic(t *testing.T) {
	cells := map[string]string{"loc-a": "cell-a"}
	placer, hub, err := NewPlacer(config.PlacementConfig{Static: cells})
	if err != nil {
		t.Fatalf("NewPlacer: %v", err)
	}
	if hub != nil {
		t.Errorf("hub = %v, want nil for static placement", hub)
	}
	static, ok := placer.(placement.StaticPlacer)
	if !ok {
		t.Fatalf("placer is %T, want placement.StaticPlacer", placer)
	}
	if static.Cells["loc-a"] != "cell-a" {
		t.Errorf("Cells = %v, want the configured map", static.Cells)
	}
}

func TestNewPlacerFederation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte(syntheticKubeconfig), 0o600); err != nil {
		t.Fatalf("writing kubeconfig: %v", err)
	}
	placer, hub, err := NewPlacer(config.PlacementConfig{
		Federation: &config.FederationPlacementConfig{
			Kubeconfig:    path,
			LocationLabel: "example.test/location",
			OptInLabel:    "example.test/enabled",
		},
	})
	if err != nil {
		t.Fatalf("NewPlacer: %v", err)
	}
	if hub == nil {
		t.Fatal("hub is nil, want a federation hub cluster")
	}
	cp, ok := placer.(placement.ClusterPlacer)
	if !ok {
		t.Fatalf("placer is %T, want placement.ClusterPlacer", placer)
	}
	if cp.LocationLabel != "example.test/location" {
		t.Errorf("LocationLabel = %q", cp.LocationLabel)
	}
	if cp.OptInLabel != "example.test/enabled" {
		t.Errorf("OptInLabel = %q", cp.OptInLabel)
	}
	if cp.Reader == nil {
		t.Error("Reader is nil, want the hub client")
	}
}

func TestNewPlacerFederationMissingKubeconfig(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent")
	placer, hub, err := NewPlacer(config.PlacementConfig{
		Federation: &config.FederationPlacementConfig{Kubeconfig: missing},
	})
	if err == nil {
		t.Fatal("NewPlacer succeeded with a missing kubeconfig, want an error")
	}
	if placer != nil || hub != nil {
		t.Errorf("got placer %v and hub %v alongside the error, want nil", placer, hub)
	}
}
