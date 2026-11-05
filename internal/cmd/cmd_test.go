package cmd

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

func TestFakeBackendStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	root := New(ctx)
	root.SetArgs([]string{"fake-backend", "--listen", "127.0.0.1:0"})

	done := make(chan error, 1)
	go func() { done <- root.Execute() }()
	time.Sleep(200 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("fake-backend returned %v after cancel", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("fake-backend did not stop after cancel")
	}
}

func TestManagerRejectsInvalidConfig(t *testing.T) {
	root := New(t.Context())
	root.SetArgs([]string{"manager", "--config", "testdata/missing.yaml"})
	if err := root.Execute(); err == nil {
		t.Fatal("manager started with a missing config file")
	}
}

func TestExecuteReportsErrors(t *testing.T) {
	t.Run("before logger setup", func(t *testing.T) {
		var stderr bytes.Buffer
		if code := Execute(t.Context(), []string{"--no-such-flag"}, &stderr); code != 1 {
			t.Fatalf("exit code = %d, want 1", code)
		}
		if !strings.HasPrefix(stderr.String(), "error: unknown flag") {
			t.Errorf("stderr = %q, want a plain error line", stderr.String())
		}
	})
	t.Run("after logger setup", func(t *testing.T) {
		// The logger writes the error event; stderr gets no second copy.
		var stderr bytes.Buffer
		code := Execute(t.Context(), []string{"manager", "--config", "testdata/missing.yaml", "--zap-log-level", "panic"}, &stderr)
		if code != 1 {
			t.Fatalf("exit code = %d, want 1", code)
		}
		if stderr.Len() != 0 {
			t.Errorf("stderr = %q, want it empty when the logger reported the error", stderr.String())
		}
	})
}
