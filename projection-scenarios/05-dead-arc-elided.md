An arc closed by a fatal error, dropped
=======================================

Spec: [Foreign arcs](../PROJECTION.md#foreign-arcs)

U1 failed before U2 was asked. A fatal error closes its arc, but a closed-by-
failure arc is not one the log shows as *finished*, so `foreign_arc` governs it
like any other arc still outstanding.

    Modes     foreign_arc=elide
    Arc       U2
    Events    U1, R1.1, FE1.1x, U2

Messages
--------

    1  u(U2)      "describe node n1"

Why
---

The arc goes as a unit: the user message, the response and the error. Keeping
`u(U1)` and dropping the rest would leave a request in the transcript that the
model can neither answer nor account for.

One mode governs U1 whether it is open, dead or withdrawn, so this transcript
is the same wherever the failure sits in the log.

The same setting drops an arc that is merely open, which is
[03-open-arc-elided.md](03-open-arc-elided.md). Under `keep` or `note` the two
differ in one word: `n{dead:U1}` here, `n{inflight:U1}` there.

Whether the user is ever told that their request failed is a separate question.
The transcript is what the model reads, and the model cannot repeat an attempt
it never saw.
