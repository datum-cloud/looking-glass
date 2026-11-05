package v1alpha_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	lgv1alpha "go.datum.net/looking-glass/api/v1alpha"
)

// crd holds the CRD fields the IAM registration must agree with.
type crd struct {
	Spec struct {
		Group string `json:"group"`
		Names struct {
			Kind     string `json:"kind"`
			Plural   string `json:"plural"`
			Singular string `json:"singular"`
		} `json:"names"`
		Versions []struct {
			Subresources map[string]any `json:"subresources"`
		} `json:"versions"`
	} `json:"spec"`
}

type subresource struct {
	Name        string   `json:"name"`
	Permissions []string `json:"permissions"`
}

// protectedResource holds the Milo iam.miloapis.com/v1alpha1 fields checked here.
type protectedResource struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		ServiceRef struct {
			Name string `json:"name"`
		} `json:"serviceRef"`
		Kind            string        `json:"kind"`
		Plural          string        `json:"plural"`
		Singular        string        `json:"singular"`
		Permissions     []string      `json:"permissions"`
		Subresources    []subresource `json:"subresources"`
		ParentResources []struct {
			APIGroup string `json:"apiGroup"`
			Kind     string `json:"kind"`
		} `json:"parentResources"`
	} `json:"spec"`
}

func readYAML(t *testing.T, path string, into any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(data, into); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}

// TestEveryCRDHasAProtectedResource keeps the Milo IAM registrations in step
// with the generated CRDs, so a new or renamed kind cannot ship unprotected.
func TestEveryCRDHasAProtectedResource(t *testing.T) {
	root := filepath.Join("..", "..", "config")
	crdPaths, err := filepath.Glob(filepath.Join(root, "crd", "bases", "*.yaml"))
	if err != nil || len(crdPaths) == 0 {
		t.Fatalf("no CRDs found: %v", err)
	}
	prPaths, err := filepath.Glob(filepath.Join(root, "iam", "protected-resources", "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	byKind := map[string]protectedResource{}
	for _, p := range prPaths {
		if filepath.Base(p) == "kustomization.yaml" {
			continue
		}
		var pr protectedResource
		readYAML(t, p, &pr)
		if pr.APIVersion != "iam.miloapis.com/v1alpha1" || pr.Kind != "ProtectedResource" {
			t.Errorf("%s: %s %s, want iam.miloapis.com/v1alpha1 ProtectedResource", p, pr.APIVersion, pr.Kind)
			continue
		}
		byKind[pr.Spec.Kind] = pr
	}
	if len(byKind) != len(crdPaths) {
		t.Errorf("%d ProtectedResources for %d CRDs", len(byKind), len(crdPaths))
	}

	for _, p := range crdPaths {
		var c crd
		readYAML(t, p, &c)
		names := c.Spec.Names
		pr, ok := byKind[names.Kind]
		if !ok {
			t.Errorf("CRD %s has no ProtectedResource", names.Kind)
			continue
		}
		if want := c.Spec.Group + "-" + names.Singular; pr.Metadata.Name != want {
			t.Errorf("%s: name %q, want %q", names.Kind, pr.Metadata.Name, want)
		}
		if pr.Spec.ServiceRef.Name != c.Spec.Group {
			t.Errorf("%s: serviceRef %q, want %q", names.Kind, pr.Spec.ServiceRef.Name, c.Spec.Group)
		}
		if pr.Spec.Plural != names.Plural || pr.Spec.Singular != names.Singular {
			t.Errorf("%s: plural/singular %q/%q, want %q/%q",
				names.Kind, pr.Spec.Plural, pr.Spec.Singular, names.Plural, names.Singular)
		}
		for _, verb := range []string{"get", "list", "watch", "create", "delete"} {
			if !slices.Contains(pr.Spec.Permissions, verb) {
				t.Errorf("%s: missing permission %q", names.Kind, verb)
			}
		}
		for _, v := range c.Spec.Versions {
			for sub := range v.Subresources {
				if !slices.ContainsFunc(pr.Spec.Subresources, func(s subresource) bool { return s.Name == sub }) {
					t.Errorf("%s: subresource %q not registered", names.Kind, sub)
				}
			}
		}
		if len(pr.Spec.ParentResources) != 1 ||
			pr.Spec.ParentResources[0].APIGroup != "resourcemanager.miloapis.com" ||
			pr.Spec.ParentResources[0].Kind != "Project" {
			t.Errorf("%s: parentResources %+v, want only resourcemanager.miloapis.com Project", names.Kind, pr.Spec.ParentResources)
		}
	}

	// The contract's diagnostic-specific permissions.
	session := byKind["LookingGlassSession"]
	for _, perm := range []string{"probe", "readroutes"} {
		if !slices.Contains(session.Spec.Permissions, perm) {
			t.Errorf("LookingGlassSession: missing permission %q", perm)
		}
	}
}

// role holds the Milo iam.miloapis.com/v1alpha1 Role fields checked here.
type role struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		InheritedRoles []struct {
			Name string `json:"name"`
		} `json:"inheritedRoles"`
		IncludedPermissions []string `json:"includedPermissions"`
	} `json:"spec"`
}

// registeredPermissions returns every permission string the ProtectedResources
// define, in Milo's "<service>/<plural>[/<subresource>].<verb>" form.
func registeredPermissions(t *testing.T) map[string]bool {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("..", "..", "config", "iam", "protected-resources", "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	perms := map[string]bool{}
	for _, p := range paths {
		if filepath.Base(p) == "kustomization.yaml" {
			continue
		}
		var pr protectedResource
		readYAML(t, p, &pr)
		for _, v := range pr.Spec.Permissions {
			perms[pr.Spec.ServiceRef.Name+"/"+pr.Spec.Plural+"."+v] = true
		}
		for _, sub := range pr.Spec.Subresources {
			for _, v := range sub.Permissions {
				perms[pr.Spec.ServiceRef.Name+"/"+pr.Spec.Plural+"/"+sub.Name+"."+v] = true
			}
		}
	}
	return perms
}

func readRoles(t *testing.T) map[string]role {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("..", "..", "config", "iam", "roles", "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	roles := map[string]role{}
	for _, p := range paths {
		if filepath.Base(p) == "kustomization.yaml" {
			continue
		}
		var r role
		readYAML(t, p, &r)
		if r.APIVersion != "iam.miloapis.com/v1alpha1" || r.Kind != "Role" {
			t.Errorf("%s: %s %s, want iam.miloapis.com/v1alpha1 Role", p, r.APIVersion, r.Kind)
		}
		roles[r.Metadata.Name] = r
	}
	return roles
}

// effectivePermissions resolves a role's inherited roles.
func effectivePermissions(t *testing.T, roles map[string]role, name string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	var walk func(string, int)
	walk = func(n string, depth int) {
		if depth > len(roles) {
			t.Fatalf("role %s: inheritance cycle", name)
		}
		r, ok := roles[n]
		if !ok {
			t.Errorf("role %s inherits unknown role %s", name, n)
			return
		}
		for _, p := range r.Spec.IncludedPermissions {
			out[p] = true
		}
		for _, in := range r.Spec.InheritedRoles {
			walk(in.Name, depth+1)
		}
	}
	walk(name, 0)
	return out
}

// TestRolesMatchContract keeps the roles in step with the ProtectedResources
// and the contract's permission table.
func TestRolesMatchContract(t *testing.T) {
	perms := registeredPermissions(t)
	roles := readRoles(t)
	const svc = "lookingglass.datumapis.com/"
	for name, r := range roles {
		for _, p := range r.Spec.IncludedPermissions {
			switch {
			case strings.HasPrefix(p, svc):
				if !perms[p] {
					t.Errorf("role %s: permission %s is not registered", name, p)
				}
			case strings.HasPrefix(p, "quota.miloapis.com/allowancebuckets"):
			default:
				t.Errorf("role %s: unexpected foreign permission %s", name, p)
			}
		}
	}

	sessions := svc + "lookingglasssessions."
	locations := svc + "lookingglasslocations."
	read := []string{
		sessions + "get", sessions + "list", sessions + "watch",
		locations + "get", locations + "list", locations + "watch",
	}
	readWrite := append(slices.Clone(read), sessions+"create", sessions+"delete")
	tests := []struct {
		role    string
		want    []string
		without []string
	}{
		{"lookingglass.datumapis.com-viewer", read,
			[]string{sessions + "create", sessions + "delete", sessions + "probe", sessions + "readroutes"}},
		{"lookingglass.datumapis.com-prober", append(slices.Clone(readWrite), sessions+"probe"),
			[]string{sessions + "readroutes"}},
		{"lookingglass.datumapis.com-route-viewer", append(slices.Clone(readWrite), sessions+"readroutes"),
			[]string{sessions + "probe"}},
		{"lookingglass.datumapis.com-admin", append(slices.Clone(readWrite), sessions+"probe", sessions+"readroutes"), nil},
	}
	if len(roles) != len(tests) {
		t.Errorf("%d roles, want %d", len(roles), len(tests))
	}
	for _, tc := range tests {
		got := effectivePermissions(t, roles, tc.role)
		for _, p := range tc.want {
			if !got[p] {
				t.Errorf("%s: missing %s", tc.role, p)
			}
		}
		for _, p := range tc.without {
			if got[p] {
				t.Errorf("%s: must not grant %s", tc.role, p)
			}
		}
		// Customers never write sessions in place, and only read locations.
		for p := range got {
			inPlace := strings.HasSuffix(p, ".update") || strings.HasSuffix(p, ".patch")
			locationWrite := strings.HasPrefix(p, locations) && !slices.Contains(read, p)
			if inPlace || locationWrite {
				t.Errorf("%s: must not grant %s", tc.role, p)
			}
		}
	}
}

// TestQuotaCountsSessions checks that the quota registration, claim policy and
// default grant agree on the resource type and the contract's allowance.
func TestQuotaCountsSessions(t *testing.T) {
	root := filepath.Join("..", "..", "config", "quota")
	const resourceType = "lookingglass.datumapis.com/lookingglasssessions"

	var reg struct {
		Spec struct {
			ResourceType      string `json:"resourceType"`
			ClaimingResources []struct {
				APIGroup string `json:"apiGroup"`
				Kind     string `json:"kind"`
			} `json:"claimingResources"`
		} `json:"spec"`
	}
	readYAML(t, filepath.Join(root, "resource-registration.yaml"), &reg)
	if reg.Spec.ResourceType != resourceType {
		t.Errorf("registration resourceType %q, want %q", reg.Spec.ResourceType, resourceType)
	}
	if cr := reg.Spec.ClaimingResources; len(cr) != 1 || cr[0].Kind != "LookingGlassSession" ||
		cr[0].APIGroup != lgv1alpha.GroupVersion.Group {
		t.Errorf("registration claimingResources %+v", cr)
	}

	var claim struct {
		Spec struct {
			Trigger struct {
				Resource struct {
					APIVersion string `json:"apiVersion"`
					Kind       string `json:"kind"`
				} `json:"resource"`
			} `json:"trigger"`
			Target struct {
				ResourceClaimTemplate struct {
					Spec struct {
						Requests []struct {
							ResourceType string `json:"resourceType"`
							Amount       int64  `json:"amount"`
						} `json:"requests"`
					} `json:"spec"`
				} `json:"resourceClaimTemplate"`
			} `json:"target"`
		} `json:"spec"`
	}
	readYAML(t, filepath.Join(root, "claim-creation-policy.yaml"), &claim)
	if r := claim.Spec.Trigger.Resource; r.APIVersion != lgv1alpha.GroupVersion.String() || r.Kind != "LookingGlassSession" {
		t.Errorf("claim trigger %+v", r)
	}
	reqs := claim.Spec.Target.ResourceClaimTemplate.Spec.Requests
	if len(reqs) != 1 || reqs[0].ResourceType != resourceType || reqs[0].Amount != 1 {
		t.Errorf("claim requests %+v, want one %s of 1", reqs, resourceType)
	}

	var grant struct {
		Spec struct {
			Target struct {
				ResourceGrantTemplate struct {
					Spec struct {
						Allowances []struct {
							ResourceType string `json:"resourceType"`
							Buckets      []struct {
								Amount int64 `json:"amount"`
							} `json:"buckets"`
						} `json:"allowances"`
					} `json:"spec"`
				} `json:"resourceGrantTemplate"`
			} `json:"target"`
		} `json:"spec"`
	}
	readYAML(t, filepath.Join(root, "grant-creation-policy.yaml"), &grant)
	al := grant.Spec.Target.ResourceGrantTemplate.Spec.Allowances
	if len(al) != 1 || al[0].ResourceType != resourceType || len(al[0].Buckets) != 1 || al[0].Buckets[0].Amount != 20 {
		t.Errorf("grant allowances %+v, want 20 %s", al, resourceType)
	}
}
