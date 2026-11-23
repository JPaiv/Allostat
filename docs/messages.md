# Messages and what to do

This page lists every message Allostat gives, what it means, and what to
do. Text in `<...>` stands for a name, a number or a time.

A refusal is not a fault. The coordinator said no because the contract
does not allow the action now. Read the reason before you change anything,
and change the system, not the contract, unless the contract is wrong.

## Why a request was refused

These are in `status.reasons` of the request and in its `DecisionRecord`.
A request can have more than one. A refusal is final for that request:
correct the cause, then ask again with a new request.

### The condition is not met

| Message | What it means | What to do |
|---|---|---|
| `"<condition>" for "<target>" is <status> (from "<source>")` | The newest report says the condition is `False` or `Unknown`. | Do not act. Find out why the condition does not hold and repair that. With `Unknown`, the source cannot measure: look at the source. |
| `no observation of "<condition>" for "<target>"` | Nobody reported this condition for this target. | Check that the reporter runs and writes to the namespace of the request. Check that `spec.subject` of its observations is the same name as `spec.target.name` of the request. |
| `observation of "<condition>" for "<target>" is <age> old, limit is <maxAge>` | The newest report is too old to trust. | Check that the reporter still runs. If it runs and reports less often than `maxAge`, make it report more often or raise `maxAge` in the contract. |
| `observation of "<condition>" for "<target>" is from the future (<difference> ahead, limit is <clockSkew>)` | The clock of the reporter is ahead of the clock of the coordinator by more than the contract allows. | Repair the time sync of the two hosts. Raise `clockSkew` in the contract only if that difference is normal for your hosts. |

### The budget or the target is in use

| Message | What it means | What to do |
|---|---|---|
| `budget <label>=<value> is full: <used> of <max> in use, <overdue> overdue, <failed> failed` | As many grants as the budget allows are open in this group. | With 0 overdue and 0 failed: wait, the other actions are in progress. If not, a grant is stuck: see [A grant that stays open](#a-grant-that-stays-open). |
| `target "<target>" already has an open grant (request "<request>")<state>` | Another request holds a grant for this target. `<state>` is empty, `, overdue` or `, reported as failed`. | With an empty state: wait for that request to end. If not, see [A grant that stays open](#a-grant-that-stays-open). |
| `target "<target>" has no label "<label>"` | The contract has a budget for this label, and the request does not say in which group the target is. | Create a new request with the label in `spec.target.labels`. |

### The request or the contract

| Message | What it means | What to do |
|---|---|---|
| `request expired at <time>, before it was decided` | The coordinator did not decide before `spec.expiresAt`. | Ask again with a new request. If this happens often, the coordinator is down or slow: look at it. See [When there is no answer](no-answer.md). |
| `no contract covers action "<action>" in namespace "<namespace>"` | There is no contract for the action. Without a contract every request is refused. | Create a contract for the action in that namespace, or correct `spec.action` of the request. |
| `<count> contracts cover action "<action>" in namespace "<namespace>", want exactly one` | The coordinator does not choose between contracts. | Delete contracts until one is left for the action. |
| `contract has a negative clock skew` | `clockSkew` in the contract is below zero. | Set `clockSkew` to zero or more. |
| `contract has no grant TTL` | `grantTTL` in the contract is zero or below. | Set `grantTTL` to the time the action needs, with a margin. |
| `contract covers action "<action>", not "<action>"` | The decision core got a contract for another action. The coordinator never does this. | Only if you call the decision core from your own code: give it the contract of the action. |
| `request has no ID or no target name` | The decision core got an incomplete request. The API server does not accept one. | Only if you call the decision core from your own code: set both. |

## A grant that stays open

A grant stays open after its deadline, or after a failure report, until
something shows that the action is over. It keeps its place in the budget,
so new requests for the same group are refused. This is on purpose: the
coordinator does not know the state of the target.

To close it, do one of these:

1. If the action is in fact done, let the requester report `Succeeded` in
   `spec.result`.
2. If the contract has `releaseWhen`, make sure the reporter of those
   conditions runs. The coordinator closes the grant when each condition
   is `True`, fresh, and measured after the grant went wrong.
3. Look at the target yourself. When it is safe, delete the request.

A release is in `status.released` of the request:

| Message | What it means | What to do |
|---|---|---|
| `<condition>=<status> for "<target>" from "<source>", measured at <time>` | A line of `status.released.evidence`: one observation that closed the grant. | Nothing. The `ReleaseRecord` named in `status.released.record` holds the full input. |

These say why the coordinator does not close a grant. They are in the
output of `allostat replay` when a `ReleaseRecord` does not hold:

| Message | What it means | What to do |
|---|---|---|
| `contract has no release conditions` | The contract has no `releaseWhen`. | Close the grant by hand, or add `releaseWhen` to the contract. |
| `grant is not overdue and not reported as failed` | The action is still in its time. | Wait. |
| `the time of the failure report is not known` | The coordinator has not seen the failure report yet. | Wait for its next look. |
| `observation of "<condition>" for "<target>" is not newer than <time>, when the grant went wrong` | The report is from before the deadline or the failure. It says nothing about the target now. | Wait for a newer report, and check that the reporter runs. |

The messages about observations from the first table can be there too.

## The API server rejects a write

| Message | What it means | What to do |
|---|---|---|
| `a result can be reported only for a granted request` | `spec.result` was written to a request that has no grant. | Read `status.decision` first. Report a result only after `Granted`. |
| `the result cannot change after it is reported` | A second report tried to change or remove the first one. | Nothing. The first report counts. |
| `action, requester and target cannot change after the request is created` | A request is one question and cannot be edited. | Create a new request. |
| `expiresAt cannot change after the request is created` | The time to stop waiting is fixed when the request is created. | Create a new request with the new time. |
| `a decision record cannot change` | Something tried to edit a record. | Nothing. Find out who tried: only the coordinator writes records. |
| `a release record cannot change` | The same, for the record of a release. | The same. |

## `allostat replay`

`allostat replay` prints `DIFFERENT` and one of these when a record does
not give its recorded answer. It means one of two things: the record was
changed, or this version of the decision core decides differently from the
one that made the decision. Replay with the version that made the decision
to tell them apart.

| Message | What it means |
|---|---|
| `the record says <decision>, the replay says <decision> (reasons: <reasons>)` | The answer is different. |
| `the record has the reasons <reasons>, the replay has <reasons>` | Both refuse, for different reasons. |
| `the record has the deadline <time>, the replay has <time>` | Both grant, with different deadlines. |
| `the record is a refusal with a deadline` | A refusal has no deadline. The record is damaged. |
| `a record without a contract must be a refusal with a reason, it is "<decision>" with <count> reasons` | A record without a contract cannot be a grant. The record is damaged. |
| `the record says released, the replay keeps the grant open (reasons: <reasons>)` | The evidence in the record does not close the grant. |
| `the record has the evidence <evidence>, the replay has <evidence>` | The grant is released, by other observations than the record names. |
| `the record is about the request "<request>", its grant belongs to "<request>"` | The record holds the grant of another request. The record is damaged. |

`no replay` with `refused without a contract` is not an error. The
coordinator refused before it asked the decision core, so there is nothing
to play again:

| Message | What it means |
|---|---|
| `the record has no contract: the coordinator refused before the decision core was asked` | The same, when you call `Replay` from your own code. |

These end the command before it plays anything:

| Message | What to do |
|---|---|
| `read the records: <error>` | The input is not YAML or JSON. Give it the output of `kubectl get decisionrecords,releaserecords -o yaml`. |
| `read the record: <error>` | An object has a field that a record does not have, or a value of the wrong type. Check that the version of `allostat` is the version of the resource types in the cluster. |
| `no record found in the input` | The input is empty or an empty list. Check the namespace in the `kubectl` command. |
| `the input has an object of kind "<kind>": want DecisionRecord or ReleaseRecord` | Give it only records. |
| `<count> of <count> records do not give their recorded answer` | Read the `DIFFERENT` lines above it. |
