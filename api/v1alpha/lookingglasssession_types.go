package v1alpha

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// LookingGlassSessionSpec is one diagnostic at one location. It is immutable:
// a different request is a new session.
//
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec is immutable; create a new session instead"
type LookingGlassSessionSpec struct {
	// Location is the name of a Location visible in the project that offers
	// Looking Glass.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Location string `json:"location"`

	// VantagePoint is where the diagnostic runs from.
	// +optional
	// +kubebuilder:default=FabricEdge
	VantagePoint VantagePoint `json:"vantagePoint,omitempty"`

	// Diagnostic is the diagnostic to run.
	// +required
	Diagnostic Diagnostic `json:"diagnostic"`

	// ClientPublicKey is the client's ed25519 public key as 64 lowercase hex
	// characters. It is also the client's iroh endpoint ID, and the gateway
	// verifies connection signatures against it.
	// +required
	// +kubebuilder:validation:MinLength=64
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{64}$`
	ClientPublicKey string `json:"clientPublicKey"`
}

// Diagnostic is a discriminated union: exactly the member named by Type is set.
//
// +kubebuilder:validation:XValidation:rule="has(self.routeLookup) == (self.type == 'RouteLookup')",message="routeLookup must be set if and only if type is RouteLookup"
// +kubebuilder:validation:XValidation:rule="has(self.bgpSummary) == (self.type == 'BGPSummary')",message="bgpSummary must be set if and only if type is BGPSummary"
// +kubebuilder:validation:XValidation:rule="has(self.ping) == (self.type == 'Ping')",message="ping must be set if and only if type is Ping"
// +kubebuilder:validation:XValidation:rule="has(self.traceroute) == (self.type == 'Traceroute')",message="traceroute must be set if and only if type is Traceroute"
type Diagnostic struct {
	// Type names the diagnostic and the member that carries its inputs.
	// +required
	Type DiagnosticType `json:"type"`

	// RouteLookup looks up a destination in each router's BGP table.
	// +optional
	RouteLookup *RouteLookupDiagnostic `json:"routeLookup,omitempty"`

	// BGPSummary summarizes each router's BGP peers.
	// +optional
	BGPSummary *BGPSummaryDiagnostic `json:"bgpSummary,omitempty"`

	// Ping pings a destination from each router.
	// +optional
	Ping *PingDiagnostic `json:"ping,omitempty"`

	// Traceroute traces the path to a destination from each router.
	// +optional
	Traceroute *TracerouteDiagnostic `json:"traceroute,omitempty"`
}

// RouteDestination is a bare IPv4 or IPv6 address (longest match) or a prefix
// with no host bits set (exact match). Names are never resolved.
//
// +kubebuilder:validation:MinLength=1
// +kubebuilder:validation:MaxLength=43
// +kubebuilder:validation:XValidation:rule="isIP(self) || (isCIDR(self) && cidr(self).ip() == cidr(self).masked().ip())",message="must be an IP address, or a prefix with no host bits set"
type RouteDestination string

// ProbeDestination is a numeric unicast IPv4 or IPv6 address. Names, zones
// and IPv4-mapped IPv6 addresses are rejected. Admission additionally applies
// the public destination policy.
//
// +kubebuilder:validation:MinLength=1
// +kubebuilder:validation:MaxLength=43
// +kubebuilder:validation:XValidation:rule="isIP(self)",message="must be a numeric IP address"
// +kubebuilder:validation:XValidation:rule="!isIP(self) || ip(self).isGlobalUnicast()",message="must be a unicast address"
type ProbeDestination string

// RouteLookupDiagnostic looks up a destination in each router's BGP table.
//
// +kubebuilder:validation:XValidation:rule="!has(self.addressFamily) || !(isIP(self.destination) || isCIDR(self.destination)) || (isIP(self.destination) ? ip(self.destination).family() : cidr(self.destination).ip().family()) == (self.addressFamily == 'IPv4' ? 4 : 6)",message="addressFamily must match destination"
type RouteLookupDiagnostic struct {
	// Destination is the address or prefix to look up.
	// +required
	Destination RouteDestination `json:"destination"`

	// AddressFamily is inferred from Destination. If set, it must match.
	// +optional
	AddressFamily AddressFamily `json:"addressFamily,omitempty"`
}

// BGPSummaryDiagnostic summarizes each router's BGP peers.
type BGPSummaryDiagnostic struct {
	// AddressFamily selects the peers' address family.
	// +required
	AddressFamily AddressFamily `json:"addressFamily"`
}

// PingDiagnostic pings a destination from each router. The probe count, wait
// and payload size are fixed by the backend in alpha.
type PingDiagnostic struct {
	// Destination is the address to ping.
	// +required
	Destination ProbeDestination `json:"destination"`
}

// TracerouteDiagnostic traces the path to a destination from each router. The
// hop limit, wait and payload size are fixed by the backend in alpha.
type TracerouteDiagnostic struct {
	// Destination is the address to trace.
	// +required
	Destination ProbeDestination `json:"destination"`
}

// LookingGlassSessionStatus is written only by controllers.
type LookingGlassSessionStatus struct {
	// ObservedGeneration is the spec generation this status describes.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Cell is the cell the control plane bound the session to.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	Cell string `json:"cell,omitempty"`

	// Connection says where to connect. It is set only while Ready is
	// SessionReady, and it identifies the endpoint without granting access.
	// +optional
	Connection *SessionConnection `json:"connection,omitempty"`

	// ConnectBefore is the connection deadline.
	// +optional
	ConnectBefore *metav1.Time `json:"connectBefore,omitempty"`

	// StartedAt is when the gateway durably claimed the session.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// FinishedAt is when the session reached a terminal reason.
	// +optional
	FinishedAt *metav1.Time `json:"finishedAt,omitempty"`

	// ExpiresAt is when the control plane deletes the terminal session.
	// +optional
	ExpiresAt *metav1.Time `json:"expiresAt,omitempty"`

	// RetryAfter is, for Throttled only, the backend's advisory delay before
	// a new session. It is not a reservation.
	// +optional
	RetryAfter *metav1.Duration `json:"retryAfter,omitempty"`

	// Coverage summarizes which routers answered. It is set once the gateway
	// received the backend's snapshot.
	// +optional
	Coverage *Coverage `json:"coverage,omitempty"`

	// Conditions holds the Ready condition, whose reason drives clients.
	// +optional
	// +listType=map
	// +listMapKey=type
	// +kubebuilder:validation:MaxItems=8
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// SessionConnection identifies the gateway endpoint a client connects to.
type SessionConnection struct {
	// EndpointID is the gateway's iroh endpoint ID.
	// +required
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{64}$`
	EndpointID string `json:"endpointID"`

	// RelayURLs are the relays to use, nearest first.
	// +optional
	// +listType=atomic
	// +kubebuilder:validation:MaxItems=8
	// +kubebuilder:validation:items:MaxLength=2048
	RelayURLs []string `json:"relayURLs,omitempty"`

	// Target is the gateway's host:port for the CONNECT request.
	// +required
	// +kubebuilder:validation:MaxLength=261
	Target string `json:"target"`
}

// Coverage copies the backend's terminal counts.
type Coverage struct {
	// Complete is true only when the backend's terminal Complete event was
	// received. When false, the counts are the outcomes actually received and
	// nothing is inferred about unresolved routers.
	Complete bool `json:"complete"`

	// Expected is the number of routers in the snapshot.
	// +kubebuilder:validation:Minimum=0
	Expected int32 `json:"expected"`

	// Selected is the number of routers the snapshot selected.
	// +kubebuilder:validation:Minimum=0
	Selected int32 `json:"selected"`

	// Successful is the number of observations received.
	// +kubebuilder:validation:Minimum=0
	Successful int32 `json:"successful"`

	// Failed is the number of router failures received.
	// +kubebuilder:validation:Minimum=0
	Failed int32 `json:"failed"`

	// Omitted is the number of routers that were not selected.
	// +kubebuilder:validation:Minimum=0
	Omitted int32 `json:"omitted"`

	// Truncated is the number of successful observations whose payload is
	// incomplete.
	// +kubebuilder:validation:Minimum=0
	Truncated int32 `json:"truncated"`
}

// LookingGlassSession runs one diagnostic at one location for a client that
// connects through Datum Connect.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced
// +kubebuilder:printcolumn:name="Location",type=string,JSONPath=`.spec.location`
// +kubebuilder:printcolumn:name="Diagnostic",type=string,JSONPath=`.spec.diagnostic.type`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type LookingGlassSession struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +required
	Spec LookingGlassSessionSpec `json:"spec"`

	// +optional
	Status LookingGlassSessionStatus `json:"status,omitempty"`
}

// LookingGlassSessionList is a list of LookingGlassSessions.
//
// +kubebuilder:object:root=true
type LookingGlassSessionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []LookingGlassSession `json:"items"`
}
