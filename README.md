# Homeostat

Homeostat lets independently built infrastructure controllers keep shared
system-level conditions, through operational contracts and coordination.

Status: early research code. Nothing here is ready for production.

## The problem

A fleet is run by many controllers: one upgrades nodes, one balances disks,
one watches replicas. Each is correct by itself. None knows what the others
are doing, so a person checks all of them before each risky step. Homeostat
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

## Planned

- Contracts, observations and requests as Kubernetes custom resources.
- A controller that runs the decision core and writes each decision to the
  request's status.

## Development

```sh
go test ./...
```

## License

Apache-2.0
