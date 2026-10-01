Event sink
==========

Status
--------------------
Built. `EventSink`, `PublishEvents`, `TokenSink`, `ArcRef` and `RunOptions` are
in `pkg/agent`, and `cmd/devchat` is wired through them.

The agent used to notify an interface directly: `AgentConfig.Observer` took
every event it folded and `AgentConfig.Tokens` took the model's text as it
streamed. Both were set at `NewAgent` and both ran inside the write path. The
agent is meant to be a worker process with no interface attached, so neither
belonged there.

This document says where notification goes instead, and what the two streams
are allowed to promise. It also takes the session out of construction, because
the token stream cannot be addressed without it and the worker tier cannot be
pooled with it.

What was wrong with the observer
--------------------
*It fails runs.* `persistAndFold` returns the error from `Observer.Consume`,
`steps` passes it to `recordFailure`, and the arc ends with a fatal `ERROR`
event. A watcher that disconnects can close an arc. Nothing a screen does
should be able to write to the log.

*It blocks the loop.* The observer is called between the fold and the next
step. In one process that is a channel send. Across processes it is a network
round trip in the middle of the agent loop.

*It cannot tell two arcs apart.* One agent serves every arc it is handed, and
one observer serves every arc the agent serves. The token sink is worse: it is
`Sink[string]`, so two arcs streaming at once interleave with nothing to
separate them.

*It does not see the whole log.* It only saw what `persistAndFold` wrote. Four
writers went round it:

    session.turn              USER_MESSAGE
    session.cancel            CANCEL
    TaskReports.Report        the task's notification, request or result
    SettleAbandonedTasks      ASYNC_TASK_RESULT for a dead runner's tasks

So an interface following the observer missed the user's own message, every
cancel, and everything a task said. That is the argument for the placement
below: the thing every writer has in common is not the agent, it is the
persistor.

Two streams, not one
--------------------
    events    durable, in the log, replayable, ordered by Seq
    tokens    transient, never in the log, lossy, no replay

They are separate because one of them is a record and the other is a preview of
a record. An `ASSISTANT_MESSAGE` event carries the whole reply. The tokens
leading up to it carry the same text earlier and less reliably. A subscriber
that missed every token still ends up with the reply; a subscriber that missed
the event is simply wrong.

That asymmetry is what lets the token stream be cheap. It needs no durability,
no ordering guarantee across messages and no replay, because the event behind
it has all three.

The event sink
--------------------
Notification hangs off `EventPersistor`, not off the agent.

    // EventSink is told about events once they are written. It is the
    // notification half of a write and never part of it.
    //
    // Consume returns nothing, so a sink cannot fail a write. It must also
    // return promptly: an implementation that talks to something remote
    // buffers internally rather than blocking its caller.
    type EventSink interface {
        Consume(ctx context.Context, evs ...*events.Event)
    }

    // PublishEvents hands every batch that was written to a sink.
    func PublishEvents(persistor EventPersistor, sink EventSink) EventPersistor

No error return is the point rather than an oversight. A sink that could return
one would eventually have a caller that checked it, and that caller would be
back to failing a run over a screen. An implementation that cares whether its
publishes land says so through its own logging and metrics.

A batch is published whole, after it is stored, in the order it was written.
Whole, because `Persist` writes all of a batch or none, and a subscriber that
saw an `ASSISTANT_MESSAGE` without the `TOOL_CALL` events beside it would read
an answer the model never gave — the same reason the batch exists. After,
because an unwritten event is not news. In the order written, because the
decorator calls `Consume` synchronously; a decorator that spawned a goroutine
per batch would reorder them.

*Decoration order.* The publisher goes outermost:

    PublishEvents(MintIdentity(GuardCancellation(store, store)), sink)

Outermost, so a write the guard refuses publishes nothing. The context is
whichever one the write used, which means a record written under
`context.WithoutCancel` is published too — see CANCELLATION.md on which writes
honour a cancel.

*What a subscriber may assume.* At most once, in write order within a batch,
with no ordering promised between concurrent writers and no redelivery. Every
event carries `SessionID`, `CorrelationID` and `Seq`, so a subscriber routes
and orders without the publisher knowing anything about it.

The reconciliation rule follows from that: **a gap in `Seq` is the subscriber's
cue to re-read the log, not the publisher's cue to retry.** `EventSource` is
how it catches up. This is the same stance `CancelWatcher` takes in
CANCELLATION.md — a lost notification costs latency, never correctness — and it
is why the sink is allowed to drop on a full buffer.

*Who constructs it.* One sink per process, handed to every site that builds a
persistor. The four writers above do not live with the agent and are not moved
there; they are built separately and given a persistor wrapped with the same
sink. Intra-process that sink is a hub over channels. Across processes it is a
publisher to Redis or whatever else. One line differs, and the agent has no
field for either.

`AgentConfig.Observer` was deleted rather than renamed. The agent has no notion
of notification at all, and `AgentConfig` is left holding the system prompt.

In `cmd/devchat` this removed the two places the screen was written to by hand.
The user's message is no longer put on the screen when enter is pressed, since
the write publishes it straight back, and a task no longer tells the interface
what it reported, since the report is published by the persistor that wrote it.
What a task still reports directly is a report that could not be written at
all, which is the one case with nothing in the log to publish.

The token sink
--------------------
Tokens are passed per run and are optional. A background worker with nobody
watching passes nothing.

Three layers, each knowing only what it can know:

    the provider    Sink[string]      text, no identity
    the agent       Sink[Token]       Token{MessageID, Content}
    the worker      TokenSink         StreamToken{SessionID, ArcID,
                                      MessageID, Content}

    // Token is one piece of a reply as the agent sees it.
    type Token struct {
        MessageID string
        Content   string
    }

    // StreamToken is the same piece addressed well enough to route from
    // another process.
    type StreamToken struct {
        SessionID string
        ArcID     string
        MessageID string
        Content   string
    }

    // TokenSink takes a reply as it is written. Like EventSink it cannot
    // fail its caller and must not block.
    type TokenSink interface {
        Token(ctx context.Context, tok StreamToken)
    }

`agent.Token` was already in `llm.go` with exactly these two fields and used
nowhere. This is what it was for.

The agent takes its `Sink[Token]` in `RunOptions`, alongside the persistor and
the history it is already handed per run. `ArcWorker` holds the `TokenSink`,
and builds the per-run adapter by closing over the session and arc it was
given. The provider's signature does not change: the agent wraps the message ID
onto a `Sink[string]` before handing it down, so no LLM implementation learns
about arcs.

*The message ID is minted earlier.* `recordResponse` used to mint it after the
stream had finished, which is too late to label the tokens that produced it. It
moved up into `llmStep`, which binds the token sink to it and then hands the
same ID to `recordResponse`.

*A streamed message is provisional.* A run that streams tokens and then dies
before writing the `ASSISTANT_MESSAGE` leaves a subscriber holding text for a
message the log does not have. So: a subscriber replaces streamed text with the
recorded content when the event arrives, and discards it when the arc ends
without one. `cmd/devchat`'s transcript does the first part positionally
(`transcript.token`, then the `ASSISTANT_MESSAGE` case overwrites it), which is
a guess about which block was streaming. The message ID is now on the token, so
that guess can be replaced whenever the harness shows more than one arc.

*A nil sink does not change what the agent does.* It still calls
`StreamResponse` and discards. Switching to the non-streaming call when nobody
is watching would make a run's behaviour — its timeouts, its provider request,
its failure modes — depend on whether a screen happened to be attached, and a
bug reproducible only with the UI open is not worth the saved allocation.

Session is a per-run value
--------------------
`ArcWorker` used to take `sessionID` at construction and stamp it onto every
history it replayed; `StampIdentity` took one and stamped it onto every event
written. Both were fixed for the life of the object. That is a per-session
worker, and the tier described in RE-ENTRY.md claims arcs off a shared queue
without knowing whose they are.

    // ArcRef names one arc well enough to find it from any process.
    type ArcRef struct {
        SessionID string
        ArcID     string
    }

*The queue carries `ArcRef`.* `Enqueue(ctx, ref)`, and `Claim.Ref() ArcRef` in
place of `Claim.ArcID()`. The worker loop in RE-ENTRY.md is otherwise unchanged.
The queue itself is still a proposal.

*`Work` takes it.* `Work(ctx, ref ArcRef) (bool, error)`, and `arcHistory`
replays under `ref.SessionID`. The events carry their own `SessionID`, so the
replay checks them against the ref and fails loudly on a mismatch — that is
what catches a queue item pointing at the wrong session.

*The lease keys on both.* Arc IDs are random and collision is not the worry; a
lease that cannot name the session cannot be sharded or audited by it. The
signature was fixed in the same pass, since it was being touched anyway:

    Acquire(ctx context.Context, ref ArcRef) (release func(), ok bool, err error)

`ok` false means somebody else holds the arc, which is not a failure. An error
means the lease could not be reached, and the caller learns nothing about who
holds it. A caller that read an unreachable lock service as "nobody holds it"
would run the arc twice, which is why the two are now separate answers.

What is still missing is a TTL and a fencing token. A holder that dies holds
the arc until whatever granted it decides otherwise, and a holder whose lease
expired goes on writing, because `EventPersistor` has no way to say what the
writer expected the log to look like. Both need a store that can refuse a
write, so they are a change to the persistor rather than to the lease.

*`EventSource` and `CancelSource` take it.* `GetEventsForArc(ctx, ref)` and
`Cancelled(ctx, ref)`. A store that partitions by session cannot serve either
query from an arc ID alone.

*The guard remembers nothing.* `guardedPersistor.seen` was an unguarded map
written from every run that shared the persistor, which is a live race, and it
grew for the life of the process. Rather than lock it, it is gone: the guard
refuses one write and the run ends there, so the cache saved about one read per
run. A guarded write now reads the cancel afresh.

*`TaskRef` gains the session.* It has `ArcID`, `ID` and `Name`, so a task
reporting in from another process cannot be routed: `TaskReports.Report` calls
`arcHistory(ctx, ref.ArcID)` and has no session to replay under. Embedding
`ArcRef` keeps the two in step.

*Nothing invents a session ID.* `StampIdentity` lost its `sessionID` argument
and became `MintIdentity`, filling in only the ID and timestamp. Every writer
sets `SessionID` on the event itself: the agent reads it from the history it
was given, `cmd/devchat`'s session from its own. One rule, no helper, and an
event that arrives without a session is refused rather than filed somewhere
plausible.

What this touched
--------------------
    pkg/agent/agent.go          Observer and Tokens off AgentConfig; Run takes
                                RunOptions; message ID minted in llmStep; the
                                agent stamps the session on what it writes
    pkg/agent/persistence.go    EventSink, PublishEvents, MintIdentity,
                                EventSource takes ArcRef
    pkg/agent/arcwork.go        ArcRef, TokenSink, StreamToken; Work takes a
                                ref; Acquire takes a context and can fail
    pkg/agent/cancellation.go   CancelSource takes ArcRef; the cache is gone
    pkg/agent/tasks.go          TaskRef embeds ArcRef
    pkg/agent/reports.go        Report routes by ArcRef
    internal/filestore          both queries take ArcRef and scope by session
    cmd/devchat                 one sink per stream, wired into the persistor
                                and the worker rather than into the agent

    docs/RE-ENTRY.md            ArcQueue, ArcClaims and Claim carry ArcRef
    docs/CANCELLATION.md        CancelNotifier and CancelWatcher carry ArcRef

What this does not fix
--------------------
A publish is still best effort, and that is deliberate. An interface that must
not miss anything reads the log.

The agent still folds only what it writes, so a `CANCEL` from another process
reaches a run through `GuardCancellation` and nothing else. See CANCELLATION.md.

An arc with a running task and no pending agent work has no worker on it, so
nothing publishes on its behalf between the task's reports. The subscriber sees
exactly what the task files, which is correct but quiet.
