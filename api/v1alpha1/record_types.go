package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// DecisionRecord holds everything one decision was made from, and the
// answer. Played through the decision core again, it gives the same
// answer, so a decision can be checked later.
//
// The coordinator writes one before it writes a decision, and the status
// of the request names it. It is never changed. It is owned by its
// request and goes away with it.
//
// The name ends in a hash of the content. A record that lost a race
// between two coordinators can stay behind; the one that counts is the one
// the request names.
//
// +kubebuilder:object:root=true
// +kubebuilder:printcolumn:name="Request",type=string,JSONPath=`.spec.request.name`
// +kubebuilder:printcolumn:name="Decision",type=string,JSONPath=`.spec.decision.decision`
// +kubebuilder:printcolumn:name="Time",type=date,JSONPath=`.spec.time`
type DecisionRecord struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="a decision record cannot change"
	Spec DecisionRecordSpec `json:"spec"`
}

// DecisionRecordSpec is the input and the output of one decision.
type DecisionRecordSpec struct {
	// Time is the time the decision was made with.
	Time metav1.MicroTime `json:"time"`

	// Request is the request that was decided.
	Request RecordedRequest `json:"request"`

	// Contract is the contract that was used. It is missing when the
	// coordinator refused because there was not exactly one contract for
	// the action; the reason is then in the decision.
	// +optional
	Contract *RecordedContract `json:"contract,omitempty"`

	// Observations holds every observation the decision could look at:
	// those about the target for a condition the contract requires, in the
	// order they were read.
	// +optional
	// +listType=atomic
	Observations []ObservationSpec `json:"observations,omitempty"`

	// OpenGrants holds every grant in the namespace that was open.
	// +optional
	// +listType=atomic
	OpenGrants []RecordedGrant `json:"openGrants,omitempty"`

	// Decision is the answer.
	Decision RecordedDecision `json:"decision"`
}

// RecordedRequest is a request as it was when it was decided.
type RecordedRequest struct {
	Name string    `json:"name"`
	UID  types.UID `json:"uid"`

	Action    string `json:"action"`
	Requester string `json:"requester"`
	Target    Target `json:"target"`
	// +optional
	ExpiresAt *metav1.Time `json:"expiresAt,omitempty"`
}

// RecordedContract is a contract as it was when it was used.
type RecordedContract struct {
	Name string       `json:"name"`
	Spec ContractSpec `json:"spec"`
}

// RecordedGrant is a grant that was open at the time of a decision.
type RecordedGrant struct {
	// Request is the name of the request that held the grant.
	Request   string           `json:"request"`
	Action    string           `json:"action"`
	Target    Target           `json:"target"`
	GrantedAt metav1.MicroTime `json:"grantedAt"`
	Deadline  metav1.MicroTime `json:"deadline"`
	// Failed is true when a failure was reported for the grant.
	// +optional
	Failed bool `json:"failed,omitempty"`
	// +optional
	FailedAt *metav1.MicroTime `json:"failedAt,omitempty"`
}

// RecordedDecision is the answer in a record.
type RecordedDecision struct {
	Decision Decision `json:"decision"`
	// +optional
	// +listType=atomic
	Reasons []string `json:"reasons,omitempty"`
	// Deadline is set for a grant.
	// +optional
	Deadline *metav1.MicroTime `json:"deadline,omitempty"`
}

// DecisionRecordList is a list of decision records.
//
// +kubebuilder:object:root=true
type DecisionRecordList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DecisionRecord `json:"items"`
}

// ReleaseRecord holds everything one release by evidence was decided
// from, and the evidence that closed the grant. Played through the
// decision core again, it gives the same answer.
//
// The coordinator writes one before it writes a release, and
// status.released of the request names it. It is never changed. It is
// owned by its request and goes away with it.
//
// +kubebuilder:object:root=true
// +kubebuilder:printcolumn:name="Request",type=string,JSONPath=`.spec.request.name`
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.spec.grant.target.name`
// +kubebuilder:printcolumn:name="Time",type=date,JSONPath=`.spec.time`
type ReleaseRecord struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="a release record cannot change"
	Spec ReleaseRecordSpec `json:"spec"`
}

// ReleaseRecordSpec is the input and the output of one release.
type ReleaseRecordSpec struct {
	// Time is the time the release was decided with.
	Time metav1.MicroTime `json:"time"`

	// Request names the request that held the grant.
	Request RecordedRequestRef `json:"request"`

	// Contract is the contract that was used.
	Contract RecordedContract `json:"contract"`

	// Grant is the grant that was closed, as it was at that time.
	Grant RecordedGrant `json:"grant"`

	// Observations holds every observation the release could look at:
	// those about the target for a condition in releaseWhen, in the order
	// they were read.
	// +optional
	// +listType=atomic
	Observations []ObservationSpec `json:"observations,omitempty"`

	// Evidence holds the observations that closed the grant, one for each
	// condition in releaseWhen.
	// +listType=atomic
	Evidence []ObservationSpec `json:"evidence"`
}

// RecordedRequestRef names a request in a release record.
type RecordedRequestRef struct {
	Name string    `json:"name"`
	UID  types.UID `json:"uid"`
}

// ReleaseRecordList is a list of release records.
//
// +kubebuilder:object:root=true
type ReleaseRecordList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ReleaseRecord `json:"items"`
}
