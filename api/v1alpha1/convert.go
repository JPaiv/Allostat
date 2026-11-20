package v1alpha1

import (
	"time"

	"github.com/JPaiv/Allostat/admission"
)

// ToAdmission returns the contract in the form the decision core uses.
func (c *Contract) ToAdmission() admission.Contract {
	out := admission.Contract{
		Action:   c.Spec.Action,
		GrantTTL: c.Spec.GrantTTL.Duration,
	}
	for _, r := range c.Spec.Requires {
		out.Requires = append(out.Requires, admission.Requirement{
			Condition: r.Condition,
			MaxAge:    r.MaxAge.Duration,
		})
	}
	for _, b := range c.Spec.Budgets {
		out.Budgets = append(out.Budgets, admission.Budget{
			Label: b.Label,
			Value: b.Value,
			Max:   int(b.Max),
		})
	}
	return out
}

// ToAdmission returns the observation in the form the decision core uses.
func (o *Observation) ToAdmission() admission.Observation {
	return admission.Observation{
		Condition:  o.Spec.Condition,
		Subject:    o.Spec.Subject,
		Status:     admission.Status(o.Spec.Status),
		Source:     o.Spec.Source,
		ObservedAt: o.Spec.ObservedAt.Time,
	}
}

// ToAdmission returns the request in the form the decision core uses.
// The request ID is the object name: a retry reads the same object.
func (r *ActionRequest) ToAdmission() admission.Request {
	return admission.Request{
		ID:        r.Name,
		Action:    r.Spec.Action,
		Requester: r.Spec.Requester,
		Target:    r.Spec.Target.toAdmission(),
	}
}

// OpenGrant returns the grant this request holds, if it holds one.
//
// A granted request holds its grant until the requester reports success or
// the request is deleted. A reported failure does not close the grant: the
// target may still be out.
func (r *ActionRequest) OpenGrant() (admission.Grant, bool) {
	s := r.Status
	if s.Decision != DecisionGranted || s.DecidedAt == nil || s.Deadline == nil {
		return admission.Grant{}, false
	}
	if r.Succeeded() {
		return admission.Grant{}, false
	}
	return r.GrantAt(s.DecidedAt.Time, s.Deadline.Time), true
}

// Succeeded reports if the requester reported success.
func (r *ActionRequest) Succeeded() bool {
	return r.Spec.Result != nil && r.Spec.Result.Outcome == OutcomeSucceeded
}

// GrantAt returns the grant of this request with the given times. The
// caller must know that the request holds a grant.
func (r *ActionRequest) GrantAt(grantedAt, deadline time.Time) admission.Grant {
	return admission.Grant{
		// Any result that is not a success counts as a failure.
		Failed:    r.Spec.Result != nil && !r.Succeeded(),
		RequestID: r.Name,
		Action:    r.Spec.Action,
		Target:    r.Spec.Target.toAdmission(),
		GrantedAt: grantedAt,
		Deadline:  deadline,
	}
}

func (t Target) toAdmission() admission.Target {
	return admission.Target{Name: t.Name, Labels: t.Labels}
}
