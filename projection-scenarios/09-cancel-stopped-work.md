The user stopped the agent mid-approach
=======================================

Spec: [Cancellation](../PROJECTION.md#cancellation)

U1 was working when a human ended it.

    Modes     foreign_arc=elide
    Arc       U2
    Events    U1, R1.1, T1.1.1, C1.1x, U2

Messages
--------

    1  u(U1)                "clean up the failed jobs in batch"

    2  a(R1.1)              "I'll delete every job in batch with status
                              Failed."
       [t(T1.1.1)]          kubectl_delete{kind: job, namespace: batch,
                                            selector: status=Failed}

    3  r(T1.1.1:cancel)     "[cancelled before this call completed]"

    4  n{stopped:U1}        "[a human stopped this request while it was
                              running. The approach above did not
                              finish.]"

    5  u(U2)                "delete only the ones older than a week"

Notes
-----

- The mode says elide and is overruled. A cancel that stopped work in progress
  is always disclosed.

- Drop messages 2 through 4 and U2 reads as a fresh narrow request against a
  clean slate. Keep them and U2 reads as a correction.

- Disclosure is all or nothing. Work without the note reads as an approach to
  resume. The note without the work refers to nothing.

- `Cancel.Reason` was empty, so message 4 states that a human stopped this and
  not why. Quote `Reason` where it is present.

- Message 3 is derived. The fold records the call as CANCELLED with the
  cancel's reason as its result. Compare message 3 in
  [04-pending-call.md](04-pending-call.md), which is synthesized.
