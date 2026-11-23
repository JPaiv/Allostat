package v1alpha1

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/JPaiv/Allostat/admission"
)

// NewDecisionRecord builds the record of one decision.
//
// contract is nil when the coordinator refused without one. observations
// is everything that was read in the namespace; the record keeps the ones
// the decision could look at. open is every open grant that was given to
// the decision core.
func NewDecisionRecord(ar *ActionRequest, contract *Contract, observations []Observation, open []admission.Grant, now time.Time, d admission.Decision) (*DecisionRecord, error) {
	spec := DecisionRecordSpec{
		Time: metav1.NewMicroTime(now),
		Request: RecordedRequest{
			Name: ar.Name, UID: ar.UID,
			Action: ar.Spec.Action, Requester: ar.Spec.Requester,
			Target: *ar.Spec.Target.DeepCopy(), ExpiresAt: ar.Spec.ExpiresAt.DeepCopy(),
		},
		Decision: RecordedDecision{Decision: DecisionRefused, Reasons: slices.Clone(d.Reasons)},
	}
	if d.Granted {
		deadline := metav1.NewMicroTime(d.Grant.Deadline)
		spec.Decision = RecordedDecision{Decision: DecisionGranted, Deadline: &deadline}
	}
	if contract != nil {
		spec.Contract = &RecordedContract{Name: contract.Name, Spec: *contract.Spec.DeepCopy()}
		required := map[string]bool{}
		for _, r := range contract.Spec.Requires {
			required[r.Condition] = true
		}
		for i := range observations {
			o := &observations[i].Spec
			if o.Subject == ar.Spec.Target.Name && required[o.Condition] {
				spec.Observations = append(spec.Observations, *o.DeepCopy())
			}
		}
	}
	for _, g := range open {
		spec.OpenGrants = append(spec.OpenGrants, recordGrant(g))
	}

	// The name is the request name and a hash of the content. The same
	// decision made twice gives the same record; another one gives another.
	meta, err := recordMeta(ar, spec)
	if err != nil {
		return nil, err
	}
	return &DecisionRecord{ObjectMeta: meta, Spec: spec}, nil
}

// recordMeta names a record after its request and a hash of its content,
// and makes the request its owner. The same content gives the same name.
func recordMeta(ar *ActionRequest, spec any) (metav1.ObjectMeta, error) {
	content, err := json.Marshal(spec)
	if err != nil {
		return metav1.ObjectMeta{}, err
	}
	sum := sha256.Sum256(content)
	name := ar.Name
	if len(name) > 200 {
		name = name[:200]
	}
	return metav1.ObjectMeta{
		Namespace: ar.Namespace,
		Name:      name + "-" + hex.EncodeToString(sum[:6]),
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: GroupVersion.String(), Kind: "ActionRequest",
			Name: ar.Name, UID: ar.UID,
		}},
	}, nil
}

func recordGrant(g admission.Grant) RecordedGrant {
	rg := RecordedGrant{
		Request: g.RequestID, Action: g.Action,
		Target:    Target{Name: g.Target.Name, Labels: g.Target.Labels},
		GrantedAt: metav1.NewMicroTime(g.GrantedAt), Deadline: metav1.NewMicroTime(g.Deadline),
		Failed: g.Failed,
	}
	if !g.FailedAt.IsZero() {
		at := metav1.NewMicroTime(g.FailedAt)
		rg.FailedAt = &at
	}
	return *rg.DeepCopy()
}

func (g RecordedGrant) toAdmission() admission.Grant {
	ag := admission.Grant{
		RequestID: g.Request, Action: g.Action, Target: g.Target.toAdmission(),
		GrantedAt: g.GrantedAt.Time, Deadline: g.Deadline.Time, Failed: g.Failed,
	}
	if g.FailedAt != nil {
		ag.FailedAt = g.FailedAt.Time
	}
	return ag
}

func observationsToAdmission(specs []ObservationSpec) []admission.Observation {
	out := make([]admission.Observation, 0, len(specs))
	for i := range specs {
		out = append(out, (&Observation{Spec: specs[i]}).ToAdmission())
	}
	return out
}

// ErrNoContract says that a record has no contract. The coordinator
// refused before it asked the decision core, so there is nothing to play
// again.
var ErrNoContract = errors.New("the record has no contract: the coordinator refused before the decision core was asked")

// Replay plays the record through the decision core and returns the
// answer the core gives now.
func (r *DecisionRecord) Replay() (admission.Decision, error) {
	s := &r.Spec
	if s.Contract == nil {
		return admission.Decision{}, ErrNoContract
	}
	contract := (&Contract{Spec: s.Contract.Spec}).ToAdmission()
	request := admission.Request{
		ID: s.Request.Name, Action: s.Request.Action, Requester: s.Request.Requester,
		Target: s.Request.Target.toAdmission(),
	}
	if s.Request.ExpiresAt != nil {
		request.ExpiresAt = s.Request.ExpiresAt.Time
	}
	observations := observationsToAdmission(s.Observations)
	open := make([]admission.Grant, 0, len(s.OpenGrants))
	for _, g := range s.OpenGrants {
		open = append(open, g.toAdmission())
	}
	return admission.Decide(contract, request, observations, open, s.Time.Time), nil
}

// Verify plays the record again and reports how the answer differs from
// the recorded one. It returns nil if they are the same.
//
// A record without a contract cannot be played. It is accepted if it is a
// refusal with a reason.
func (r *DecisionRecord) Verify() error {
	recorded := r.Spec.Decision
	got, err := r.Replay()
	if errors.Is(err, ErrNoContract) {
		if recorded.Decision != DecisionRefused || len(recorded.Reasons) == 0 {
			return fmt.Errorf("a record without a contract must be a refusal with a reason, it is %q with %d reasons", recorded.Decision, len(recorded.Reasons))
		}
		return nil
	}
	if err != nil {
		return err
	}

	replayed := DecisionRefused
	if got.Granted {
		replayed = DecisionGranted
	}
	if replayed != recorded.Decision {
		return fmt.Errorf("the record says %s, the replay says %s (reasons: %q)", recorded.Decision, replayed, got.Reasons)
	}
	if !slices.Equal(got.Reasons, recorded.Reasons) {
		return fmt.Errorf("the record has the reasons %q, the replay has %q", recorded.Reasons, got.Reasons)
	}
	if got.Granted {
		if recorded.Deadline == nil || !recorded.Deadline.Time.Equal(got.Grant.Deadline) {
			return fmt.Errorf("the record has the deadline %v, the replay has %s", recorded.Deadline, got.Grant.Deadline.UTC().Format(time.RFC3339Nano))
		}
	} else if recorded.Deadline != nil {
		return errors.New("the record is a refusal with a deadline")
	}
	return nil
}

// NewReleaseRecord builds the record of one release by evidence.
//
// observations is everything that was read in the namespace; the record
// keeps the ones the release could look at. evidence is what the decision
// core returned.
func NewReleaseRecord(ar *ActionRequest, contract *Contract, grant admission.Grant, observations []Observation, now time.Time, evidence []admission.Observation) (*ReleaseRecord, error) {
	spec := ReleaseRecordSpec{
		Time:     metav1.NewMicroTime(now),
		Request:  RecordedRequestRef{Name: ar.Name, UID: ar.UID},
		Contract: RecordedContract{Name: contract.Name, Spec: *contract.Spec.DeepCopy()},
		Grant:    recordGrant(grant),
	}
	conditions := map[string]bool{}
	for _, r := range contract.Spec.ReleaseWhen {
		conditions[r.Condition] = true
	}
	for i := range observations {
		o := &observations[i].Spec
		if o.Subject == grant.Target.Name && conditions[o.Condition] {
			spec.Observations = append(spec.Observations, *o.DeepCopy())
		}
	}
	for _, o := range evidence {
		spec.Evidence = append(spec.Evidence, recordObservation(o))
	}
	meta, err := recordMeta(ar, spec)
	if err != nil {
		return nil, err
	}
	return &ReleaseRecord{ObjectMeta: meta, Spec: spec}, nil
}

func recordObservation(o admission.Observation) ObservationSpec {
	return ObservationSpec{
		Condition: o.Condition, Subject: o.Subject, Status: metav1.ConditionStatus(o.Status),
		Source: o.Source, ObservedAt: metav1.NewMicroTime(o.ObservedAt),
	}
}

// Replay plays the record through the decision core and returns the
// answer the core gives now.
func (r *ReleaseRecord) Replay() admission.ReleaseDecision {
	s := &r.Spec
	contract := (&Contract{Spec: s.Contract.Spec}).ToAdmission()
	return admission.Release(contract, s.Grant.toAdmission(), observationsToAdmission(s.Observations), s.Time.Time)
}

// Verify plays the record again. It returns nil if the grant is released
// again and by the same evidence.
func (r *ReleaseRecord) Verify() error {
	if r.Spec.Grant.Request != r.Spec.Request.Name {
		return fmt.Errorf("the record is about the request %q, its grant belongs to %q", r.Spec.Request.Name, r.Spec.Grant.Request)
	}
	got := r.Replay()
	if !got.Released {
		return fmt.Errorf("the record says released, the replay keeps the grant open (reasons: %q)", got.Reasons)
	}
	evidence := make([]ObservationSpec, 0, len(got.Evidence))
	for _, o := range got.Evidence {
		evidence = append(evidence, recordObservation(o))
	}
	if !slices.EqualFunc(evidence, r.Spec.Evidence, func(a, b ObservationSpec) bool {
		return a.Condition == b.Condition && a.Subject == b.Subject && a.Status == b.Status &&
			a.Source == b.Source && a.ObservedAt.Equal(&b.ObservedAt)
	}) {
		return fmt.Errorf("the record has the evidence %+v, the replay has %+v", r.Spec.Evidence, evidence)
	}
	return nil
}
