package manager

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	lgv1alpha "go.datum.net/looking-glass/api/v1alpha"
	"go.datum.net/looking-glass/internal/config"
)

// TestManagerBindsSessionsWithStaticPlacement runs the real manager wiring and
// checks that a session is bound to its configured cell while still Pending.
func TestManagerBindsSessionsWithStaticPlacement(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS is unset; run `task test` to provide envtest binaries")
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	restCfg, err := env.Start()
	if err != nil {
		t.Fatalf("starting envtest: %v", err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Errorf("stopping envtest: %v", err)
		}
	})

	cfg := config.Default()
	cfg.Metrics.BindAddress = "0"
	cfg.Health.BindAddress = freeAddr(t)
	cfg.Placement.Static = map[string]string{testLoc: "cell-1"}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, restCfg, cfg) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("manager: %v", err)
		}
	})
	waitForOK(t, "http://"+cfg.Health.BindAddress+"/readyz")

	scheme, err := NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	admin, err := client.New(restCfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	s := &lgv1alpha.LookingGlassSession{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "lg-", Namespace: testNS},
		Spec: lgv1alpha.LookingGlassSessionSpec{
			Location: testLoc,
			Diagnostic: lgv1alpha.Diagnostic{
				Type: lgv1alpha.DiagnosticPing,
				Ping: &lgv1alpha.PingDiagnostic{Destination: publicV4},
			},
			ClientPublicKey: testKey,
		},
	}
	if err := admin.Create(t.Context(), s); err != nil {
		t.Fatalf("creating session: %v", err)
	}

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if err := admin.Get(t.Context(), client.ObjectKeyFromObject(s), s); err != nil {
			t.Fatal(err)
		}
		if s.Status.Cell != "" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if s.Status.Cell != "cell-1" || s.State() != lgv1alpha.SessionStatePending {
		t.Fatalf("cell, state = %q, %v; want cell-1, Pending", s.Status.Cell, s.State())
	}
}
