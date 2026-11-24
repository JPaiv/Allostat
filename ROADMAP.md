# Roadmap

This is the planned order of work. It is a plan, not a promise: an item
can move when a real user needs something else first.

What is done is in the [README](README.md).

## 1. Make it possible to run

The first release, `v0.1`.

- A container image and manifests: resource types, access rules and a
  deployment of the coordinator.
- A release process: tags, a changelog, and the image published from CI.
- A quickstart: install on a local cluster and run one contract by hand.

## 2. Make it easy to use from a controller

- A client. `allostat ask` and `allostat report` as commands, and a small
  Go package that does the same: create the request, wait for the answer,
  report the result. Today each requester writes this itself.
- A stable code for each reason in the request status, so a requester can
  act on a code and not on text.
- `allostat explain <request>`: the decision, its reasons, and what to do
  for each, from [Messages and what to do](docs/messages.md).

## 3. Make it visible

- Metrics: decisions by answer and reason; open, overdue and failed
  grants; the time to decide.
- Events on the request for a grant, a refusal and a release.
- A status on the contract: how many grants are open in each budget.

## 4. Close the known limits

- A requester that starts after the deadline can act on a grant that was
  released.
- A coordinator clock that is behind makes old observations look newer.
- A record holds every open grant of the namespace.
- A refusal without exactly one contract cannot be played again.
- A ledger that is deleted between the two writes of a grant can open a
  gap.

## 5. Widen the contract

These wait for a user who needs them.

- A budget over more than one action.
- A condition about a group, not only about the target.
- A dry run: decide and record, but mark the answer as advice.
- A check of a contract when it is written.

## 6. A stable API

- From `v1alpha1` to `v1beta1`, with a plan for the change and a promise
  that old records can still be played.
