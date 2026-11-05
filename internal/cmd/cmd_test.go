package cmd

import (
	"context"
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
