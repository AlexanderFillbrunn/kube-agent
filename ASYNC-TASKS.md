Async tasks
===========

Status
--------------------
A proposal, not a specification. [PROJECTION.md](PROJECTION.md) and
[ENCODING.md](ENCODING.md) describe what the code does; this describes
something it does not do yet, and nothing here is binding until it is folded
into those two.

It concerns one decision: whether an async task is something an arc owns, or
something that stands beside an arc. Today it is the first. This is the case
for the second, with what it costs.

The shape today
--------------------
A task is invoked inside an arc and belongs to it. The arc stays open for as
long as the task runs, because the agent records `KX` only once nothing under
the arc is still running, and a cancel on the arc is what stops the task. Every
disclosure that work is in flight is derived from the arc's status, and a task
that stops to ask for input is disclosed as a note inside the arc that started
it.

One consequence is awkward enough to be the reason this document exists. A task
asks for input, the agent puts the question to the user, and the user answers.
That answer is a new user message, so it opens an arc of its own. The
projection's cutoff gives an arc everything up to its own opening plus
everything correlated to it, so the answer is never visible to the arc that
asked: it arrives as a note about a sentence, while the sentence itself sits
in an arc the model is not reading.

    U1, R1.1, A1.1, AQ1.1.1, R1.2, U2, R2.1, T2.1.1, AP1.1.2, K2x, AR1.1

`R1.2` asks the question. `U2` answers it. `U2` is not in arc 1's projection at
any cutoff, so arc 1 learns what the user said only from `n{task auditor was
given input: …}`.

The proposal
--------------------
An arc closes when the agent has nothing synchronous left to do. A task that is
still running does not hold it open. When the task next speaks — it asks for
input, or it finishes — that event opens a new arc, the way a user message
does.

    U1, R1.1, A1.1, K1x, AQ1.1.1, X2, R2.1, K2x, U3, R3.1, T3.1.1, AP1.1.2,
    K3x, AR1.1, X4, R4.1, K4x

`X2` and `X4` are arc-opening events the dispatcher writes in reaction to the
task. Arc 2 asks the user the task's question and closes. Arc 3 is the user's
answer, handed to the task. Arc 4 reports what came of it, and its cutoff
includes `U3`, so the model reads the user's own words rather than a note about
them.

An arc becomes one unit of agent work, and what triggers it is a kind rather
than always a user message.

What it buys
--------------------
*The cutoff starts working for the design instead of against it.* This is the
strongest argument and the one the others are a distant second to. The note
machinery for an ask and its answer stays for tasks belonging to other arcs,
but the arc that actually has to act on the answer no longer needs it.

*The arc state machine collapses back to one rule.* No hold-open for a running
task, no comparison of a task's settling sequence against the last response. A
response with no tool calls closes the arc. Deciding when the agent gets a turn
becomes the dispatcher's job, which is the more honest home for it: that is
scheduling, not conversation state.

*Arcs get short, so they stop overlapping.* Most of the difficulty in the
projection is an arc that stays open across everything that follows, and a slow
task is the main way that happens. A one-hour audit currently puts a status note
in front of the model on every turn for an hour.

Ownership
--------------------
A task-triggered arc has no human in it, and the agent cannot run without
knowing whose it is. This is a prerequisite for the design rather than
bookkeeping that can follow it, because the ask has to reach somebody: an arc
opened by `AQ` exists precisely to put a question to a person, and an arc that
cannot name that person has nothing to do.

Four things need the answer. Who may cancel the task. Which tenant's data the
run may read. Who the ask is put to. Who the work is attributed to when it is
rendered.

*The chain already exists.* `AsyncTask.CorrelationID` is the arc that invoked
the task, that arc carries `Actor`, and the arc-opening event names the task
it reacted to. So the owner is recoverable by walking arc → task → origin arc
→ actor, and the cutoff guarantees the walk is possible: a task-triggered arc
opens after everything that led to it, so all of it is in the history the
projection is built from.

*The chain should not be walked.* Ownership is read on the write path, to
decide whether a run may proceed and where a question goes, and a walk is the
wrong shape for that. It also lengthens without bound: a task started by an
agent turn in a task-triggered arc is one more link, and there is no limit on
how many times that composes.

*So the actor is copied forward.* The invocation records the actor it was
started by, and every arc opened in reaction to the task carries the same one.
Each link copies what it was given, so a chain of any length costs one field.
`events.Event` already carries `Actor`, and `UserMessagePayload` already
duplicates it into the payload, so there is precedent for both halves of this.

Two things this actor is not. It is not the correlation ID: correlation names
the arc a record belongs to, and a task-triggered arc's opening event belongs to
the new arc, not the old one. The link to the origin is causation, which the
task events already carry. And it is not a live grant of authority. It records
who started the work, which is a fact that never changes. Whether a task may
still act on that person's behalf an hour after they logged out is a policy
question the log does not answer and should not pretend to.

What it costs
--------------------
*Cancellation loses its handle.* Cancel is arc-scoped, and cancelling a task
today means cancelling the arc that owns it. If that arc closes as soon as the
task starts, a cancel against it is one that `TerminatedWork: false` says
stopped nothing, and the task runs on. Cancellation would have to address
tasks directly. That is a second stop-axis where the log has exactly one
today, and it reaches `guardedPersistor`, whose refusal to write
`ASYNC_TASK_INVOKE` into a cancelled arc stops meaning anything for a
task-triggered arc.

*"Has this been answered?" loses its anchor.* The open, dead and withdrawn
statuses work because an arc pairs one request with the agent's answer to it.
Under this proposal arc 1 closes having answered nothing, and the answer
arrives two arcs later in an arc the user never opened. Anything reporting
outstanding work walks the chain instead of reading a status.

*In-flight disclosure moves from arcs to tasks.* Every note that says work is
happening is derived from arc status today. With the arc closed, a running task
sits under an arc that reads as finished, which is the exact situation the
hold-open rule was added to prevent. The note has to come from
`AsyncTask.Status` instead. The gap is widest between the ask and the answer:
no arc is open at all then, and that the system is waiting on a person exists
only as `WAITING` on the task.

This is the whole trade in one line. What it buys is paid for in the
projection; what it costs is paid in the control plane. It is worth it only if
the task becomes a fully first-class object — cancellable, queryable, and
disclosed from its own status — rather than a hybrid where the arc still owns
half of it.

What would have to change
--------------------
*A distinct arc-opening event.* `AQ` and `AR` cannot open arcs themselves. They
are correlated to the arc that owns the task, which is what lets that arc's
slice be read back, and one correlation field cannot both belong to the old arc
and open a new one. So the dispatcher writes an opening event of its own,
correlated to the new arc, with causation naming the task event it reacted to.

*Arc opening becomes a kind.* `applyUserMessage` is what mints an arc in the
fold, and `Store.GetEventsForArc` finds an arc by scanning for a `USER_MESSAGE`
with that correlation ID. Both need "the event that opens an arc" as a concept.
The cutoff rule itself is unchanged and is what makes the proposal work.

*The actor on the invocation.* `AsyncTaskInvokePayload` carries a name and
arguments and nothing about who asked for it.

*PROJECTION.md.* "A running task holds its arc open" goes. Fixed rule 4, that a
task is always two notes, needs reworking, since one of those notes becomes an
arc in its own right for the arc that reacts to it and stays a note for
everyone else.

*Rendering.* `RenderChat` draws an arc as a user bubble followed by replies.

Open questions
--------------------
*What a task-triggered arc quotes.* `Arc.Content` is the user's words, and
`foreign_arc: note` quotes it to say what the other request was about. A
task-triggered arc has no such words. The ask's message is the obvious
candidate for an arc opened by `AQ`; an arc opened by `AR` has nothing but the
task's name.

*Whether a notification can open an arc.* Only the ask and the result do here.
A task that reports progress should not get the model a turn, because whether
those notifications reach the model at all is a projection setting, and work
the model may never be told about must not decide when it speaks. That argues
the rule belongs in the dispatcher and not in the task.

*Two arcs at once.* An ask and a result can arrive close together, and each
opens an arc. Two runs answering the same person in parallel is a problem
whether or not they share an arc; it is only more visible here.

*Cost.* An arc per task event is a full transcript per task event. It is
bounded — two arcs per task, not one per notification — but it is not free.
