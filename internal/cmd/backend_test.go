package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.datum.net/looking-glass/pkg/driver"
)

// cmdTestPlatform is a driver platform registered for these tests only.
const cmdTestPlatform = "cmd-test"

func init() {
	driver.Register(cmdTestPlatform, func(driver.Router) (driver.Driver, error) { return nopDriver{}, nil })
}

type nopDriver struct{ driver.Driver }

func (nopDriver) Capabilities() driver.Capabilities { return driver.Capabilities{} }
func (nopDriver) Close() error                      { return nil }

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "backend.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const backendConfig = `cell: cell-a
listen: 127.0.0.1:0
metrics:
  bindAddress: 127.0.0.1:0
locations:
  - name: example-location
    routers:
      - name: router-a
        platform: cmd-test
        asn: 64496
`

func TestBackendStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	root := New(ctx)
	root.SetArgs([]string{"backend", "--config", writeConfig(t, backendConfig)})
	done := make(chan error, 1)
	go func() { done <- root.Execute() }()
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("backend returned %v after cancel", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("backend did not stop after cancel")
	}
}

func TestBackendRejectsConfig(t *testing.T) {
	tests := map[string]struct {
		config string
		want   string
	}{
		"plaintext on a public address": {strings.Replace(backendConfig, "127.0.0.1:0\nmetrics", "0.0.0.0:9090\nmetrics", 1), "tls.certFile"},
		"unregistered platform":         {strings.Replace(backendConfig, "cmd-test", "no-such-platform", 1), "no driver for platform"},
		"unknown field":                 {backendConfig + "color: blue\n", "color"},
		"missing TLS files": {strings.Replace(backendConfig, "listen: 127.0.0.1:0\n",
			"listen: 127.0.0.1:0\ntls:\n  certFile: /nonexistent/tls.crt\n  keyFile: /nonexistent/tls.key\n  clientCAFile: /nonexistent/ca.crt\n", 1), "tls.certFile"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			root := New(t.Context())
			root.SetArgs([]string{"backend", "--config", writeConfig(t, tt.config)})
			err := root.Execute()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("backend = %v, want an error mentioning %q", err, tt.want)
			}
		})
	}
	root := New(t.Context())
	root.SetArgs([]string{"backend"})
	if err := root.Execute(); err == nil {
		t.Error("backend started without --config")
	}
}
