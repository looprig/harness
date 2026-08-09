# Collaboration Broker Shutdown Design

## Goal

Ensure cancellation of a session collaboration broker revokes authority and
stops broker-owned lifecycle goroutines without waiting forever on a foreign
controller that ignores context cancellation.

## Root cause

`newCollabBrokerAt` starts a context watcher that calls
`Close(context.Background())`. `Close` waits for `handlersDone` after closing
the endpoint and canceling admitted calls. A foreign `DelegateController`
that never returns therefore leaves the watcher blocked forever, even when a
caller invoked `Close` with a deadline and received `context.DeadlineExceeded`.

## Chosen ownership model

Split broker shutdown into two phases:

1. `stop` is idempotent and nonblocking. It marks the broker closed, snapshots
   listener/connections/principals under the broker lock, cancels the broker
   context, closes the listener and connections, revokes every principal (which
   cancels cooperative calls), and removes the endpoint and owner directory.
   This phase establishes the security/lifecycle boundary first and never waits
   for a handler.
2. `Close(ctx)` calls `stop`, then joins `acceptDone` and the current
   `handlersDone` channel while `ctx` remains live. The caller owns this bounded
   wait and receives the context error when an external handler is
   noncooperative.

The session-context watcher calls only `stop`, records completion on a
`watchDone` channel used by lifecycle tests, and returns. It never calls a
blocking `Close` and never owns a handler join. A noncooperative external
handler may remain until its controller returns; no broker watcher or cleanup
goroutine waits for it.

## Error and race behavior

`closeOnce` protects the stop phase when the watcher and an explicit `Close`
race. The endpoint is removed and capability principals are revoked before a
bounded `Close` wait can return. Existing handler cleanup continues to use
the existing `handlerCount`/`handlersDone` transition; channels are not sent
to or closed by the stop phase, so repeated close calls cannot double-close a
completion channel.

## Verification

Add a regression test with a controller that remains blocked indefinitely
during the shutdown assertion. The test will prove, through `watchDone` and
`acceptDone`, that broker-owned watcher/accept goroutines finish while the
caller receives a bounded close result; it will also create repeated canceled
broker sessions and assert every lifecycle channel closes and no endpoint
remains. The test cleanup releases the intentionally held controller only
after those assertions so the test itself leaves no goroutine behind.

