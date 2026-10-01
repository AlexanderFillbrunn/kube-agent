Re-entry
========

Status
--------------------
Half built. The lease and the worker are in `pkg/agent/arcwork.go`. The queue
that drives them is a proposal, and nothing implements it.

An arc is opened once and worked more than once. This document covers the
second part: who may run an arc that already exists, how two runs are kept off
one, and how the work of running one reaches a worker.

The three ways in
--------------------
An arc is entered for more than one reason.

    a person speaks          opens the arc and runs it
    a task reports           picks up an arc that exists
    a human answers an ask   picks up an arc that exists

Only the first opens an arc. All three do the same work: hold the arc, replay
it, run the agent. That is `ArcWorker.Work`, and `ArcLease` is what keeps two
runs off one arc.

Every way in shares one worker, and therefore one lease. A caller that builds
its own worker has built its own lease and excludes nobody.

A person's turn is not special. Nothing about somebody speaking makes it safe
to run beside a task report on the same arc, so `session.turn` acquires the
lease like everything else.

The invariant
--------------------
*Every write that changes what an arc has to do must be followed by a run of
that arc, and no trigger may be lost.*

A run replays the arc once and decides from that replay. A run that started
before a write cannot act on it, and no writer knows whether its write is the
one that unblocks the arc: writes arrive from interfaces this code does not
own, in any order, together or minutes apart.

So the writer owes the arc a run, every time. [INTERRUPTS.md](INTERRUPTS.md)
works an example through, where three approvals produce three runs and only
the last one does anything.

This produces runs that do nothing, and that is the design working. Each one
costs a replay. A single lost trigger strands the arc, with the last run that
ended returning nil to say it finished.

The queue
--------------------
Re-entry is driven by a queue carrying one message.

*The item is "this arc needs a run".* It is not a command. The run decides from
the log what the item means, so an item that warrants nothing costs a read.
That is what makes a duplicate safe.

*Duplicates are safe, so an implementation may collapse them. It may only
collapse items nobody has claimed.* A claim pins a run to a point in the log. A
write landing after that point needs a delivery of its own, including while
the claim is still in flight. Dedupe keyed on a time window breaks this. SQS
`MessageDeduplicationId` is such a window and must not be used here.

*An item that cannot be worked goes back. It is never dropped.* A worker that
finds the arc held cannot tell a holder that will cover the item from one that
replayed too early, so it releases the item rather than settling it.

*A worker never waits on a held arc.* Waiting blocks a worker on one arc while
others go unserved, and the waiters are redundant with each other.

Partitioning by arc ID is worth it where the transport offers it, with SQS FIFO
`MessageGroupId` as the closest fit. It serializes an arc's items at delivery,
which turns the release path from the common case into the recovery case. It
does not replace the lease. A visibility timeout expires because a run is slow,
not because a worker died, and entry points that never touch the queue are not
partitioned by it.
Interfaces
--------------------
Producer and consumer are separate. Nothing holds both, and the split falls on
the deployment boundary: an API tier enqueues, a worker tier claims. It also
keeps `Claim` out of reach of the interfaces that call `Decide`.

    // ArcQueue is where an arc is put when it needs a run.
    type ArcQueue interface {
        Enqueue(ctx context.Context, arcID string) error
    }

    // ArcClaims hands out arcs that need a run, one at a time.
    type ArcClaims interface {
        Claim(ctx context.Context) (Claim, error)
    }

    // Claim is one arc taken off the queue and held until it is settled. A
    // claim nobody settles comes back.
    type Claim interface {
        ArcID() string

        // Attempt counts how often this arc was handed out and given back. A
        // backoff is computed from it, so the policy stays with the worker.
        Attempt() int

        // Done drops the claim. The arc was replayed, so the trigger is spent.
        Done(ctx context.Context) error

        // Release gives the arc back, claimable no sooner than after delay.
        // The delay is a hint. An implementation that cannot honor it says so.
        Release(ctx context.Context, delay time.Duration) error
    }

`Enqueue` takes no delay. The only delay the design needs is on the retry path,
and that lives on the claim.

`ArcLease` does not change and does not merge into the queue. A person's turn
enters through `Agent.Run` and an in-process task through `Report`, and neither
passes the queue.
The worker loop
--------------------
One loop holds the release rule for every transport.

    for {
        claim, err := claims.Claim(ctx)
        if err != nil {
            return err
        }
        worked, err := w.Work(ctx, claim.ArcID())
        switch {
        case err != nil:
            // The arc was not replayed. Settling the claim would spend a
            // trigger on a run that did not happen.
            claim.Release(ctx, backoff.For(claim.Attempt()))
        case !worked:
            // Somebody holds the arc, and they may have replayed before the
            // write this item stands for.
            claim.Release(ctx, backoff.For(claim.Attempt()))
        default:
            claim.Done(ctx)
        }
    }

An arc that fails repeatedly goes somewhere a human looks. It is never
discarded, because a discarded item is a lost trigger.
Implementations
--------------------
    MemoryQueue       a timer per claim      a set of unclaimed arc IDs
    Redis             a reaper over inflight member uniqueness, ZADD LT
    SQS FIFO          visibility timeout     none

For Redis, a sorted set scored by ready time gives dedupe and delay in one
structure. `ZADD LT` is required rather than plain `ZADD`: a fresh response
arriving against an item that is backed off must move it earlier, and plain
`ZADD` overwrites the score in either direction. Claim is a Lua script over
`ZRANGEBYSCORE`, `ZREM` and an insert into an inflight set, so that a claim
cannot interleave with a merge. `Enqueue` adds to the pending set even when the
arc is in flight, which can produce a duplicate when the reaper returns the
in-flight entry. That is the correct trade, since a duplicate is safe and a
lost trigger is not.

For the harness, `MemoryQueue` satisfies both interfaces. A crash is not a case
there.
