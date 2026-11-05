package manager

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	lgv1alpha "go.datum.net/looking-glass/api/v1alpha"
	"go.datum.net/looking-glass/internal/admission"
	"go.datum.net/looking-glass/internal/config"
)

// These tests run the manager's real admission wiring against an API server
// with the generated CRDs and webhook configurations installed. Authorization
// is the API server's RBAC, which, like Milo's authorizer, evaluates the
// custom probe and readroutes verbs in a SubjectAccessReview. Callers
// impersonate a user carrying the extras Milo adds to project requests.

const (
	testKey     = "3b6a27bcceb6a42d62a3a8d02a6f0d73653215771de243a63ac048a18b59da29"
	testNS      = "default"
	testLoc     = "example-location"
	prober      = "prober@example.com"
	routeViewer = "route-viewer@example.com"
	publicV4    = "1.1.1.1"
)

type admissionEnv struct {
	admin client.Client
	cfg   *rest.Config
}

func startAdmissionEnv(t *testing.T, burst int) *admissionEnv {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS is unset; run `task test` to provide envtest binaries")
	}
	root := filepath.Join("..", "..", "config")
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join(root, "crd", "bases")},
		ErrorIfCRDPathMissing: true,
		WebhookInstallOptions: envtest.WebhookInstallOptions{
			Paths: []string{filepath.Join(root, "webhook", "manifests.yaml")},
		},
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
	cfg.Webhook.Enabled = true
	cfg.Webhook.Host = env.WebhookInstallOptions.LocalServingHost
	cfg.Webhook.Port = env.WebhookInstallOptions.LocalServingPort
	cfg.Webhook.CertDir = env.WebhookInstallOptions.LocalServingCertDir
	cfg.Admission.CreateRateLimit = config.RateLimitConfig{QPS: 0.001, Burst: burst}
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
	waitForTLS(t, net.JoinHostPort(cfg.Webhook.Host, fmt.Sprint(cfg.Webhook.Port)))

	scheme, err := NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	admin, err := client.New(restCfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	e := &admissionEnv{admin: admin, cfg: restCfg}
	e.grant(t, prober, admission.PermissionProbe)
	e.grant(t, routeViewer, admission.PermissionReadRoutes)
	e.offerLocation(t)
	return e
}

func waitForTLS(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	d := &tls.Dialer{Config: &tls.Config{InsecureSkipVerify: true}} //nolint:gosec // test-only probe of a local server
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		conn, err := d.DialContext(ctx, "tcp", addr)
		cancel()
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("webhook server at %s never served TLS", addr)
}

// grant gives user create and the custom verbs on sessions.
func (e *admissionEnv) grant(t *testing.T, user string, verbs ...string) {
	t.Helper()
	ctx := t.Context()
	name := "lg-" + fmt.Sprint(time.Now().UnixNano())
	role := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{lgv1alpha.GroupVersion.Group},
			Resources: []string{"lookingglasssessions"},
			Verbs:     append([]string{"create", "get", "list"}, verbs...),
		}},
	}
	binding := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: name},
		Subjects:   []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: rbacv1.UserKind, Name: user}},
	}
	for _, obj := range []client.Object{role, binding} {
		if err := e.admin.Create(ctx, obj); err != nil {
			t.Fatal(err)
		}
	}
}

func (e *admissionEnv) offerLocation(t *testing.T) {
	t.Helper()
	ctx := t.Context()
	loc := &lgv1alpha.LookingGlassLocation{ObjectMeta: metav1.ObjectMeta{Name: testLoc}}
	if err := e.admin.Create(ctx, loc); err != nil {
		t.Fatal(err)
	}
	loc.Status = lgv1alpha.LookingGlassLocationStatus{
		Available:  true,
		ObservedAt: &metav1.Time{Time: time.Now()},
		VantagePoints: []lgv1alpha.VantagePointCapabilities{{
			Type: lgv1alpha.VantagePointFabricEdge,
			Diagnostics: []lgv1alpha.DiagnosticCapability{
				{Type: lgv1alpha.DiagnosticPing, AddressFamilies: []lgv1alpha.AddressFamily{lgv1alpha.AddressFamilyIPv4}},
				{Type: lgv1alpha.DiagnosticRouteLookup, AddressFamilies: []lgv1alpha.AddressFamily{lgv1alpha.AddressFamilyIPv4}},
			},
		}},
	}
	if err := e.admin.Status().Update(ctx, loc); err != nil {
		t.Fatal(err)
	}
}

// as returns a client that impersonates user in project. An empty project
// sends no project extras.
func (e *admissionEnv) as(t *testing.T, user, project string) client.Client {
	t.Helper()
	scheme, err := NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	c, err := client.New(e.impersonate(user, project), client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// createOnce creates s as user in project with client-go's retries disabled.
func (e *admissionEnv) createOnce(t *testing.T, user, project string, s *lgv1alpha.LookingGlassSession) error {
	t.Helper()
	cfg := e.impersonate(user, project)
	scheme, err := NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	cfg.APIPath = "/apis"
	cfg.GroupVersion = &lgv1alpha.GroupVersion
	cfg.NegotiatedSerializer = serializer.NewCodecFactory(scheme).WithoutConversion()
	rc, err := rest.RESTClientFor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return rc.Post().Namespace(s.Namespace).Resource("lookingglasssessions").
		Body(s).MaxRetries(0).Do(t.Context()).Error()
}

func (e *admissionEnv) impersonate(user, project string) *rest.Config {
	cfg := rest.CopyConfig(e.cfg)
	cfg.Impersonate = rest.ImpersonationConfig{UserName: user}
	if project != "" {
		cfg.Impersonate.Extra = map[string][]string{
			admission.ProjectNameExtraKey: {project},
			admission.ProjectTypeExtraKey: {"Project"},
		}
	}
	return cfg
}

func (e *admissionEnv) sessionCount(t *testing.T) int {
	t.Helper()
	var list lgv1alpha.LookingGlassSessionList
	if err := e.admin.List(t.Context(), &list, client.InNamespace(testNS)); err != nil {
		t.Fatal(err)
	}
	return len(list.Items)
}

func newSession(d lgv1alpha.Diagnostic) *lgv1alpha.LookingGlassSession {
	return &lgv1alpha.LookingGlassSession{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "lg-", Namespace: testNS},
		Spec: lgv1alpha.LookingGlassSessionSpec{
			Location:        testLoc,
			Diagnostic:      d,
			ClientPublicKey: testKey,
		},
	}
}

func ping(dest string) lgv1alpha.Diagnostic {
	return lgv1alpha.Diagnostic{Type: lgv1alpha.DiagnosticPing, Ping: &lgv1alpha.PingDiagnostic{Destination: lgv1alpha.ProbeDestination(dest)}}
}

func statusCode(err error) int32 {
	var s apierrors.APIStatus
	if errors.As(err, &s) {
		return s.Status().Code
	}
	return 0
}

// TestAdmissionRefusesBeforeStorage checks every refusal end to end, and that
// a refused create stores no object, so no controller, cell or backend ever
// sees it.
func TestAdmissionRefusesBeforeStorage(t *testing.T) {
	e := startAdmissionEnv(t, 100)
	ctx := t.Context()

	ok := newSession(ping(publicV4))
	if err := e.as(t, prober, "project-a").Create(ctx, ok); err != nil {
		t.Fatalf("authorized create refused: %v", err)
	}

	routes := func() *lgv1alpha.LookingGlassSession {
		return newSession(lgv1alpha.Diagnostic{
			Type:        lgv1alpha.DiagnosticRouteLookup,
			RouteLookup: &lgv1alpha.RouteLookupDiagnostic{Destination: publicV4},
		})
	}
	if err := e.as(t, routeViewer, "project-a").Create(ctx, routes()); err != nil {
		t.Fatalf("authorized route lookup refused: %v", err)
	}

	unknownLoc := newSession(ping(publicV4))
	unknownLoc.Spec.Location = "no-such-location"
	tests := []struct {
		name    string
		user    string
		project string
		session *lgv1alpha.LookingGlassSession
		code    int32
	}{
		{"no project scope", prober, "", newSession(ping(publicV4)), http.StatusForbidden},
		{"route lookup without readroutes", prober, "project-a", routes(), http.StatusForbidden},
		{"ping without probe", routeViewer, "project-a", newSession(ping(publicV4)), http.StatusForbidden},
		{"private destination", prober, "project-a", newSession(ping("10.1.2.3")), http.StatusUnprocessableEntity},
		{"documentation destination", prober, "project-a", newSession(ping("192.0.2.1")), http.StatusUnprocessableEntity},
		{"unknown location", prober, "project-a", unknownLoc, http.StatusUnprocessableEntity},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			before := e.sessionCount(t)
			err := e.as(t, tc.user, tc.project).Create(ctx, tc.session)
			if err == nil {
				t.Fatal("create was admitted")
			}
			if got := statusCode(err); got != tc.code {
				t.Errorf("status %d, want %d: %v", got, tc.code, err)
			}
			if after := e.sessionCount(t); after != before {
				t.Errorf("a refused create stored an object: %d sessions, was %d", after, before)
			}
		})
	}
}

// TestAdmissionRateLimitIsPerProject checks that one project exhausting its
// create budget is refused with 429 while another project is not affected.
func TestAdmissionRateLimitIsPerProject(t *testing.T) {
	e := startAdmissionEnv(t, 2)
	ctx := t.Context()
	a := e.as(t, prober, "project-a")
	for i := range 2 {
		if err := a.Create(ctx, newSession(ping(publicV4))); err != nil {
			t.Fatalf("create %d within the burst: %v", i+1, err)
		}
	}
	before := e.sessionCount(t)
	// client-go retries a 429 after its Retry-After, so send this one create
	// without retries to observe the refusal.
	err := e.createOnce(t, prober, "project-a", newSession(ping(publicV4)))
	if statusCode(err) != http.StatusTooManyRequests {
		t.Fatalf("create beyond the burst = %v, want 429", err)
	}
	if after := e.sessionCount(t); after != before {
		t.Errorf("a rate-limited create stored an object")
	}
	if err := e.as(t, prober, "project-b").Create(ctx, newSession(ping(publicV4))); err != nil {
		t.Fatalf("project-b was limited by project-a: %v", err)
	}
}
