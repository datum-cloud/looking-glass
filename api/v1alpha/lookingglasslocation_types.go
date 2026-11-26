package v1alpha

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// LookingGlassLocationStatus describes a location and the capabilities it
// offers. A location provider supplies the display data, and the serving
// cell's backend supplies the capabilities.
type LookingGlassLocationStatus struct {
	// DisplayName is a human-readable name for the location.
	// +optional
	// +kubebuilder:validation:MaxLength=128
	DisplayName string `json:"displayName,omitempty"`

	// Topology places the location for grouping and filtering.
	// +optional
	Topology *LocationTopology `json:"topology,omitempty"`

	// Coordinates place the location on a map.
	// +optional
	Coordinates *LocationCoordinates `json:"coordinates,omitempty"`

	// Available is false when the backend disabled the service, serves no
	// eligible router at the location, runs an unsupported API major, or its
	// capabilities are stale. An unavailable location cannot be used.
	// +optional
	Available bool `json:"available"`

	// VantagePoints lists the vantage points and the diagnostics each offers.
	// +optional
	// +listType=map
	// +listMapKey=type
	// +kubebuilder:validation:MaxItems=8
	VantagePoints []VantagePointCapabilities `json:"vantagePoints,omitempty"`

	// Limits are the session limits at this location.
	// +optional
	Limits *LocationLimits `json:"limits,omitempty"`

	// ObservedAt is when the capabilities were read from the cell.
	// +optional
	ObservedAt *metav1.Time `json:"observedAt,omitempty"`
}

// LocationTopology places a location for grouping and filtering.
type LocationTopology struct {
	// CityCode is a short code for the location's city.
	// +optional
	// +kubebuilder:validation:MaxLength=16
	CityCode string `json:"cityCode,omitempty"`

	// Region is the region the location belongs to.
	// +optional
	// +kubebuilder:validation:MaxLength=63
	Region string `json:"region,omitempty"`
}

// LocationCoordinates are a location's latitude and longitude in decimal
// degrees, as strings so that no precision is lost.
type LocationCoordinates struct {
	// Latitude in decimal degrees, from -90 to 90.
	// +required
	// +kubebuilder:validation:Pattern=`^-?([0-8]?[0-9](\.[0-9]+)?|90(\.0+)?)$`
	Latitude string `json:"latitude"`

	// Longitude in decimal degrees, from -180 to 180.
	// +required
	// +kubebuilder:validation:Pattern=`^-?((1[0-7][0-9]|[0-9]?[0-9])(\.[0-9]+)?|180(\.0+)?)$`
	Longitude string `json:"longitude"`
}

// VantagePointCapabilities lists the diagnostics one vantage point offers.
type VantagePointCapabilities struct {
	// Type is the vantage point.
	// +required
	Type VantagePoint `json:"type"`

	// Diagnostics lists the diagnostics offered from this vantage point.
	// +optional
	// +listType=map
	// +listMapKey=type
	// +kubebuilder:validation:MaxItems=16
	Diagnostics []DiagnosticCapability `json:"diagnostics,omitempty"`
}

// DiagnosticCapability is one diagnostic and the address families it supports.
type DiagnosticCapability struct {
	// Type is the diagnostic.
	// +required
	Type DiagnosticType `json:"type"`

	// AddressFamilies lists the supported address families.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MaxItems=2
	AddressFamilies []AddressFamily `json:"addressFamilies,omitempty"`
}

// LocationLimits are the session limits at a location.
type LocationLimits struct {
	// ConnectWindow is how long a client has to connect after SessionReady.
	// +optional
	ConnectWindow *metav1.Duration `json:"connectWindow,omitempty"`

	// ExecutionTimeout bounds execution after the session is claimed.
	// +optional
	ExecutionTimeout *metav1.Duration `json:"executionTimeout,omitempty"`

	// MaxRouters is the most routers one session fans out to.
	// +optional
	// +kubebuilder:validation:Minimum=0
	MaxRouters int32 `json:"maxRouters,omitempty"`
}

// LookingGlassLocation is a location that offers Looking Glass, with its
// display data and capabilities. It is the only location API clients need.
// With the default Milo location provider, it has the same name as its Milo
// Location.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Available",type=boolean,JSONPath=`.status.available`
// +kubebuilder:printcolumn:name="Observed",type=date,JSONPath=`.status.observedAt`
type LookingGlassLocation struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +optional
	Status LookingGlassLocationStatus `json:"status,omitempty"`
}

// LookingGlassLocationList is a list of LookingGlassLocations.
//
// +kubebuilder:object:root=true
type LookingGlassLocationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []LookingGlassLocation `json:"items"`
}
