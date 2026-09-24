An ask nobody has answered yet
==============================

Spec: [Interrupts](../PROJECTION.md#interrupts)

U1 is waiting on a human when U2 opens. The decision is outstanding at the
cutoff, so the transcript holds the ask and nothing that answers it.

    Modes     foreign_arc=keep, pending=stand_in, interrupt=note
    Arc       U2
    Events    U1, R1.1, T1.1.1, I1.1.1, U2

Messages
--------

    1  u(U1)                "drain node n1 so I can patch it"

    2  a(R1.1)              "n1 has 14 pods to evict. I need approval
                              first."
       [t(T1.1.1)]          kubectl_drain{node: n1}

    3  r{T1.1.1:pending}    "[this call is waiting for a human to approve
                              it]"

    4  n{inflight:U1}       "[another request is still being worked on:
                              "drain node n1 so I can patch it". It is not
                              yours to answer and its result is not available
                              yet.]"

    5  n{ask:I1.1.1}        "[interrupt i1.1.1: a human was asked to approve
                              this call before it runs: draining evicts 14
                              running pods]"

    6  u(U2)                "meanwhile, how many pods are pending?"

Notes
-----

- Message 3 is a stand-in and says which kind of waiting this is. A call
  nobody has answered and a call nobody has approved are both unanswered, and
  only one of them is something a person can unblock. Compare message 3 in
  [04-pending-call.md](04-pending-call.md), which says "[this call has not
  returned yet]" because there is nobody to chase.

- Message 5 is the same ask that appears in
  [12-interrupt-approved.md](12-interrupt-approved.md). When the decision
  arrives, the approval is appended at its own position and this message does
  not change, so the prefix a provider has cached survives the answer.

- Message 4 comes before message 5. The in-flight note belongs to the arc and
  sits after its last response; the ask belongs to the interrupt and sits
  where the interrupt was raised, which is later in the log. Both describe the
  same wait from different sides.

- The arc is open, so `foreign_arc` governs it. `ArcStatusInterrupted` is
  what an arc holding an unanswered ask reports, and it is derived from the
  call rather than stored, because a call is the only thing an interrupt can
  hold up.
