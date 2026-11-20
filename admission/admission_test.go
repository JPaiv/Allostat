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
			want: "from the future",
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
			want: "budget shard=1 is full: 1 of 1 in use, 0 overdue",
		},
		{
			name: "overdue grant still holds the budget", contract: restartContract(), req: request("r2", a),
			obs:  healthy("a"),
			open: []Grant{openGrant("r1", b, now.Add(-time.Hour))},
			want: "budget shard=1 is full: 1 of 1 in use, 1 overdue",
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
