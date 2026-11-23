package admission

import (
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func restartContract() Contract {
	return Contract{
		Action: "restart",
		Requires: []Requirement{
			{Condition: "PeerCanServe", MaxAge: time.Minute},
			{Condition: "DiskHeadroomOK", MaxAge: 5 * time.Minute},
		},
		Budgets: []Budget{
			{Label: "shard", Max: 1},
			{Label: "gated", Value: "true", Max: 1},
		},
		GrantTTL: 10 * time.Minute,
	}
}

func node(name, shard, gated string) Target {
	return Target{Name: name, Labels: map[string]string{"shard": shard, "gated": gated}}
}

func healthy(subject string) []Observation {
	return []Observation{
		{Condition: "PeerCanServe", Subject: subject, Status: True, Source: "replica-health", ObservedAt: now.Add(-10 * time.Second)},
		{Condition: "DiskHeadroomOK", Subject: subject, Status: True, Source: "disk-pressure", ObservedAt: now.Add(-time.Minute)},
	}
}

func request(id string, t Target) Request {
	return Request{ID: id, Action: "restart", Requester: "upgrade", Target: t}
}

func openGrant(id string, t Target, deadline time.Time) Grant {
	return Grant{RequestID: id, Action: "restart", Target: t, GrantedAt: deadline.Add(-10 * time.Minute), Deadline: deadline}
}

func failedGrant(id string, t Target) Grant {
	g := openGrant(id, t, now.Add(-time.Hour))
	g.Failed = true
	return g
}

func TestGrantWhenEvidenceIsFreshAndBudgetsAreFree(t *testing.T) {
	a := node("a", "1", "false")
	d := Decide(restartContract(), request("r1", a), healthy("a"), nil, now)
	if !d.Granted || len(d.Reasons) != 0 {
		t.Fatalf("want grant, got %+v", d)
	}
	if d.Grant.Target.Name != "a" || !d.Grant.GrantedAt.Equal(now) || !d.Grant.Deadline.Equal(now.Add(10*time.Minute)) {
		t.Fatalf("wrong grant: %+v", d.Grant)
	}
}

func TestRefusals(t *testing.T) {
	a := node("a", "1", "false")
	b := node("b", "1", "false")
	gatedA := node("a", "1", "true")
	gatedC := node("c", "2", "true")
	plainC := node("c", "2", "false")

	replace := func(obs []Observation, condition string, change func(*Observation)) []Observation {
		out := append([]Observation(nil), obs...)
		for i := range out {
			if out[i].Condition == condition {
				change(&out[i])
			}
		}
		return out
	}

	cases := []struct {
		name     string
		contract Contract
		req      Request
		obs      []Observation
		open     []Grant
		want     string
	}{
		{
			name: "no observation", contract: restartContract(), req: request("r1", a),
			obs:  healthy("a")[1:],
			want: `no observation of "PeerCanServe"`,
		},
		{
			name: "observation of another subject does not count", contract: restartContract(), req: request("r1", a),
			obs:  healthy("b"),
			want: `no observation of "PeerCanServe" for "a"`,
		},
		{
			name: "stale observation", contract: restartContract(), req: request("r1", a),
			obs:  replace(healthy("a"), "PeerCanServe", func(o *Observation) { o.ObservedAt = now.Add(-2 * time.Minute) }),
			want: "2m0s old, limit is 1m0s",
		},
		{
			name: "false observation", contract: restartContract(), req: request("r1", a),
			obs:  replace(healthy("a"), "DiskHeadroomOK", func(o *Observation) { o.Status = False }),
			want: `"DiskHeadroomOK" for "a" is False`,
		},
		{
			name: "unknown observation", contract: restartContract(), req: request("r1", a),
			obs:  replace(healthy("a"), "PeerCanServe", func(o *Observation) { o.Status = Unknown }),
			want: `"PeerCanServe" for "a" is Unknown`,
		},
		{
			name: "observation from the future", contract: restartContract(), req: request("r1", a),
			obs:  replace(healthy("a"), "PeerCanServe", func(o *Observation) { o.ObservedAt = now.Add(time.Minute) }),
			want: "from the future (1m0s ahead, limit is 0s)",
		},
		{
			name: "observation ahead by more than the clock skew",
			contract: func() Contract {
				c := restartContract()
				c.ClockSkew = 2 * time.Second
				return c
			}(),
			req:  request("r1", a),
			obs:  replace(healthy("a"), "PeerCanServe", func(o *Observation) { o.ObservedAt = now.Add(2*time.Second + time.Millisecond) }),
			want: "from the future (2.001s ahead, limit is 2s)",
		},
		{
			name: "negative clock skew",
			contract: func() Contract {
				c := restartContract()
				c.ClockSkew = -time.Second
				return c
			}(),
			req: request("r1", a), obs: healthy("a"),
			want: "negative clock skew",
		},
		{
			name: "newest observation wins over an older good one", contract: restartContract(), req: request("r1", a),
			obs: append(healthy("a"), Observation{
				Condition: "PeerCanServe", Subject: "a", Status: False, Source: "replica-health", ObservedAt: now.Add(-time.Second),
			}),
			want: `"PeerCanServe" for "a" is False`,
		},
		{
			name: "peer in the same shard holds a grant", contract: restartContract(), req: request("r2", a),
			obs:  healthy("a"),
			open: []Grant{openGrant("r1", b, now.Add(5*time.Minute))},
			want: "budget shard=1 is full: 1 of 1 in use, 0 overdue, 0 failed",
		},
		{
			name: "overdue grant still holds the budget", contract: restartContract(), req: request("r2", a),
			obs:  healthy("a"),
			open: []Grant{openGrant("r1", b, now.Add(-time.Hour))},
			want: "budget shard=1 is full: 1 of 1 in use, 1 overdue, 0 failed",
		},
		{
			name: "failed grant still holds the budget", contract: restartContract(), req: request("r2", a),
			obs:  healthy("a"),
			open: []Grant{failedGrant("r1", b)},
			want: "budget shard=1 is full: 1 of 1 in use, 0 overdue, 1 failed",
		},
		{
			name: "target with a failed grant is not granted again", contract: restartContract(), req: request("r2", a),
			obs:  healthy("a"),
			open: []Grant{failedGrant("r1", a)},
			want: `target "a" already has an open grant (request "r1"), reported as failed`,
		},
		{
			name: "another gated node holds a grant", contract: restartContract(), req: request("r2", gatedA),
			obs:  healthy("a"),
			open: []Grant{openGrant("r1", gatedC, now.Add(5*time.Minute))},
			want: "budget gated=true is full",
		},
		{
			name: "target without the budget label", contract: restartContract(),
			req:  request("r1", Target{Name: "a", Labels: map[string]string{"shard": "1"}}),
			obs:  healthy("a"),
			want: `target "a" has no label "gated"`,
		},
		{
			name: "target already has a grant from another request", contract: restartContract(), req: request("r2", a),
			obs:  healthy("a"),
			open: []Grant{openGrant("r1", a, now.Add(5*time.Minute))},
			want: `target "a" already has an open grant (request "r1")`,
		},
		{
			name: "wrong action", contract: restartContract(),
			req:  Request{ID: "r1", Action: "reboot", Requester: "upgrade", Target: a},
			obs:  healthy("a"),
			want: `contract covers action "restart", not "reboot"`,
		},
		{
			name: "contract without a grant TTL",
			contract: func() Contract {
				c := restartContract()
				c.GrantTTL = 0
				return c
			}(),
			req: request("r1", a), obs: healthy("a"),
			want: "no grant TTL",
		},
		{
			name: "request without an ID", contract: restartContract(), req: request("", a),
			obs:  healthy("a"),
			want: "no ID or no target name",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := Decide(tc.contract, tc.req, tc.obs, tc.open, now)
			if d.Granted || d.Grant != nil {
				t.Fatalf("want refusal, got grant %+v", d.Grant)
			}
			if !strings.Contains(strings.Join(d.Reasons, "\n"), tc.want) {
				t.Fatalf("reasons %q do not contain %q", d.Reasons, tc.want)
			}
		})
	}

	t.Run("grants outside the group do not count", func(t *testing.T) {
		open := []Grant{openGrant("r1", plainC, now.Add(5*time.Minute))}
		d := Decide(restartContract(), request("r2", gatedA), healthy("a"), open, now)
		if !d.Granted {
			t.Fatalf("want grant, got %q", d.Reasons)
		}
	})
}

func TestObservationWithinTheClockSkewCounts(t *testing.T) {
	a := node("a", "1", "false")
	c := restartContract()
	c.ClockSkew = 2 * time.Second
	obs := healthy("a")
	obs[0].ObservedAt = now.Add(2 * time.Second) // exactly at the limit

	d := Decide(c, request("r1", a), obs, nil, now)
	if !d.Granted {
		t.Fatalf("want grant, got %q", d.Reasons)
	}
}

// The clock skew is only for times in the future. It must not make an old
// observation count for longer.
func TestClockSkewDoesNotExtendMaxAge(t *testing.T) {
	a := node("a", "1", "false")
	c := restartContract()
	c.ClockSkew = time.Hour
	obs := healthy("a")
	obs[0].ObservedAt = now.Add(-time.Minute - time.Second)

	d := Decide(c, request("r1", a), obs, nil, now)
	if d.Granted || !strings.Contains(strings.Join(d.Reasons, "\n"), "1m1s old, limit is 1m0s") {
		t.Fatalf("want a refusal for an old observation, got %+v", d)
	}
}

func TestRefusalListsEveryReason(t *testing.T) {
	a := node("a", "1", "false")
	b := node("b", "1", "false")
	open := []Grant{openGrant("r1", b, now.Add(5*time.Minute))}
	d := Decide(restartContract(), request("r2", a), nil, open, now)
	if d.Granted || len(d.Reasons) != 3 {
		t.Fatalf("want 3 reasons (two missing observations, one full budget), got %q", d.Reasons)
	}
}

func TestRetryGetsTheSameGrant(t *testing.T) {
	a := node("a", "1", "false")
	first := openGrant("r1", a, now.Add(5*time.Minute))
	// No observations at all: a retry must not depend on new evidence.
	d := Decide(restartContract(), request("r1", a), nil, []Grant{first}, now)
	if !d.Granted || !d.Grant.Deadline.Equal(first.Deadline) || !d.Grant.GrantedAt.Equal(first.GrantedAt) {
		t.Fatalf("want the first grant back, got %+v", d)
	}
}

func releaseContract() Contract {
	c := restartContract()
	c.ReleaseWhen = []Requirement{{Condition: "TargetServing", MaxAge: time.Minute}}
	return c
}

func serving(subject string, at time.Time) []Observation {
	return []Observation{{Condition: "TargetServing", Subject: subject, Status: True, Source: "replica-health", ObservedAt: at}}
}

func TestReleaseOfAnOverdueGrant(t *testing.T) {
	a := node("a", "1", "false")
	deadline := now.Add(-5 * time.Minute)
	g := openGrant("r1", a, deadline)

	d := Release(releaseContract(), g, serving("a", now.Add(-10*time.Second)), now)
	if !d.Released || len(d.Evidence) != 1 || d.Evidence[0].Condition != "TargetServing" {
		t.Fatalf("want a release with the evidence, got %+v", d)
	}
}

func TestReleaseOfAFailedGrant(t *testing.T) {
	a := node("a", "1", "false")
	// Failed before the deadline: the failure time counts, not the deadline.
	g := openGrant("r1", a, now.Add(5*time.Minute))
	g.Failed = true
	g.FailedAt = now.Add(-30 * time.Second)

	d := Release(releaseContract(), g, serving("a", now.Add(-10*time.Second)), now)
	if !d.Released {
		t.Fatalf("want a release, got %q", d.Reasons)
	}
}

func TestGrantStaysOpen(t *testing.T) {
	a := node("a", "1", "false")
	deadline := now.Add(-5 * time.Minute)
	overdue := openGrant("r1", a, deadline)
	fresh := serving("a", now.Add(-10*time.Second))

	failedAt := func(at time.Time) Grant {
		g := overdue
		g.Failed = true
		g.FailedAt = at
		return g
	}
	skewed := releaseContract()
	skewed.ClockSkew = 30 * time.Second

	cases := []struct {
		name     string
		contract Contract
		grant    Grant
		obs      []Observation
		reason   string
	}{
		{
			name:     "contract has no release conditions",
			contract: restartContract(), grant: overdue, obs: fresh,
			reason: "contract has no release conditions",
		},
		{
			name:     "grant is in time and has no report",
			contract: releaseContract(), grant: openGrant("r1", a, now.Add(time.Minute)), obs: fresh,
			reason: "not overdue and not reported as failed",
		},
		{
			name:     "grant is exactly at its deadline",
			contract: releaseContract(), grant: openGrant("r1", a, now), obs: fresh,
			reason: "not overdue and not reported as failed",
		},
		{
			name:     "failure time is not known",
			contract: releaseContract(), grant: failedAt(time.Time{}), obs: fresh,
			reason: "time of the failure report is not known",
		},
		{
			name:     "no evidence",
			contract: releaseContract(), grant: overdue,
			reason: `no observation of "TargetServing" for "a"`,
		},
		{
			name:     "evidence is for another target",
			contract: releaseContract(), grant: overdue, obs: serving("b", now.Add(-10*time.Second)),
			reason: `no observation of "TargetServing" for "a"`,
		},
		{
			name:     "evidence is old",
			contract: releaseContract(), grant: overdue, obs: serving("a", now.Add(-2*time.Minute)),
			reason: "2m0s old, limit is 1m0s",
		},
		{
			name:     "evidence is from the future",
			contract: releaseContract(), grant: overdue, obs: serving("a", now.Add(time.Second)),
			reason: "from the future",
		},
		{
			name:     "evidence is False",
			contract: releaseContract(), grant: overdue,
			obs:    []Observation{{Condition: "TargetServing", Subject: "a", Status: False, Source: "replica-health", ObservedAt: now}},
			reason: `"TargetServing" for "a" is False`,
		},
		{
			name:     "evidence is Unknown",
			contract: releaseContract(), grant: overdue,
			obs:    []Observation{{Condition: "TargetServing", Subject: "a", Status: Unknown, Source: "replica-health", ObservedAt: now}},
			reason: `"TargetServing" for "a" is Unknown`,
		},
		{
			name:     "a newer False wins over an older True",
			contract: releaseContract(), grant: overdue,
			obs: append(serving("a", now.Add(-20*time.Second)),
				Observation{Condition: "TargetServing", Subject: "a", Status: False, Source: "replica-health", ObservedAt: now.Add(-10 * time.Second)}),
			reason: `"TargetServing" for "a" is False`,
		},
		{
			name:     "evidence is from before the deadline",
			contract: releaseContract(), grant: openGrant("r1", a, now.Add(-10*time.Second)), obs: serving("a", now.Add(-20*time.Second)),
			reason: "is not newer than",
		},
		{
			name:     "evidence is from exactly the deadline",
			contract: releaseContract(), grant: openGrant("r1", a, now.Add(-10*time.Second)), obs: serving("a", now.Add(-10*time.Second)),
			reason: "is not newer than",
		},
		{
			name:     "evidence is after the deadline but before the failure report",
			contract: releaseContract(), grant: failedAt(now.Add(-5 * time.Second)), obs: fresh,
			reason: "is not newer than",
		},
		{
			name:     "evidence is newer only because of the allowed clock skew",
			contract: skewed, grant: failedAt(now.Add(-20 * time.Second)), obs: fresh,
			reason: "is not newer than",
		},
		{
			name:     "contract is for another action",
			contract: func() Contract { c := releaseContract(); c.Action = "drain"; return c }(), grant: overdue, obs: fresh,
			reason: `contract covers action "drain", not "restart"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := Release(tc.contract, tc.grant, tc.obs, now)
			if d.Released || len(d.Evidence) != 0 {
				t.Fatalf("the grant must stay open, got %+v", d)
			}
			if !strings.Contains(strings.Join(d.Reasons, "\n"), tc.reason) {
				t.Fatalf("reasons %q do not contain %q", d.Reasons, tc.reason)
			}
		})
	}
}

// Every release condition must hold, not only one.
func TestReleaseNeedsEveryCondition(t *testing.T) {
	a := node("a", "1", "false")
	c := releaseContract()
	c.ReleaseWhen = append(c.ReleaseWhen, Requirement{Condition: "ReplicationCaughtUp", MaxAge: time.Minute})
	g := openGrant("r1", a, now.Add(-5*time.Minute))
	obs := serving("a", now.Add(-10*time.Second))

	if d := Release(c, g, obs, now); d.Released {
		t.Fatalf("one of two conditions must not release, got %+v", d)
	}
	obs = append(obs, Observation{Condition: "ReplicationCaughtUp", Subject: "a", Status: True, Source: "replication", ObservedAt: now})
	if d := Release(c, g, obs, now); !d.Released || len(d.Evidence) != 2 {
		t.Fatalf("want a release with two observations, got %+v", d)
	}
}

// A request can say how long its requester waits. An answer after that
// time is a refusal: a grant would hold the budget for nobody.
func TestExpiredRequestIsRefused(t *testing.T) {
	a := node("a", "1", "false")
	cases := []struct {
		name      string
		expiresAt time.Time
		granted   bool
	}{
		{"no expiry", time.Time{}, true},
		{"expires later", now.Add(time.Second), true},
		{"expires exactly now", now, false},
		{"expired", now.Add(-time.Second), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := request("r1", a)
			req.ExpiresAt = tc.expiresAt
			d := Decide(restartContract(), req, healthy("a"), nil, now)
			if d.Granted != tc.granted {
				t.Fatalf("granted = %v, want %v; reasons %q", d.Granted, tc.granted, d.Reasons)
			}
			if !tc.granted && !strings.Contains(strings.Join(d.Reasons, "\n"), "request expired at") {
				t.Fatalf("reasons %q do not say that the request expired", d.Reasons)
			}
		})
	}
}

// A request that already holds a grant keeps it after its expiry: the
// expiry is only about the first answer.
func TestExpiryDoesNotTakeAGrantBack(t *testing.T) {
	a := node("a", "1", "false")
	req := request("r1", a)
	req.ExpiresAt = now.Add(time.Minute)
	first := Decide(restartContract(), req, healthy("a"), nil, now)
	if !first.Granted {
		t.Fatalf("want grant, got %q", first.Reasons)
	}
	again := Decide(restartContract(), req, nil, []Grant{*first.Grant}, now.Add(time.Hour))
	if !again.Granted || !again.Grant.Deadline.Equal(first.Grant.Deadline) {
		t.Fatalf("want the same grant, got %+v", again)
	}
}
