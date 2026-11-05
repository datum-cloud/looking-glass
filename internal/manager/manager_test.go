package manager

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"go.datum.net/looking-glass/internal/config"
)

// startEnv starts a local API server. `task test` sets KUBEBUILDER_ASSETS; a
// plain `go test` without it skips, so unit tests still run anywhere.
func startEnv(t *testing.T) *envtest.Environment {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS is unset; run `task test` to provide envtest binaries")
	}
	env := &envtest.Environment{}
	if _, err := env.Start(); err != nil {
		t.Fatalf("starting envtest: %v", err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Errorf("stopping envtest: %v", err)
		}
	})
	return env
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

func get(t *testing.T, url string) (*http.Response, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	return http.DefaultClient.Do(req)
}

func waitForStatus(t *testing.T, url string, want int) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		resp, err := get(t, url)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == want {
				return
			}
			last = fmt.Sprintf("status %d", resp.StatusCode)
		} else {
			last = err.Error()
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("GET %s never returned %d; last result: %s", url, want, last)
}

func TestRunServesProbesAndShutsDown(t *testing.T) {
	env := startEnv(t)

	cfg := config.Default()
	cfg.Metrics.BindAddress = freeAddr(t)
	cfg.Health.BindAddress = freeAddr(t)
	cfg.GracefulShutdownTimeout = metav1.Duration{Duration: 5 * time.Second}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Run(ctx, env.Config, cfg) }()

	base := "http://" + cfg.Health.BindAddress
	waitForStatus(t, base+"/healthz", http.StatusOK)
	waitForStatus(t, base+"/readyz", http.StatusOK)
	waitForStatus(t, "http://"+cfg.Metrics.BindAddress+"/metrics", http.StatusOK)

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v after cancel", err)
		}
	case <-time.After(cfg.GracefulShutdownTimeout.Duration + 5*time.Second):
		t.Fatal("Run did not return within the graceful shutdown timeout")
	}

	// The probe server must be gone once Run returns.
	if resp, err := get(t, base+"/healthz"); err == nil {
		_ = resp.Body.Close()
		t.Fatal("health endpoint still serving after shutdown")
	}
}

func TestNewRequiresRESTConfig(t *testing.T) {
	cfg := config.Default()
	cfg.Metrics.BindAddress = "0"
	cfg.Health.BindAddress = freeAddr(t)
	if _, err := New(nil, cfg); err == nil {
		t.Fatal("New accepted a nil REST config")
	}
}
