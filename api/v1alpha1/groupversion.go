// Package v1alpha1 holds the Allostat resource types.
//
// +kubebuilder:object:generate=true
// +groupName=allostat.io
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// GroupVersion is the API group and version of all types in this package.
var GroupVersion = schema.GroupVersion{Group: "allostat.io", Version: "v1alpha1"}

var (
	// SchemeBuilder collects the functions that add the types to a scheme.
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)
	// AddToScheme adds the types to a scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

func addKnownTypes(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion,
		&Contract{}, &ContractList{},
		&Observation{}, &ObservationList{},
		&ActionRequest{}, &ActionRequestList{},
		&Ledger{}, &LedgerList{},
	)
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
}
