Cancellation
============

Status
--------------------
The record, the guard, the watcher and the reach into work elsewhere are all
in the code. What is missing is distance: `LocalCancels` is the only
`CancelWatcher` there is, so the watcher only crosses a goroutine, and
`TaskController` has no implementation that reaches another process.

Cancelling an arc is not one action. It is a record plus up to three reaches
into work that is already under way, each with a different mechanism, and
every one of them stops something the others cannot.

What a cancel has to stop
--------------------
    the record      CANCEL in the log. The authority, and the only durable part
    the guard       work the worker has not started yet
    the watcher     work the worker is doing right now
    the controller  work that is not in the worker at all

The guard and the watcher are not fast and slow versions of each other. The
guard refuses the next step and cannot touch a tool already running. The
watcher reaches into the running tool and guarantees nothing. Neither covers
the other's case.

The record
--------------------
Whoever decided the cancel writes it. The agent never writes one of its own,
because it does not know the actor.

The write goes straight to the log and takes no lease, so a cancel is never
blocked by whoever is working the arc. `applyCancel` settles the pending calls
that were never dispatched, cancels an unanswered interrupt, and closes the
arc. A call already handed to its tool keeps its phase, since the cancel does
not know whether it took effect.

Everything below is a way of making the world agree with that record sooner.
Each one may fail without making the record wrong.

The guard
--------------------
`GuardCancellation` wraps the worker's persistor and refuses writes that start
work, reporting `ErrCancelled`. It reads the cancel afresh on every guarded
write and remembers nothing between them. Only two events start work.

    TOOL_DISPATCHED      written immediately before a tool runs
    ASYNC_TASK_INVOKE    written immediately before a task is started

So the worker's stopping points are exactly those two writes. Everything else
it writes records something that already happened and goes through, which is
why a cancelled run still files the result of the tool it was in the middle
of.

This is the part that already crosses a process boundary. The worker learns
about the cancel by reading the log through `CancelSource`, not by being told,
so it needs no connection to whoever cancelled. It is also the only mechanism
here that cannot be lost: a run whose every notification went missing still
stops before it starts anything else.

What it cannot do is bounded by its own granularity. Between two dispatches
the worker may be streaming a reply or sitting inside a tool, and neither has
a bound. A four minute rollout runs for four more minutes.

One thing the guard does not do is reach the run's own fold. A `CANCEL`
written by another process is not folded into a history a run is already
holding, because `persistAndFold` only folds what that run writes. Within one
run, `Arc.Status()` changes only through that run's own events. The guard is
the whole of the cross-process path.

The watcher
--------------------
The watcher cancels the run's context, which is what reaches work already
under way: a tool handler selecting on `ctx.Done()`, an LLM stream mid-token.
Without it those run to completion and their cost is spent whatever the log
says.

It is best effort. Pub/sub is at most once, a subscriber that reconnects
misses what it was not there for, and none of that is allowed to matter: the
guard still stops the run at its next step. So a lost notification costs time
and money, never correctness.

Producer and consumer are separate, as with the queue. The canceller
announces, the worker watches, and nothing holds both. `LocalCancels` is both
for one process, which is what `devchat` wires.

    // CancelNotifier tells watchers that an arc has been cancelled. It is
    // best effort: a failure here does not fail the cancel.
    type CancelNotifier interface {
        NotifyCancel(ctx context.Context, ref ArcRef) error
    }

    // CancelWatcher reports that an arc has been cancelled, as soon as it can.
    type CancelWatcher interface {
        // Watch returns a channel closed when the arc is cancelled. The
        // caller calls stop when it is done with the arc.
        Watch(ctx context.Context, ref ArcRef) (cancelled <-chan struct{}, stop func())
    }

A channel rather than a derived context, so the context plumbing lives once in
`ArcWorker` and not in every implementation.

It hooks in `Work`, which is the one place every way into an arc passes and
the only place that holds the run's context. `watchForCancel` takes the lease's
context and hands back the one the run is stopped through, plus the stop to
call when the run is over. A worker built without a watcher gets its context
back unchanged and stops only between steps.

The subscribe comes before the replay. A cancel written before the subscribe
is in what the replay reads, and one written after it is delivered. Subscribing
afterwards leaves a window where a cancel is published to nobody. That window
costs latency rather than correctness, because of the guard, which is the
reason the two mechanisms are worth keeping apart.

In one process, `Watch` is a map of `ArcRef` to channels and `NotifyCancel`
closes them. Across processes it is a subscription per arc, or one pattern
subscription fanned out inside the implementation where a worker holds many
arcs at once.

Reaching work elsewhere
--------------------
An async task is not in the worker. It may be a job, a queue consumer or a
service, and the only thing that can stop it is whatever knows how to talk to
it. That is `TaskController`, which already says exactly this much and assumes
nothing about where the task runs.

    Cancel(ctx context.Context, ref TaskRef, reason string) error

Nothing about the interface needs to change for a remote task. An
implementation that posts to a job API or publishes a message satisfies it as
well as one that closes a channel.

The worker calls it. An arc with a running task and no pending agent work has
no worker on it, because `deriveArcState` returns `ArcStateDone` while a task
is unfinished, so there is nobody holding the arc to notice the cancel. The
reach is scheduled instead: the cancel leaves the arc owed a run, a worker
takes it, and stopping the arc's unfinished tasks is what working a cancelled
arc means.

`Agent.Run` reaches `stopArcTasks` for an arc that carries a cancel, before the
step loop, and asks every unfinished task to stop under the reason the cancel
carried. That keeps task-reaching on the side that holds a registry and a
controller, and it survives a canceller that stops after writing the record.

It writes nothing and does not go through `recordFailure`: the arc is already
closed, and a controller nobody could reach is not a second thing to record. A
task that could not be reached fails the run, so whoever handed the arc over
comes back with it. Asking a task to stop twice is harmless, which is what
makes coming back safe.

A cancel in `devchat` writes the record, announces it, and leaves the arc
owed a run.

Asking a task to stop is not settling it. The cancel ends the waiting, and
what became of work already under way is the runner's to report. A task whose
runner never reports is settled by `SettleAbandonedTasks`.

A cancel is a write like any other
--------------------
A cancel takes agent work away from an arc and leaves work of its own: the
tasks it has to reach. So the invariant in [RE-ENTRY.md](RE-ENTRY.md) covers it
like anything else. Write the record, notify the watchers, enqueue the arc.

A cancelled arc with no unfinished tasks produces a run that replays and does
nothing, which is the same cheap no-op the queue already deals in.

A stale item for a cancelled arc is harmless for the same reason. The worker
replays, reads a status that is not pending, and returns.

What this does not fix
--------------------
A tool that ignores its context is not stopped by anything here. The context
is an offer, and a handler that never selects on it runs to the end. That is
the handler's bug, and the log records whatever it finally said.

A tool that is stopped has to say what it did. A mutating tool interrupted
after it issued its change reports that in its result rather than returning an
error, because an error is recorded as a definite failure and a model told the
restart failed is a model that restarts it again. `restartDeployment` in
`devchat/tools.go` is the worked example.

What a cancelled context reaches
--------------------
The watcher reaches the agent as a cancelled context, so which of its writes
honour that context is a rule rather than a judgement per call site. It is the
same rule the guard uses.

    starts work      TOOL_DISPATCHED, ASYNC_TASK_INVOKE. Honours the cancel,
                     because not starting the work is the point
    everything else  records something that already happened. Written on
                     context.WithoutCancel, because losing it leaves the log
                     saying less than the agent did

`persistAndFold` applies it to every event the agent writes, keyed on the same
`startsWork` the guard refuses on. A new event type is a record until somebody
adds it to that list, which is the safe way round. A batch is governed as one:
a single event that starts work makes the whole batch honour the cancel, the
same way one such event makes the guard refuse the whole batch.

What the rule holds is the losing case. A tool's result is the one write that
says what the tool did, so a store honouring a cancelled context would turn a
settled call into one nobody ever heard back from.

Events that say one thing together are kept together by the write rather than
by the context, which covers a store error and a crash as well as a cancel.
`EventPersistor.Persist` takes events in a batch and writes all of them or
none. `recordResponse` uses it for a reply and the calls it requested, minting
the message ID up front so the calls can name it before anything is written,
and `startTask` for its dispatch and its invocation. A log holding only the
message would read as an answer the model never gave, which the next run closes
the arc on.

The rule does not reach writes outside the agent. `TaskReports.Report` writes
on the context of whoever is reporting, which has a different lifetime, and
`SettleAbandonedTasks` runs at startup where nothing is cancelling.
