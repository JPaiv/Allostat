package v1alpha1

import (
	"reflect"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/JPaiv/Allostat/admission"
)

var t0 = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func TestContractToAdmission(t *testing.T) {
	c := Contract{Spec: ContractSpec{
		Action:    "restart",
		Requires:  []Requirement{{Condition: "PeerCanServe", MaxAge: metav1.Duration{Duration: time.Minute}}},
		Budgets:   []Budget{{Label: "shard", Max: 1}, {Label: "gated", Value: "true", Max: 2}},
		GrantTTL:  metav1.Duration{Duration: 10 * time.Minute},
		ClockSkew: &metav1.Duration{Duration: 2 * time.Second},
	}}
	want := admission.Contract{
		Action:    "restart",
		Requires:  []admission.Requirement{{Condition: "PeerCanServe", MaxAge: time.Minute}},
		Budgets:   []admission.Budget{{Label: "shard", Max: 1}, {Label: "gated", Value: "true", Max: 2}},
		GrantTTL:  10 * time.Minute,
		ClockSkew: 2 * time.Second,
	}
	if got := c.ToAdmission(); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestObservationToAdmission(t *testing.T) {
	o := Observation{Spec: ObservationSpec{
		Condition: "PeerCanServe", Subject: "a", Status: metav1.ConditionFalse,
		Source: "replica-health", ObservedAt: metav1.NewMicroTime(t0),
	}}
	want := admission.Observation{
		Condition: "PeerCanServe", Subject: "a", Status: admission.False,
		Source: "replica-health", ObservedAt: t0,
	}
	if got := o.ToAdmission(); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func request() ActionRequest {
	return ActionRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "r1"},
		Spec: ActionRequestSpec{
			Action: "restart", Requester: "upgrade",
			Target: Target{Name: "a", Labels: map[string]string{"shard": "1"}},
		},
	}
}

func TestRequestToAdmission(t *testing.T) {
	r := request()
	want := admission.Request{
		ID: "r1", Action: "restart", Requester: "upgrade",
		Target: admission.Target{Name: "a", Labels: map[string]string{"shard": "1"}},
	}
	if got := r.ToAdmission(); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestRequestExpiryToAdmission(t *testing.T) {
	r := request()
	if got := r.ToAdmission().ExpiresAt; !got.IsZero() {
		t.Fatalf("a request without expiresAt must not expire, got %s", got)
	}
	at := metav1.NewTime(t0.Add(time.Minute))
	r.Spec.ExpiresAt = &at
	if got := r.ToAdmission().ExpiresAt; !got.Equal(at.Time) {
		t.Fatalf("got %s, want %s", got, at.Time)
	}
}

func TestOpenGrant(t *testing.T) {
	decided := metav1.NewMicroTime(t0)
	deadline := metav1.NewMicroTime(t0.Add(10 * time.Minute))

	granted := ActionRequestStatus{Decision: DecisionGranted, DecidedAt: &decided, Deadline: &deadline}

	released := granted
	released.Released = &Release{At: deadline, Evidence: []string{"TargetServing=True"}}

	cases := []struct {
		name   string
		status ActionRequestStatus
		result *Result
		open   bool
		failed bool
	}{
		{name: "not decided"},
		{name: "refused", status: ActionRequestStatus{Decision: DecisionRefused, DecidedAt: &decided}},
		{name: "granted", status: granted, open: true},
		{name: "granted without a deadline is not a grant", status: ActionRequestStatus{Decision: DecisionGranted, DecidedAt: &decided}},
		{name: "success closes the grant", status: granted, result: &Result{Outcome: OutcomeSucceeded}},
		{name: "failure keeps the grant open", status: granted, result: &Result{Outcome: OutcomeFailed}, open: true, failed: true},
		{name: "an unknown outcome keeps the grant open", status: granted, result: &Result{Outcome: "Other"}, open: true, failed: true},
		{name: "a release closes an overdue grant", status: released},
		{name: "a release closes a failed grant", status: released, result: &Result{Outcome: OutcomeFailed}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := request()
			r.Status = tc.status
			r.Spec.Result = tc.result
			g, open := r.OpenGrant()
			if open != tc.open || g.Failed != tc.failed {
				t.Fatalf("open = %v, failed = %v; want %v, %v", open, g.Failed, tc.open, tc.failed)
			}
			if open && (g.RequestID != "r1" || g.Target.Name != "a" || !g.GrantedAt.Equal(t0) || !g.Deadline.Equal(t0.Add(10*time.Minute))) {
				t.Fatalf("wrong grant: %+v", g)
			}
		})
	}
}

// A grant built by the decision core must come back the same after it is
// stored in a request status.
func TestGrantRoundTrip(t *testing.T) {
	r := request()
	c := admission.Contract{Action: "restart", GrantTTL: 10 * time.Minute}
	d := admission.Decide(c, r.ToAdmission(), nil, nil, t0)
	if !d.Granted {
		t.Fatalf("want grant, got %q", d.Reasons)
	}
	decided, deadline := metav1.NewMicroTime(d.Grant.GrantedAt), metav1.NewMicroTime(d.Grant.Deadline)
	r.Status = ActionRequestStatus{Decision: DecisionGranted, DecidedAt: &decided, Deadline: &deadline}

	g, open := r.OpenGrant()
	if !open || !reflect.DeepEqual(g, *d.Grant) {
		t.Fatalf("got %+v, want %+v", g, *d.Grant)
	}
	again := admission.Decide(c, r.ToAdmission(), nil, []admission.Grant{g}, t0.Add(time.Hour))
	if !again.Granted || !again.Grant.Deadline.Equal(d.Grant.Deadline) {
		t.Fatalf("a retry must get the same grant, got %+v", again)
	}
}

func TestFailureTimeIsInTheGrant(t *testing.T) {
	decided := metav1.NewMicroTime(t0)
	deadline := metav1.NewMicroTime(t0.Add(10 * time.Minute))
	seen := metav1.NewMicroTime(t0.Add(time.Minute))

	r := request()
	r.Status = ActionRequestStatus{Decision: DecisionGranted, DecidedAt: &decided, Deadline: &deadline, FailureSeenAt: &seen}
	if g, _ := r.OpenGrant(); !g.FailedAt.IsZero() {
		t.Fatalf("a grant without a failure report must have no failure time, got %s", g.FailedAt)
	}
	r.Spec.Result = &Result{Outcome: OutcomeFailed}
	if g, _ := r.OpenGrant(); !g.FailedAt.Equal(seen.Time) {
		t.Fatalf("failure time = %s, want %s", g.FailedAt, seen.Time)
	}
	r.Status.FailureSeenAt = nil
	if g, _ := r.OpenGrant(); !g.Failed || !g.FailedAt.IsZero() {
		t.Fatalf("want a failed grant without a time, got %+v", g)
	}
}
