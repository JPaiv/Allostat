package v1alpha1

import (
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
// A granted request holds its grant for as long as the object exists.
// Reporting a result will close it; that is not defined yet.
func (r *ActionRequest) OpenGrant() (admission.Grant, bool) {
	s := r.Status
	if s.Decision != DecisionGranted || s.DecidedAt == nil || s.Deadline == nil {
		return admission.Grant{}, false
	}
	return admission.Grant{
		RequestID: r.Name,
		Action:    r.Spec.Action,
		Target:    r.Spec.Target.toAdmission(),
		GrantedAt: s.DecidedAt.Time,
		Deadline:  s.Deadline.Time,
	}, true
}

func (t Target) toAdmission() admission.Target {
	return admission.Target{Name: t.Name, Labels: t.Labels}
}
