An approval that let the call run
=================================

Spec: [Interrupts](../PROJECTION.md#interrupts)

A human was asked before the call ran and said yes. The call then ran like any
other, so the log's only trace of the human is the interrupt itself.

    Modes     interrupt=note
    Arc       U2
    Events    U1, R1.1, T1.1.1, I1.1.1, IR1.1.1+, TR1.1.1, R1.2, K1x, U2

Messages
--------

    1  u(U1)                "drain node n1 so I can patch it"

    2  a(R1.1)              "n1 has 14 pods to evict. I need approval
                              first."
       [t(T1.1.1)]          kubectl_drain{node: n1}

    3  r(T1.1.1:ok)         "node/n1 drained"

    4  n{ask:I1.1.1}        "[interrupt i1.1.1: a human was asked to approve
                              this call before it runs: draining evicts 14
                              running pods. They were shown: {"node": "n1",
                              "pods_evicted": 14}]"

    5  n{approved:I1.1.1}   "[interrupt i1.1.1: a human approved it]"

    6  a(R1.2)              "n1 is drained and ready to patch."

    7  u(U2)                "thanks"

Notes
-----

- An approval leaves no mark on the call. T1.1.1 ran and returned exactly as
  it would have if nobody had been asked, so message 3 says nothing about the
  human. Notes are the only place the approval can be told, and `interrupt`
  governs them. Under `interrupt: elide` this transcript is messages 1, 2, 3,
  6 and 7, and nothing records that a person was involved.

- Messages 4 and 5 sit after message 3 even though both events happened before
  the result arrived. A call and its result travel together, so the pair is
  emitted where the call was made and anything else at its own position lands
  after it. See [Ordering](../PROJECTION.md#ordering).

- Message 4 carries `Reason` and `Context`. The model is told what the human
  was shown, so a refusal it reads later is something it can reason about
  rather than guess at.

- Message 4 is byte-identical in every transcript that contains it. It is
  built from fields that are final when the interrupt is raised, so message 5
  arriving is an append and not a rewrite. See
  [14-interrupt-unanswered.md](14-interrupt-unanswered.md), where the same ask
  appears with nothing after it.
