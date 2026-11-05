package admission

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/netip"
	"slices"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	lgv1alpha "go.datum.net/looking-glass/api/v1alpha"
)

const (
	// ProjectNameExtraKey and ProjectTypeExtraKey are the user extras Milo's
	// API server adds to every request in a project control plane. Webhook
	// requests carry them in UserInfo.
	ProjectNameExtraKey = "iam.miloapis.com/parent-name"
	ProjectTypeExtraKey = "iam.miloapis.com/parent-type"

	// PermissionProbe is required to create Ping and Traceroute sessions.
	PermissionProbe = "probe"
	// PermissionReadRoutes is required to create RouteLookup and BGPSummary
	// sessions.
	PermissionReadRoutes = "readroutes"

	// CapabilityMaxAge is how old a location's capabilities may be before
	// the location cannot be used.
	CapabilityMaxAge = 5 * time.Minute

	sessionResource = "lookingglasssessions"
)

var sessionGroupResource = schema.GroupResource{Group: lgv1alpha.GroupVersion.Group, Resource: sessionResource}

// ProjectResolver returns a client for one project's control plane. Location
// reads and SubjectAccessReviews for a request go only to the control plane of
// the project the request was made in.
type ProjectResolver interface {
	ForProject(ctx context.Context, project string) (client.Client, error)
}

// SingleClusterResolver serves every project from one control plane. It is
// for local development and tests, where there is one API server.
type SingleClusterResolver struct {
	Client client.Client
}

// ForProject implements ProjectResolver.
func (r SingleClusterResolver) ForProject(context.Context, string) (client.Client, error) {
	return r.Client, nil
}

// SessionAdmission refuses a LookingGlassSession create that is not authorized
// or not bounded. A refused create stores no object, so nothing downstream
// (placement, a cell, Galactic) ever sees it.
type SessionAdmission struct {
	// Projects resolves the control plane of the request's project.
	Projects ProjectResolver
	// RateLimiter limits creates per project. Nil disables the limit.
	RateLimiter *ProjectRateLimiter
	// Destinations is the probe destination policy.
	Destinations DestinationPolicy
	// Now returns the current time. Nil means time.Now.
	Now func() time.Time
}

var (
	_ admission.Defaulter[*lgv1alpha.LookingGlassSession] = &SessionAdmission{}
	_ admission.Validator[*lgv1alpha.LookingGlassSession] = &SessionAdmission{}
)

// +kubebuilder:webhook:path=/mutate-lookingglass-datumapis-com-v1alpha-lookingglasssession,mutating=true,failurePolicy=fail,sideEffects=None,groups=lookingglass.datumapis.com,resources=lookingglasssessions,verbs=create,versions=v1alpha,name=mlookingglasssession.lookingglass.datumapis.com,admissionReviewVersions=v1
// +kubebuilder:webhook:path=/validate-lookingglass-datumapis-com-v1alpha-lookingglasssession,mutating=false,failurePolicy=fail,sideEffects=None,groups=lookingglass.datumapis.com,resources=lookingglasssessions,verbs=create,versions=v1alpha,name=vlookingglasssession.lookingglass.datumapis.com,admissionReviewVersions=v1

// SetupWithManager registers the session webhooks on mgr's webhook server.
func (a *SessionAdmission) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &lgv1alpha.LookingGlassSession{}).
		WithDefaulter(a).
		WithValidator(a).
		Complete()
}

// Default runs every admission check and changes nothing. It refuses in
// mutating admission because Milo claims project quota after mutating and
// before validating admission: refusing here keeps a refused request from
// holding quota and gives the caller the real reason instead of a quota
// denial. It is also the only place the create rate limit is charged, so one
// request consumes one token.
func (a *SessionAdmission) Default(ctx context.Context, s *lgv1alpha.LookingGlassSession) error {
	req, err := admission.RequestFromContext(ctx)
	if err != nil {
		return err
	}
	if req.Operation != admissionv1.Create {
		return nil
	}
	project, err := projectOf(req, s)
	if err != nil {
		return err
	}
	if err := a.chargeRate(req, project); err != nil {
		return err
	}
	return a.admit(ctx, req, project, s)
}

// ValidateCreate repeats Default's checks, except the rate limit, against the
// session as every mutating webhook left it.
func (a *SessionAdmission) ValidateCreate(ctx context.Context, s *lgv1alpha.LookingGlassSession) (admission.Warnings, error) {
	req, err := admission.RequestFromContext(ctx)
	if err != nil {
		return nil, err
	}
	project, err := projectOf(req, s)
	if err != nil {
		return nil, err
	}
	return nil, a.admit(ctx, req, project, s)
}

// ValidateUpdate allows every update. The spec is immutable by schema, and
// status is written through the status subresource, which this webhook does
// not intercept.
func (a *SessionAdmission) ValidateUpdate(context.Context, *lgv1alpha.LookingGlassSession, *lgv1alpha.LookingGlassSession) (admission.Warnings, error) {
	return nil, nil
}

// ValidateDelete allows every delete: deleting a live session revokes it.
func (a *SessionAdmission) ValidateDelete(context.Context, *lgv1alpha.LookingGlassSession) (admission.Warnings, error) {
	return nil, nil
}

func (a *SessionAdmission) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

// projectOf returns the project the request was made in. A request without a
// project scope is refused: every session belongs to exactly one project.
func projectOf(req admission.Request, s *lgv1alpha.LookingGlassSession) (string, error) {
	extra := req.UserInfo.Extra
	names, types := extra[ProjectNameExtraKey], extra[ProjectTypeExtraKey]
	if len(names) != 1 || names[0] == "" || len(types) != 1 || types[0] != "Project" {
		return "", apierrors.NewForbidden(sessionGroupResource, s.Name,
			errors.New("sessions can only be created in a project control plane"))
	}
	return names[0], nil
}

func (a *SessionAdmission) chargeRate(req admission.Request, project string) error {
	if a.RateLimiter == nil || (req.DryRun != nil && *req.DryRun) {
		return nil
	}
	ok, wait := a.RateLimiter.Allow(project)
	if ok {
		return nil
	}
	seconds := max(int(math.Ceil(wait.Seconds())), 1)
	return apierrors.NewTooManyRequests(
		fmt.Sprintf("too many LookingGlassSession creates in this project; retry in %ds", seconds), seconds)
}

// admit authorizes the diagnostic, then checks the location and destination.
func (a *SessionAdmission) admit(ctx context.Context, req admission.Request, project string, s *lgv1alpha.LookingGlassSession) error {
	cl, err := a.Projects.ForProject(ctx, project)
	if err != nil {
		return apierrors.NewInternalError(fmt.Errorf("resolving project control plane: %w", err))
	}
	if err := authorize(ctx, cl, req, s); err != nil {
		return err
	}
	errs, err := a.checkLocation(ctx, cl, s)
	if err != nil {
		return err
	}
	errs = append(errs, a.checkDestination(s)...)
	if len(errs) > 0 {
		return apierrors.NewInvalid(lgv1alpha.GroupVersion.WithKind("LookingGlassSession").GroupKind(), s.Name, errs)
	}
	return nil
}

// PermissionFor returns the custom permission a diagnostic needs on top of
// create.
func PermissionFor(t lgv1alpha.DiagnosticType) (string, bool) {
	switch t {
	case lgv1alpha.DiagnosticPing, lgv1alpha.DiagnosticTraceroute:
		return PermissionProbe, true
	case lgv1alpha.DiagnosticRouteLookup, lgv1alpha.DiagnosticBGPSummary:
		return PermissionReadRoutes, true
	}
	return "", false
}

// authorize asks the project's authorizer whether the requester holds the
// diagnostic's permission on sessions. The requester's extras are copied, so
// Milo evaluates the review against the same project.
func authorize(ctx context.Context, cl client.Client, req admission.Request, s *lgv1alpha.LookingGlassSession) error {
	verb, ok := PermissionFor(s.Spec.Diagnostic.Type)
	if !ok {
		return apierrors.NewForbidden(sessionGroupResource, s.Name,
			fmt.Errorf("diagnostic %q has no permission", s.Spec.Diagnostic.Type))
	}
	extra := make(map[string]authorizationv1.ExtraValue, len(req.UserInfo.Extra))
	for k, v := range req.UserInfo.Extra {
		extra[k] = authorizationv1.ExtraValue(slices.Clone(v))
	}
	review := &authorizationv1.SubjectAccessReview{
		Spec: authorizationv1.SubjectAccessReviewSpec{
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Namespace: req.Namespace,
				Verb:      verb,
				Group:     lgv1alpha.GroupVersion.Group,
				Version:   lgv1alpha.GroupVersion.Version,
				Resource:  sessionResource,
				Name:      s.Name,
			},
			User:   req.UserInfo.Username,
			Groups: slices.Clone(req.UserInfo.Groups),
			UID:    req.UserInfo.UID,
			Extra:  extra,
		},
	}
	if err := cl.Create(ctx, review); err != nil {
		return apierrors.NewInternalError(fmt.Errorf("checking %s permission: %w", verb, err))
	}
	if !review.Status.Allowed || review.Status.Denied {
		return apierrors.NewForbidden(sessionGroupResource, s.Name,
			fmt.Errorf("creating a %s session requires the %q permission on %s", s.Spec.Diagnostic.Type, verb, sessionResource))
	}
	return nil
}

// checkLocation refuses a location that does not offer the requested vantage
// point, diagnostic and address family.
func (a *SessionAdmission) checkLocation(ctx context.Context, cl client.Reader, s *lgv1alpha.LookingGlassSession) (field.ErrorList, error) {
	spec := field.NewPath("spec")
	locPath := spec.Child("location")
	var loc lgv1alpha.LookingGlassLocation
	if err := cl.Get(ctx, client.ObjectKey{Name: s.Spec.Location}, &loc); err != nil {
		if apierrors.IsNotFound(err) {
			return field.ErrorList{field.Invalid(locPath, s.Spec.Location, "location does not offer Looking Glass")}, nil
		}
		return nil, apierrors.NewInternalError(fmt.Errorf("reading location: %w", err))
	}
	st := loc.Status
	if !st.Available {
		return field.ErrorList{field.Invalid(locPath, s.Spec.Location, "location is not available")}, nil
	}
	if st.ObservedAt == nil || a.now().Sub(st.ObservedAt.Time) > CapabilityMaxAge {
		return field.ErrorList{field.Invalid(locPath, s.Spec.Location, "location capabilities are out of date")}, nil
	}

	vp := s.Spec.VantagePoint
	if vp == "" {
		vp = lgv1alpha.VantagePointFabricEdge
	}
	vpIdx := slices.IndexFunc(st.VantagePoints, func(v lgv1alpha.VantagePointCapabilities) bool { return v.Type == vp })
	if vpIdx < 0 {
		return field.ErrorList{field.Invalid(spec.Child("vantagePoint"), vp, "location does not offer this vantage point")}, nil
	}
	d := s.Spec.Diagnostic
	diags := st.VantagePoints[vpIdx].Diagnostics
	dIdx := slices.IndexFunc(diags, func(c lgv1alpha.DiagnosticCapability) bool { return c.Type == d.Type })
	if dIdx < 0 {
		return field.ErrorList{field.Invalid(spec.Child("diagnostic", "type"), d.Type, "location does not offer this diagnostic")}, nil
	}
	family, famPath, ok := familyOf(d)
	if !ok {
		// The schema guarantees a parsable target; refuse rather than guess.
		return field.ErrorList{field.Invalid(famPath, family, "address family cannot be determined")}, nil
	}
	if !slices.Contains(diags[dIdx].AddressFamilies, family) {
		return field.ErrorList{field.Invalid(famPath, family,
			fmt.Sprintf("location does not offer %s for %s", d.Type, family))}, nil
	}
	return nil, nil
}

// checkDestination applies the destination policy to active probes.
func (a *SessionAdmission) checkDestination(s *lgv1alpha.LookingGlassSession) field.ErrorList {
	d := s.Spec.Diagnostic
	var dest lgv1alpha.ProbeDestination
	var path *field.Path
	switch {
	case d.Type == lgv1alpha.DiagnosticPing && d.Ping != nil:
		dest, path = d.Ping.Destination, field.NewPath("spec", "diagnostic", "ping", "destination")
	case d.Type == lgv1alpha.DiagnosticTraceroute && d.Traceroute != nil:
		dest, path = d.Traceroute.Destination, field.NewPath("spec", "diagnostic", "traceroute", "destination")
	default:
		return nil
	}
	addr, err := netip.ParseAddr(string(dest))
	if err != nil {
		return field.ErrorList{field.Invalid(path, dest, "must be a numeric IP address")}
	}
	if err := a.Destinations.Check(addr); err != nil {
		return field.ErrorList{field.Invalid(path, dest, err.Error())}
	}
	return nil
}

// familyOf returns the address family a diagnostic targets and the field it
// comes from.
func familyOf(d lgv1alpha.Diagnostic) (lgv1alpha.AddressFamily, *field.Path, bool) {
	diag := field.NewPath("spec", "diagnostic")
	switch d.Type {
	case lgv1alpha.DiagnosticBGPSummary:
		p := diag.Child("bgpSummary", "addressFamily")
		if d.BGPSummary == nil {
			return "", p, false
		}
		return d.BGPSummary.AddressFamily, p, d.BGPSummary.AddressFamily != ""
	case lgv1alpha.DiagnosticRouteLookup:
		p := diag.Child("routeLookup", "destination")
		if d.RouteLookup == nil {
			return "", p, false
		}
		if d.RouteLookup.AddressFamily != "" {
			return d.RouteLookup.AddressFamily, diag.Child("routeLookup", "addressFamily"), true
		}
		dest := string(d.RouteLookup.Destination)
		if addr, err := netip.ParseAddr(dest); err == nil {
			return familyOfAddr(addr), p, true
		}
		if pfx, err := netip.ParsePrefix(dest); err == nil {
			return familyOfAddr(pfx.Addr()), p, true
		}
		return "", p, false
	case lgv1alpha.DiagnosticPing, lgv1alpha.DiagnosticTraceroute:
		var dest lgv1alpha.ProbeDestination
		p := diag.Child("ping", "destination")
		if d.Ping != nil {
			dest = d.Ping.Destination
		} else if d.Traceroute != nil {
			dest, p = d.Traceroute.Destination, diag.Child("traceroute", "destination")
		}
		addr, err := netip.ParseAddr(string(dest))
		if err != nil {
			return "", p, false
		}
		return familyOfAddr(addr), p, true
	}
	return "", diag.Child("type"), false
}

func familyOfAddr(a netip.Addr) lgv1alpha.AddressFamily {
	if a.Unmap().Is4() {
		return lgv1alpha.AddressFamilyIPv4
	}
	return lgv1alpha.AddressFamilyIPv6
}
