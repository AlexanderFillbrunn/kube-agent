Compaction
==========

Status
--------------------
A proposal, not a specification. [PROJECTION.md](PROJECTION.md) and
[ENCODING.md](ENCODING.md) describe what the code does; this describes
something it does not do yet. Nothing here is binding until it is folded into
those two, and the code follows neither this document nor
[ASYNC-TASKS.md](ASYNC-TASKS.md).

It works through one plan: a request event, a worker that picks it up, a
result event carrying the summary, playback from the last result, and the
session held while it happens — by a record rather than a lock. The plan
survives, with its two halves split apart.

Nothing needs locking for the log to stay correct, because compaction only
ever appends. But a compaction takes tens of seconds, and an arc opened
inside that window projects the full history by the ordinary cutoff rule —
the history that was just judged too large. So the session does need to be
held, not to protect the log but to make compaction any use under load, and
the record that holds it is the plan's request event.

What changes is what that record is. Not a work item to be claimed and handed
back, which is what needs a reaper when a worker dies mid-compaction, but a
fact carrying an expiry: a compaction covering this much was begun at this
time and was expected to take this long. Outstanding, satisfied and abandoned
are then all derived from the log and the clock, and nothing has to be
retracted. Whether a compaction is *due* stays derived and needs no record at
all.

What compaction is
--------------------
A session grows without bound and a provider's context does not. Compaction
replaces a run of conversation with a summary of it, so that a session can
continue past the point where its own history stops fitting.

It is the only operation in the system that takes material away from the
model. Everything in PROJECTION.md decides what a record becomes; compaction
decides that a span of records becomes one thing between them. That is the
one place the mode vocabulary does not reach, because no mode packs several
records into one message, and it is why compaction is a document rather than
an eighth mode.

The log is unchanged by it. Compaction appends, like everything else. What is
compacted is what the next projection reads, not what the store holds, and a
compacted session can still be replayed whole by a reader that ignores the
record. This is worth stating first because most of the reasoning below
follows from it.

Compaction is not an arc
--------------------
An arc is one unit of agent work: something is said to the agent, the agent
works until it has nothing synchronous left to do, and the arc closes. A
compaction is none of that. It runs no tools, produces no reply, answers
nobody, and the model it calls is not the session's model acting as the
agent. Giving it an arc buys the queueing machinery an arc already has and
costs the cutoff rule its meaning.

It also costs more than it looks. `Store.GetEventsForArc` finds an arc by
scanning for a `USER_MESSAGE` with a given correlation ID, and
`applyUserMessage` is the only thing that mints one. An arc opened by
something else needs "the event that opens an arc" to become a kind — the
same prerequisite ASYNC-TASKS.md identifies, for a different reason. Neither
proposal should pay for it alone, and compaction does not need it at all.

So compaction's records carry no correlation ID. That is not a gap; it is the
behaviour we want, and the existing cutoff already delivers it. The query is

    Seq <= opening || correlatesTo(arcID)

so an uncorrelated event written before an arc opened is in that arc's
history, and one written after it is not. Read that twice: it means a
compaction takes effect for every arc opened after it, and changes nothing
for an arc already under way.

That second half matters more than the first. An arc that has been running
for ten minutes has a cached prefix at the provider and a model halfway
through a train of thought. A compaction landing in the middle of it would
rewrite the front of its transcript on the next tool round, invalidating the
cache and dropping reasoning the provider would no longer accept. The cutoff
prevents it without anyone arranging for it to.

The record
--------------------
One event type, `COMPACTED`, with no correlation ID and a payload along these
lines.

    type CompactedPayload struct {
        // ThroughSeq is the last event this summary accounts for.
        // Everything at or below it is replaced; everything above it
        // survives.
        ThroughSeq int

        // Summary is the replacement material, as text.
        Summary string

        // Supersedes names the compaction this one was built on top of,
        // and is nil for the first in a session.
        Supersedes *string

        // Modes records the projection settings the summarised transcript
        // was built under, since a summary made under other settings
        // describes a conversation this session never had.
        Modes history.Modes

        ModelID string
        Usage   *TokenUsage
    }

`ThroughSeq` is the whole contract. It is a watermark over the log, and
every question below — which compaction wins, what a reader loads, whether
two workers conflict — is answered by comparing watermarks.

The record's `CausationID` names the `COMPACTION_STARTED` it answers. That is
the request half of the pair: whoever finds compaction due writes the request
and yields, a worker picks it up, and the result points back. `Supersedes`
stays as well — one names the request this result answers, the other the
result this one was built on top of — and the pair is checkable by a reader
who does not trust the writer.

Compaction is recursive: the second summary is made from the first summary
plus what has happened since. `Supersedes` records the chain so a reader can
check that coverage is continuous rather than assume it. It is not how the
compaction in force is found. See Which compaction is in force.

Where the cut goes
--------------------
A watermark cannot fall anywhere. Three things constrain it.

*A call and its result travel together.* Providers require the pair to be
contiguous, and PROJECTION.md makes it the one exception to log order. A
watermark between them would leave a result whose call is gone.

*A record names the record it acts on.* A tool result names its call, an
async result names its task, an error may name its cause. Today every one of
those references stays inside one arc, so a cut on an arc boundary leaves
nothing dangling. That is a property worth knowing is load-bearing, because
ASYNC-TASKS.md would break it: an arc opened in reaction to a task event
carries causation naming a record in another arc.

*An unfinished arc must not be summarised away.* `foreign_arc` exists so the
model is told about work that is open, dead or withdrawn. An arc below the
watermark is no longer available to be told about, and an open arc that
vanishes into a summary is work nobody can account for — the same failure the
hold-open rule for running tasks was added to prevent.

The simple rule that satisfies all three is to put the watermark immediately
below the opening event of the oldest arc that is not closed.

    ThroughSeq = min(arc.Seq for arcs where arc.Status() != closed) - 1

Everything below it belongs to arcs that are over. It is one pass over the
folded history, it needs no interleaving analysis, and it is checkable by a
reader who does not trust the writer.

Its cost is that one ancient open arc pins compaction for the whole session.
That is the same failure mode `SettleAbandonedTasks` already exists for, and
it wants the same answer: something eventually settles work that will never
report, and compaction becomes possible again. It is a reason to want the
reaper, not a reason to want a cleverer watermark.

The richer rule — compact the closed material and carry the still-open arcs
forward verbatim — is available later and is not worth its cost now. It turns
the load query from "everything above a sequence number" into "everything
above a sequence number, plus everything belonging to a set of arcs below
it", which gives up the load-time saving and keeps only the token saving.

Two views of one prefix
--------------------
An arc that is open when a compaction lands keeps the full history; an arc
opened after it gets the summary. So two runs can be under way at once with
different views of the same prefix.

    U1, O1, U2, R1.1, R2.1

`R1.1` is written against the full head and `R2.1` against the compacted one.
That is the cutoff working as specified rather than a hole in it, and the
watermark rule bounds how far the two can diverge.

Because the watermark sits below the oldest arc that is not closed, an arc
open at the moment of compaction lies entirely above it. None of its own
material is ever summarised, and nothing it can see below the watermark is
visible to the later arc either, because that is the region the summary
replaced. The two views therefore differ in the resolution of one region and
not in what that region contains. They are not in disagreement; one is
lossier.

The finer view is the older one, which is the safe direction. Consider the
alternative, where a compaction applies to arcs already under way so that
both agree. Arc 1's transcript would then change between one response and the
next: the cached prefix is invalidated mid-run, and the detail the run was
working from is taken away while it is still working from it. Worse, an event
written after an arc opened would decide what that arc's transcript says,
and the projection would stop being a function of the query's cutoff. That is
the causality problem. Declining to apply a compaction retroactively is what
avoids it.

Stated positively: a compaction is chosen once per arc, when the arc opens,
and does not change for the arc's lifetime. It is the rule the cutoff already
applies to every other record written by someone else, and compaction is not
an exception to it.

One cost survives the argument. Under `foreign_arc: keep`, the later arc
reads the earlier arc's responses, and a response written against the full
head may refer to detail the later arc's head no longer holds. It is bounded
— only material below the watermark can be missing — and it is the same risk
compaction carries anyway, which is that the model refers back to something
the summary dropped.

What the divergence does expose is a limit worth being explicit about.
Compaction cannot relieve the arc that is running. The arc whose context is
too large is usually the one in flight, and this design only ever helps arcs
opened later, so a single long arc with many tool rounds is out of its reach.
Relieving that one means rewriting a transcript while a run is reading it,
paying the cache and the reasoning deliberately, and it is a second mechanism
rather than a setting on this one.

The narrow version that does reach under a running arc is the next section.
It covers the case that happens in practice — an arc that starts near the
threshold and grows past it through its own rounds — and leaves the long arc
where this section leaves it.

Compacting under an open arc
--------------------
An arc opened just under the soft budget can cross the hard one through its
own tool rounds. Letting it finish means a turn the provider refuses, so the
session has one move left: compact the material *before* the arc while the arc
is held, and resume it on the summary plus its own rounds.

The cut goes immediately below the arc's opening, and that is the ordinary
watermark rule, not a new one. While the arc is held it is the oldest arc that
is not closed, so the closed-arc rule already puts the watermark exactly
there. Nothing of the arc's own material is summarised: its opening event
survives above the cut, its tool rounds keep their pairs contiguous, and the
model keeps the request it is working on verbatim. What the summary replaces
is the conversation history the arc was working on top of, which is the
material compaction is for. No arc is carried across its own compaction,
because no arc is compacted.

The hold is the guard's shape. A run refuses writes that start work while a
compaction request is outstanding — the same `startsWork` seam
`GuardCancellation` refuses on — so the arc stops at a round boundary, the
boundary being the one place the cut is safe, and the queue re-delivers the
arc when the compaction lands. The resumed run's view is the summary, then the
arc's own rounds whole.

One rule genuinely changes: which compaction an arc reads. The `COMPACTED`
event lands after the arc's opening and carries no correlation ID, so the
cutoff excludes it from the arc's own query — the mechanism that protects an
in-flight arc also defeats the resume. The compaction in force is therefore
pinned at each *run's* start rather than the arc's opening: the resumed run
reads the compaction in force as of its re-entry point and finishes on that
view. The invariant weakens from one view per arc to one view per run, with
the arc spanning two — the same shape RE-ENTRY already uses, where a re-worked
arc is a new view of the same record. For an arc never held mid-flight the
two rules are indistinguishable, since nothing lands between its opening and
its runs.

The costs are the ordinary compaction's, paid mid-conversation: the front of
the transcript is rewritten, so the resumed run pays full price for the whole
prompt, and the summary may drop detail from before the arc that the model
had been leaning on. That is why the move belongs at the hard budget, beside
blocking compaction — a rescue where the alternative is a failed turn, not a
path a session with headroom should ever take.

What this still does not reach is the arc whose *own* rounds outgrow the
window, since its own material is never compacted. That is not a compaction
problem: it is a loop that should be a task, reported through ASYNC-TASKS so
the arc closes and the watermark advances, or a second mechanism that
rewrites a live transcript, which this document declines.

A near neighbour of this is the reason the session has to be held while a
compaction runs, since an arc opened during the window is in the same
position as one already under way. See The window.

Reading a compacted log
--------------------
Read from the watermark, never from the record. This is the one sentence in
the document that a reader cannot afford to skim, because getting it wrong
silently deletes conversation.

A `COMPACTED` event sits at a sequence number of its own, and that number is
not its watermark. The two are separated by however long the model took —
tens of seconds, and everything written in them.

    ...  100   101 ...... 140    141
         W     U2 opens, runs,   COMPACTED{ThroughSeq: 100}
               closes

Reading from 141 loses arc 2 entirely, and loses it quietly: the log still
holds it, the fold is never asked for it, and nothing anywhere reports a
gap. Reading from `ThroughSeq` is the whole of the fix, and it is why the
watermark is in the payload rather than being taken from the record's own
position.

The gap is not a rare case to be engineered away. Admission control holds new
arcs, not all writes, so even a blocked session takes tool results from arcs
already running, task reports, cancels and closes during the window. The
gap is essentially never empty.

So the store gains two queries.

*The compaction in force,* which is the one with the greatest `ThroughSeq`,
restricted to those written at or before the point being read from. For a
session read whole that is the whole log; for an arc it is the arc's opening,
which is what pins a compaction to an arc for the arc's lifetime. See Two
views of one prefix — and Compacting under an open arc for the one exception:
an arc held mid-flight pins at each run's start instead, since its own opening
is below the compaction the resumed run must read.

*The events above that watermark,* with the existing cutoff still applied on
top for an arc read:

    compaction in force as of `opening`, plus
    ThroughSeq < Seq <= opening, plus
    Seq > opening where correlatesTo(arcID)

Where there is no compaction the first clause is empty, the second is
everything up to the opening, and the query is what it is today.

The fold gains one field, `History.LastCompaction`, and one rule for setting
it: a `COMPACTED` replaces the one held only when its watermark is greater,
never merely because it arrived later. The same coverage rule as everywhere
else, in one more place.

It is needed because a superseded compaction can land inside the read region.
Two compactions running at once, the earlier one slower:

    A  starts at 200, watermark 180, written at 260
    B  starts at 210, watermark 205, written at 250

B is in force, so the region read is `Seq > 205`, and A's record at 260 falls
inside it. A fold that assigned on arrival would end on A and hand the
session a summary covering 25 events fewer than the one it was given. Most of
the time the superseded record falls below the watermark instead and is never
folded at all, which is exactly what makes this the case that gets missed.

`Replay` already accepts a log that begins partway through, since
`NewHistory` starts `LastSeq` at -1 and `Update` only requires sequence
numbers to increase.

What the fold does not tolerate is a reference below the watermark, and until
recently it did not tolerate it by panicking. Three apply functions read
`state.arcs[*ev.CorrelationID]` and used the result without checking it was
there — the ones for an assistant message, an async task invocation and an
error — so a truncated log whose first event was a reply to a compacted-away
arc crashed inside the fold, naming neither the event nor the arc. Those now
return an error, as the ones for a cancel and an arc close always did, and
`applyUserMessage` and `applyAssistantMessage` have the nil-correlation
guards they were missing.

A correct watermark never produces such a log, and that is the argument for
the checks rather than against them. The watermark is where the invariant is
enforced: one computation, in one place, that can be wrong. The check is
where it is detected, at the event that breaks it. None of it is only about
compaction — a correlation ID naming no arc is a malformed log however it
arose — but compaction is what first makes a truncated log something the
system produces on purpose.

Projecting a compaction
--------------------
The summary goes at the head of the transcript, before the first surviving
arc, and nothing precedes it but the system prompt.

It is synthesized: no participant said it. By PROJECTION.md's own definition
that makes it a note, and `ModelNoteMessage` is the shape for it. Reusing the
shape is right — the set of message shapes is closed on purpose, and a
summary needs nothing from a provider that a note does not already get. What
has to change is the sentence in that document saying a note is one line.
That was written about status notes and the `quote_limit` setting; a summary
is the one note that is not status and is not short.

The alternative is an INSTRUCTION message, `s{…}`. It is the wrong role: an
instruction is a directive about how to behave, and a summary is a report of
what happened. Rendering it as one teaches the model to read its own past as
a set of orders.

Under the shorthand the record is `OX` and the projected message is `c{O1}`,
with no dotted path because it belongs to no arc. `C` is taken by cancel.

No mode governs it. A session that compacts has decided the material is gone,
and a setting that could restore it would mean keeping the material, which is
the thing compaction is for.

What is summarised
--------------------
The summary is made from the projected transcript, not from the history and
not from the log.

The projection has already decided what the model is told. Summarising the
log instead summarises material the modes elided, and lets it back in through
the one message no mode reaches. A session that runs `error: elide` and then
compacts would find its errors reappearing in prose.

So compaction sits after projection in the pipeline and before encoding.

    log ──fold──▶ History ──project──▶ ModelTranscript ──compact──▶
    ModelTranscript ──encode──▶ provider messages

Which makes the compactor's input and output the same type: it takes a
transcript and returns a shorter one. That is the honest signature, and it is
what makes compaction testable without a provider — a fake compactor is a
function that returns a fixed string.

`Modes` goes in the payload because of this. A summary is only a faithful
account of the conversation under the settings it was made with, and a
session whose settings changed has a summary describing a transcript it would
no longer produce.

The compactor
--------------------
A separate interface, almost certainly the same implementation.

    type Compactor interface {
        Compact(ctx context.Context, t history.ModelTranscript,
            through int) (CompactionResult, error)
    }

Tightly coupled to a provider is a fact about the implementation. It need not
be a fact about the type. Nothing outside the compactor cares whether the
summary came from a completion, from a provider's own context-management
endpoint, or from a smaller model chosen because the job is cheap — and a
session may well want the last of those, which an `LLM` method on the
session's model cannot express.

One constraint on any implementation. Whatever a provider offers, what lands
in the log must be text. A provider that returns an opaque handle to
server-held state gives a summary that only that provider can read, and the
session stops being portable: devchat already lets `/model` change models
mid-session on the strength of every reply recording which model wrote it,
and an opaque summary ends that. Text costs the tokens the handle would have
saved and keeps the log a thing you can read with `cat`.

Reasoning does not survive compaction, and nothing needs to be done about it.
It belongs to one assistant message, and a message that has been summarised
away takes it with it.

Concurrency: correctness and liveness are different questions
--------------------
The plan asks how to lock a session while a compaction runs. There are two
questions inside that and they have different answers. Nothing needs locking
for the log to stay correct. Something does need to give for compaction to be
any use under load. Taking them together is what makes the problem look like
it needs Redis.

Correctness first, because the reason is the first paragraph of this
document: compaction appends.

*Readers.* An arc being projected while a compaction runs reads the log as it
stands. It is unaffected, because the compaction is not there yet, and it
stays unaffected once the compaction lands, because the cutoff excludes
events written after the arc opened. No exclusion needed.

*Ordinary writers.* An event appended while a compaction is being computed
gets a sequence number above the watermark that compaction was computed
against. It therefore survives the compaction untouched. The summary does not
claim to cover it and nothing removes it. No exclusion needed.

*A second compaction.* Two compactions at once waste money and are not
incorrect, provided the rule for which one is in force is coverage rather
than recency. See below.

So no fencing tokens, no lease on the write path, and nothing that has to be
correct for the log to be readable. Whatever admission control is added on
top of this is an optimisation and cannot corrupt anything by failing. That
is worth establishing before adding it, because it is what lets the admission
control be sloppy — advisory, expiring, best-effort — instead of a
distributed lock.

Which compaction is in force
--------------------
The compaction with the greatest `ThroughSeq`, not the last one written.

This one line is what makes a duplicated or abandoned compaction harmless.
Suppose two workers run at once: A computes through 100, B through 90, and B
is written second. Under a recency rule the session ends up using B, which is
fine but wasteful, and worse, reasoning about it requires knowing who wrote
last. Under a coverage rule the session uses A, B is inert, and the outcome
does not depend on the interleaving at all.

Nothing has to be deleted for this to hold. A superseded compaction stays in
the log as a record of what was summarised and when, which is exactly the
sort of thing you want when a model's later answer has to be explained.

The rule generalises the failure cases too. A compaction whose watermark is
below the one in force is already-answered work, discarded on read with no
special case. A worker that crashed after computing but before writing left
nothing behind at all. And a worker whose claim on the job expired while it
was still working writes a perfectly good compaction that is still accepted,
which is why the expiry below needs no fencing.

The window
--------------------
A compaction is a model call over a long transcript and takes tens of
seconds. That window is the whole difficulty, and it is not a correctness
problem.

An arc opened during the window does not get the compaction. The cutoff gives
an arc the events written before it opened, and the `COMPACTED` event is not
written yet, so the arc projects the full history — the history that was just
judged too large. Under a request rate faster than the compaction takes,
every arc falls in a window, and the compaction helps nobody while costing a
model call each time. The thing compaction exists to prevent is what happens
to every arc that arrives while it runs.

This is what the plan's blocker event is for, and it is right. The reasoning
below about deriving rather than recording covers whether a compaction is
*due*, which is a fact about the conversation. Whether one is *running* is
not a fact about the conversation at all — it is a fact about a worker — and
nothing in the log implies it. A record is therefore unavoidable, and the
question is only what kind of record it is.

Triggering: whether it is due is derived
--------------------
Whether compaction is due is a pure function of the log: the watermark in
force, and the input tokens the last reply reported against a budget.
`AssistantMessagePayload.Usage` already carries it, and already distinguishes
nil from zero so that a reply whose cost went unreported is not counted as a
free one.

So the fold gains a predicate — `History.CompactionDue(budget)` or similar —
and the worker that closes an arc consults it. The repository has made this
argument once already, in `TaskReports.Report`: whether there is work to do
"is decided by reading the log, in one place, and a report that warrants
nothing costs a read and no reply. Duplicating that judgement here would be a
second answer to a question already answered."

The consequence is that the queue carries nothing but a session ID. A queue
message that is duplicated is re-derived as not-due and dropped. A queue
message that is lost is re-derived at the next arc close. Neither needs a
record in the log, because neither is a fact about the session.

Admission control: a started record, not a claim
--------------------
That a compaction is running does need recording, for the reason in The
window and for a second reason of its own: the interface has to be able to
say what the session is doing during those tens of seconds, and it reads the
log rather than the agent's state. The `Observer` sink already hands the
interface every folded event, so a record here is a state the interface gets
for free and a side channel is a state it would have to be told about twice.

    type CompactionStartedPayload struct {
        // Watermark is the cut the compaction is being computed against.
        Watermark int

        // Expiry is how long this may hold the session before it is
        // disregarded, as a duration from the event's own timestamp.
        Expiry time.Duration

        // Actor is who asked, where a person did.
        Actor *users.Actor
    }

The watermark in the request is the closed-arc cut from Where the cut goes,
capped at the request's own sequence number: the cut can name nothing the
request has not seen. The cap matters only in a session whose arcs interleave,
where the request and the last closed arc can be far apart, and it is what
makes the request the compaction's vantage — the cut is a function of the log
up to and including the request, and of nothing written after it.

What keeps this from being the work item the previous section argues against
is that it is written as a fact and never as a claim. It says a compaction
covering this much was begun at this time and was expected to take this long.
It says nothing about who holds it, and there is nothing to hand back.

Everything about its status is then derived, in the same style as the rest.
It is outstanding while no `COMPACTED` has landed after it and its expiry has
not passed. It is satisfied the moment one does, with no acknowledgement
event. It is abandoned once the expiry passes, with no retraction event and
nothing to reap.

Satisfaction is by recency, and the compaction in force is by coverage — two
rules for two different questions. A request is a plea — compact this
session — and any `COMPACTED` written after it answers it, whatever the
watermark; which compaction the model reads is the other question, and stays
the greatest `ThroughSeq`. The reason recency is needed and not merely tidy
is the skipped request: see Parallel arcs and one compaction.

*The expiry is the whole trick, and it is what the crash case turns on.*
Without it a record that blocks the session is strictly worse than no record:
a worker that dies at forty seconds leaves a session blocked for ever, and
clearing that is exactly the reaper the plan was trying to avoid. With it,
the block decays on its own, and the next arc either proceeds or starts a
fresh compaction. It costs one field and it is only safe because a compaction
that arrives after its own expiry is still a valid compaction — see Which
compaction is in force. In a system that mutated state this would need a
fence; here it needs nothing.

It also removes the need for a lease. A worker that sees an outstanding,
unexpired record does not start a second compaction, which is the same
exclusion an `ArcLease` would give and one fewer thing to stand up. Two
workers that start in the same instant write two records and compute two
summaries; that is waste, bounded by the coverage rule, and not a fault.

Blocking and parallel compaction
--------------------
Whether a session holds its arcs while a compaction runs is a mode, chosen by
the session's configuration. Both modes share the whole design above — one
record, one result, one coverage rule — and differ only in who waits.

*Parallel* is the ordinary path and the one a well-configured session never
leaves. A compaction runs beside the conversation: arcs opened during the
window project the full history, which is large but works, and the arc after
the window gets the summary. Nothing is held, no admission check is made,
and the cost of the mode is one oversized turn per compaction.

*Blocking* holds the admission of new arcs while the request is outstanding.
It is the mode the hard budget argues for — see Two thresholds — where an arc
projecting the full history would be rejected by the provider rather than
merely cost more. Blocking is not a second mechanism: it is the same request
record, read by whoever admits an arc instead of ignored.

The mode is a property of the session's configuration rather than a fact the
log carries, which is safe because it changes nothing about correctness — a
session that switched modes mid-flight would change only whether the next arc
waits, never what any arc reads.

Session interleaving and one lease
--------------------
A session also configures whether its arcs interleave. One arc at a time
is a real requirement — a session driven by a single user, or one whose
turns must be serial for the product's own reasons — and it needs
session-level mutual exclusion that the framework does not otherwise have:
the `ArcLease` is per arc, and nothing stops two arcs of one session being
worked at once.

The exclusion it needs is a lease over the session, and that is the same
mechanism with a different key: the lease's holder, TTL, heartbeat and
holder-checked release are all key-agnostic, and `redisinfra` gains the
session-scoped key shape rather than a second lease. One mechanism, two
jobs — and the second job is blocking compaction. A session held to one arc
at a time already excludes everything else from working its arcs, so a
blocking compaction there takes the same session lease and the exclusion
comes for free. In a session whose arcs interleave, the lease is not taken
and the request record does the holding alone.

The exclusion matrix
--------------------
The natural shape for all of this looks like a session-level read/write
lock: compaction the writer, arc work the readers, arcs shared among
themselves. Laid out as a matrix of who must wait for whom, it comes out
asymmetric, and the asymmetry is why it is not a lock pair.

                      compaction    a new arc    an arc under way
    compaction        exclude        exclude       must not wait
    another arc       never          never         never

An arc under way never blocks a compaction, for the two reasons the rest of
this document builds: the cutoff means the compaction does not reach it, and
the closed-arc watermark means the compaction does not cover anything it is
entitled to read uncompacted. A compaction therefore never waits for a
reader, which is the half of a read/write lock that exists to make writers
wait — and it is the half nobody here needs.

What remains is one-directional: a compaction holds the admission of new
arcs, and nothing holds a compaction. That is a gate, not a lock, and the
gate's state is the request record — derived from the log and the clock,
decaying on its own — rather than held in a lock that a crashed worker would
leave closed for ever. In a session held to one arc at a time the gate is
the session lease, which the previous section gives that mode anyway.

The read/write vocabulary survives only as a description: "read" in the
sense of *not compaction*, not in the sense of needing exclusion. Readers
take nothing, share everything, and the writer takes the gate.

The admission race
--------------------
A gate read from the log has a race: an arc can pass the check in the
instant between reading no outstanding request and the compactor writing
one. That arc projects the full history — wasteful in parallel mode, a
failed turn at the hard budget — and the coverage rule is what keeps it a
cost rather than a fault. The next arc gets the summary; nothing is
incorrect.

Closing the race is possible and cheap if it ever needs closing: a Redis
gate key, set with a TTL before the compactor computes and checked by
whoever admits an arc. It is the lease machinery again, and it narrows the
race without promising to close it — the check and the user's write are not
one transaction and cannot be made one. Build the record first; add the
gate key if the measurement says the race is being lost often enough to
matter.

Parallel arcs and one compaction
--------------------
Two arcs closing near each other can both find compaction due, and a model
call is the expensive thing in the system, so the duplicate is worth closing
rather than merely surviving. The layers are cheap and each one narrows the
race further.

*The request is session-scoped.* `COMPACTION_STARTED` carries no correlation
ID, so it belongs to the session rather than to the arc that noticed. Two
arcs deriving due from the same log produce one job and one duplicate, not
two jobs; nothing here needs an arc-scoped request.

*The log orders the requests.* A worker that writes a request re-reads, and
the requests have sequence numbers: the earlier outstanding, unexpired
request stands and the later worker stands down before paying for anything.
One extra read per request, and the simultaneous-start race shrinks to the
instant between the re-read and the call.

*Due is re-derived at the compute, not at the request.* This is the layer that
protects the money. A worker about to call the model re-checks the log: is
there an earlier outstanding request, and does the watermark in force already
clear the budget? A duplicate that queued while the first compaction computed
drops here, at the cost of a read rather than a call. Derivation at enqueue
dedupe nothing, because both requests were honest when made; derivation at
compute dupe, because by then the log has moved.

*The expiry bounds the window and coverage bounds the loss.* A duplicate that
slips through anyway computes a summary the coverage rule makes inert, so the
loss is one compaction, never a wrong one. The Redis gate key from the
admission race is the last notch if measurement ever demands it — single-flight
at a key, TTL for the crashed worker, advisory so its failure degrades to the
re-derivation above.

A skipped request needs no marker
--------------------
The skip writes nothing, and the skipped request needs no acknowledgement,
for the same reason the design has no satisfaction event anywhere: status is
derived. But the derivation has to be the right one, and the obvious choice
has a trap in it.

The skip rule, exactly: a worker about to compute a request skips it when an
earlier request for the session is outstanding and unexpired. The comparison
is by sequence number alone — no watermarks. Watermarks cannot decide it,
because they are monotone: the cut below the oldest open arc only advances,
so a later request's watermark is never below an earlier one's, and a
"covered by an earlier request" test would skip nothing but exact equals.

Stated once, positively: compute the earliest unfulfilled request. Every
worker reaching for the log reaches the same verdict about whose turn it is,
which is what makes this single-flight without a lock — the sequence numbers
totally order the requests, so among several outstanding exactly one is the
earliest, and the log, not any worker's memory, is the arbiter. The due
re-derivation still applies at the moment of the call: the earliest request
whose reason has evaporated drops without one.

The trap is in satisfaction. A request satisfied only when a compaction in
force covers *its* watermark — the rule the normal case suggests — leaves a
skipped request outstanding for ever: the compaction it waited for lands
with a watermark at or below its own, never above, so nothing ever clears
it, and in a blocking session it holds admission until its own expiry
passes. A zombie — a request nobody will compute, blocking for no reason.

That is why the previous section's derivation is by recency: the skipped
request is satisfied the instant the compaction it waited for lands, and
admission opens. If the answer was thin — the skipped request's watermark
above the landed one, the region between the cuts left uncompacted — it is
not silently absorbed: due is re-derived at the next arc close, and a fresh
request with a fresh watermark and expiry takes over if the budget is still
crossed. The recursive case is the same machinery, one generation on.

The expiry stays the backstop for the request whose worker died: abandoned
rather than reaped, and the next derivation starts again.

Where a blocked arc waits
--------------------
A blocked session holds the user's message outside the log rather than
writing it and deferring the work.

Writing it first is the tempting shape, and `TaskReports.Work` is the
precedent for it: take the work, find it held, return false, let the caller
come back. It does not survive contact with the cutoff. An arc written at
sequence 101 and worked at 141 still has its cutoff at 101, so it still does
not see the `COMPACTED` event at 141 and still projects the oversized
history. Deferring the work changes nothing about what the arc reads.
Recovering it would need the compaction exempted from the cutoff, which is
the retroactivity Two views of one prefix argues against.

Held outside the log, the message is written once the compaction lands, its
opening sequence number falls above the `COMPACTED` event, and the cutoff
gives it the summary with no exemption and no special case. The mechanism
that makes this awkward is the one that makes it correct.

The cost is real and should be stated plainly: the user's words are not
recorded for as long as the block lasts, so a process that dies in the window
loses them and the person retypes. That is the ordinary behaviour of a
message that has not been sent yet, the interface can show it as such, and
the expiry bounds how long it can go on.

Two thresholds
--------------------
Blocking a session is a heavy thing to do for something that is meant to be
routine, and it is only necessary when compaction has become urgent. That
argues for two budgets rather than one.

The soft budget sits well below what the provider will accept. Crossing it
starts a compaction and blocks nothing: arcs opened during the window project
the full history, which is large but works, and the arc after the window gets
the summary. This is the ordinary path and it should be the only one that
ever runs.

The hard budget sits close to the limit. Crossing it means an arc projecting
the full history would be rejected by the provider, so letting it through
achieves nothing but a failed turn. That is where the session blocks, and
blocking is not a cost there — the alternative is not a faster answer but no
answer.

Two budgets also make the failure mode legible. Reaching the hard budget
means the soft one did not fire in time, which is a thing to alert on rather
than a thing to absorb silently. A session that blocks regularly is one whose
soft budget is set too close to its hard one, or one whose arcs arrive faster
than a compaction takes, and those want different fixes.

Async tasks
--------------------
Tasks and compaction meet at the watermark, and the meeting is worse than it
first looks. A running task holds its arc open — `agent.go` returns
`ArcStateDone` rather than `ArcStateClose` while any task under the arc is
unfinished — and the watermark may not pass an arc that is not closed. So one
background task pins the watermark for as long as it runs.

A one-hour audit started in the third arc of a long session makes every arc
after it uncompactable for an hour. Compaction is starved in exactly the
sessions that need it most: long ones, with work going on in the background.
That is not a corner case. ASYNC-TASKS.md names the same behaviour from the
other side, that a slow task "currently puts a status note in front of the
model on every turn for an hour", and treats long-open arcs as the normal
condition rather than a fault.

The same rule is also doing something valuable, which is why it cannot simply
be dropped. Fixed rule 4 says a task is always two notes, at invocation and
at completion, and that neither is optional: "Without the first the model
cannot account for work it started. Without the second it waits for an answer
that already arrived." Hold-open is what guarantees a watermark never falls
between them. The arc cannot close while the task runs, the watermark cannot
pass an arc that has not closed, so the pair is always on the same side of
the cut.

One mechanism, two faces. It blocks compaction and it protects fixed rule 4,
and any change that unpins the watermark has to replace what it was doing for
free.

*Under ASYNC-TASKS.md the protection goes and the problem arrives.* That
proposal closes an arc as soon as the agent has nothing synchronous left to
do, so arcs get short and the watermark moves freely — which fixes the
starvation. But the task's completion then opens an arc of its own, arbitrarily
later, and nothing keeps the invocation above the watermark any more. The
model is handed a result for work whose invocation has been summarised away.

Carrying, not summarising
--------------------
The general rule the watermark needs is this. Anything finished may be
summarised. Anything unfinished must be either pinned behind the watermark or
carried across it, and it is never enough to hope the summary mentions it.

Open arcs are pinned, which is the rule under Where the cut goes and is the
simpler of the two. Unsettled tasks could be either, and pinning them is what
starves compaction, so they are the case for carrying. The compaction payload
gains the tasks that were in flight at the watermark — id, name, owning arc,
status, and a waiting task's outstanding question — and the projection
synthesizes the invocation note from that record rather than from an event it
can no longer see.

Three reasons it has to be structural rather than left to the prose.

*The fold needs it before the projection does.* `applyAsyncTaskResult` and its
neighbours look the task up in `FoldState` and return an error when it is not
there. A result arriving for an invocation below the watermark does not
degrade the transcript; it fails the fold outright, and the session stops
loading. So the carried tasks have to be re-seeded into `FoldState` the way
`FoldStateFromHistory` re-seeds a history loaded from storage. The carry is
fold machinery that the projection also happens to use.

*A summary cannot hold an ID.* PROJECTION.md requires a task's note to name it
whole, because "a shortened ID would have to be matched by prefix, which
resolves the wrong task rather than failing to find one". The model answers a
waiting task by naming it. Prose written by a model will not reliably
reproduce thirty-two hex characters, so a task whose notes were summarised
loses its addressability even when the summary mentions it perfectly well.

*Lossy is the wrong property for in-flight work.* A summary is lossy by
design, and that is acceptable for a conversation that is over. Work that is
still running is state, and state that is summarised is state that is
sometimes lost.

Fixed rule 4 then needs one amendment: the first note may be synthesized from
carried state rather than from the invocation event. What the rule requires —
that both notes exist — is unchanged.

Task reports are not held
--------------------
Admission control holds new arcs. It must not hold task reports, and the
reason is that holding them would achieve nothing.

A task report works an arc that already exists, and an arc with an unsettled
task is an arc that is open, so the watermark is below it by construction. The
compaction being computed cannot cover any of that arc's material, and the
arc's query takes the compaction in force as of its own opening, so the new
one would not reach it even after it lands. Waiting buys the arc nothing and
costs the task its turn.

`TaskReports.Report` already has the right shape for this and needs no change:
it persists the report, then works the arc, and returns whether it worked it.
The report is a fact and is recorded whatever else is happening.

The consequence is the limitation from Two views of one prefix, in its most
common form. An arc kept open by a long task cannot be relieved by compaction
— not by waiting for one, not by a later one — and a session whose context
grew large inside such an arc has no remedy here. Tasks are the main way an
arc stays open long enough for that to matter.

The reaper is a prerequisite
--------------------
`SettleAbandonedTasks` closes off tasks whose runners died. Without
compaction it is hygiene: an arc that stays open for ever puts a status note
in front of the model on every later turn. With compaction it is structural.
An unsettled task pins the watermark, so a task nobody will ever hear from
pins it for ever, and a session that accumulates one abandoned task can never
be compacted again.

So the order matters. Settling abandoned tasks is what lets arcs close, arcs
closing is what lets the watermark advance, and the watermark advancing is
the whole of compaction. A reaper with high latency is fine; not having one
is not.

What it costs
--------------------
*The cached prefix.* Every provider caches on a prefix match, and a
compaction rewrites the front of the transcript. The first turn after a
compaction pays full price for the whole prompt. That is a real cost with a
real implication: compact rarely and in large steps. A low threshold that
compacts a little and often can spend more on cache misses than it saves on
context, and the budget wants choosing with that in mind rather than by
picking a round number under the context limit.

*Fidelity, unrecoverably.* Everything else in the projection is a view — a
record reduced to a note in one arc's transcript is projected whole in the
next. A compaction is not a view. Once the watermark moves, no later
projection can get the material back, and a summary that dropped the thing
that mattered has dropped it for the rest of the session. This is the
argument for compacting the transcript rather than the log — the log keeps
everything either way — and for a conservative budget.

*A second thing the model must be told is not a participant's words.* Notes
already occupy that ground and the summary is much larger, which makes the
carrier question from ENCODING.md heavier than it is for a one-line note. A
provider with an operator role can carry it; on Mistral and Gemini it is user
text, and a page of user text at the head of a conversation reads as
something the user said.

What would have to change
--------------------
`pkg/events` gains `COMPACTED` and `COMPACTION_STARTED` with their payloads,
and the codec gains their cases.

`pkg/history` gains `LastCompaction` on `History`, an apply function for the
event that sets it by coverage rather than arrival, and `CompactionDue`. The
missing-arc and nil-correlation checks that a truncated log needs are already
in.

`pkg/history/projection.go` emits the summary at the head of the transcript.
`Modes` gains nothing, because compaction has no mode.

`pkg/agent` gains the `Compactor` interface, the derivation at arc close, and
the admission check that holds a message while a compaction is outstanding.
In a session whose arcs interleave the started record does that job alone; in
a session held to one arc at a time the session lease does it, and that lease
exists for the interleaving mode regardless. `TaskReports` is unchanged,
since a report is never held.
`GuardCancellation` needs no change: `startsWork` covers dispatches and task
invocations, and a compaction is neither. Compacting under an open arc reuses
the same seam from the other side — a run held mid-flight refuses its next
dispatch while a request is outstanding — and the re-derivation at the
compute is the derivation at arc close applied a second time, against a log
that has moved.

`redisinfra` gains the session-scoped key shape for the lease, so the same
`ArcLease` mechanism serves a session as well as an arc, and, if the admission
race is ever to be narrowed, the compaction gate key. The session's
configuration gains the two settings the modes are made of: whether arcs
interleave, and whether compaction blocks.

The store gains the two queries — the compaction in force, and the events
above a watermark. `internal/filestore` can implement them by scanning, like
it does everything else, and the package comment's "never compacts" describes
the file on disk and stays true.

PROJECTION.md gains a section, and loses the sentence saying a note is one
line.

Rendering follows: `RenderChat` and devchat's transcript both draw a session
as a sequence of arcs, and a compacted session is a summary block followed by
arcs.

Open questions
--------------------
*What the budget is measured in.* Reported input tokens are the honest
measure and are the provider's, arriving one turn late and absent when a
provider reports nothing. An estimate from the transcript is available
immediately and is wrong by an unknown amount. The first is probably right
and the nil case needs an answer.

*Whether a summary is one block or several.* One block per compaction is
simplest and means the second compaction summarises a summary, losing a
little more each time. Keeping each generation as its own block bounds the
loss and grows the head of the transcript without bound. Nothing here decides
it.

*What the compactor is told about the tail.* The summary is better if it
knows what the conversation went on to be about, and the tail is exactly the
material that is not being summarised. Passing it costs tokens on a call made
to save tokens.

*Whether an arc may be compacted mid-session by the agent's own choice.* The
model knows better than a token count which of its history matters. It is
also the party with an interest in keeping it. Not now.

*What the expiry should be.* Long enough that a healthy compaction is never
disregarded while it is still working, short enough that a dead worker does
not hold the session past a person's patience. Those pull in opposite
directions and the gap between them is how long a crash costs. A measured
distribution of compaction times decides it; a guess does not.

*Whether outstanding tasks are carried or the watermark stays pinned.* Under
the async model as it stands, pinning is free and carrying is unnecessary.
Under ASYNC-TASKS.md, pinning is unavailable and carrying is required. Which
is right therefore depends on a decision that has not been made, and building
the carry before that decision is building for a design that may not land.

*Whether the hard budget is reachable at all.* If the soft budget is set
well, the session never blocks, and the whole of Where a blocked arc waits is
machinery for a case that does not arise. Keeping it costs a held message and
some code on a path nobody takes; dropping it means a session that does reach
the limit fails its turns instead. It should be built and then measured, and
the measurement is what decides whether it stays.
