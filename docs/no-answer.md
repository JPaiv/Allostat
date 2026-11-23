# When there is no answer

The coordinator can be down, slow, or cut off from the API server. This
page says what happens then and what a requester must do.

## The rule

**No answer means no new action.** A requester acts only after it has read
`status.decision: Granted` on its own request. An empty status is not a
grant, and an error when it reads the request is not a grant.

**An action that started goes on.** The coordinator gives permission to
start. It does not take part in the action. A requester never stops a
half-done action because the coordinator is away; it follows its own
procedure to the end and reports the result.

## What a requester does

1. Create the request. Set `spec.expiresAt` to the time you stop waiting.
2. Read the request until it has a decision, or until that time.
3. `Refused`: do not act. The reasons are in the status. Ask again later
   with a new request; a refusal is final for this one.
4. `Granted`: act, but start only if there is enough time left before
   `status.deadline`. Do not start after the deadline. Then report the
   result in `spec.result`.
5. No decision when you stop waiting: do not act. If you set `expiresAt`,
   you can leave the request; the coordinator refuses it when it comes
   back. If you did not, delete the request.

If the report in step 4 cannot be written, finish the action and try the
report again. A report is accepted at any time, also after the deadline.
Until it is written the grant stays open and keeps its place in the
budgets.

## Why `expiresAt`

Without it, a request that waits while the coordinator is down gets its
answer when the coordinator comes back, maybe hours later. If that answer
is a grant, nobody uses it, and it blocks its group until the deadline
passes and somebody closes it. With `expiresAt` the late answer is a
refusal. It cannot be changed after the request is created.

## What the coordinator does when it comes back

- It keeps no state of its own. It reads the contracts, observations,
  requests and the ledger from the API server at the moment of each
  decision, not from a cache.
- A request that waited is decided with the evidence of that moment. What
  was true when the request was created does not count. Evidence that got
  old while the coordinator was away is refused like any old evidence.
- A grant it gave before is still open, and still counts in the budgets,
  until its result is reported or the request is deleted.
- A grant that became overdue while it was away is still overdue. It can
  be closed by evidence only if the contract has `releaseWhen`, and only
  by evidence from after the deadline.
- If it stopped between the two writes of a grant, it finds the grant in
  the ledger and finishes it.

## What this depends on

- **The API server.** If it cannot be reached, the coordinator decides
  nothing and requesters read nothing. Nobody starts a new action.
- **Clocks.** The age of an observation is the coordinator's time minus
  the reporter's time. A reporter clock that is ahead is refused, or
  allowed up to `clockSkew` in the contract. A coordinator clock that is
  behind makes old observations look newer than they are, by the size of
  the error. Keep the clocks of the coordinator and the reporters in sync,
  and keep `maxAge` well above the error you expect.
- **Requesters that follow the rule.** The coordinator cannot stop a
  requester that acts without a grant or starts after the deadline.

## Where this is tested

`internal/controller/actionrequest_test.go`, from
`TestWaitingRequestIsDecidedWithCurrentEvidence` on, and
`TestExpiredRequestIsRefused` in `admission/admission_test.go`.
