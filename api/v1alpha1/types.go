package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Contract is the agreement for one kind of action in a namespace.
//
// A request is judged by the contract in its own namespace that has the
// same action. The requester cannot choose a contract.
//
// +kubebuilder:object:root=true
// +kubebuilder:printcolumn:name="Action",type=string,JSONPath=`.spec.action`
// +kubebuilder:printcolumn:name="Grant TTL",type=string,JSONPath=`.spec.grantTTL`
type Contract struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec ContractSpec `json:"spec"`
}

// ContractSpec says what must hold before an action may start.
type ContractSpec struct {
	// Action is the kind of action this contract covers, for example
	// "restart".
	// +kubebuilder:validation:MinLength=1
	Action string `json:"action"`

	// Requires lists the conditions that must be True and fresh for the
	// target.
	// +optional
	// +listType=map
	// +listMapKey=condition
	Requires []Requirement `json:"requires,omitempty"`

	// Budgets limit how many targets of one group may have an open grant.
	// +optional
	// +listType=atomic
	Budgets []Budget `json:"budgets,omitempty"`

	// GrantTTL is how long the requester has to finish and report.
	GrantTTL metav1.Duration `json:"grantTTL"`
}

// Requirement names a condition that must be True and fresh.
type Requirement struct {
	// Condition is the name of the observed fact.
	// +kubebuilder:validation:MinLength=1
	Condition string `json:"condition"`

	// MaxAge is the oldest observation that still counts.
	MaxAge metav1.Duration `json:"maxAge"`
}

// Budget limits open grants among targets that share a label value.
type Budget struct {
	// Label is the target label that forms the groups. A target without
	// this label is refused.
	// +kubebuilder:validation:MinLength=1
	Label string `json:"label"`

	// Value limits the budget to targets with this label value. Targets
	// with another value are outside this budget.
	// +optional
	Value string `json:"value,omitempty"`

	// Max is the number of open grants allowed in one group.
	// +kubebuilder:validation:Minimum=0
	Max int32 `json:"max"`
}

// ContractList is a list of contracts.
//
// +kubebuilder:object:root=true
type ContractList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Contract `json:"items"`
}

// Observation is one fact that a controller reports about a subject.
//
// The controller that owns the fact writes the whole object. The
// coordinator only reads it, so the fact is in the spec and there is no
// status.
//
// +kubebuilder:object:root=true
// +kubebuilder:printcolumn:name="Condition",type=string,JSONPath=`.spec.condition`
// +kubebuilder:printcolumn:name="Subject",type=string,JSONPath=`.spec.subject`
// +kubebuilder:printcolumn:name="Status",type=string,JSONPath=`.spec.status`
// +kubebuilder:printcolumn:name="Observed",type=date,JSONPath=`.spec.observedAt`
type Observation struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec ObservationSpec `json:"spec"`
}

// ObservationSpec is the reported fact.
type ObservationSpec struct {
	// Condition is the name of the fact, for example "PeerCanServe".
	// +kubebuilder:validation:MinLength=1
	Condition string `json:"condition"`

	// Subject is the name of the target the fact is about.
	// +kubebuilder:validation:MinLength=1
	Subject string `json:"subject"`

	// Status is the value of the fact.
	// +kubebuilder:validation:Enum=True;False;Unknown
	Status metav1.ConditionStatus `json:"status"`

	// Source is the controller that reported the fact.
	// +kubebuilder:validation:MinLength=1
	Source string `json:"source"`

	// ObservedAt is when the source measured the fact, not when it wrote
	// this object.
	ObservedAt metav1.MicroTime `json:"observedAt"`
}

// ObservationList is a list of observations.
//
// +kubebuilder:object:root=true
type ObservationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Observation `json:"items"`
}

// ActionRequest asks for permission to do one action on one target.
//
// The requester writes the spec once. The coordinator writes the status.
// A retry is a read of the same object, so it gets the same answer.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Action",type=string,JSONPath=`.spec.action`
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.spec.target.name`
// +kubebuilder:printcolumn:name="Decision",type=string,JSONPath=`.status.decision`
// +kubebuilder:printcolumn:name="Deadline",type=date,JSONPath=`.status.deadline`
type ActionRequest struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec cannot change after the request is created"
	Spec ActionRequestSpec `json:"spec"`

	// +optional
	Status ActionRequestStatus `json:"status,omitempty"`
}

// ActionRequestSpec is what the requester wants to do.
type ActionRequestSpec struct {
	// Action is the kind of action, for example "restart".
	// +kubebuilder:validation:MinLength=1
	Action string `json:"action"`

	// Requester is the controller that asks and that will do the action.
	// +kubebuilder:validation:MinLength=1
	Requester string `json:"requester"`

	// Target is the thing the action changes.
	Target Target `json:"target"`
}

// Target is the thing an action changes.
type Target struct {
	// Name identifies the target. Observations use it as their subject.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Labels put the target into the groups that budgets count.
	// +optional
	Labels map[string]string `json:"labels,omitempty"`
}

// Decision is the coordinator's answer to a request.
// +kubebuilder:validation:Enum=Granted;Refused
type Decision string

const (
	DecisionGranted Decision = "Granted"
	DecisionRefused Decision = "Refused"
)

// ActionRequestStatus is the coordinator's answer.
type ActionRequestStatus struct {
	// Decision is empty until the coordinator has answered.
	// +optional
	Decision Decision `json:"decision,omitempty"`

	// Reasons lists every failed check of a refusal.
	// +optional
	// +listType=atomic
	Reasons []string `json:"reasons,omitempty"`

	// DecidedAt is the time the decision was made with.
	// +optional
	DecidedAt *metav1.MicroTime `json:"decidedAt,omitempty"`

	// Deadline is set for a grant: the requester must finish and report
	// before it.
	// +optional
	Deadline *metav1.MicroTime `json:"deadline,omitempty"`
}

// ActionRequestList is a list of action requests.
//
// +kubebuilder:object:root=true
type ActionRequestList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ActionRequest `json:"items"`
}
