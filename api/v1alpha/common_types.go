package v1alpha

// VantagePoint is where a diagnostic runs from.
// +kubebuilder:validation:Enum=FabricEdge
type VantagePoint string

// VantagePointFabricEdge runs a diagnostic on the fabric edge routers at a
// location. It is the only vantage point in alpha.
const VantagePointFabricEdge VantagePoint = "FabricEdge"

// DiagnosticType names a diagnostic.
// +kubebuilder:validation:Enum=RouteLookup;BGPSummary;Ping;Traceroute
type DiagnosticType string

const (
	// DiagnosticRouteLookup looks up a destination in each router's BGP table.
	DiagnosticRouteLookup DiagnosticType = "RouteLookup"
	// DiagnosticBGPSummary summarizes each router's BGP peers for one address family.
	DiagnosticBGPSummary DiagnosticType = "BGPSummary"
	// DiagnosticPing pings a destination from each router.
	DiagnosticPing DiagnosticType = "Ping"
	// DiagnosticTraceroute traces the path to a destination from each router.
	DiagnosticTraceroute DiagnosticType = "Traceroute"
)

// AddressFamily is an IP address family.
// +kubebuilder:validation:Enum=IPv4;IPv6
type AddressFamily string

const (
	// AddressFamilyIPv4 is IPv4.
	AddressFamilyIPv4 AddressFamily = "IPv4"
	// AddressFamilyIPv6 is IPv6.
	AddressFamilyIPv6 AddressFamily = "IPv6"
)
