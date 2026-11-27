package placement

import (
	"errors"
	"testing"

	clusterv1alpha1 "github.com/karmada-io/api/cluster/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const location = "loc-a"

type clusterOpt func(*clusterv1alpha1.Cluster)

func notReady(c *clusterv1alpha1.Cluster) {
	c.Status.Conditions[0].Status = metav1.ConditionFalse
}

func deleting(c *clusterv1alpha1.Cluster) {
	now := metav1.Now()
	c.DeletionTimestamp = &now
	c.Finalizers = []string{"example.com/hold"}
}

func withLabel(key, value string) clusterOpt {
	return func(c *clusterv1alpha1.Cluster) { c.Labels[key] = value }
}

func withoutLabel(key string) clusterOpt {
	return func(c *clusterv1alpha1.Cluster) { delete(c.Labels, key) }
}

func newCluster(name, loc string, opts ...clusterOpt) *clusterv1alpha1.Cluster {
	c := &clusterv1alpha1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				DefaultLocationLabel: loc,
				DefaultOptInLabel:    "true",
			},
		},
		Status: clusterv1alpha1.ClusterStatus{
			Conditions: []metav1.Condition{{
				Type:   clusterv1alpha1.ClusterConditionReady,
				Status: metav1.ConditionTrue,
				Reason: "ClusterReady",
			}},
		},
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

func newPlacer(t *testing.T, objs ...client.Object) ClusterPlacer {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clusterv1alpha1.Install(scheme); err != nil {
		t.Fatal(err)
	}
	return ClusterPlacer{Reader: fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()}
}

func TestClusterPlacerEligibility(t *testing.T) {
	tests := []struct {
		name     string
		clusters []client.Object
		want     string
	}{
		{
			name:     "the one Ready opted-in cell for the location",
			clusters: []client.Object{newCluster("cell-1", location), newCluster("cell-2", "loc-b")},
			want:     "cell-1",
		},
		{
			name: "ineligible cells are skipped",
			clusters: []client.Object{
				newCluster("cell-1", location),
				newCluster("cell-2", location, notReady),
				newCluster("cell-3", location, deleting),
				newCluster("cell-4", location, withLabel(DefaultOptInLabel, "false")),
				newCluster("cell-5", location, withoutLabel(DefaultOptInLabel)),
			},
			want: "cell-1",
		},
		{name: "no cell", clusters: nil},
		{name: "not Ready", clusters: []client.Object{newCluster("cell-1", location, notReady)}},
		{name: "deleting", clusters: []client.Object{newCluster("cell-1", location, deleting)}},
		{name: "opted out", clusters: []client.Object{newCluster("cell-1", location, withLabel(DefaultOptInLabel, "false"))}},
		{name: "no opt-in label", clusters: []client.Object{newCluster("cell-1", location, withoutLabel(DefaultOptInLabel))}},
		{
			name:     "two eligible cells are ambiguous",
			clusters: []client.Object{newCluster("cell-1", location), newCluster("cell-2", location)},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := newPlacer(t, tt.clusters...).CellFor(t.Context(), location)
			if tt.want == "" {
				if !errors.Is(err, ErrNotPlaceable) {
					t.Fatalf("CellFor() = %q, %v; want ErrNotPlaceable", got, err)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("CellFor() = %q, %v; want %q", got, err, tt.want)
			}
		})
	}
}

func TestClusterPlacerIsDeterministic(t *testing.T) {
	p := newPlacer(t, newCluster("cell-2", "loc-b"), newCluster("cell-1", location), newCluster("cell-3", "loc-c"))
	for range 10 {
		got, err := p.CellFor(t.Context(), location)
		if err != nil || got != "cell-1" {
			t.Fatalf("CellFor() = %q, %v; want cell-1", got, err)
		}
	}
}

func TestClusterPlacerCustomLabels(t *testing.T) {
	c := newCluster("cell-1", location)
	c.Labels = map[string]string{"example.com/site": location, "example.com/lg": "true"}
	p := newPlacer(t, c)
	if _, err := p.CellFor(t.Context(), location); !errors.Is(err, ErrNotPlaceable) {
		t.Fatalf("default labels: CellFor() error = %v; want ErrNotPlaceable", err)
	}
	p.LocationLabel, p.OptInLabel = "example.com/site", "example.com/lg"
	if got, err := p.CellFor(t.Context(), location); err != nil || got != "cell-1" {
		t.Fatalf("custom labels: CellFor() = %q, %v; want cell-1", got, err)
	}
}

func TestStaticPlacer(t *testing.T) {
	p := StaticPlacer{Cells: map[string]string{location: "cell-1", "loc-b": ""}}
	if got, err := p.CellFor(t.Context(), location); err != nil || got != "cell-1" {
		t.Fatalf("CellFor(%q) = %q, %v; want cell-1", location, got, err)
	}
	for _, loc := range []string{"loc-b", "loc-unknown"} {
		if _, err := p.CellFor(t.Context(), loc); !errors.Is(err, ErrNotPlaceable) {
			t.Errorf("CellFor(%q) error = %v; want ErrNotPlaceable", loc, err)
		}
	}
}
