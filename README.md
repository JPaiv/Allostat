# Allostat

Allostat lets independently built infrastructure controllers keep shared
system-level conditions, through operational contracts and coordination.

Status: early research code. Nothing here is ready for production.

The name comes from allostasis: staying stable by changing. A fleet is
never at rest, so the goal is not to hold it still but to let it change
without breaking. The project was first called Homeostat.

## The problem

A fleet is run by many controllers: one upgrades nodes, one balances disks,
one watches replicas. Each is correct by itself. None knows what the others
are doing, so a person checks all of them before each risky step. Allostat
tests if that part can be made explicit and automatic.

## How it works

- A controller that wants to act asks first: "may I restart node `a` now?"
- Other controllers report facts they own: "the peer of `a` can serve",
  "disk headroom is fine". Each fact has a time stamp.
- A **contract** says which facts must be true and fresh, and how many
  targets of one group may be out at the same time.
- The coordinator answers yes or no and records why. It never touches the
  target. The controller that asked does the work and reports the result.

In doubt the answer is no. Missing, old or unclear facts never give a grant.
A fact with a time in the future is refused too, unless the contract allows
a small clock difference between the reporter and the coordinator.
A grant stays open until its result is reported, also after its deadline,
because the target may still be out.

## What exists

`admission` is the decision core: a pure function from contract, request,
observations, open grants and time to a decision. It has no I/O.

```go
contract := admission.Contract{
	Action: "restart",
	Requires: []admission.Requirement{
		{Condition: "PeerCanServe", MaxAge: time.Minute},
	},
	Budgets:  []admission.Budget{{Label: "shard", Max: 1}},
	GrantTTL: 10 * time.Minute,
}
decision := admission.Decide(contract, request, observations, openGrants, time.Now())
```

`api/v1alpha1` has the Kubernetes resource types, in the API group
`allostat.io`. The files to install them are in `config/crd`.

| Resource | Written by | What it holds |
|---|---|---|
| `Contract` | whoever owns the fleet's rules | required conditions, budgets and grant TTL for one action |
| `Observation` | the controller that owns the fact | one fact about one target, with its time |
| `ActionRequest` | the controller that wants to act | the action and the target; the decision is in the status |
| `Ledger` | the coordinator only | the grants it gave in the namespace that are not closed |

A request is judged by the contract in its own namespace that has the same
action. The requester cannot pick a contract, and it cannot change the
action or the target after the request is created.

`cmd/allostat` is the coordinator. It decides each new request once and
writes the answer to the request's status. A decision is final: a refused
request stays refused, and the requester asks again with a new request.

When the action has ended, the requester reports the result in
`spec.result`, once:

- `Succeeded` closes the grant. The target's place in the budgets is free.
- `Failed` keeps the grant open, because the target may still be out.

A grant with a failure report, or with no report after its deadline, is
closed in one of two ways:

- A person deletes the request.
- The contract lists conditions in `releaseWhen`, for example "the target
  serves again". The coordinator closes the grant when each of them is
  true, fresh, and measured after the grant went wrong: after the deadline,
  or after the coordinator saw the failure report. An older fact says
  nothing about the target now. The request status records the release and
  the facts it used.

Without `releaseWhen` only a person closes such a grant. With it, the
requester must not start the action after the deadline, because the grant
may be gone by then.

The coordinator keeps no state of its own. It reads everything from the API
server at the moment of the decision.

Every grant is first written to the `Ledger` of the namespace. The API
server accepts that write only if nobody changed the ledger since it was
read. Two coordinators that decide from the same state can therefore never
both give a grant: the second write fails, and that request is decided
again with the first grant counted. Leader election is still on by default,
but the budgets do not depend on it.

The access rules the coordinator needs are in `config/rbac`. Nobody else
should be able to write or delete a ledger.

## Planned

- A container image and an example that runs in a local cluster.

## Development

```sh
make test
```

## License

Apache-2.0
