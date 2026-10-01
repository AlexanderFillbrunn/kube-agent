Interrupts
==========

Status
--------------------
A proposal, not a specification. [PROJECTION.md](PROJECTION.md) and
[ENCODING.md](ENCODING.md) describe what the code does.

Interrupts are a special case among the proposals. Half of one is already
built and specified: the two event types, the fold, the phase, the arc status,
and the projection notes all exist and are tested. What is missing is the
other half. Nothing in `pkg/agent` ever raises an interrupt, and nothing in
`cmd/devchat` can answer one.

This document covers that half: how the agent asks, and how a human answers.
What happens to the arc once the answer is in the log is the same question a
task report raises, and it is [RE-ENTRY.md](RE-ENTRY.md).

What is already there
--------------------
`INTERRUPT` names one tool call and holds it. `INTERRUPT_RESPONSE` settles it.
A refusal settles the call `REFUSED` and carries the human's words as the
call's result. An approval leaves the call pending, so it runs.

    pkg/events/payloads.go      InterruptPayload, InterruptResponsePayload
    pkg/history/history.go      applyInterrupt, applyInterruptResponse
    pkg/history/history.go      ToolCallPhaseAwaitingApproval, ArcStatusInterrupted
    pkg/history/projection.go   projectInterrupt, the ask and approval notes

Two properties of the fold carry most of the design below.

*A pending interrupt is a property of the arc, not of one call.* `Arc.Status()`
returns `INTERRUPTED` while any call in the arc waits on a human. The agent
reads that status before every step, so one unanswered ask stops every call in
the arc, including calls a human already approved.

*A refusal releases the gate.* A refused call is settled and no longer pending,
so an arc where one call is refused and two are approved returns to `PENDING`
and runs the two.

The gate
--------------------
Raising an interrupt ends the run. Answering it starts a new one.

No run waits on a human. The agent folds the `INTERRUPT` event, the arc status
becomes `INTERRUPTED`, `deriveArcState` returns `ArcStateDone`, and `Run`
returns nil. Whatever happens next happens in another run, built from the log.

    U1, R1.1, I1.1.1, I1.1.2, I1.1.3     the run raises three asks and ends
    IR1.1.1+, IR1.1.2+                   two approvals. the arc is still held
    IR1.1.3-                             a refusal. the gate opens
    T1.1.1, T1.1.2, R1.2, K1x            a later run executes the two approvals

Nothing runs until every ask is answered. That is the arc-wide status doing the
work, and it needs no code of its own.

Asking for several calls at once
--------------------
One assistant message can request several calls, and a human should decide on
them together. The asks are therefore raised together, in one pass before any
call runs.

The pass is needed. The call loop in `toolStep` re-reads the arc status before
each call, so an interrupt raised inside the loop would stop the loop before
the second ask was written. The result would be one ask per run, which is a bad
interface and more runs than the work needs.

    pending := collect()
    if raised, err := a.raiseApprovals(ctx, pending, state); err != nil || raised > 0 {
        return err
    }
    // the existing loop, unchanged

The status check inside the loop stays. It is what catches a cancel recorded
somewhere else.

Each ask is still one event naming one call. A single interrupt carrying n
calls was considered and rejected. `InterruptResponsePayload` has one
`IsRejected`, so approving two of three calls would need a per-call verdict
inside the response, without the fold's interrupt index to resolve it. The
refusal text would also be shared by calls that were refused for different
reasons. Grouping is a question for the interface, and an interface can group:
every interrupt names a call, and every call names the message that requested
it.

The policy
--------------------
An agent option supplies the rule. The agent asks it once per pending call.

    type ApprovalPolicy interface {
        // Approval returns nil for a call that may run, and the terms of the
        // ask for one that may not.
        Approval(ctx context.Context, call *history.ToolCall) (*Approval, error)
    }

    type Approval struct {
        Reason  string
        Context any
    }

The policy reads the call and nothing else. It is consulted again on every
resumed run, so a policy that answers differently on the second reading asks
twice about a call a human already decided. A call that already carries an
interrupt is skipped before the policy is reached, because the fold refuses a
second interrupt on one call.

`Reason` and `Context` are what the human is shown. The projection puts both in
the ask note, so the model is told what the human was told and can account for
a refusal without guessing.

Answering an ask
--------------------
A decision is written and the arc is re-entered in one call, so that a caller
cannot write a response and forget the run. It is the counterpart of
`TaskReports.Report`, over the same `ArcWorker` and the same lease, which
[RE-ENTRY.md](RE-ENTRY.md) describes.

    type InterruptRef struct{ ArcID, ID string }

    func (w *ArcWorker) Decide(ctx context.Context, ref InterruptRef, d Decision) (bool, error)

`Interrupt.CorrelationID` carries the arc. The reference is checked rather than
believed, as `holdsTask` checks a task reference, because it arrives from
outside the process.

The invariant
--------------------
Every interrupt response must be followed by a run of its arc, and no trigger
may be lost. That is the general rule in [RE-ENTRY.md](RE-ENTRY.md), and
interrupts are its sharpest case: no response knows whether it is the one that
opens the gate.

This produces runs that do nothing, and that is the design working.

    IR1.1.1+ written, arc enqueued
    run replays. 1.1.2 and 1.1.3 are still pending, so the run does nothing
    IR1.1.2+ written, arc enqueued
    run replays. 1.1.3 is still pending, so the run does nothing
    IR1.1.3+ written, arc enqueued
    run replays. the gate is open, and the three calls run

A single lost trigger strands the arc here with every call approved, no run
scheduled, and nothing on the screen to say so.

What the interface still owes
--------------------
`cmd/devchat` is the harness and is not the product, but it is where this is
exercised first.

    fromHistory and stateOf ignore interrupts, so a session reopened on a log
    with a pending ask shows a queued call and offers no way to answer it

    transcript.event handles neither interrupt payload, and there is no tool
    state or glyph for a call that waits on a person

    the model has two states, running and not. An arc that waits on a human is
    neither, and nothing stops a user from opening a new arc and stranding it

    m.owed and the taskOwedMsg requeue are the in-process instance of the
    queue in RE-ENTRY.md. They requeue with no delay, which is a hot loop
    while a task goroutine holds the lease

`DefaultModes` sets `Interrupt` to `TreatmentElide`, so the model is not told a
human was asked. A session that gates calls should pass `TreatmentNote`, or an
approved restart reads to the model as a call that simply ran. A refusal
reaches the model either way, as the call's result.
