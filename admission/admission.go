// Package admission decides if one bounded action may start now.
//
// It is the core of the coordinator. It has no I/O and no clock of its own:
// the caller passes the contract, the request, the observations, the grants
// that are still open, and the current time. The same input always gives the
// same decision, so every decision can be replayed from its record.
//
// The rule for all doubt is the same: refuse. Missing, old or unclear
// evidence never leads to a grant.
package admission

import (
	"fmt"
	"time"
)

// Status is the value of an observation.
type Status string

const (
	True    Status = "True"
	False   Status = "False"
	Unknown Status = "Unknown"
)

// Observation is one fact that a controller reports about a subject.
// The controller that reports it stays the owner of that fact.
type Observation struct {
	// Condition is the name of the fact, for example "PeerCanServe".
	Condition string
	// Subject is the thing the fact is about, for example a node name.
	Subject string
	Status  Status
	// Source is the controller that reported it.
	Source string
	// ObservedAt is when the source measured it, not when it arrived.
	ObservedAt time.Time
}

// Requirement names a condition that must be True and fresh for the target.
type Requirement struct {
	Condition string
	// MaxAge is the oldest observation that still counts.
	MaxAge time.Duration
}

// Budget limits how many targets of one group may have an open grant.
//
// Targets are grouped by the value of Label. With Label "shard" and Max 1,
// two nodes of the same shard never hold a grant at the same time.
type Budget struct {
	Label string
	// Value, if set, limits the budget to targets with this label value.
	// Targets with another value are outside this budget.
	Value string
	Max   int
}

// Contract is the agreement for one kind of action.
type Contract struct {
	// Action is the kind of action, for example "restart".
	Action   string
	Requires []Requirement
	Budgets  []Budget
	// GrantTTL is how long the requester has to finish and report.
	GrantTTL time.Duration
}

// Target is the thing the action changes.
type Target struct {
	Name   string
	Labels map[string]string
}

// Request asks for permission to do one action on one target.
type Request struct {
	// ID is chosen by the requester. A retry uses the same ID.
	ID        string
	Action    string
	Requester string
	Target    Target
}

// Grant is a permission that was given and is not yet reported as finished.
//
// A grant stays open until the requester reports success. Passing the
// deadline does not close it, and a reported failure does not close it: in
// both cases the target may still be out, so the grant keeps its place in
// every budget.
type Grant struct {
	RequestID string
	Action    string
	Target    Target
	GrantedAt time.Time
	Deadline  time.Time
	// Failed is true when the requester reported that the action failed.
	Failed bool
}

// Overdue reports if the requester did not report in time.
func (g Grant) Overdue(now time.Time) bool {
	return now.After(g.Deadline)
}

// Decision is the answer to a request, with every reason for a refusal.
type Decision struct {
	Granted bool
	// Reasons is empty for a grant and lists all failed checks for a refusal.
	Reasons []string
	// Grant is set only when Granted is true.
	Grant *Grant
}

// Decide answers one request.
//
// open must hold every grant that is not reported as finished, for all
// targets. Decide does not store anything; the caller records the grant.
func Decide(c Contract, req Request, observations []Observation, open []Grant, now time.Time) Decision {
	// A retry of a request that already holds a grant gets the same grant.
	for i := range open {
		if open[i].RequestID == req.ID && open[i].Target.Name == req.Target.Name && open[i].Action == req.Action {
			g := open[i]
			return Decision{Granted: true, Grant: &g}
		}
	}

	var reasons []string
	refuse := func(format string, args ...any) {
		reasons = append(reasons, fmt.Sprintf(format, args...))
	}

	if req.ID == "" || req.Target.Name == "" {
		refuse("request has no ID or no target name")
	}
	if req.Action != c.Action {
		refuse("contract covers action %q, not %q", c.Action, req.Action)
	}
	if c.GrantTTL <= 0 {
		refuse("contract has no grant TTL")
	}

	for _, g := range open {
		if g.Target.Name == req.Target.Name {
			refuse("target %q already has an open grant (request %q)%s", g.Target.Name, g.RequestID, stateNote(g, now))
		}
	}

	for _, r := range c.Requires {
		checkRequirement(r, req.Target.Name, observations, now, refuse)
	}
	for _, b := range c.Budgets {
		checkBudget(b, req.Target, open, now, refuse)
	}

	if len(reasons) > 0 {
		return Decision{Reasons: reasons}
	}
	return Decision{Granted: true, Grant: &Grant{
		RequestID: req.ID,
		Action:    req.Action,
		Target:    req.Target,
		GrantedAt: now,
		Deadline:  now.Add(c.GrantTTL),
	}}
}

func checkRequirement(r Requirement, subject string, observations []Observation, now time.Time, refuse func(string, ...any)) {
	var newest *Observation
	for i := range observations {
		o := &observations[i]
		if o.Condition != r.Condition || o.Subject != subject {
			continue
		}
		if newest == nil || o.ObservedAt.After(newest.ObservedAt) {
			newest = o
		}
	}
	if newest == nil {
		refuse("no observation of %q for %q", r.Condition, subject)
		return
	}
	age := now.Sub(newest.ObservedAt)
	switch {
	case age < 0:
		refuse("observation of %q for %q is from the future (%s ahead)", r.Condition, subject, -age)
	case age > r.MaxAge:
		refuse("observation of %q for %q is %s old, limit is %s", r.Condition, subject, age, r.MaxAge)
	case newest.Status != True:
		refuse("%q for %q is %s (from %q)", r.Condition, subject, newest.Status, newest.Source)
	}
}

func checkBudget(b Budget, target Target, open []Grant, now time.Time, refuse func(string, ...any)) {
	value, ok := target.Labels[b.Label]
	if !ok {
		// Without the label we cannot tell which group the target is in.
		refuse("target %q has no label %q", target.Name, b.Label)
		return
	}
	if b.Value != "" && value != b.Value {
		return
	}
	used, overdue, failed := 0, 0, 0
	for _, g := range open {
		if g.Target.Labels[b.Label] != value {
			continue
		}
		used++
		switch {
		case g.Failed:
			failed++
		case g.Overdue(now):
			overdue++
		}
	}
	if used >= b.Max {
		refuse("budget %s=%s is full: %d of %d in use, %d overdue, %d failed", b.Label, value, used, b.Max, overdue, failed)
	}
}

// stateNote says why a grant that should be closed is still open.
func stateNote(g Grant, now time.Time) string {
	switch {
	case g.Failed:
		return ", reported as failed"
	case g.Overdue(now):
		return ", overdue"
	}
	return ""
}
