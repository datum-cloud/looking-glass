// Package placement finds the one cell that serves a Looking Glass location.
//
// Placement reads only trusted sources: an operator-configured map, or the
// Karmada Cluster objects on the federation hub, whose labels the platform
// writes. It never reads data a cell writes, so a cell cannot steer sessions
// to itself.
package placement

import (
	"context"
	"errors"
	"fmt"

	clusterv1alpha1 "github.com/karmada-io/api/cluster/v1alpha1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// DefaultLocationLabel is the label the platform writes on each cell's
	// Karmada Cluster to name the location the cell serves. Milo locations
	// own it (ServingLocationTopologyLabel in go.miloapis.com/locations), and
	// Compute selects cells by the same label.
	DefaultLocationLabel = "topology.datum.net/location"

	// DefaultOptInLabel is the label that opts a cell's Karmada Cluster into
	// Looking Glass. Looking Glass owns it; the platform sets it to "true" on
	// cells that run the Looking Glass gateway and backend.
	DefaultOptInLabel = "lookingglass.datumapis.com/enabled"
)

// ErrNotPlaceable means no single eligible cell serves the location.
// Placement may succeed later, for example once a cell becomes Ready.
var ErrNotPlaceable = errors.New("no single eligible cell serves the location")

// Placer finds the cell that serves a location.
type Placer interface {
	// CellFor returns the name of the one eligible cell that serves
	// location. It returns an error wrapping ErrNotPlaceable when there is no
	// such cell, and any other error when the source could not be read.
	CellFor(ctx context.Context, location string) (string, error)
}

// StaticPlacer serves locations from a fixed location-to-cell map, for
// development and tests.
type StaticPlacer struct {
	// Cells maps a location name to the cell that serves it.
	Cells map[string]string
}

// CellFor implements Placer.
func (p StaticPlacer) CellFor(_ context.Context, location string) (string, error) {
	cell, ok := p.Cells[location]
	if !ok || cell == "" {
		return "", fmt.Errorf("%w: location %q has no configured cell", ErrNotPlaceable, location)
	}
	return cell, nil
}

// ClusterPlacer serves locations from the Karmada Cluster objects on the
// federation hub. A cluster is eligible for a location when it carries the
// location label with that location's name and the opt-in label set to
// "true", its Ready condition is True, and it is not being deleted. Exactly
// one cluster must be eligible: the contract gives every location one cell,
// so two eligible clusters are a platform misconfiguration, not a choice.
type ClusterPlacer struct {
	// Reader reads Karmada Clusters on the federation hub.
	Reader client.Reader
	// LocationLabel defaults to DefaultLocationLabel.
	LocationLabel string
	// OptInLabel defaults to DefaultOptInLabel.
	OptInLabel string
}

// CellFor implements Placer.
func (p ClusterPlacer) CellFor(ctx context.Context, location string) (string, error) {
	locationLabel := p.LocationLabel
	if locationLabel == "" {
		locationLabel = DefaultLocationLabel
	}
	optInLabel := p.OptInLabel
	if optInLabel == "" {
		optInLabel = DefaultOptInLabel
	}
	var clusters clusterv1alpha1.ClusterList
	if err := p.Reader.List(ctx, &clusters, client.MatchingLabels{
		locationLabel: location,
		optInLabel:    "true",
	}); err != nil {
		return "", fmt.Errorf("listing cells for location %q: %w", location, err)
	}
	var eligible []string
	for i := range clusters.Items {
		if Eligible(&clusters.Items[i]) {
			eligible = append(eligible, clusters.Items[i].Name)
		}
	}
	switch len(eligible) {
	case 0:
		return "", fmt.Errorf("%w: no Ready cell for location %q", ErrNotPlaceable, location)
	case 1:
		return eligible[0], nil
	default:
		return "", fmt.Errorf("%w: %d Ready cells for location %q", ErrNotPlaceable, len(eligible), location)
	}
}

// Eligible reports whether a cluster can take new sessions: it is Ready and
// not being deleted. Labels are checked by the caller's selector.
func Eligible(c *clusterv1alpha1.Cluster) bool {
	if c.DeletionTimestamp != nil {
		return false
	}
	return meta.IsStatusConditionPresentAndEqual(c.Status.Conditions, clusterv1alpha1.ClusterConditionReady, metav1.ConditionTrue)
}
