# Contracts

This page explains the four things Allostat works with: an observation, a
requirement, a budget and a grant. A contract puts requirements and
budgets together for one kind of action.

All resources are in the API group `allostat.io/v1alpha1` and all of them
live in a namespace. A request is judged only by what is in its own
namespace.

## The rule for doubt

**In doubt the answer is no.**

A grant needs proof for every requirement and room in every budget. If a
fact is missing, too old, `Unknown`, or has a time in the future, it is
not proof. If the coordinator cannot tell which group a target is in, or
which contract applies, it refuses. A refusal lists every reason, not only
the first.

The other direction has the same rule. A grant is closed only by proof
that it is over. A grant with no report stays open.

## Observation

An observation is one fact about one subject, written by the controller
that owns the fact. The coordinator only reads it.

```yaml
apiVersion: allostat.io/v1alpha1
kind: Observation
metadata:
  name: peercanserve-node-a
  namespace: fleet
spec:
  condition: PeerCanServe
  subject: node-a
  status: "True"
  source: replica-health
  observedAt: "2026-01-01T12:00:00.000000Z"
```

| Field | Meaning |
|---|---|
| `condition` | The name of the fact. Reporters and contracts must use the same name. |
| `subject` | The name of the target the fact is about. |
| `status` | `True`, `False` or `Unknown`. Only `True` is proof. |
| `source` | The controller that reports it. It is shown in refusals. |
| `observedAt` | When the source measured the fact, not when it wrote the object. |

If there are several observations of the same condition for the same
subject, the one with the newest `observedAt` counts, also when it is
`False` and an older one is `True`. A reporter can keep one object for
each fact and update it, or create a new object each time.

A reporter that cannot measure writes `Unknown` or writes nothing. Both
lead to a refusal: the first at once, the second when the last observation
is too old.

## Requirement

A requirement names a condition that must be `True` and fresh for the
target of the request. It is part of a contract.

```yaml
apiVersion: allostat.io/v1alpha1
kind: Contract
metadata:
  name: restart
  namespace: fleet
spec:
  action: restart
  grantTTL: 10m
  requires:
    - condition: PeerCanServe
      maxAge: 1m
    - condition: DiskHeadroomOK
      maxAge: 5m
```

With this contract a restart of `node-a` needs two observations with
subject `node-a`: `PeerCanServe`, at most one minute old, and
`DiskHeadroomOK`, at most five minutes old. Both must be `True`.

`maxAge` is how long you trust the fact after it was measured. Choose it
from how fast the fact can change, and keep it above the interval of the
reporter, or every request is refused for old evidence.

An observation with a time in the future is refused: either a clock is
wrong or the fact is not real. `clockSkew` in the contract allows a small
difference between the reporter's clock and the coordinator's. An
observation up to that far ahead counts as measured now. It does not make
an old observation count for longer.

```yaml
apiVersion: allostat.io/v1alpha1
kind: Contract
metadata:
  name: restart-with-skew
  namespace: other-fleet
spec:
  action: restart
  grantTTL: 10m
  clockSkew: 2s
  requires:
    - condition: PeerCanServe
      maxAge: 1m
```

There must be exactly one contract for an action in a namespace. With
none or with two, every request for that action is refused.

## Budget

A budget limits how many targets of one group may hold an open grant at
the same time. The groups come from a label on the target.

```yaml
apiVersion: allostat.io/v1alpha1
kind: Contract
metadata:
  name: restart-with-budgets
  namespace: budget-fleet
spec:
  action: restart
  grantTTL: 10m
  requires:
    - condition: PeerCanServe
      maxAge: 1m
  budgets:
    - label: shard
      max: 1
    - label: tier
      value: gold
      max: 2
```

| Field | Meaning |
|---|---|
| `label` | The target label that forms the groups. |
| `value` | Optional. The budget then counts only targets with this value. Targets with another value are outside this budget. |
| `max` | The number of open grants allowed in one group. |

The first budget says: among targets with the same `shard` value, at most
one may be out. The second says: among targets with `tier: gold`, at most
two may be out, and other tiers are not limited by it.

The labels are in the request, not on a Kubernetes object:

```yaml
apiVersion: allostat.io/v1alpha1
kind: ActionRequest
metadata:
  name: restart-node-a-1
  namespace: budget-fleet
spec:
  action: restart
  requester: upgrade-controller
  expiresAt: "2026-01-01T12:05:00Z"
  target:
    name: node-a
    labels:
      shard: "1"
      tier: gold
```

A target that does not have the label of a budget is refused: the
coordinator cannot tell which group it is in. This is true for every
budget of the contract, also one with a `value`.

A target can hold only one open grant, whatever the budgets say.

A budget with `max: 0` refuses every target of its groups. That is a way
to stop an action for a tier or for the whole namespace.

## Grant

A grant is a yes to one request. The requester reads it in the status of
its request.

```yaml
apiVersion: allostat.io/v1alpha1
kind: ActionRequest
metadata:
  name: restart-node-b-1
  namespace: budget-fleet
spec:
  action: restart
  requester: upgrade-controller
  target:
    name: node-b
    labels:
      shard: "1"
      tier: gold
status:
  decision: Granted
  decidedAt: "2026-01-01T12:00:05.000000Z"
  deadline: "2026-01-01T12:10:05.000000Z"
```

The deadline is the time of the decision plus `grantTTL` of the contract.
The requester must finish and report before it, and must not start the
action after it.

A refusal looks like this. It is final; the requester asks again with a
new request.

```yaml
apiVersion: allostat.io/v1alpha1
kind: ActionRequest
metadata:
  name: restart-node-c-1
  namespace: budget-fleet
spec:
  action: restart
  requester: upgrade-controller
  target:
    name: node-c
    labels:
      shard: "1"
      tier: gold
status:
  decision: Refused
  decidedAt: "2026-01-01T12:00:06.000000Z"
  reasons:
    - 'observation of "PeerCanServe" for "node-c" is 1m30s old, limit is 1m0s'
    - 'budget shard=1 is full: 1 of 1 in use, 0 overdue, 0 failed'
```

### How a grant ends

The requester reports the result once, in `spec.result`:

```yaml
apiVersion: allostat.io/v1alpha1
kind: ActionRequest
metadata:
  name: restart-node-d-1
  namespace: budget-fleet
spec:
  action: restart
  requester: upgrade-controller
  target:
    name: node-d
    labels:
      shard: "2"
      tier: gold
  result:
    outcome: Succeeded
    message: restarted in 90 seconds
status:
  decision: Granted
  decidedAt: "2026-01-01T12:00:05.000000Z"
  deadline: "2026-01-01T12:10:05.000000Z"
```

A result is accepted only on a granted request, and it cannot change
after it is written.

| What happened | The grant |
|---|---|
| `Succeeded` is reported | is closed. Its place in the budgets is free. |
| `Failed` is reported | stays open. The target may still be out. |
| Nothing is reported and the deadline passes | stays open, and is shown as overdue in refusals. |
| The request is deleted | is closed. A person does this after a look at the target. |

An open grant always counts in the budgets, also when it is failed or
overdue. One lost report therefore stops its group. A contract can allow
a way out with `releaseWhen`:

```yaml
apiVersion: allostat.io/v1alpha1
kind: Contract
metadata:
  name: restart-with-release
  namespace: release-fleet
spec:
  action: restart
  grantTTL: 10m
  requires:
    - condition: PeerCanServe
      maxAge: 1m
  budgets:
    - label: shard
      max: 1
  releaseWhen:
    - condition: TargetServing
      maxAge: 1m
```

The coordinator then closes a failed or overdue grant when every
condition in `releaseWhen` is `True` and fresh for the target, and was
measured after the grant went wrong: after the deadline for an overdue
grant, after the coordinator saw the failure report for a failed one. An
older fact says nothing about the target now. The status of the request
records the release:

```yaml
apiVersion: allostat.io/v1alpha1
kind: ActionRequest
metadata:
  name: restart-node-e-1
  namespace: release-fleet
spec:
  action: restart
  requester: upgrade-controller
  target:
    name: node-e
    labels:
      shard: "3"
status:
  decision: Granted
  decidedAt: "2026-01-01T12:00:05.000000Z"
  deadline: "2026-01-01T12:10:05.000000Z"
  released:
    at: "2026-01-01T12:11:00.000000Z"
    evidence:
      - 'TargetServing=True for "node-e" from "replica-health", measured at 2026-01-01T12:10:58Z'
```

Choose release conditions that show the target is back and the action is
over. Without `releaseWhen`, only a person closes such a grant.

## All fields of a contract

| Field | Required | Meaning |
|---|---|---|
| `action` | yes | The kind of action the contract covers. |
| `grantTTL` | yes | How long the requester has to finish and report. |
| `requires` | no | Conditions that must be `True` and fresh for the target. |
| `budgets` | no | Limits on open grants in a group of targets. |
| `clockSkew` | no | How far ahead of the coordinator's clock an observation may be. |
| `releaseWhen` | no | Conditions that close a failed or overdue grant. |

A contract without `requires` and without `budgets` grants every request,
one for each target at a time. Durations are written like `90s`, `10m` or
`1h30m`.

See also [When there is no answer](no-answer.md) for what a requester does
when the coordinator is away.
