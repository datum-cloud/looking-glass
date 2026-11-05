package v1alpha_test

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"sigs.k8s.io/yaml"
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
