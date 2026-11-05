package v1alpha

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// LookingGlassLocationStatus is the capabilities a location offers, kept
// current from its cell's backend.
type LookingGlassLocationStatus struct {
	// Available is false when the cell disabled the service, lacks a required
	// backend feature, or its capabilities are stale. An unavailable location
	// cannot be used.
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

// LookingGlassLocation is the read-only capability record of a Location that
// offers Looking Glass. It has the same name as the Location.
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
