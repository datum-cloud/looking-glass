package driver_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	backendv1 "go.datum.net/looking-glass/api/lookingglass/backend/v1"
	"go.datum.net/looking-glass/pkg/driver"
)

type stubDriver struct {
	driver.Driver
	router driver.Router
}

func TestRegistry(t *testing.T) {
	driver.Register("test-registry", func(r driver.Router) (driver.Driver, error) {
		if r.Settings["mode"] == "bad" {
			return nil, errors.New("unsupported mode")
		}
		return stubDriver{router: r}, nil
	})
	if !slices.Contains(driver.Platforms(), "test-registry") {
		t.Fatalf("Platforms() = %v, want it to list test-registry", driver.Platforms())
	}

	d, err := driver.New(driver.Router{Name: "r1", Platform: "test-registry"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := d.(stubDriver).router.Name; got != "r1" {
		t.Errorf("factory got router %q, want r1", got)
	}

	_, err = driver.New(driver.Router{Name: "r2", Platform: "test-registry", Settings: map[string]string{"mode": "bad"}})
	if err == nil || !strings.Contains(err.Error(), `router "r2"`) || !strings.Contains(err.Error(), "unsupported mode") {
		t.Errorf("New with a factory error = %v, want it to name the router and the cause", err)
	}

	_, err = driver.New(driver.Router{Name: "r3", Platform: "no-such-platform"})
	if err == nil || !strings.Contains(err.Error(), `"no-such-platform"`) || !strings.Contains(err.Error(), "test-registry") {
		t.Errorf("New with an unknown platform = %v, want it to name the platform and list registered ones", err)
	}
}

func TestRegisterPanics(t *testing.T) {
	f := func(driver.Router) (driver.Driver, error) { return nil, nil }
	driver.Register("test-duplicate", f)
	for name, register := range map[string]func(){
		"duplicate":   func() { driver.Register("test-duplicate", f) },
		"no platform": func() { driver.Register("", f) },
		"no factory":  func() { driver.Register("test-nil", nil) },
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("Register did not panic")
				}
			}()
			register()
		})
	}
}

func TestReasonOf(t *testing.T) {
	tests := []struct {
		err  error
		want driver.Reason
	}{
		{driver.Errorf(driver.ReasonRouterUnreachable, "dial failed"), driver.ReasonRouterUnreachable},
		{fmt.Errorf("query: %w", driver.Errorf(driver.ReasonParseError, "bad reply")), driver.ReasonParseError},
		{context.DeadlineExceeded, driver.ReasonTimeout},
		{fmt.Errorf("read: %w", context.Canceled), driver.ReasonCancelled},
		{errors.New("something else"), driver.ReasonInternal},
	}
	for _, tt := range tests {
		if got := driver.ReasonOf(tt.err); got != tt.want {
			t.Errorf("ReasonOf(%v) = %s, want %s", tt.err, got, tt.want)
		}
	}
}

func TestErrorUnwrapsCause(t *testing.T) {
	cause := errors.New("connection reset")
	err := &driver.Error{Reason: driver.ReasonRouterUnavailable, Message: "session closed", Err: cause}
	if !errors.Is(err, cause) {
		t.Error("errors.Is does not find the cause")
	}
	if got := err.Error(); !strings.Contains(got, "RouterUnavailable") || !strings.Contains(got, "session closed") {
		t.Errorf("Error() = %q, want the reason and message", got)
	}
}

// TestReasonsAreBackendReasons pins every driver reason to a backend reason
// code, so the backend can report a driver failure without translation.
func TestReasonsAreBackendReasons(t *testing.T) {
	backend := []string{
		backendv1.ReasonRouterUnreachable, backendv1.ReasonRouterUnavailable, backendv1.ReasonRouterBusy,
		backendv1.ReasonTimeout, backendv1.ReasonCancelled, backendv1.ReasonQueryTypeUnavailable,
		backendv1.ReasonProbeSourceUnavailable, backendv1.ReasonDestinationNotAllowed,
		backendv1.ReasonResponseTooLarge, backendv1.ReasonParseError, backendv1.ReasonInternal,
	}
	for _, r := range []driver.Reason{
		driver.ReasonRouterUnreachable, driver.ReasonRouterUnavailable, driver.ReasonRouterBusy,
		driver.ReasonTimeout, driver.ReasonCancelled, driver.ReasonQueryTypeUnavailable,
		driver.ReasonProbeSourceUnavailable, driver.ReasonDestinationNotAllowed,
		driver.ReasonResponseTooLarge, driver.ReasonParseError, driver.ReasonInternal,
	} {
		if !slices.Contains(backend, string(r)) {
			t.Errorf("driver reason %s is not a backend reason code", r)
		}
	}
}

func TestCapabilitiesServes(t *testing.T) {
	c := driver.Capabilities{Diagnostics: []driver.DiagnosticCapability{
		{Type: driver.DiagnosticPing, Families: []driver.Family{driver.FamilyIPv4}},
	}}
	if !c.Serves(driver.DiagnosticPing, driver.FamilyIPv4) {
		t.Error("Serves(Ping, IPv4) = false, want true")
	}
	if c.Serves(driver.DiagnosticPing, driver.FamilyIPv6) || c.Serves(driver.DiagnosticTraceroute, driver.FamilyIPv4) {
		t.Error("Serves reported a diagnostic or family the router does not serve")
	}
}
