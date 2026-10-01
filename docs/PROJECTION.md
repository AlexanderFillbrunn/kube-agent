Projection
==========

Purpose
--------------------
A session is stored as an event log. A model provider accepts a flat ordered
list of messages. Projection is the step that turns the first into the second.

The log is not a transcript with extra fields. It records interrupts, cancels,
errors, guardrail blocks and async tasks, none of which a provider has a
message shape for. It records overlapping work, which a flat list has no way to
express. Projection decides, for every record in the log, whether the model is
told about it and in what form.

Pipeline
--------------------
Three representations, two transitions.

    event log  ──fold──▶  History  ──project──▶  ModelTranscript  ──encode──▶
    provider messages

    event log           what happened, append-only, one session
    History             the log folded into arcs. `pkg/history/history.go`
    ModelTranscript     provider-agnostic messages. `pkg/history/projection.go`
    provider messages   one vendor's wire format

The fold is mechanical. It nests each record under the arc named by its
correlation ID and keeps everything.

Projection is where the decisions are. It is a pure function of three inputs.

    project(history, arc, modes) → ModelTranscript

Encoding is per provider: message shapes, ID formats, and the alternation and
pairing rules a given API enforces. Encoding does not decide what the model is
told. Anything it would have to invent is a projection decision that was left
open.

This document specifies projection only. Encoding is specified in
[ENCODING.md](ENCODING.md).

Output
--------------------
`ModelTranscript` is a slice of `ModelMessage`, in send order. Four shapes,
defined in `pkg/history/projection.go`.

    ModelTextMessage        text with a role, USER or INSTRUCTION
    ModelAssistantMessage   the model's text, plus the calls it requested
    ModelToolMessage        one call's outcome, keyed by tool call ID
    ModelNoteMessage        synthesized status, carrying no role
    ModelToolCall           one call, carried by the assistant message

`ModelAssistantMessage` carries the model's reasoning alongside its text and
its calls, as the opaque bytes the provider sent. See Reasoning.

`ModelToolMessage.Outcome` states how the call ended as data, not only as the
text the model reads. An encoder or a reader of the transcript can tell a
policy block from a human refusal from a cancel without parsing a sentence.

The set is closed by an unexported method, so a projection cannot emit a shape
no provider understands.

Input
--------------------
A projection is made for one arc. The history it receives holds every event up
to that arc's own user message, plus every later event correlated to it. The
read-model applies that cutoff when it builds the history.

Four consequences:

1. Other arcs are frozen at the cutoff. Work in flight when the arc opened
   stays in flight for the whole projection. `:pending` states a fact about the
   snapshot and does not guess at timing.

2. Nothing arrives out of causal order. The projection cannot observe an event
   past the cutoff, so placement is the only ordering question. See Ordering.

3. The same record may project differently across turns. A call shown with a
   `:pending` stand-in in one arc's transcript may carry its real result in the
   next arc's. A transcript is a view from one point in the log and does not
   accumulate.

4. The query is the cutoff. The projection walks the arcs the history holds,
   once, and emits what the modes admit.

The slice has gaps. Another arc's events fall between this arc's whenever the
two overlapped, and the query keeps only one of them, so the fold requires
events in increasing sequence order.

Event shorthand
--------------------
IDs are parent-indexed. The prefix names the kind, the dotted suffix names the
path from the arc down to the record. `U2` is the second user message.
`T2.1.3` is the third tool call of its first response.

    UX          User message X, opens arc X
    RX.Y        Agent response Y in arc X
    TX.Y.Z      Tool call Z of response X.Y
    TRX.Y.Z     Tool result for tool call X.Y.Z
    TEX.Y.Z.W   Tool event W emitted by tool call X.Y.Z
    GX.Y.Z      Guardrail block of tool call X.Y.Z, decided by a policy
    IX.Y.Z      Interrupt on tool call X.Y.Z, put to a human
    IRX.Y.Z     Interrupt response, the human's decision on IX.Y.Z
    AX.Y        Async task Y invoked in arc X
    ARX.Y       Async task result for task X.Y
    AEX.Y.Z     Async task notification, entry Z in task X.Y's transcript
    AQX.Y.Z     Async task input requested, entry Z in task X.Y's transcript
    APX.Y.Z     Async task input provided, entry Z in task X.Y's transcript
    EX.Y        Error Y in arc X, non-fatal
    FEX.Y       Fatal error Y in arc X, closes the arc
    CX.Y        Cancellation Y of arc X
    KX          Close of arc X, recorded by the agent

AE, AQ and AP share one counter per task, because they share one transcript.
`A1.1, AE1.1.1, AQ1.1.2, AP1.1.3, AR1.1` reads in the order it happened.

TR and G are mutually exclusive for a given call. Both settle it and the first
to arrive wins.

Modifiers
--------------------
    IRX.Y.Z+     Interrupt approved
    IRX.Y.Z-     Interrupt refused
    E1.2@T1.1.1  Causation, where an error or block is about one record
    U2!          The arc being projected
    K1x          The event that closed its arc

Only `KX`, `FEX.Y` and `CX.Y` can close an arc, and each names its arc in its
own ID. An arc is open at a cutoff when no `x` event for it appears.

`KX` is the agent's own record that it is finished with the arc. It is emitted
when a response requested no tool calls and no async task under the arc is
still running. A response is therefore not a closing event, however final it
reads: work it started can outlive it, and an arc whose task is still running
stays open until that task returns and the agent closes it.

`FEX.Y` always closes its arc. `CX.Y` closes only when the arc was not already
closed, which `Cancel.TerminatedWork` records. Where two closing records exist,
the earlier one closed the arc. The shorthand assumes one closing event per
arc.

Message shorthand
--------------------
Role is the letter. Provenance is the bracket.

    (…)     derived from the record named inside
    {…}     synthesized by the projector

    u(U1)       ModelTextMessage, USER. The user's own words.
    a(R1.1)     ModelAssistantMessage. The model's own words.
    t(T1.1.1)   A call, carried by the assistant message that requested it.
    r(T1.1.1)   ModelToolMessage. The outcome of that call.
    s{…}        ModelTextMessage, INSTRUCTION. A real system prompt.
    n{…}        A note. Synthesized status and anything else the model must be
                told that no participant said.

A note carries no role. Whether notes reach the model as user text, as
instructions or as something else is open. See Left to the encoder.

Calls belong to the assistant message that requested them and go in brackets
after it. Everything else is one token per message, in emission order.

    a(R1.1)[t(T1.1.1) t(T1.1.2)]
    a(R1.3)                        a response with no calls. K1 follows once
                                   nothing it started is still running.

Tool results carry an outcome, `ModelToolMessage.Outcome`, the projected form
of `ToolCallStatus`.

    r(T1.1.1:ok)        SUCCESS
    r(T1.1.1:err)       FAILED. IsError is set.
    r(T1.1.1:blocked)   BLOCKED. A policy refused it, asking nobody.
    r(T1.1.1:refused)   REFUSED. A human refused it when asked.
    r(T1.1.1:cancel)    CANCELLED. Content is the cancel reason.
    r{T1.1.1:pending}   Synthesized. The call has no result at the cutoff.

A call that never ran names who stopped it. A policy is a rule to work within,
a human is someone to go back to, and a cancel is neither. The three read alike
otherwise, and the model acts differently on each.

A call's progress events become one note each, `n{event:TE1.1.1.1}`, or `∅`.

Repairs
--------------------
A message list is flat, sequential and single-threaded. A log is none of these.
Three gaps open between them.

    provider shape  the transcript must be a certain shape to be accepted
    concurrency     arcs overlap, so work is unfinished and messages interleave
    causality       which request a message belongs to

The projection closes them with synthesized messages. `{…}` and `n{…}` mark
every one. `r(T1.1.1:cancel)` is what the log says. `r{T1.1.1:pending}` is what
the projection made up. A mode gates each repair.

The gaps compound. Concurrency creates the unfinished call. Provider shape
makes it fatal. Causality decides what the stand-in has to say.

*Unanswered calls.* Most providers reject a transcript where a call has no
result. The call gets a stand-in, with a note that explains it.

    a(R1.1)[t(T1.1.1)] r{T1.1.1:pending} n{inflight:U1}

The stand-in is per call, because each call has to be answered. The note is per
arc. Several waiting calls under U1 produce one note between them.

*Attribution.* Every source ID names its own arc, so `r{T1.1.1:pending}` in the
middle of arc 2 is about arc 1. Whether the projected text carries the
attribution is a wording decision, settled in the expanded form.

Modes
--------------------
A mode is a session-wide decision about how much of one record kind to
disclose. One vocabulary covers all modes, ordered by how much it tells the
model.

    keep        project it in full
    stand_in    replace it with a synthesized message of the same shape
    note        reduce it to a synthesized note
    elide       drop it, written `∅` where a scenario needs to say so

A mode looks at one record and decides what it becomes. No mode packs several
records into one message.

`*` marks the default.

    foreign_arc    an arc other than the projected one that the log does not
                   show as closed: open, dead, or withdrawn
                   keep* | note | elide

    pending        tool calls with no result
                   stand_in* | note | elide

    tool_events    progress notifications emitted by a running call
                   note | elide*

    async_events   notifications a running task emitted
                   note* | elide

    async_input    a task stopping to ask a human for input
                   note* | elide

    error          a non-fatal error the arc survived
                   note | elide*

    interrupt      the human approval a tool call was held up for, the ask
                   and an approval
                   note | elide*

Settings
--------------------
One session-wide setting is not a treatment. Every mode above decides whether
a record is disclosed; this decides how much of one is spelled out in a note
that was already going to be sent.

    quote_limit    how much of a request a note quotes, in characters
                   32* | 0 for the whole request

A note names the request it is about so the model can tell which one is meant,
and naming is not repeating. A request quoted whole is put in front of the
model twice, once as itself and once as the note about it, and under
`foreign_arc: note` the quote becomes the only trace of a request the mode
just decided to reduce. Enough to recognise it is enough.

A quote is folded onto one line, because a note is one line. A request that
was cut ends in `…`, so that a shortened request cannot be read as one that
was always that short.

Fixed rules
--------------------
Four cases are not modes.

1. A cancel with `TerminatedWork` false is always elided. It stopped nothing.

2. A cancel that stopped work in progress is always disclosed.

3. A refusal is always disclosed, as `r(T:refused)` on the call it refused.
   The call has to be answered and the refusal is the answer. No mode reaches
   it, because no mode reaches a tool result.

4. An async task is always two notes, one at invocation and one at completion.

Reasoning
--------------------
A provider may return a record of how the model reached a message, and accept
it back on a later turn so the model can pick up its own train of thought. It
is opaque: the projection carries the bytes and reads nothing in them.

Reasoning is not a record of its own and has no mode. It belongs to one
assistant message, the way that message's calls do, and travels with it.

It travels only when the message survives intact. A provider that takes
reasoning back validates it against the message it arrives with, so a message
that lost a call to `pending: note` or `pending: elide` is no longer the message
the reasoning describes, and the reasoning is dropped. Under `pending: stand_in`
every call is answered and every message keeps all of its calls, which is why
the default carries reasoning everywhere and the other two values give it up.

Dropping it costs the model its own train of thought on that turn. That is the
price of a mode that rewrites the turn, and it is the reason to prefer
`stand_in` on a provider that has reasoning to lose.

Execution records
--------------------
One record kind in the log is not about the conversation. `TOOL_DISPATCHED`
says that a tool call was handed to its tool, and exists so a worker that
inherits an abandoned arc can tell a call that may have taken effect from one
that provably never ran.

It has no mode and no projected form. There is no setting at which the model is
told a call was dispatched, because at any ordinary cutoff the answer is the
same as `pending` already gives: no result yet. What the model needs to know
arrives through the outcome of the call the marker names. A worker that refuses
to repeat an inherited call settles it, and `r(T:cancel)` carries why.

The marker is past tense. It says the call left the building, not that anything
is still running, which is what keeps it true after the worker holding it died.

It has no shorthand and appears in no scenario. A record that never projects has
nothing to write on a scenario line, and a token for it would suggest otherwise.

A further execution-plane record has to make this case again. The vocabulary in
this document is about disclosure, so a record with nothing to disclose is a
deliberate exception rather than an established second category.

Async tasks
--------------------
A task is always two notes.

    n{task:A2.1 started}    work the model began is now running
    n{task:A2.1 → ok}       what came of it

Neither note is optional. Without the first the model cannot account for work
it started. Without the second it waits for an answer that already arrived.

Tasks do not project through the tool-call protocol. A task with no result is
the normal state, and tool shape would require `pending` to invent an answer
every turn until the task finished. The cost of notes is the structural link
between what the model started and what came back, which has to live in the
wording instead.

Every note names its task by name and ID. The ID is there because the model
answers a waiting task by naming it, and a note is the only place it is told
which task is meant. It is written whole: a shortened ID would have to be
matched by prefix, which resolves the wrong task rather than failing to find
one.

The second note reports what the runner said, and a runner has three things it
can say. A task that was called off is not one that went wrong, so `cancelled`
is an outcome of its own rather than an error, and the model is told the work
was stopped rather than that it failed.

A running task holds its arc open. The agent records `KX` only once nothing
under the arc is still running, so an arc that answered and left a task behind
is still governed by `foreign_arc`, and a cancel that stops that task is a
cancel that stopped work. Without this a task nobody can account for keeps
running under an arc that reads as finished.

`async_events` decides whether task notifications are told at all. A task that
reported three times and then finished yields three event notes and two task
notes.

`async_input` covers a task that stops to ask a human. The ask and the answer
are one note each, placed where each happened.

    n{ask:AQ2.1.1} n{answer:AP2.1.2}
        answered right away, so the notes sit together

    n{ask:AQ2.1.1} … a(R2.1) … n{answer:AP2.1.2}
        the model spoke in between, so the notes stay apart

The second case is why the ask keeps its own position. An ask left unanswered
across a turn was disclosed as a state, and the model may have told the user
that the task is waiting on them. An unanswered ask is also the only thing that
conveys a task moving to WAITING.

Interrupts
--------------------
An interrupt is the human-in-the-loop gate and nothing else. It always names
one tool call and holds that call until a human decides. A policy that refuses
a call on its own asks nobody and blocks it instead, which is a different
record and a different outcome.

The decision reaches the model in one of two ways, depending on what it was.

    refused     the call is settled REFUSED and the outcome is `r(T:refused)`.
                No note, no mode: the call had to be answered and this is the
                answer.

    approved    the call runs as if nobody had been asked, so the log's only
                trace is the interrupt itself. A note is the only place the
                approval can be told, and the `interrupt` mode governs it.

The ask is a note of its own, at the position it happened.

    n{ask:I1.1.1}        a human was asked to approve this call before it runs
    n{approved:I1.1.1}   a human approved it

An interrupt carries `Reason`, what the human was asked in words, and
`Context`, whatever else the decision was about: what the call would touch,
which policy asked for a human, anything the person deciding was shown that the
call's own arguments do not say. Both go into the ask, so the model is told
what the human was told and can account for a refusal without guessing what was
on the screen.

Both are written when the interrupt is raised and never change. That is what
keeps the ask stable: it is built from fields that are already final.

The ask is written from the interrupt alone and says nothing about the outcome.
Every transcript that contains it contains the same bytes, so a decision
arriving later appends a message instead of rewriting one and the cached prefix
survives. An ask that reported its own status would have to be rewritten the
moment the answer came back.

The two notes can sit far apart. A human who takes an hour leaves other arcs'
turns between them, which is why the approval names the interrupt rather than
relying on sitting next to the ask.

An unanswered ask is what tells the model it is waiting on a person rather than
on a machine. The call's stand-in says the same thing from the other side:
`r{T:pending}` reads differently for a call nobody has answered and a call
nobody has approved.

Cancellation
--------------------
A cancel is three cases and only one is a mode.

    too late    `TerminatedWork` false. `KX` had already closed the arc.
                Always elided.

    withdrawn   Cancelled before the agent responded. Nothing was attempted.
                The arc is not closed, so `foreign_arc` decides.

    stopped     Cancelled while the agent was working. Always disclosed.

A cancel that stops nothing but a running async task is a `stopped` cancel. The
arc was open, because a running task is what keeps it open, and the work the
cancel ended belonged to it.

For the stopped case the work and the cancel are disclosed together.

    a(R1.1)[t(T1.1.1)] r(T1.1.1:cancel) n{stopped:U1}

Eliding the work and keeping the note leaves a stop with nothing to refer to.
Keeping the work and eliding the note makes an abandoned attempt read as
unfinished business to pick back up.

The note must not guess at motive. `Cancel.Reason` is optional free text, so
the log usually records that a human stopped this and not why. "The user
stopped this before it finished" is supported. "The user rejected this
approach" is not. Quote `Reason` where it is present.

What a cancel settles
--------------------
A cancel settles the calls its tools never saw, and only those. A call with no
dispatch provably never ran, nothing more will arrive for it, and it is
reported as cancelled.

A call that was already dispatched is not the cancel's to settle. The tool may
still report, and whether it took effect is not something the cancel knows. It
stays pending in the DISPATCHED phase, and a result arriving later settles it
the ordinary way — which is how a call that completed after everything stopped
still reaches the model as completed.

So a stopped arc answers its calls three ways, and the differences are the ones
that decide whether the model tries the work again.

    never dispatched    "[this call was cancelled before it ran]"
                        Definite. Nothing happened.

    dispatched, no      "[this call was handed to its tool and then stopped
    result yet          before it reported. Whether it took effect is not
                        known.]"

    dispatched, result  the result. The cancel stopped the run, not the call.

The middle one is the only thing the log cannot say, and it must not be
resolved by guessing. A model told the call did not run is a model that runs it
again.

The same three readings apply to an arc that died of a fatal error, which is
over for the same reason: nothing further is coming.

An async task is governed by the same rule, with no first case: a task that has
been invoked has started, so a cancel settles none of them. It runs where the
cancel cannot reach it and outlives the run that began it, so cancelling ends
the waiting rather than the task. What became of it is the runner's to report,
and a result arriving after the cancel settles the task the ordinary way.

A runner that stops when asked reports `cancelled`, which is how a task reaches
that status: no cancel writes it, because a cancel does not know whether the
task stopped or what it did first. The first report settles the task, so a
runner that acknowledged a stop and then finished anyway does not overwrite
what it already said.

Fixed rule 4 still holds, so a task the arc stopped on gets its second note
from the stop rather than from a completion that will never arrive.

    [task auditor (id 4f1c…) was still running when this request was stopped.
     Whether it finished is not known.]

The note is placed where the arc ended, which is after everything the task
managed to report.

Errors
--------------------
`Error.IsFatal` decides which mode applies. A fatal error closes its arc, so
the arc becomes a foreign arc the log does not show as closed and `foreign_arc`
governs its material. A non-fatal error leaves the arc running, and `error`
governs the error record alone.

`Error.CausationID` names the record the failure was about, where there is
one. A note that reduces an error may use it to say what failed.

Foreign arcs
--------------------
`foreign_arc` governs every arc but the projected one that the log does not
show as closed. Three statuses land here: still open, dead of a fatal error,
and withdrawn before anything was attempted. A foreign arc that closed
normally is ordinary conversation history and is projected as such.

    keep    the arc's material is projected like any other conversation, and
            a note reports its status: inflight, dead or withdrawn
    note    the status note alone. It may quote what was asked, and it says
            that someone else is on it.
    elide   nothing

Both disclosing values report the status. They differ in whether the arc's
material comes with it, not in whether the model is told the request belongs to
another turn. Use `keep` when the model should see the request in the user's
own words, `note` when an account of it is enough.

`keep` always reports the status. A bare user turn reads as a request
addressed to the current turn, and an assistant message belongs to exactly one
arc, so a model that answers both files the answer under the wrong arc and
leaves the foreign one open with no response of its own. An arc whose material
was all elided by a finer mode reaches the transcript bare and gets the note
for the same reason.

One mode covers all three statuses, so an arc is governed the same way at every
cutoff as its status changes. The three share one setting: a session that
discloses open arcs discloses dead and withdrawn ones too.

Ordering
--------------------
Messages are emitted in log order. An arc's material is never gathered up and
presented contiguously.

An arc can surround another. U1 opens, U2 opens and finishes, U1 finishes, U3
opens.

    U1, R1.1, T1.1.1, U2, R2.1, K2x, TR1.1.1, R1.2, K1x, U3!

U1's material lies on both sides of U2's and stays there. R2.1 was written
while U1 was unfinished and may say so. Putting U1's closing response in front
of R2.1 would leave the model reading its own sentence about work in progress
after that work had visibly finished.

A call and its result are the one exception. Providers require the pair to be
contiguous, so it travels together and sits where the call was made. This
understates how long the call took whenever another arc's turn falls inside it.

Composition
--------------------
Modes apply outside in. `foreign_arc` decides whether a foreign arc's material
reaches the transcript at all. Only what survives is offered to `pending`,
`tool_events` and the rest.

`foreign_arc` governs an arc while it is open, once it dies and once it is
withdrawn, so which mode governs a record is fixed for the session along with
the mode's value. See Foreign arcs.

Phases
--------------------
    1  elision     apply modes
    2  attribution synthesize notes for what survived from other arcs

Elision runs first because attribution has to see what survived. An assistant
message left with no text and no surviving calls is dropped in phase 1.

Provider constraints are satisfied by mode selection, not by a repair pass.
`pending: stand_in` answers every call. A provider's requirements constrain
which modes are legal for a session.

Attribution has one placement rule with no mode. A note may not land between a
call and its result. Notes go before the assistant message that opens the block
or after the last result in it.

Left to the encoder
--------------------
Two questions about a projected transcript are answered per provider, in
[ENCODING.md](ENCODING.md).

*Note rendering.* `n{…}` carries no role. Anthropic and OpenAI have an operator
role that can carry a note at a position. Mistral and Gemini do not, so a note
is user text there.

*Role alternation.* Interleaved arcs can put two user messages in a row.
Anthropic combines them, OpenAI accepts them, Mistral and Gemini reject them.
The encoder merges them, and a merged message labels each part with its origin
so that two people's words do not read as one turn.

Merging messages is the one case where an encoder changes the message list
rather than the wire format. Everything else it does is a shape translation.

Known gaps
--------------------
*Per-task event settings.* `async_events` is session-wide. A long-running
deployment and a quick lookup want different answers and the task definition
knows which it is.

Scenario format
--------------------
Compact form, one line, space-separated, in emission order.

    Messages: u(U1) a(R1.1)[t(T1.1.1)] r(T1.1.1:cancel) n{cancelled:U1} u(U2)

Expanded form, for scenarios where the synthesized text is under discussion.
Calls get an unnumbered line under their message.

    1  u(U1)              "how many pods are pending?"
    2  a(R1.1)            "let me check"
       [t(T1.1.1)]        kubectl_get{kind: pod}
    3  r(T1.1.1:cancel)   "superseded by a later request"
    4  n{cancelled:U1}    "[the previous request was cancelled before it
                            finished. Its result is unknown.]"
    5  u(U2)              "never mind, describe node n1"

Header fields:

    Modes     the session's mode settings
    Arc       the arc being projected
    Events    the events the projection was given, with `x` on any event
              that closed its arc

Scenarios
--------------------
One file each, under [projection-scenarios/](projection-scenarios/).

    01  open-arc-kept                an open foreign arc, projected in full
    02  open-arc-noted               the same, reduced to a status note
    03  open-arc-elided              the same, dropped entirely
    04  pending-call                 a call with no result at the cutoff
    05  dead-arc-elided              an arc closed by a fatal error, dropped
    08  cancel-too-late              a cancel that stopped nothing
    09  cancel-stopped-work          a cancel that stopped something
    10  cancel-withdrawn             a request taken back before it ran
    11  surrounding-arc              one arc wrapped around another
    12  interrupt-approved           a human said yes and the call ran
    13  interrupt-refused            a human said no and answered the call
    14  interrupt-unanswered         an ask still outstanding at the cutoff

01 through 03 are one log under three values of `foreign_arc`. 12 and 14 share
an ask, once answered and once not. Numbers are stable identifiers, not
positions.
