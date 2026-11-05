package admission

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	lgv1alpha "go.datum.net/looking-glass/api/v1alpha"
)

const (
	testKey     = "3b6a27bcceb6a42d62a3a8d02a6f0d73653215771de243a63ac048a18b59da29"
	testUser    = "user@example.com"
	testNS      = "default"
	testProject = "project-a"
	otherProj   = "project-b"
	testLoc     = "example-location"
	publicV4    = "1.1.1.1"
	publicV6    = "2606:4700::1111"
)

var testNow = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

// grants maps user -> permission -> allowed, per project.
type grants map[string]map[string]bool

// fakeProjects is a ProjectResolver with one fake control plane per project.
// Each one answers SubjectAccessReviews from its own grants and records them.
type fakeProjects struct {
	clients map[string]client.Client
	reviews map[string][]authorizationv1.SubjectAccessReview
	sarErr  error
}

func newFakeProjects(t *testing.T, projects map[string]struct {
	locations []*lgv1alpha.LookingGlassLocation
	grants    grants
}) *fakeProjects {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := lgv1alpha.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := authorizationv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	fp := &fakeProjects{clients: map[string]client.Client{}, reviews: map[string][]authorizationv1.SubjectAccessReview{}}
	for name, p := range projects {
		objs := make([]client.Object, 0, len(p.locations))
		for _, l := range p.locations {
			objs = append(objs, l)
		}
		g := p.grants
		fp.clients[name] = fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(objs...).
			WithInterceptorFuncs(interceptor.Funcs{
				Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
					sar, ok := obj.(*authorizationv1.SubjectAccessReview)
					if !ok {
						return c.Create(ctx, obj, opts...)
					}
					if fp.sarErr != nil {
						return fp.sarErr
					}
					fp.reviews[name] = append(fp.reviews[name], *sar.DeepCopy())
					sar.Status.Allowed = g[sar.Spec.User][sar.Spec.ResourceAttributes.Verb]
					return nil
				},
			}).
			Build()
	}
	return fp
}

func (f *fakeProjects) ForProject(_ context.Context, project string) (client.Client, error) {
	c, ok := f.clients[project]
	if !ok {
		return nil, errors.New("unknown project")
	}
	return c, nil
}

// fullLocation offers every diagnostic in both families, except Traceroute,
// which is IPv4 only, as in the contract's example.
func fullLocation(name string, observed time.Time) *lgv1alpha.LookingGlassLocation {
	both := []lgv1alpha.AddressFamily{lgv1alpha.AddressFamilyIPv4, lgv1alpha.AddressFamilyIPv6}
	return &lgv1alpha.LookingGlassLocation{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: lgv1alpha.LookingGlassLocationStatus{
			Available:  true,
			ObservedAt: &metav1.Time{Time: observed},
			VantagePoints: []lgv1alpha.VantagePointCapabilities{{
				Type: lgv1alpha.VantagePointFabricEdge,
				Diagnostics: []lgv1alpha.DiagnosticCapability{
					{Type: lgv1alpha.DiagnosticRouteLookup, AddressFamilies: both},
					{Type: lgv1alpha.DiagnosticBGPSummary, AddressFamilies: both},
					{Type: lgv1alpha.DiagnosticPing, AddressFamilies: both},
					{Type: lgv1alpha.DiagnosticTraceroute, AddressFamilies: []lgv1alpha.AddressFamily{lgv1alpha.AddressFamilyIPv4}},
				},
			}},
		},
	}
}

func session(d lgv1alpha.Diagnostic) *lgv1alpha.LookingGlassSession {
	return &lgv1alpha.LookingGlassSession{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "lg-", Namespace: testNS},
		Spec: lgv1alpha.LookingGlassSessionSpec{
			Location:        testLoc,
			VantagePoint:    lgv1alpha.VantagePointFabricEdge,
			Diagnostic:      d,
			ClientPublicKey: testKey,
		},
	}
}

func ping(dest string) lgv1alpha.Diagnostic {
	return lgv1alpha.Diagnostic{Type: lgv1alpha.DiagnosticPing, Ping: &lgv1alpha.PingDiagnostic{Destination: lgv1alpha.ProbeDestination(dest)}}
}

func traceroute(dest string) lgv1alpha.Diagnostic {
	return lgv1alpha.Diagnostic{Type: lgv1alpha.DiagnosticTraceroute, Traceroute: &lgv1alpha.TracerouteDiagnostic{Destination: lgv1alpha.ProbeDestination(dest)}}
}

func routeLookup(dest string, af lgv1alpha.AddressFamily) lgv1alpha.Diagnostic {
	return lgv1alpha.Diagnostic{Type: lgv1alpha.DiagnosticRouteLookup, RouteLookup: &lgv1alpha.RouteLookupDiagnostic{Destination: lgv1alpha.RouteDestination(dest), AddressFamily: af}}
}

func bgpSummary(af lgv1alpha.AddressFamily) lgv1alpha.Diagnostic {
	return lgv1alpha.Diagnostic{Type: lgv1alpha.DiagnosticBGPSummary, BGPSummary: &lgv1alpha.BGPSummaryDiagnostic{AddressFamily: af}}
}

func request(project string) admission.Request {
	extra := map[string]authenticationv1.ExtraValue{}
	if project != "" {
		extra[ProjectNameExtraKey] = authenticationv1.ExtraValue{project}
		extra[ProjectTypeExtraKey] = authenticationv1.ExtraValue{"Project"}
	}
	return admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Operation: admissionv1.Create,
		Namespace: testNS,
		UserInfo: authenticationv1.UserInfo{
			Username: testUser,
			UID:      "uid-1",
			Groups:   []string{"system:authenticated"},
			Extra:    extra,
		},
	}}
}

func withRequest(t *testing.T, req admission.Request) context.Context {
	t.Helper()
	return admission.NewContextWithRequest(t.Context(), req)
}

// standard returns one project holding the location, where testUser has
// probe, and a second project where testUser has readroutes but no location.
func standard(t *testing.T) *fakeProjects {
	t.Helper()
	return newFakeProjects(t, map[string]struct {
		locations []*lgv1alpha.LookingGlassLocation
		grants    grants
	}{
		testProject: {
			locations: []*lgv1alpha.LookingGlassLocation{fullLocation(testLoc, testNow.Add(-time.Minute))},
			grants:    grants{testUser: {PermissionProbe: true}},
		},
		otherProj: {
			grants: grants{testUser: {PermissionReadRoutes: true}},
		},
	})
}

func newAdmission(fp *fakeProjects) *SessionAdmission {
	return &SessionAdmission{
		Projects: fp,
		Now:      func() time.Time { return testNow },
	}
}

func statusCode(err error) int32 {
	var s apierrors.APIStatus
	if errors.As(err, &s) {
		return s.Status().Code
	}
	return 0
}

func TestAdmitAllowsAuthorizedSessions(t *testing.T) {
	fp := newFakeProjects(t, map[string]struct {
		locations []*lgv1alpha.LookingGlassLocation
		grants    grants
	}{
		testProject: {
			locations: []*lgv1alpha.LookingGlassLocation{fullLocation(testLoc, testNow.Add(-time.Minute))},
			grants:    grants{testUser: {PermissionProbe: true, PermissionReadRoutes: true}},
		},
	})
	a := newAdmission(fp)
	tests := map[string]lgv1alpha.Diagnostic{
		"ping IPv4":                    ping(publicV4),
		"ping IPv6":                    ping(publicV6),
		"traceroute IPv4":              traceroute(publicV4),
		"route lookup private address": routeLookup("10.1.2.3", ""),
		"route lookup IPv6 prefix":     routeLookup("2001:db8::/32", ""),
		"route lookup explicit family": routeLookup("192.0.2.0/24", lgv1alpha.AddressFamilyIPv4),
		"bgp summary IPv6":             bgpSummary(lgv1alpha.AddressFamilyIPv6),
	}
	for name, d := range tests {
		t.Run(name, func(t *testing.T) {
			s := session(d)
			ctx := withRequest(t, request(testProject))
			if err := a.Default(ctx, s); err != nil {
				t.Fatalf("Default: %v", err)
			}
			if _, err := a.ValidateCreate(ctx, s); err != nil {
				t.Fatalf("ValidateCreate: %v", err)
			}
		})
	}
}

func TestAdmitRefuses(t *testing.T) {
	tests := []struct {
		name    string
		project string
		mutate  func(*lgv1alpha.LookingGlassSession)
		code    int32
		want    string
	}{
		{"no project scope", "", nil, http.StatusForbidden, "project control plane"},
		{"probe without permission", otherProj, nil, http.StatusForbidden, `"probe" permission`},
		{"route lookup without readroutes", testProject, func(s *lgv1alpha.LookingGlassSession) {
			s.Spec.Diagnostic = routeLookup(publicV4, "")
		}, http.StatusForbidden, `"readroutes" permission`},
		{"bgp summary without readroutes", testProject, func(s *lgv1alpha.LookingGlassSession) {
			s.Spec.Diagnostic = bgpSummary(lgv1alpha.AddressFamilyIPv4)
		}, http.StatusForbidden, `"readroutes" permission`},
		{"unknown location", testProject, func(s *lgv1alpha.LookingGlassSession) {
			s.Spec.Location = "no-such-location"
		}, http.StatusUnprocessableEntity, "does not offer Looking Glass"},
		{"traceroute family not offered", testProject, func(s *lgv1alpha.LookingGlassSession) {
			s.Spec.Diagnostic = traceroute(publicV6)
		}, http.StatusUnprocessableEntity, "does not offer Traceroute for IPv6"},
		{"vantage point not offered", testProject, func(s *lgv1alpha.LookingGlassSession) {
			s.Spec.VantagePoint = "VPC"
		}, http.StatusUnprocessableEntity, "vantage point"},
		{"private destination", testProject, func(s *lgv1alpha.LookingGlassSession) {
			s.Spec.Diagnostic = ping("10.1.2.3")
		}, http.StatusUnprocessableEntity, "special-use range 10.0.0.0/8"},
		{"documentation destination", testProject, func(s *lgv1alpha.LookingGlassSession) {
			s.Spec.Diagnostic = traceroute("192.0.2.1")
		}, http.StatusUnprocessableEntity, "special-use range"},
		{"unique local destination", testProject, func(s *lgv1alpha.LookingGlassSession) {
			s.Spec.Diagnostic = ping("fd00::1")
		}, http.StatusUnprocessableEntity, "not IPv6 global unicast"},
		{"platform destination", testProject, func(s *lgv1alpha.LookingGlassSession) {
			s.Spec.Diagnostic = ping("1.1.1.9")
		}, http.StatusUnprocessableEntity, "platform range 1.1.1.8/29"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := newAdmission(standard(t))
			a.Destinations = DestinationPolicy{Deny: mustPrefixes("1.1.1.8/29")}
			s := session(ping(publicV4))
			if tc.mutate != nil {
				tc.mutate(s)
			}
			ctx := withRequest(t, request(tc.project))
			for _, call := range []struct {
				name string
				run  func() error
			}{
				{"Default", func() error { return a.Default(ctx, s) }},
				{"ValidateCreate", func() error { _, err := a.ValidateCreate(ctx, s); return err }},
			} {
				err := call.run()
				if err == nil {
					t.Fatalf("%s admitted the session", call.name)
				}
				if got := statusCode(err); got != tc.code {
					t.Errorf("%s: status %d, want %d (%v)", call.name, got, tc.code, err)
				}
				if !strings.Contains(err.Error(), tc.want) {
					t.Errorf("%s: error %q, want it to mention %q", call.name, err, tc.want)
				}
			}
		})
	}
}

func TestAdmitRefusesUnusableLocation(t *testing.T) {
	stale := fullLocation("stale", testNow.Add(-CapabilityMaxAge-time.Second))
	unavailable := fullLocation("unavailable", testNow)
	unavailable.Status.Available = false
	unobserved := fullLocation("unobserved", testNow)
	unobserved.Status.ObservedAt = nil
	fp := newFakeProjects(t, map[string]struct {
		locations []*lgv1alpha.LookingGlassLocation
		grants    grants
	}{
		testProject: {
			locations: []*lgv1alpha.LookingGlassLocation{stale, unavailable, unobserved},
			grants:    grants{testUser: {PermissionProbe: true}},
		},
	})
	a := newAdmission(fp)
	for loc, want := range map[string]string{
		"stale":       "out of date",
		"unavailable": "not available",
		"unobserved":  "out of date",
	} {
		t.Run(loc, func(t *testing.T) {
			s := session(ping(publicV4))
			s.Spec.Location = loc
			err := a.Default(withRequest(t, request(testProject)), s)
			if err == nil || statusCode(err) != http.StatusUnprocessableEntity || !strings.Contains(err.Error(), want) {
				t.Fatalf("Default = %v, want 422 mentioning %q", err, want)
			}
		})
	}
}

func TestAdmitDestinationAllowList(t *testing.T) {
	a := newAdmission(standard(t))
	a.Destinations = DestinationPolicy{Allow: mustPrefixes("10.255.255.0/24")}
	if err := a.Default(withRequest(t, request(testProject)), session(ping("10.255.255.1"))); err != nil {
		t.Fatalf("explicitly allowed destination refused: %v", err)
	}
	if err := a.Default(withRequest(t, request(testProject)), session(ping("10.255.254.1"))); err == nil {
		t.Fatal("destination outside the allow list admitted")
	}
}

// TestAdmitIsolatesProjects checks that a request reads locations and asks for
// authorization only in its own project: project-b's grants do not authorize
// a create in project-a, and project-a's location is not usable from
// project-b.
func TestAdmitIsolatesProjects(t *testing.T) {
	fp := standard(t)
	a := newAdmission(fp)

	// project-b grants readroutes, but the location lives only in project-a.
	err := a.Default(withRequest(t, request(otherProj)), session(routeLookup(publicV4, "")))
	if err == nil || !strings.Contains(err.Error(), "does not offer Looking Glass") {
		t.Fatalf("project-b used project-a's location: %v", err)
	}
	// project-a has the location, but readroutes is granted only in project-b.
	err = a.Default(withRequest(t, request(testProject)), session(routeLookup(publicV4, "")))
	if statusCode(err) != http.StatusForbidden {
		t.Fatalf("project-a used project-b's grant: %v", err)
	}

	if n := len(fp.reviews[otherProj]); n != 1 {
		t.Errorf("%d reviews in project-b, want 1", n)
	}
	reviews := fp.reviews[testProject]
	if len(reviews) != 1 {
		t.Fatalf("%d reviews in project-a, want 1", len(reviews))
	}
	sar := reviews[0].Spec
	ra := sar.ResourceAttributes
	if sar.User != testUser || sar.UID != "uid-1" || len(sar.Groups) != 1 {
		t.Errorf("review subject %+v", sar)
	}
	if got := sar.Extra[ProjectNameExtraKey]; len(got) != 1 || got[0] != testProject {
		t.Errorf("review project extra = %v, want %s", got, testProject)
	}
	if ra.Verb != PermissionReadRoutes || ra.Group != lgv1alpha.GroupVersion.Group ||
		ra.Version != lgv1alpha.GroupVersion.Version || ra.Resource != "lookingglasssessions" || ra.Namespace != testNS {
		t.Errorf("review attributes %+v", ra)
	}
}

func TestAdmitFailsClosedOnAuthorizerError(t *testing.T) {
	fp := standard(t)
	fp.sarErr = errors.New("authorizer unavailable")
	err := newAdmission(fp).Default(withRequest(t, request(testProject)), session(ping(publicV4)))
	if statusCode(err) != http.StatusInternalServerError {
		t.Fatalf("Default = %v, want 500", err)
	}
}

func TestAdmitRequiresProjectType(t *testing.T) {
	req := request(testProject)
	req.UserInfo.Extra[ProjectTypeExtraKey] = authenticationv1.ExtraValue{"Organization"}
	err := newAdmission(standard(t)).Default(withRequest(t, req), session(ping(publicV4)))
	if statusCode(err) != http.StatusForbidden {
		t.Fatalf("Default = %v, want 403", err)
	}
}

func TestRateLimitChargedOncePerCreate(t *testing.T) {
	a := newAdmission(standard(t))
	a.RateLimiter = NewProjectRateLimiter(0.001, 2)
	a.RateLimiter.now = func() time.Time { return testNow }

	ctx := withRequest(t, request(testProject))
	for i := range 2 {
		s := session(ping(publicV4))
		if err := a.Default(ctx, s); err != nil {
			t.Fatalf("create %d: %v", i+1, err)
		}
		// The validating pass must not charge again.
		if _, err := a.ValidateCreate(ctx, s); err != nil {
			t.Fatalf("create %d validate: %v", i+1, err)
		}
	}

	dry := request(testProject)
	dry.DryRun = new(true)
	if err := a.Default(withRequest(t, dry), session(ping(publicV4))); err != nil {
		t.Fatalf("dry run was rate limited: %v", err)
	}

	err := a.Default(ctx, session(ping(publicV4)))
	if statusCode(err) != http.StatusTooManyRequests {
		t.Fatalf("third create = %v, want 429", err)
	}
	var s apierrors.APIStatus
	if errors.As(err, &s) && (s.Status().Details == nil || s.Status().Details.RetryAfterSeconds < 1) {
		t.Errorf("429 without a retry-after: %+v", s.Status())
	}

	// Another project is not limited by project-a's creates.
	if err := a.Default(withRequest(t, request(otherProj)), session(routeLookup(publicV4, ""))); statusCode(err) == http.StatusTooManyRequests {
		t.Fatal("project-b was rate limited by project-a")
	}
}

func TestDefaultIgnoresOtherOperations(t *testing.T) {
	req := request("")
	req.Operation = admissionv1.Update
	if err := newAdmission(standard(t)).Default(withRequest(t, req), session(ping("10.1.2.3"))); err != nil {
		t.Fatalf("Default on update = %v, want nil", err)
	}
}

func TestPermissionFor(t *testing.T) {
	for d, want := range map[lgv1alpha.DiagnosticType]string{
		lgv1alpha.DiagnosticPing:        PermissionProbe,
		lgv1alpha.DiagnosticTraceroute:  PermissionProbe,
		lgv1alpha.DiagnosticRouteLookup: PermissionReadRoutes,
		lgv1alpha.DiagnosticBGPSummary:  PermissionReadRoutes,
	} {
		if got, ok := PermissionFor(d); !ok || got != want {
			t.Errorf("PermissionFor(%s) = %q, %v; want %q", d, got, ok, want)
		}
	}
	if _, ok := PermissionFor("MTR"); ok {
		t.Error("PermissionFor accepted an unknown diagnostic")
	}
}
