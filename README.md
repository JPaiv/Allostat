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

A request is judged by the contract in its own namespace that has the same
action. The requester cannot pick a contract, and its spec cannot change
after it is created.

`cmd/allostat` is the coordinator. It decides each new request once and
writes the answer to the request's status. A decision is final: a refused
request stays refused, and the requester asks again with a new request. A
granted request holds its grant until the request is deleted.

The coordinator keeps no state of its own. It decides one request at a time
and reads everything from the API server at that moment, so a grant that was
just written always counts for the next decision. Run one coordinator, with
leader election (the default). The access rules it needs are in
`config/rbac`.

## Planned

- A way for the requester to report the result, which closes the grant.
- A container image and an example that runs in a local cluster.

## Development

```sh
make test
```

## License

Apache-2.0
