# Decision record

Every decision can be checked later. The coordinator writes down what it
decided from, and the same input played through the decision core again
must give the same answer.

## What is in a record

A `DecisionRecord` holds the input of one decision and its output:

| Field | What it holds |
|---|---|
| `spec.time` | The time the decision was made with. |
| `spec.request` | The request: name, UID, action, requester, target, `expiresAt`. |
| `spec.contract` | The name and the full content of the contract that was used. |
| `spec.observations` | Every observation the decision could look at: those about the target, for a condition the contract requires. |
| `spec.openGrants` | Every grant in the namespace that was open, with its target and labels. |
| `spec.decision` | The answer: `Granted` with the deadline, or `Refused` with every reason. |

The contract and the observations are copied into the record. They can
change or be deleted after the decision; the record does not.

```yaml
apiVersion: allostat.io/v1alpha1
kind: DecisionRecord
metadata:
  name: restart-node-b-1-3fa9c2d1e07b
  namespace: fleet
spec:
  time: "2026-01-01T12:00:06.000000Z"
  request:
    name: restart-node-b-1
    uid: 6f1c2f0e-8f1d-4c7e-9a55-1f2b3c4d5e6f
    action: restart
    requester: upgrade-controller
    target:
      name: node-b
      labels:
        shard: "1"
  contract:
    name: restart
    spec:
      action: restart
      grantTTL: 10m
      requires:
        - condition: PeerCanServe
          maxAge: 1m
      budgets:
        - label: shard
          max: 1
  observations:
    - condition: PeerCanServe
      subject: node-b
      status: "False"
      source: replica-health
      observedAt: "2026-01-01T12:00:05.000000Z"
  openGrants:
    - request: restart-node-a-1
      action: restart
      target:
        name: node-a
        labels:
          shard: "1"
      grantedAt: "2026-01-01T12:00:01.000000Z"
      deadline: "2026-01-01T12:10:01.000000Z"
  decision:
    decision: Refused
    reasons:
      - '"PeerCanServe" for "node-b" is False (from "replica-health")'
      - 'budget shard=1 is full: 1 of 1 in use, 0 overdue, 0 failed'
```

## Where it is kept

A record is an object in the namespace of its request.

- The status of the request names it, in `status.record`.
- It is owned by the request. When the request is deleted, the cluster
  deletes the record.
- It cannot be changed. The API server refuses every update.
- Only the coordinator needs to create records. Give nobody else that
  right.

The coordinator writes the record first and the answer after it. An answer
therefore never exists without its record. For a grant the ledger entry
names the record too, so a coordinator that stopped between its writes
finishes with the right record.

The name of a record is the name of the request and a hash of the content.
If two coordinators decide the same request at the same moment, each
writes its own record. Only one of them can write the answer, and the
status names the record of that one. The other record can stay behind
until the request is deleted; it is not the record of the decision.

## How to check a record

```sh
kubectl get decisionrecords -n fleet -o yaml | allostat replay
allostat replay record.yaml
```

`allostat replay` needs no cluster. It plays each record through the
decision core and prints one line for each:

```
same       restart-node-b-1-3fa9c2d1e07b (request restart-node-b-1, Refused)
```

It prints `DIFFERENT` with the difference, and ends with an error, if a
record does not give its recorded answer. That means one of two things:
the record was changed, or the decision core of this version decides
differently from the one that made the decision. Use the version that
made the decision to tell them apart.

To check the decision of one request, read `status.record` of the request
and replay that record. Compare `spec.decision` of the record with the
status of the request.

## Limits

- **No contract.** If there was not exactly one contract for the action,
  the coordinator refuses before it asks the decision core. The record has
  the reason and no contract, and there is nothing to play again.
- **Releases.** A grant that is closed by evidence has that evidence as
  text in `status.released` of the request. It has no record of its own
  yet, and it cannot be played again.
- **Size.** A record holds every open grant of the namespace. Keep one
  fleet in one namespace, not all fleets in one.
- **Lifetime.** A record lives as long as its request. To keep records for
  longer, copy them out before requests are deleted.
- **What the record cannot show.** It shows that the answer follows from
  the input. It does not show that the observations were true.
