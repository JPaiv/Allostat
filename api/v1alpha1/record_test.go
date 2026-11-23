package v1alpha1

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/JPaiv/Allostat/admission"
)

func recordContract() *Contract {
	return &Contract{
		ObjectMeta: metav1.ObjectMeta{Name: "restart"},
		Spec: ContractSpec{
			Action: "restart",
			Requires: []Requirement{
				{Condition: "PeerCanServe", MaxAge: metav1.Duration{Duration: time.Minute}},
				{Condition: "DiskHeadroomOK", MaxAge: metav1.Duration{Duration: 5 * time.Minute}},
			},
			Budgets:   []Budget{{Label: "shard", Max: 1}},
			GrantTTL:  metav1.Duration{Duration: 10 * time.Minute},
			ClockSkew: &metav1.Duration{Duration: 2 * time.Second},
		},
	}
}

func observed(condition, subject string, status metav1.ConditionStatus, ago time.Duration) Observation {
	return Observation{Spec: ObservationSpec{
		Condition: condition, Subject: subject, Status: status, Source: "reporter",
		ObservedAt: metav1.NewMicroTime(t0.Add(-ago)),
	}}
}

// decideAndRecord does what the coordinator does: it runs the decision
// core and builds the record from the same input.
func decideAndRecord(t *testing.T, ar *ActionRequest, c *Contract, obs []Observation, open []admission.Grant) (admission.Decision, *DecisionRecord) {
	t.Helper()
	var in []admission.Observation
	for i := range obs {
		in = append(in, obs[i].ToAdmission())
	}
	d := admission.Decide(c.ToAdmission(), ar.ToAdmission(), in, open, t0)
	rec, err := NewDecisionRecord(ar, c, obs, open, t0, d)
	if err != nil {
		t.Fatal(err)
	}
	return d, rec
}

func namedRequest() *ActionRequest {
	r := request()
	r.Namespace, r.UID = "fleet", "uid-1"
	return &r
}

func TestRecordReplaysToTheSameAnswer(t *testing.T) {
	healthy := []Observation{
		observed("PeerCanServe", "a", metav1.ConditionTrue, 10*time.Second),
		observed("DiskHeadroomOK", "a", metav1.ConditionTrue, time.Minute),
	}
	expired := metav1.NewTime(t0.Add(-time.Second))
	failedAt := t0.Add(-time.Minute)
	other := admission.Target{Name: "b", Labels: map[string]string{"shard": "1"}}

	cases := []struct {
		name    string
		change  func(*ActionRequest, *Contract)
		obs     []Observation
		open    []admission.Grant
		granted bool
	}{
		{name: "grant", obs: healthy, granted: true},
		{name: "refusal without evidence"},
		{name: "refusal with False, old and missing evidence", obs: []Observation{
			observed("PeerCanServe", "a", metav1.ConditionFalse, 10*time.Second),
			observed("PeerCanServe", "a", metav1.ConditionTrue, 20*time.Second),
		}},
		{name: "grant with an observation inside the clock skew", obs: []Observation{
			observed("PeerCanServe", "a", metav1.ConditionTrue, -2*time.Second),
			observed("DiskHeadroomOK", "a", metav1.ConditionTrue, time.Minute),
		}, granted: true},
		{name: "refusal for a full budget with a failed grant", obs: healthy, open: []admission.Grant{{
			RequestID: "r0", Action: "restart", Target: other,
			GrantedAt: t0.Add(-5 * time.Minute), Deadline: t0.Add(5 * time.Minute), Failed: true, FailedAt: failedAt,
		}}},
		{name: "refusal for a full budget with an overdue grant", obs: healthy, open: []admission.Grant{{
			RequestID: "r0", Action: "restart", Target: other,
			GrantedAt: t0.Add(-time.Hour), Deadline: t0.Add(-50 * time.Minute),
		}}},
		{name: "refusal for an expired request", obs: healthy, change: func(r *ActionRequest, _ *Contract) { r.Spec.ExpiresAt = &expired }},
		{name: "refusal for a target without the label", obs: healthy, change: func(r *ActionRequest, _ *Contract) { r.Spec.Target.Labels = nil }},
		{name: "grant from a contract without rules", change: func(_ *ActionRequest, c *Contract) { c.Spec.Requires, c.Spec.Budgets = nil, nil }, granted: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ar, c := namedRequest(), recordContract()
			if tc.change != nil {
				tc.change(ar, c)
			}
			d, rec := decideAndRecord(t, ar, c, tc.obs, tc.open)
			if d.Granted != tc.granted {
				t.Fatalf("granted = %v, want %v; reasons %q", d.Granted, tc.granted, d.Reasons)
			}
			if err := rec.Verify(); err != nil {
				t.Fatalf("the record does not replay: %v", err)
			}
			// The record must also hold after it was stored and read back.
			stored := &DecisionRecord{}
			roundTrip(t, rec, stored)
			if err := stored.Verify(); err != nil {
				t.Fatalf("the stored record does not replay: %v", err)
			}
		})
	}
}

func TestRecordKeepsOnlyWhatTheDecisionCouldLookAt(t *testing.T) {
	obs := []Observation{
		observed("PeerCanServe", "a", metav1.ConditionTrue, 10*time.Second),
		observed("PeerCanServe", "b", metav1.ConditionFalse, 10*time.Second), // another target
		observed("TargetServing", "a", metav1.ConditionTrue, 10*time.Second), // not required
		observed("DiskHeadroomOK", "a", metav1.ConditionTrue, time.Minute),
		observed("PeerCanServe", "a", metav1.ConditionTrue, 5*time.Minute), // older, still about the target
	}
	_, rec := decideAndRecord(t, namedRequest(), recordContract(), obs, nil)
	if len(rec.Spec.Observations) != 3 {
		t.Fatalf("the record has %d observations, want 3: %+v", len(rec.Spec.Observations), rec.Spec.Observations)
	}
	for _, o := range rec.Spec.Observations {
		if o.Subject != "a" || o.Condition == "TargetServing" {
			t.Fatalf("the record has an observation the decision could not look at: %+v", o)
		}
	}
}

func TestRecordNameComesFromTheContent(t *testing.T) {
	healthy := []Observation{
		observed("PeerCanServe", "a", metav1.ConditionTrue, 10*time.Second),
		observed("DiskHeadroomOK", "a", metav1.ConditionTrue, time.Minute),
	}
	_, first := decideAndRecord(t, namedRequest(), recordContract(), healthy, nil)
	_, same := decideAndRecord(t, namedRequest(), recordContract(), healthy, nil)
	_, other := decideAndRecord(t, namedRequest(), recordContract(), healthy[:1], nil)

	if first.Name != same.Name {
		t.Fatalf("the same decision has two names: %q and %q", first.Name, same.Name)
	}
	if first.Name == other.Name {
		t.Fatalf("two decisions have the same name %q", first.Name)
	}
	if !strings.HasPrefix(first.Name, "r1-") || len(first.Name) != len("r1-")+12 {
		t.Fatalf("unexpected name %q", first.Name)
	}
	if first.Namespace != "fleet" || len(first.OwnerReferences) != 1 || first.OwnerReferences[0].UID != "uid-1" || first.OwnerReferences[0].Kind != "ActionRequest" {
		t.Fatalf("the record is not in the namespace of its request or not owned by it: %+v", first.ObjectMeta)
	}

	long := namedRequest()
	long.Name = strings.Repeat("x", 253)
	_, rec := decideAndRecord(t, long, recordContract(), healthy, nil)
	if len(rec.Name) > 253 {
		t.Fatalf("the name has %d characters, the limit is 253", len(rec.Name))
	}
}

// A record that was changed after the decision must not pass.
func TestVerifyFindsAChangedRecord(t *testing.T) {
	healthy := []Observation{
		observed("PeerCanServe", "a", metav1.ConditionTrue, 10*time.Second),
		observed("DiskHeadroomOK", "a", metav1.ConditionTrue, time.Minute),
	}
	later := metav1.NewMicroTime(t0.Add(time.Hour))
	cases := []struct {
		name   string
		obs    []Observation
		change func(*DecisionRecordSpec)
		want   string
	}{
		{"an observation was changed", healthy, func(s *DecisionRecordSpec) { s.Observations[0].Status = metav1.ConditionFalse }, "the record says Granted, the replay says Refused"},
		{"an observation was removed", healthy, func(s *DecisionRecordSpec) { s.Observations = s.Observations[:1] }, "the record says Granted, the replay says Refused"},
		{"the time was moved", healthy, func(s *DecisionRecordSpec) { s.Time = later }, "the record says Granted, the replay says Refused"},
		{"the deadline was moved", healthy, func(s *DecisionRecordSpec) { s.Decision.Deadline = &later }, "the record has the deadline"},
		{"the contract was made stricter", healthy, func(s *DecisionRecordSpec) { s.Contract.Spec.Budgets[0].Max = 0 }, "the record says Granted, the replay says Refused"},
		{"a refusal was turned into a grant", nil, func(s *DecisionRecordSpec) {
			s.Decision = RecordedDecision{Decision: DecisionGranted, Deadline: &later}
		}, "the record says Granted, the replay says Refused"},
		{"a reason was removed", nil, func(s *DecisionRecordSpec) { s.Decision.Reasons = s.Decision.Reasons[:1] }, "the record has the reasons"},
		{"a refusal got a deadline", nil, func(s *DecisionRecordSpec) { s.Decision.Deadline = &later }, "a refusal with a deadline"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, rec := decideAndRecord(t, namedRequest(), recordContract(), tc.obs, nil)
			tc.change(&rec.Spec)
			err := rec.Verify()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want an error with %q, got %v", tc.want, err)
			}
		})
	}
}

// The coordinator refuses without a contract, or with two. Such a record
// has nothing to play again.
func TestRecordWithoutAContract(t *testing.T) {
	ar := namedRequest()
	d := admission.Decision{Reasons: []string{`no contract covers action "restart" in namespace "fleet"`}}
	rec, err := NewDecisionRecord(ar, nil, nil, nil, t0, d)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rec.Replay(); !errors.Is(err, ErrNoContract) {
		t.Fatalf("want ErrNoContract, got %v", err)
	}
	if err := rec.Verify(); err != nil {
		t.Fatalf("a refusal without a contract must pass: %v", err)
	}
	rec.Spec.Decision = RecordedDecision{Decision: DecisionGranted}
	if err := rec.Verify(); err == nil {
		t.Fatal("a grant without a contract must not pass")
	}
}

// roundTrip writes a record as JSON and reads it back, as the API server
// does.
func roundTrip(t *testing.T, in, out *DecisionRecord) {
	t.Helper()
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, out); err != nil {
		t.Fatal(err)
	}
}
