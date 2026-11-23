package replay

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/JPaiv/Allostat/admission"
)

var start = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

const (
	sec = time.Second
	min = time.Minute
)

// contract is a restart contract for a fleet of shards with two replicas.
// One replica of a shard may be out at a time.
func contract() admission.Contract {
	return admission.Contract{
		Action: "restart",
		Requires: []admission.Requirement{
			{Condition: "PeerCanServe", MaxAge: min},
			{Condition: "DiskHeadroomOK", MaxAge: 5 * min},
		},
		Budgets:     []admission.Budget{{Label: "shard", Max: 1}},
		GrantTTL:    10 * min,
		ReleaseWhen: []admission.Requirement{{Condition: "TargetServing", MaxAge: min}},
	}
}

func node(name, shard string) admission.Target {
	return admission.Target{Name: name, Labels: map[string]string{"shard": shard}}
}

func ask(at time.Duration, id string, t admission.Target) Event {
	return Ask(at, id, "restart", t)
}

// healthy reports both required conditions for a node.
func healthy(at time.Duration, name string) []Event {
	return []Event{
		Observe(at, "PeerCanServe", name, admission.True),
		Observe(at, "DiskHeadroomOK", name, admission.True),
	}
}

func join(parts ...[]Event) []Event {
	var out []Event
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func play(t *testing.T, c admission.Contract, events []Event) Result {
	t.Helper()
	res := Run(Timeline{Contract: c, Start: start, Events: events})
	if len(res.Violations) > 0 {
		t.Fatalf("unsafe state reached:\n%s\n\nlog:\n%s", strings.Join(res.Violations, "\n"), strings.Join(res.Log, "\n"))
	}
	return res
}

func wantGranted(t *testing.T, res Result, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if d, ok := res.Decisions[id]; !ok || !d.Granted {
			t.Fatalf("%s: want a grant, got %+v\n\nlog:\n%s", id, d, strings.Join(res.Log, "\n"))
		}
	}
}

func wantRefused(t *testing.T, res Result, id, reason string) {
	t.Helper()
	d, ok := res.Decisions[id]
	if !ok || d.Granted {
		t.Fatalf("%s: want a refusal, got %+v\n\nlog:\n%s", id, d, strings.Join(res.Log, "\n"))
	}
	if !strings.Contains(strings.Join(d.Reasons, "\n"), reason) {
		t.Fatalf("%s: reasons %q do not contain %q", id, d.Reasons, reason)
	}
}

// Two shards, two replicas each. The upgrade goes node by node and every
// step succeeds.
func TestSerialUpgrade(t *testing.T) {
	a, b, c, d := node("a", "1"), node("b", "1"), node("c", "2"), node("d", "2")
	res := play(t, contract(), join(
		healthy(0, "a"), []Event{ask(1*sec, "r-a", a), Succeed(3*min, "r-a")},
		healthy(4*min, "b"), []Event{ask(4*min+sec, "r-b", b), Succeed(7*min, "r-b")},
		healthy(8*min, "c"), []Event{ask(8*min+sec, "r-c", c), Succeed(11*min, "r-c")},
		healthy(12*min, "d"), []Event{ask(12*min+sec, "r-d", d), Succeed(15*min, "r-d")},
	))
	wantGranted(t, res, "r-a", "r-b", "r-c", "r-d")
}

// Different shards do not wait for each other, replicas of one shard do.
func TestParallelOnlyAcrossShards(t *testing.T) {
	a, b, c := node("a", "1"), node("b", "1"), node("c", "2")
	res := play(t, contract(), join(
		healthy(0, "a"), healthy(0, "b"), healthy(0, "c"),
		[]Event{ask(1*sec, "r-a", a), ask(2*sec, "r-c", c), ask(3*sec, "r-b", b)},
	))
	wantGranted(t, res, "r-a", "r-c")
	wantRefused(t, res, "r-b", "budget shard=1 is full")
}

// While a is out, its peer b stops serving. The health controller reports
// that for c and d in the other shard too late for one request and in time
// for the other.
func TestPeerFailsDuringMaintenance(t *testing.T) {
	a, b, c := node("a", "1"), node("b", "1"), node("c", "2")
	res := play(t, contract(), join(
		healthy(0, "a"), healthy(0, "b"), healthy(0, "c"),
		[]Event{
			ask(1*sec, "r-a", a),
			// The peer of c fails. The controller that owns the fact says so.
			Observe(20*sec, "PeerCanServe", "c", admission.False),
			ask(21*sec, "r-c1", c),
			// The reporter then loses sight of the peer.
			Observe(40*sec, "PeerCanServe", "c", admission.Unknown),
			ask(41*sec, "r-c2", c),
			// b in the shard of a is asked while a is still out.
			ask(42*sec, "r-b", b),
			// The peer is back and a finished.
			Succeed(3*min, "r-a"),
			Observe(3*min, "PeerCanServe", "c", admission.True),
			ask(3*min+sec, "r-c3", c),
		},
	))
	wantGranted(t, res, "r-a", "r-c3")
	wantRefused(t, res, "r-c1", `"PeerCanServe" for "c" is False`)
	wantRefused(t, res, "r-c2", `"PeerCanServe" for "c" is Unknown`)
	wantRefused(t, res, "r-b", "budget shard=1 is full")
}

// The disk controller reports pressure while another node is out. The
// node under pressure waits until the pressure is gone and reported again.
func TestDiskPressureDuringMaintenance(t *testing.T) {
	a, c := node("a", "1"), node("c", "2")
	res := play(t, contract(), join(
		healthy(0, "a"), healthy(0, "c"),
		[]Event{
			ask(1*sec, "r-a", a),
			Observe(30*sec, "DiskHeadroomOK", "c", admission.False),
			Observe(30*sec, "PeerCanServe", "c", admission.True),
			ask(31*sec, "r-c1", c),
			// An older True that arrives late must not win over the False.
			ObserveLate(50*sec, 10*sec, "DiskHeadroomOK", "c", admission.True),
			Observe(50*sec, "PeerCanServe", "c", admission.True),
			ask(51*sec, "r-c2", c),
			Observe(2*min, "DiskHeadroomOK", "c", admission.True),
			Observe(2*min, "PeerCanServe", "c", admission.True),
			ask(2*min+sec, "r-c3", c),
		},
	))
	wantGranted(t, res, "r-a", "r-c3")
	wantRefused(t, res, "r-c1", `"DiskHeadroomOK" for "c" is False`)
	wantRefused(t, res, "r-c2", `"DiskHeadroomOK" for "c" is False`)
}

// A reporter stops. Its last word gets old and then counts for nothing,
// also when it is delivered again later.
func TestOldTelemetry(t *testing.T) {
	a := node("a", "1")
	res := play(t, contract(), join(
		healthy(0, "a"),
		[]Event{
			// PeerCanServe may be one minute old, DiskHeadroomOK five.
			ask(1*min+sec, "r1", a),
			// The old measurement arrives again. Its age is what counts.
			ObserveLate(2*min, 0, "PeerCanServe", "a", admission.True),
			ask(2*min+sec, "r2", a),
			// Only the peer reporter comes back.
			Observe(6*min, "PeerCanServe", "a", admission.True),
			ask(6*min+sec, "r3", a),
			// A reporter with a clock that is ahead is not trusted.
			Observe(7*min, "DiskHeadroomOK", "a", admission.True),
			ObserveLate(7*min, 8*min, "PeerCanServe", "a", admission.True),
			ask(7*min+sec, "r4", a),
		},
		healthy(9*min, "a"),
		[]Event{ask(9*min+sec, "r5", a)},
	))
	wantRefused(t, res, "r1", `observation of "PeerCanServe" for "a" is 1m1s old`)
	wantRefused(t, res, "r2", `observation of "PeerCanServe" for "a" is 2m1s old`)
	wantRefused(t, res, "r3", `observation of "DiskHeadroomOK" for "a" is 6m1s old`)
	wantRefused(t, res, "r4", "from the future")
	wantGranted(t, res, "r5")
}

// The requester of a never reports. The shard is blocked until the target
// is seen serving after the deadline.
func TestLostReport(t *testing.T) {
	a, b := node("a", "1"), node("b", "1")
	res := play(t, contract(), join(
		healthy(0, "a"),
		[]Event{
			ask(1*sec, "r-a", a),
			// Serving before the deadline proves nothing about later.
			Observe(9*min, "TargetServing", "a", admission.True),
			Tick(10*min + 30*sec),
		},
		healthy(10*min+30*sec, "b"),
		[]Event{
			ask(10*min+31*sec, "r-b1", b),
			Observe(11*min, "TargetServing", "a", admission.True),
		},
		healthy(11*min, "b"),
		[]Event{ask(11*min+sec, "r-b2", b)},
	))
	wantGranted(t, res, "r-a", "r-b2")
	wantRefused(t, res, "r-b1", "1 of 1 in use, 1 overdue, 0 failed")
	if at, ok := res.Released["r-a"]; !ok || at != 11*min {
		t.Fatalf("r-a released at %v (%v), want at 11m", at, ok)
	}
}

// The same lost report without release conditions: the shard stays
// blocked, whatever is observed.
func TestLostReportWithoutReleaseConditions(t *testing.T) {
	a, b := node("a", "1"), node("b", "1")
	c := contract()
	c.ReleaseWhen = nil
	res := play(t, c, join(
		healthy(0, "a"),
		[]Event{ask(1*sec, "r-a", a), Observe(1*time.Hour, "TargetServing", "a", admission.True)},
		healthy(1*time.Hour, "b"),
		[]Event{ask(1*time.Hour+sec, "r-b", b)},
	))
	wantRefused(t, res, "r-b", "1 of 1 in use, 1 overdue, 0 failed")
	if len(res.Released) != 0 {
		t.Fatalf("released without release conditions: %v", res.Released)
	}
}

// The restart of a fails. The shard is blocked while a is down and free
// again when a is seen serving after the failure report.
func TestFailedAction(t *testing.T) {
	a, b := node("a", "1"), node("b", "1")
	res := play(t, contract(), join(
		healthy(0, "a"),
		[]Event{
			ask(1*sec, "r-a", a),
			Observe(1*min, "TargetServing", "a", admission.True),
			Fail(2*min, "r-a"),
			Observe(2*min+30*sec, "TargetServing", "a", admission.False),
		},
		healthy(2*min+30*sec, "b"),
		[]Event{
			ask(2*min+31*sec, "r-b1", b),
			Observe(4*min, "TargetServing", "a", admission.True),
		},
		healthy(4*min, "b"),
		[]Event{ask(4*min+sec, "r-b2", b)},
	))
	wantRefused(t, res, "r-b1", "1 of 1 in use, 0 overdue, 1 failed")
	wantGranted(t, res, "r-b2")
	if at := res.Released["r-a"]; at != 4*min {
		t.Fatalf("r-a released at %v, want at 4m", at)
	}
}

// Many random time lines. Nothing is expected of the answers, only that no
// unsafe state is reached. A failure prints the seed and the full log.
func TestRandomTimelines(t *testing.T) {
	const timelines, steps = 300, 250
	conditions := []string{"PeerCanServe", "DiskHeadroomOK", "TargetServing"}
	statuses := []admission.Status{admission.True, admission.True, admission.True, admission.False, admission.Unknown}
	var nodes []admission.Target
	for shard := 1; shard <= 3; shard++ {
		for _, replica := range []string{"a", "b", "c"} {
			nodes = append(nodes, node(fmt.Sprintf("n%d%s", shard, replica), fmt.Sprint(shard)))
		}
	}
	// A node without the budget label must never get a grant.
	nodes = append(nodes, admission.Target{Name: "stray"})

	granted, refused, released := 0, 0, 0
	for seed := int64(1); seed <= timelines; seed++ {
		rng := rand.New(rand.NewSource(seed))
		c := contract()
		c.Budgets[0].Max = 1 + rng.Intn(2)
		if rng.Intn(4) == 0 {
			c.ReleaseWhen = nil
		}
		if rng.Intn(2) == 0 {
			c.ClockSkew = time.Duration(rng.Intn(20)) * sec
		}

		var events []Event
		at := time.Duration(0)
		for i := 0; i < steps; i++ {
			at += time.Duration(rng.Intn(15)) * sec
			n := nodes[rng.Intn(len(nodes))]
			switch p := rng.Intn(10); {
			case p < 4:
				// Mostly measured now, sometimes late, sometimes ahead.
				measured := at
				switch rng.Intn(6) {
				case 0:
					measured -= time.Duration(rng.Intn(600)) * sec
				case 1:
					measured += time.Duration(rng.Intn(30)) * sec
				}
				events = append(events, ObserveLate(at, measured, conditions[rng.Intn(len(conditions))], n.Name, statuses[rng.Intn(len(statuses))]))
			case p < 9:
				// A request, most often for a node that was just seen
				// healthy.
				if rng.Intn(4) > 0 {
					events = append(events, healthy(at, n.Name)...)
				}
				id := fmt.Sprintf("r%d", i)
				events = append(events, ask(at, id, n))
				// The requester reports later, in time or late, or never.
				// A report for a refused request is ignored.
				end := at + time.Duration(1+rng.Intn(900))*sec
				switch rng.Intn(5) {
				case 0:
				case 1:
					events = append(events, Fail(end, id))
				default:
					events = append(events, Succeed(end, id))
				}
				// Sometimes the target is seen serving some time after.
				if rng.Intn(2) == 0 {
					events = append(events, Observe(end+time.Duration(rng.Intn(300))*sec, "TargetServing", n.Name, statuses[rng.Intn(len(statuses))]))
				}
			default:
				events = append(events, Tick(at))
			}
		}

		res := Run(Timeline{Contract: c, Start: start, Events: events})
		if len(res.Violations) > 0 {
			t.Fatalf("seed %d: unsafe state reached:\n%s\n\nlog:\n%s", seed, strings.Join(res.Violations, "\n"), strings.Join(res.Log, "\n"))
		}
		for _, d := range res.Decisions {
			if d.Granted {
				granted++
			} else {
				refused++
			}
		}
		released += len(res.Released)
	}
	// The time lines must reach the interesting states, or the test proves
	// nothing.
	t.Logf("%d grants, %d refusals, %d releases", granted, refused, released)
	if granted < 2000 || refused < 2000 || released < 500 {
		t.Fatalf("the random time lines are too weak: %d grants, %d refusals, %d releases", granted, refused, released)
	}
}

// The checks themselves must see an unsafe state. Each case gives one to a
// check directly.
func TestChecksSeeUnsafeStates(t *testing.T) {
	c := contract()
	a, b := node("a", "1"), node("b", "1")
	now := start.Add(10 * min)
	obs := func(condition, subject string, status admission.Status, measured time.Duration) admission.Observation {
		return admission.Observation{Condition: condition, Subject: subject, Status: status, ObservedAt: start.Add(measured)}
	}
	fresh := []admission.Observation{
		obs("PeerCanServe", "a", admission.True, 10*min),
		obs("DiskHeadroomOK", "a", admission.True, 10*min),
	}
	req := admission.Request{ID: "r", Action: "restart", Target: a}
	grant := func(t admission.Target, deadline time.Duration) admission.Grant {
		return admission.Grant{RequestID: "r-" + t.Name, Action: "restart", Target: t, Deadline: start.Add(deadline)}
	}

	if got := unsafeGrant(c, req, fresh, now); len(got) != 0 {
		t.Fatalf("a safe grant is reported: %q", got)
	}
	if got := unsafeState(c, []admission.Grant{grant(a, 20*min)}); len(got) != 0 {
		t.Fatalf("a safe state is reported: %q", got)
	}

	cases := []struct {
		name string
		got  []string
		want string
	}{
		{"two replicas of one shard", unsafeState(c, []admission.Grant{grant(a, 20*min), grant(b, 20*min)}), "budget shard=1 has 2 open grants"},
		{"two grants for one target", unsafeState(c, []admission.Grant{grant(a, 20*min), grant(a, 20*min)}), "target a has 2 open grants"},
		{"grant without evidence", unsafeGrant(c, req, nil, now), "PeerCanServe was never observed"},
		{"grant with old evidence", unsafeGrant(c, req, fresh, now.Add(2*min)), "PeerCanServe is older than"},
		{"grant with False evidence", unsafeGrant(c, req, append(fresh, obs("PeerCanServe", "a", admission.False, 10*min+sec)), now.Add(2*sec)), "PeerCanServe is False"},
		{"grant with evidence from the future", unsafeGrant(c, req, append(fresh, obs("PeerCanServe", "a", admission.True, 11*min)), now), "in the future"},
		{"grant for a target without the label", unsafeGrant(c, admission.Request{ID: "r", Action: "restart", Target: admission.Target{Name: "a"}}, fresh, now), "no label shard"},
		{"grant for another action", unsafeGrant(c, admission.Request{ID: "r", Action: "drain", Target: a}, fresh, now), "does not cover the action"},
		{"release of a grant in time", unsafeRelease(c, grant(a, 20*min), []admission.Observation{obs("TargetServing", "a", admission.True, 10*min)}, now), "not overdue and not failed"},
		{"release with evidence from before the deadline", unsafeRelease(c, grant(a, 9*min+30*sec), []admission.Observation{obs("TargetServing", "a", admission.True, 9*min+20*sec)}, now), "measured before the grant went wrong"},
		{"release without evidence", unsafeRelease(c, grant(a, 5*min), nil, now), "TargetServing was never observed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(strings.Join(tc.got, "\n"), tc.want) {
				t.Fatalf("got %q, want it to contain %q", tc.got, tc.want)
			}
		})
	}
}
