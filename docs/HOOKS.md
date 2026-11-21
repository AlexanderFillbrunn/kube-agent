Hooks
=====

Status
--------------------
A proposal, not a specification. It names the points a hook is called from
and what a hook may decide; the code follows [PROJECTION.md](PROJECTION.md)
and [ENCODING.md](ENCODING.md) for the records the agent writes today, and no
code follows this document yet. The interaction with [COMPACTION.md](COMPACTION.md)
is noted where it matters.

Summary
--------------------
Hooks let outside code influence a run without being given the log. A hook
is attached to an agent at build time, as an `AgentOption` in the shape of
`WithTasks`, because hooks differ between agents and not between runs of one
agent; everything that differs per run — the arc, the folded history —
reaches the hook as a context argument. RunOptions carries none of it.

The principle that follows the log's shape: a hook may freely change
anything not yet persisted, may refuse work before it starts, and after
that may only append. Tool arguments are in the log before the tool runs
(`recordResponse` writes the call with the message), so no hook rewrites
arguments; it refuses them instead.

The scope of this document is the pre-persist hooks: rulings a hook makes
about work that has not reached the log yet. Post-hoc correction by event —
an amendment that changes how a settled call is rendered, never what it said
— is a separate proposal and only sketched here.

The points
--------------------
Three pre-persist points, one interface each. A hook implements whichever it
wants; the agent type-asserts at each point, so an agent with no hooks added
costs nothing.

`BeforeToolCall` — before a pending call is dispatched, for every kind of
call a run makes: a tool, a task start, a cancel, an input. The hook rules
and the agent writes what the ruling says:

- `Allow` — the call proceeds to its dispatch.
- `Hold` — a human must approve it first. The agent raises an interrupt (see
  Holds below).
- `Block` — the call is refused without asking anyone. The agent settles it
  as an error outcome naming the reason. Nothing runs, nothing is
  dispatched, and the run continues: a block is a settled call, not a failed
  run.

The hook is consulted once per call. A call that has an interrupt — held
before, decided or not — is not put to the hook again; the human's answer
governs from there. That one rule is what keeps a hold from re-asking after
an approval.

`AfterToolInvoke` — after the tool has run, before the result is recorded.
The hook may replace the outcome (a redaction, a wrapped error) or leave it
standing; the agent settles the call with whatever the hook left. What the
hook returns is the log's first and only record of this outcome — the tool's
own words are gone if replaced, which is why redaction belongs here and not
in an amendment.

`AfterLLMResponse` — after the model replied, before the reply is recorded.
The hook may edit the reply — content, calls, both — because nothing of the
reply is in the log yet; `recordResponse` writes what the hook left. A hook
error here fails the run and is recorded through `recordFailure`, which is
append-only by construction.

Observer points — after the run's own writes land — are not proposed yet.
Notification of every event already hangs off the persistor as the EventSink
([EVENT-SINK.md](EVENT-SINK.md)), and that is where they belong.

What a hook sees
--------------------
A hook sees the arc being worked, named as an `ArcRef`, plain types for the
call and its arguments, and its own view structs. It does not see
`proto/pkg/events`: no payload types, no event constants, no persistor. The
translation from a ruling to a record is the agent's alone, in
`persistAndFold`, so the guarantees written in the code today apply to
hook-driven writes exactly as to the agent's own: a batch written together
or not at all, the session named by the history, the fold validating what
lands.

Holds and interrupts
--------------------
A hold is the ruling, an interrupt is the record, and [INTERRUPTS.md](INTERRUPTS.md)
names the record. `Hold` translates to an `INTERRUPT` event naming the call
and carrying the ruling's reason as the ask. The rest is already built and
needs no new code: the fold puts the arc `INTERRUPTED`, `deriveArcState`
ends the run, and nothing runs until every ask is answered.

Asks are raised together, one pass over an arc's pending calls before any of
them dispatches, and written in one batch — a run's last statement before it
ends on a hold is atomic.

Answering is not the hook's or the agent's business: an
`INTERRUPT_RESPONSE` in the log, written by whatever interface a human
answers from, folds and releases the arc exactly as [RE-ENTRY.md](RE-ENTRY.md)
describes. A refusal settles the call with the human's words; an approval
leaves it pending, and the next run dispatches it without consulting the
hook again, per the once-per-call rule above. A hook, once implemented, is
thus the missing half [INTERRUPTS.md](INTERRUPTS.md) names: nothing in the agent
raises an interrupt today, and the hold is what will.

Out of scope here: the answer path itself (an interface that writes
`INTERRUPT_RESPONSE`) and the queue that owes the arc a run after a write
that changes what it has to do. Wiring the answer straight to
`ArcWorker.Work` is the interim shape, and a run that finds nothing to do
costs a replay, which is that design working.

What a hook may not do
--------------------
A hook cannot modify the log, only see it folded and add to it. Anything
already stated by a settled event stays said: no hook path rewrites an
earlier event's payload, an argument, or a recorded result. The amendment —
a later event that changes how a settled result is *rendered*, with the
original standing and a marker naming what changed — is the proposed shape
for post-hoc correction, and it lives in the fold and the projection
([COMPACTION.md](COMPACTION.md)) rather than in a hook. Two things it must
keep: the compactor summarizes the amended rendering, not the stored
original, or a redaction is faithfully carried forward forever; and
`Replay` of the full log reproduces the same folded history as the live run,
which is the acceptance test for this whole proposal.
