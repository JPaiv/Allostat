// Package replay plays a time line of observations, requests and reports
// through the decision core and checks after every step that no unsafe
// state was reached.
//
// The checks are written again here, in the simplest form, and do not call
// the decision core. A mistake in the core therefore shows up as a
// violation and is not hidden by the same mistake in the check.
package replay

import (
	"fmt"
	"sort"
	"time"

	"github.com/JPaiv/Allostat/admission"
)

// Event is one step of a time line. Exactly one of Observe, Ask and Report
// is set. An event with none of them only moves the time.
type Event struct {
	// At is the time of the step, from the start of the time line.
	At time.Duration

	// Observe adds an observation. Its ObservedAt is set from Measured.
	Observe *admission.Observation
	// Measured is when the observation was measured, from the start of the
	// time line. It can differ from At: telemetry can arrive late, and a
	// reporter clock can be wrong.
	Measured time.Duration

	// Ask sends a request.
	Ask *admission.Request

	// Report is the ID of a request whose requester reports the result.
	Report string
	// Failed says that the reported result is a failure.
	Failed bool
}

// Observe returns an event for an observation that is measured and
// reported at the same time.
func Observe(at time.Duration, condition, subject string, status admission.Status) Event {
	return ObserveLate(at, at, condition, subject, status)
}

// ObserveLate returns an event for an observation that is reported at one
// time and measured at another.
func ObserveLate(at, measured time.Duration, condition, subject string, status admission.Status) Event {
	return Event{At: at, Measured: measured, Observe: &admission.Observation{
		Condition: condition, Subject: subject, Status: status, Source: "replay",
	}}
}

// Ask returns an event for a request.
func Ask(at time.Duration, id, action string, target admission.Target) Event {
	return Event{At: at, Ask: &admission.Request{ID: id, Action: action, Requester: "replay", Target: target}}
}

// Succeed returns an event for a success report.
func Succeed(at time.Duration, id string) Event { return Event{At: at, Report: id} }

// Fail returns an event for a failure report.
func Fail(at time.Duration, id string) Event { return Event{At: at, Report: id, Failed: true} }

// Tick returns an event that only moves the time. The coordinator looks
// for release evidence at every step, so a tick gives it a chance.
func Tick(at time.Duration) Event { return Event{At: at} }

// Timeline is a contract and the events that are played against it.
type Timeline struct {
	Contract admission.Contract
	// Start is the time of offset zero.
	Start  time.Time
	Events []Event
}

// Result is what happened during a replay.
type Result struct {
	// Decisions has the answer for every request ID. An answer is final:
	// the same ID asked again gets the same answer.
	Decisions map[string]admission.Decision
	// Released has the time, from the start, at which evidence closed a
	// grant.
	Released map[string]time.Duration
	// Violations lists every unsafe state that was reached. It must be
	// empty.
	Violations []string
	// Log has one line for each thing that happened, in order.
	Log []string
}

type grantState struct {
	grant  admission.Grant
	closed bool
}

// Run plays the time line. Events are played in the order of their time;
// events with the same time keep their order.
func Run(tl Timeline) Result {
	events := append([]Event(nil), tl.Events...)
	sort.SliceStable(events, func(i, j int) bool { return events[i].At < events[j].At })

	res := Result{Decisions: map[string]admission.Decision{}, Released: map[string]time.Duration{}}
	logf := func(at time.Duration, format string, args ...any) {
		res.Log = append(res.Log, fmt.Sprintf("%8s  ", at)+fmt.Sprintf(format, args...))
	}
	violate := func(at time.Duration, format string, args ...any) {
		v := fmt.Sprintf("at %s: ", at) + fmt.Sprintf(format, args...)
		res.Violations = append(res.Violations, v)
		logf(at, "VIOLATION: %s", fmt.Sprintf(format, args...))
	}

	var observations []admission.Observation
	var order []string // request IDs with a grant, in grant order
	grants := map[string]*grantState{}
	open := func() []admission.Grant {
		var out []admission.Grant
		for _, id := range order {
			if g := grants[id]; !g.closed {
				out = append(out, g.grant)
			}
		}
		return out
	}

	for _, ev := range events {
		now := tl.Start.Add(ev.At)

		switch {
		case ev.Observe != nil:
			o := *ev.Observe
			o.ObservedAt = tl.Start.Add(ev.Measured)
			observations = append(observations, o)
			logf(ev.At, "observe %s=%s for %s, measured at %s", o.Condition, o.Status, o.Subject, ev.Measured)

		case ev.Ask != nil:
			req := *ev.Ask
			if d, ok := res.Decisions[req.ID]; ok {
				logf(ev.At, "ask %s again: same answer, granted=%v", req.ID, d.Granted)
				break
			}
			d := admission.Decide(tl.Contract, req, observations, open(), now)
			res.Decisions[req.ID] = d
			if !d.Granted {
				logf(ev.At, "ask %s for %s: refused: %q", req.ID, req.Target.Name, d.Reasons)
				break
			}
			logf(ev.At, "ask %s for %s: granted until %s", req.ID, req.Target.Name, d.Grant.Deadline.Sub(tl.Start))
			for _, why := range unsafeGrant(tl.Contract, req, observations, now) {
				violate(ev.At, "grant %s for %s: %s", req.ID, req.Target.Name, why)
			}
			grants[req.ID] = &grantState{grant: *d.Grant}
			order = append(order, req.ID)

		case ev.Report != "":
			g, ok := grants[ev.Report]
			switch {
			case !ok:
				logf(ev.At, "report for %s ignored: it has no grant", ev.Report)
			case g.closed || g.grant.Failed:
				logf(ev.At, "report for %s ignored: closed or already reported", ev.Report)
			case ev.Failed:
				g.grant.Failed = true
				g.grant.FailedAt = now
				logf(ev.At, "report %s: failed, the grant stays open", ev.Report)
			default:
				g.closed = true
				logf(ev.At, "report %s: succeeded, the grant is closed", ev.Report)
			}
		}

		// Like the coordinator, look for release evidence at every step.
		for _, id := range order {
			g := grants[id]
			if g.closed {
				continue
			}
			if !admission.Release(tl.Contract, g.grant, observations, now).Released {
				continue
			}
			for _, why := range unsafeRelease(tl.Contract, g.grant, observations, now) {
				violate(ev.At, "release of %s for %s: %s", id, g.grant.Target.Name, why)
			}
			g.closed = true
			res.Released[id] = ev.At
			logf(ev.At, "release %s for %s by evidence", id, g.grant.Target.Name)
		}

		for _, why := range unsafeState(tl.Contract, open()) {
			violate(ev.At, "%s", why)
		}
	}
	return res
}

// newest returns the observation of a condition for a subject with the
// latest measuring time.
func newest(observations []admission.Observation, condition, subject string) (admission.Observation, bool) {
	var best admission.Observation
	found := false
	for _, o := range observations {
		if o.Condition != condition || o.Subject != subject {
			continue
		}
		if !found || o.ObservedAt.After(best.ObservedAt) {
			best, found = o, true
		}
	}
	return best, found
}

// unproven says why a condition is not proven for a subject at time now.
// It returns "" if the newest observation is True, not too old and not too
// far in the future.
func unproven(r admission.Requirement, skew time.Duration, subject string, observations []admission.Observation, now time.Time) (admission.Observation, string) {
	o, ok := newest(observations, r.Condition, subject)
	switch {
	case !ok:
		return o, fmt.Sprintf("%s was never observed", r.Condition)
	case o.Status != admission.True:
		return o, fmt.Sprintf("%s is %s", r.Condition, o.Status)
	case now.Sub(o.ObservedAt) > r.MaxAge:
		return o, fmt.Sprintf("%s is older than %s", r.Condition, r.MaxAge)
	case o.ObservedAt.Sub(now) > skew:
		return o, fmt.Sprintf("%s is more than %s in the future", r.Condition, skew)
	}
	return o, ""
}

// unsafeGrant checks a grant that was just given, before it is counted.
func unsafeGrant(c admission.Contract, req admission.Request, observations []admission.Observation, now time.Time) []string {
	var out []string
	if req.Action != c.Action {
		out = append(out, "the contract does not cover the action")
	}
	if c.GrantTTL <= 0 {
		out = append(out, "the contract has no grant TTL")
	}
	if !req.ExpiresAt.IsZero() && !req.ExpiresAt.After(now) {
		out = append(out, "the request had expired")
	}
	for _, r := range c.Requires {
		if _, why := unproven(r, c.ClockSkew, req.Target.Name, observations, now); why != "" {
			out = append(out, why)
		}
	}
	for _, b := range c.Budgets {
		if _, ok := req.Target.Labels[b.Label]; !ok {
			out = append(out, fmt.Sprintf("the target has no label %s", b.Label))
		}
	}
	return out
}

// unsafeRelease checks a release that was just decided.
func unsafeRelease(c admission.Contract, g admission.Grant, observations []admission.Observation, now time.Time) []string {
	var out []string
	var since time.Time
	switch {
	case g.Failed:
		since = g.FailedAt
	case now.After(g.Deadline):
		since = g.Deadline
	default:
		out = append(out, "the grant is not overdue and not failed")
	}
	if len(c.ReleaseWhen) == 0 {
		out = append(out, "the contract has no release conditions")
	}
	for _, r := range c.ReleaseWhen {
		o, why := unproven(r, c.ClockSkew, g.Target.Name, observations, now)
		switch {
		case why != "":
			out = append(out, why)
		case !o.ObservedAt.Add(-c.ClockSkew).After(since):
			out = append(out, fmt.Sprintf("%s was measured before the grant went wrong", r.Condition))
		}
	}
	return out
}

// unsafeState checks the open grants: at most one for each target, and no
// budget above its maximum.
func unsafeState(c admission.Contract, open []admission.Grant) []string {
	var out []string
	perTarget := map[string]int{}
	for _, g := range open {
		perTarget[g.Target.Name]++
	}
	for name, n := range perTarget {
		if n > 1 {
			out = append(out, fmt.Sprintf("target %s has %d open grants", name, n))
		}
	}
	for _, b := range c.Budgets {
		perGroup := map[string]int{}
		for _, g := range open {
			value := g.Target.Labels[b.Label]
			if b.Value != "" && value != b.Value {
				continue
			}
			perGroup[value]++
		}
		for value, n := range perGroup {
			if n > b.Max {
				out = append(out, fmt.Sprintf("budget %s=%s has %d open grants, the maximum is %d", b.Label, value, n, b.Max))
			}
		}
	}
	sort.Strings(out)
	return out
}
