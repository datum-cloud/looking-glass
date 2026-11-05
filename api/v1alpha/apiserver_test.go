package v1alpha_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/discovery"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	lgv1alpha "go.datum.net/looking-glass/api/v1alpha"
)

// These tests run the generated CRDs in a real API server, so they cover the
// schema, defaults and CEL rules exactly as a project control plane serves
// them.

const (
	testKey = "3b6a27bcceb6a42d62a3a8d02a6f0d73653215771de243a63ac048a18b59da29"
	testNS  = "default"
)

var (
	k8s  client.Client
	disc discovery.DiscoveryInterface
)

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		// Unit tests still run; the API server tests skip themselves.
		return m.Run()
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		fmt.Fprintf(os.Stderr, "starting envtest: %v\n", err)
		return 1
	}
	defer func() {
		if err := env.Stop(); err != nil {
			fmt.Fprintf(os.Stderr, "stopping envtest: %v\n", err)
		}
	}()
	scheme := runtime.NewScheme()
	if err := lgv1alpha.AddToScheme(scheme); err != nil {
		fmt.Fprintf(os.Stderr, "building scheme: %v\n", err)
		return 1
	}
	if k8s, err = client.New(cfg, client.Options{Scheme: scheme}); err != nil {
		fmt.Fprintf(os.Stderr, "building client: %v\n", err)
		return 1
	}
	if disc, err = discovery.NewDiscoveryClientForConfig(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "building discovery client: %v\n", err)
		return 1
	}
	return m.Run()
}

func requireAPIServer(t *testing.T) context.Context {
	t.Helper()
	if k8s == nil {
		t.Skip("KUBEBUILDER_ASSETS is unset; run `task test` to provide envtest binaries")
	}
	return t.Context()
}

func newSession(d lgv1alpha.Diagnostic) *lgv1alpha.LookingGlassSession {
	return &lgv1alpha.LookingGlassSession{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "lg-", Namespace: testNS},
		Spec: lgv1alpha.LookingGlassSessionSpec{
			Location:        "example-location",
			Diagnostic:      d,
			ClientPublicKey: testKey,
		},
	}
}

func ping(dest string) lgv1alpha.Diagnostic {
	return lgv1alpha.Diagnostic{
		Type: lgv1alpha.DiagnosticPing,
		Ping: &lgv1alpha.PingDiagnostic{Destination: lgv1alpha.ProbeDestination(dest)},
	}
}

func traceroute(dest string) lgv1alpha.Diagnostic {
	return lgv1alpha.Diagnostic{
		Type:       lgv1alpha.DiagnosticTraceroute,
		Traceroute: &lgv1alpha.TracerouteDiagnostic{Destination: lgv1alpha.ProbeDestination(dest)},
	}
}

func routeLookup(dest string, af lgv1alpha.AddressFamily) lgv1alpha.Diagnostic {
	return lgv1alpha.Diagnostic{
		Type: lgv1alpha.DiagnosticRouteLookup,
		RouteLookup: &lgv1alpha.RouteLookupDiagnostic{
			Destination:   lgv1alpha.RouteDestination(dest),
			AddressFamily: af,
		},
	}
}

func bgpSummary(af lgv1alpha.AddressFamily) lgv1alpha.Diagnostic {
	return lgv1alpha.Diagnostic{
		Type:       lgv1alpha.DiagnosticBGPSummary,
		BGPSummary: &lgv1alpha.BGPSummaryDiagnostic{AddressFamily: af},
	}
}

func TestSessionAcceptsValidDiagnostics(t *testing.T) {
	ctx := requireAPIServer(t)
	tests := map[string]lgv1alpha.Diagnostic{
		"route lookup IPv4 address":     routeLookup("192.0.2.10", ""),
		"route lookup IPv6 prefix":      routeLookup("2001:db8::/32", lgv1alpha.AddressFamilyIPv6),
		"route lookup IPv4 host prefix": routeLookup("198.51.100.7/32", lgv1alpha.AddressFamilyIPv4),
		"bgp summary IPv6":              bgpSummary(lgv1alpha.AddressFamilyIPv6),
		"ping IPv4":                     ping("198.51.100.7"),
		"ping IPv6":                     ping("2001:db8::7"),
		"traceroute IPv4":               traceroute("203.0.113.9"),
	}
	for name, d := range tests {
		t.Run(name, func(t *testing.T) {
			s := newSession(d)
			if err := k8s.Create(ctx, s); err != nil {
				t.Fatalf("create rejected: %v", err)
			}
			t.Cleanup(func() { _ = k8s.Delete(context.Background(), s) })
		})
	}
}

func TestSessionDefaultsVantagePoint(t *testing.T) {
	ctx := requireAPIServer(t)
	s := newSession(ping("198.51.100.7"))
	if err := k8s.Create(ctx, s); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8s.Delete(context.Background(), s) })
	if s.Spec.VantagePoint != lgv1alpha.VantagePointFabricEdge {
		t.Errorf("vantagePoint = %q, want %q", s.Spec.VantagePoint, lgv1alpha.VantagePointFabricEdge)
	}
}

func TestSessionRejectsInvalidSpec(t *testing.T) {
	ctx := requireAPIServer(t)
	tests := []struct {
		name   string
		mutate func(*lgv1alpha.LookingGlassSession)
		want   string
	}{
		{"missing location", func(s *lgv1alpha.LookingGlassSession) { s.Spec.Location = "" }, "spec.location"},
		{"location not a DNS label", func(s *lgv1alpha.LookingGlassSession) { s.Spec.Location = "Example_Location" }, "spec.location"},
		{"location too long", func(s *lgv1alpha.LookingGlassSession) { s.Spec.Location = strings.Repeat("a", 64) }, "spec.location"},
		{"uppercase key", func(s *lgv1alpha.LookingGlassSession) { s.Spec.ClientPublicKey = strings.ToUpper(testKey) }, "spec.clientPublicKey"},
		{"short key", func(s *lgv1alpha.LookingGlassSession) { s.Spec.ClientPublicKey = testKey[:63] }, "spec.clientPublicKey"},
		{"missing key", func(s *lgv1alpha.LookingGlassSession) { s.Spec.ClientPublicKey = "" }, "spec.clientPublicKey"},
		{"unknown vantage point", func(s *lgv1alpha.LookingGlassSession) { s.Spec.VantagePoint = "VPC" }, "spec.vantagePoint"},
		{"unknown diagnostic type", func(s *lgv1alpha.LookingGlassSession) { s.Spec.Diagnostic.Type = "MTR" }, "spec.diagnostic.type"},
		{"type without its member", func(s *lgv1alpha.LookingGlassSession) {
			s.Spec.Diagnostic = lgv1alpha.Diagnostic{Type: lgv1alpha.DiagnosticPing}
		}, "ping must be set if and only if type is Ping"},
		{"wrong member for type", func(s *lgv1alpha.LookingGlassSession) {
			s.Spec.Diagnostic = traceroute("198.51.100.7")
			s.Spec.Diagnostic.Type = lgv1alpha.DiagnosticPing
		}, "traceroute must be set if and only if type is Traceroute"},
		{"two members", func(s *lgv1alpha.LookingGlassSession) {
			s.Spec.Diagnostic.Traceroute = &lgv1alpha.TracerouteDiagnostic{Destination: "198.51.100.7"}
		}, "traceroute must be set if and only if type is Traceroute"},
		{"ping hostname", func(s *lgv1alpha.LookingGlassSession) { s.Spec.Diagnostic = ping("www.example.com") }, "must be a numeric IP address"},
		{"ping zone", func(s *lgv1alpha.LookingGlassSession) { s.Spec.Diagnostic = ping("fe80::1%eth0") }, "must be a numeric IP address"},
		{"ping IPv4-mapped IPv6", func(s *lgv1alpha.LookingGlassSession) { s.Spec.Diagnostic = ping("::ffff:192.0.2.1") }, "must be a numeric IP address"},
		{"ping leading zeros", func(s *lgv1alpha.LookingGlassSession) { s.Spec.Diagnostic = ping("192.0.2.010") }, "must be a numeric IP address"},
		{"ping prefix", func(s *lgv1alpha.LookingGlassSession) { s.Spec.Diagnostic = ping("192.0.2.0/24") }, "must be a numeric IP address"},
		{"ping loopback", func(s *lgv1alpha.LookingGlassSession) { s.Spec.Diagnostic = ping("127.0.0.1") }, "must be a unicast address"},
		{"ping unspecified", func(s *lgv1alpha.LookingGlassSession) { s.Spec.Diagnostic = ping("::") }, "must be a unicast address"},
		{"ping link-local", func(s *lgv1alpha.LookingGlassSession) { s.Spec.Diagnostic = ping("169.254.1.1") }, "must be a unicast address"},
		{"traceroute multicast", func(s *lgv1alpha.LookingGlassSession) { s.Spec.Diagnostic = traceroute("ff02::1") }, "must be a unicast address"},
		{"ping too long", func(s *lgv1alpha.LookingGlassSession) {
			s.Spec.Diagnostic = ping("2001:db8:" + strings.Repeat("0:", 20) + "1")
		}, "spec.diagnostic.ping.destination"},
		{"route lookup host bits set", func(s *lgv1alpha.LookingGlassSession) {
			s.Spec.Diagnostic = routeLookup("192.0.2.1/24", "")
		}, "must be an IP address, or a prefix with no host bits set"},
		{"route lookup not an address", func(s *lgv1alpha.LookingGlassSession) {
			s.Spec.Diagnostic = routeLookup("example.com", "")
		}, "must be an IP address, or a prefix with no host bits set"},
		{"route lookup family mismatch", func(s *lgv1alpha.LookingGlassSession) {
			s.Spec.Diagnostic = routeLookup("2001:db8::/32", lgv1alpha.AddressFamilyIPv4)
		}, "addressFamily must match destination"},
		{"route lookup unknown family", func(s *lgv1alpha.LookingGlassSession) {
			s.Spec.Diagnostic = routeLookup("192.0.2.1", "IPv5")
		}, "spec.diagnostic.routeLookup.addressFamily"},
		{"bgp summary without family", func(s *lgv1alpha.LookingGlassSession) {
			s.Spec.Diagnostic = bgpSummary("")
		}, "spec.diagnostic.bgpSummary.addressFamily"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newSession(ping("198.51.100.7"))
			tt.mutate(s)
			err := k8s.Create(ctx, s)
			if err == nil {
				_ = k8s.Delete(context.Background(), s)
				t.Fatal("create succeeded, want Invalid")
			}
			if !apierrors.IsInvalid(err) {
				t.Fatalf("error = %v, want Invalid", err)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want it to mention %q", err, tt.want)
			}
		})
	}
}

func TestSessionSpecIsImmutable(t *testing.T) {
	ctx := requireAPIServer(t)
	s := newSession(ping("198.51.100.7"))
	if err := k8s.Create(ctx, s); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8s.Delete(context.Background(), s) })

	s.Spec.Diagnostic = ping("198.51.100.8")
	err := k8s.Update(ctx, s)
	if !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), "spec is immutable") {
		t.Fatalf("update error = %v, want spec is immutable", err)
	}

	// Metadata stays mutable, so controllers can add finalizers and labels.
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(s), s); err != nil {
		t.Fatal(err)
	}
	s.Labels = map[string]string{"example": "label"}
	if err := k8s.Update(ctx, s); err != nil {
		t.Fatalf("metadata update rejected: %v", err)
	}
}

func TestSessionStatusIsolation(t *testing.T) {
	ctx := requireAPIServer(t)
	readyCondition := func(reason string) metav1.Condition {
		return metav1.Condition{
			Type:               lgv1alpha.ConditionReady,
			Status:             metav1.ConditionFalse,
			Reason:             reason,
			Message:            "synthetic",
			ObservedGeneration: 1,
			LastTransitionTime: metav1.Now(),
		}
	}

	// Status sent on create is dropped.
	s := newSession(ping("198.51.100.7"))
	s.Status.Cell = "example-cell"
	s.Status.Conditions = []metav1.Condition{readyCondition(lgv1alpha.ReasonSucceeded)}
	if err := k8s.Create(ctx, s); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8s.Delete(context.Background(), s) })
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(s), s); err != nil {
		t.Fatal(err)
	}
	if s.Status.Cell != "" || len(s.Status.Conditions) != 0 {
		t.Fatalf("status after create = %+v, want empty", s.Status)
	}
	if got := s.State(); got != lgv1alpha.SessionStatePending {
		t.Errorf("State() after create = %q, want Pending", got)
	}

	// Status sent on a main-resource update is ignored.
	s.Status.Cell = "example-cell"
	if err := k8s.Update(ctx, s); err != nil {
		t.Fatal(err)
	}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(s), s); err != nil {
		t.Fatal(err)
	}
	if s.Status.Cell != "" {
		t.Fatalf("status.cell after main update = %q, want empty", s.Status.Cell)
	}

	// The status subresource writes status and ignores spec changes.
	s.Status.Cell = "example-cell"
	s.Status.ObservedGeneration = s.Generation
	s.Status.Conditions = []metav1.Condition{readyCondition(lgv1alpha.ReasonNotConnected)}
	s.Spec.Location = "other-location"
	if err := k8s.Status().Update(ctx, s); err != nil {
		t.Fatalf("status update rejected: %v", err)
	}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(s), s); err != nil {
		t.Fatal(err)
	}
	if s.Spec.Location != "example-location" {
		t.Errorf("spec.location after status update = %q, want unchanged", s.Spec.Location)
	}
	if s.Status.Cell != "example-cell" {
		t.Errorf("status.cell = %q, want example-cell", s.Status.Cell)
	}
	if got := s.State(); got != lgv1alpha.SessionStateTerminal {
		t.Errorf("State() = %q, want Terminal", got)
	}
}

func TestDiscoveryServesBothKinds(t *testing.T) {
	requireAPIServer(t)
	list, err := disc.ServerResourcesForGroupVersion(lgv1alpha.GroupVersion.String())
	if err != nil {
		t.Fatal(err)
	}
	type want struct {
		kind       string
		namespaced bool
	}
	wants := map[string]want{
		"lookingglasssessions":         {"LookingGlassSession", true},
		"lookingglasssessions/status":  {"LookingGlassSession", true},
		"lookingglasslocations":        {"LookingGlassLocation", false},
		"lookingglasslocations/status": {"LookingGlassLocation", false},
	}
	got := map[string]metav1.APIResource{}
	for _, r := range list.APIResources {
		got[r.Name] = r
	}
	for name, w := range wants {
		r, ok := got[name]
		if !ok {
			t.Errorf("resource %q not served", name)
			continue
		}
		if r.Kind != w.kind || r.Namespaced != w.namespaced {
			t.Errorf("resource %q = kind %q namespaced %v, want %q %v", name, r.Kind, r.Namespaced, w.kind, w.namespaced)
		}
	}
}

func TestLocationStatusIsWrittenThroughSubresource(t *testing.T) {
	ctx := requireAPIServer(t)
	loc := &lgv1alpha.LookingGlassLocation{ObjectMeta: metav1.ObjectMeta{Name: "example-location"}}
	if err := k8s.Create(ctx, loc); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8s.Delete(context.Background(), loc) })
	loc.Status = lgv1alpha.LookingGlassLocationStatus{
		Available: true,
		VantagePoints: []lgv1alpha.VantagePointCapabilities{{
			Type: lgv1alpha.VantagePointFabricEdge,
			Diagnostics: []lgv1alpha.DiagnosticCapability{{
				Type:            lgv1alpha.DiagnosticTraceroute,
				AddressFamilies: []lgv1alpha.AddressFamily{lgv1alpha.AddressFamilyIPv4},
			}},
		}},
	}
	if err := k8s.Status().Update(ctx, loc); err != nil {
		t.Fatalf("status update rejected: %v", err)
	}
	var got lgv1alpha.LookingGlassLocation
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(loc), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Status.Available || len(got.Status.VantagePoints) != 1 {
		t.Errorf("status = %+v, want the written capabilities", got.Status)
	}
}
