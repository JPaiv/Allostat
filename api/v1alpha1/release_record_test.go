package v1alpha1

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/JPaiv/Allostat/admission"
)

func releaseContract() *Contract {
	c := recordContract()
	c.Spec.ReleaseWhen = []Requirement{
		{Condition: "TargetServing", MaxAge: metav1.Duration{Duration: time.Minute}},
		{Condition: "NoMutationRunning", MaxAge: metav1.Duration{Duration: time.Minute}},
	}
	return c
}

// overdueGrant is a grant of namedRequest whose deadline was a minute
// before t0.
func overdueGrant() admission.Grant {
	return admission.Grant{
		RequestID: "r1", Action: "restart",
		Target:    admission.Target{Name: "a", Labels: map[string]string{"shard": "1"}},
		GrantedAt: t0.Add(-11 * time.Minute), Deadline: t0.Add(-time.Minute),
	}
}

// releaseAndRecord does what the coordinator does: it asks the decision
// core and builds the record from the same input.
func releaseAndRecord(t *testing.T, c *Contract, g admission.Grant, obs []Observation) *ReleaseRecord {
	t.Helper()
	var in []admission.Observation
	for i := range obs {
		in = append(in, obs[i].ToAdmission())
	}
	d := admission.Release(c.ToAdmission(), g, in, t0)
	if !d.Released {
		t.Fatalf("want a release, got %q", d.Reasons)
	}
	rec, err := NewReleaseRecord(namedRequest(), c, g, obs, t0, d.Evidence)
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

var releaseEvidence = []Observation{
	observed("TargetServing", "a", metav1.ConditionTrue, 10*time.Second),
	observed("NoMutationRunning", "a", metav1.ConditionTrue, 20*time.Second),
}

func TestReleaseRecordReplaysToTheSameAnswer(t *testing.T) {
	failed := overdueGrant()
	failed.Deadline = t0.Add(5 * time.Minute)
	failed.Failed, failed.FailedAt = true, t0.Add(-30*time.Second)

	for name, g := range map[string]admission.Grant{"overdue": overdueGrant(), "failed": failed} {
		t.Run(name, func(t *testing.T) {
			rec := releaseAndRecord(t, releaseContract(), g, releaseEvidence)
			if err := rec.Verify(); err != nil {
				t.Fatal(err)
			}
			// The same after the API server stored it.
			data, err := json.Marshal(rec)
			if err != nil {
				t.Fatal(err)
			}
			stored := &ReleaseRecord{}
			if err := json.Unmarshal(data, stored); err != nil {
				t.Fatal(err)
			}
			if err := stored.Verify(); err != nil {
				t.Fatalf("after a round trip: %v", err)
			}
			if len(stored.Spec.Evidence) != 2 || stored.Spec.Evidence[0].Condition != "TargetServing" {
				t.Fatalf("wrong evidence: %+v", stored.Spec.Evidence)
			}
		})
	}
}

func TestReleaseRecordKeepsOnlyWhatTheReleaseCouldLookAt(t *testing.T) {
	obs := append([]Observation{
		observed("TargetServing", "b", metav1.ConditionTrue, time.Second),  // another target
		observed("PeerCanServe", "a", metav1.ConditionTrue, time.Second),   // not a release condition
		observed("TargetServing", "a", metav1.ConditionFalse, time.Minute), // older, kept
	}, releaseEvidence...)
	rec := releaseAndRecord(t, releaseContract(), overdueGrant(), obs)
	if len(rec.Spec.Observations) != 3 {
		t.Fatalf("want 3 observations, got %+v", rec.Spec.Observations)
	}
	if err := rec.Verify(); err != nil {
		t.Fatal(err)
	}
	if got := rec.OwnerReferences; len(got) != 1 || got[0].Name != "r1" || got[0].UID != "uid-1" {
		t.Fatalf("wrong owner: %+v", got)
	}
	again := releaseAndRecord(t, releaseContract(), overdueGrant(), obs)
	if again.Name != rec.Name || !strings.HasPrefix(rec.Name, "r1-") {
		t.Fatalf("the same release must give the same name, got %q and %q", rec.Name, again.Name)
	}
}

func TestVerifyFindsAChangedReleaseRecord(t *testing.T) {
	early := metav1.NewMicroTime(t0.Add(-59 * time.Second))
	cases := []struct {
		name   string
		change func(*ReleaseRecordSpec)
		want   string
	}{
		{"an observation was changed", func(s *ReleaseRecordSpec) { s.Observations[0].Status = metav1.ConditionFalse }, "the replay keeps the grant open"},
		{"an observation was removed", func(s *ReleaseRecordSpec) { s.Observations = s.Observations[:1] }, "the replay keeps the grant open"},
		// Fresh, but with the clock skew it can be from before the deadline.
		{"an observation was moved to the deadline", func(s *ReleaseRecordSpec) { s.Observations[0].ObservedAt = early }, "is not newer than"},
		{"the deadline was moved", func(s *ReleaseRecordSpec) { s.Grant.Deadline = metav1.NewMicroTime(t0.Add(time.Hour)) }, "the replay keeps the grant open"},
		{"the release conditions were removed", func(s *ReleaseRecordSpec) { s.Contract.Spec.ReleaseWhen = nil }, "the replay keeps the grant open"},
		{"the evidence was changed", func(s *ReleaseRecordSpec) { s.Evidence[0].Source = "someone-else" }, "the record has the evidence"},
		{"evidence was removed", func(s *ReleaseRecordSpec) { s.Evidence = s.Evidence[:1] }, "the record has the evidence"},
		{"the grant is of another request", func(s *ReleaseRecordSpec) { s.Grant.Request = "r2" }, `its grant belongs to "r2"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := releaseAndRecord(t, releaseContract(), overdueGrant(), releaseEvidence)
			tc.change(&rec.Spec)
			err := rec.Verify()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want an error with %q, got %v", tc.want, err)
			}
		})
	}
}
