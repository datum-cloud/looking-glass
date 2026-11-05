// Package v1alpha contains the lookingglass.datumapis.com/v1alpha project API:
// LookingGlassSession and LookingGlassLocation. docs/api/v1alpha-contract.md
// is the normative contract these types implement.
//
// +kubebuilder:object:generate=true
// +groupName=lookingglass.datumapis.com
package v1alpha

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// GroupVersion is the group and version of the Looking Glass API.
var GroupVersion = schema.GroupVersion{Group: "lookingglass.datumapis.com", Version: "v1alpha"}

var (
	// SchemeBuilder registers the API types with a runtime scheme.
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

	// AddToScheme adds the API types to a runtime scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

func addKnownTypes(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(GroupVersion,
		&LookingGlassSession{},
		&LookingGlassSessionList{},
		&LookingGlassLocation{},
		&LookingGlassLocationList{},
	)
	metav1.AddToGroupVersion(scheme, GroupVersion)
	return nil
}
