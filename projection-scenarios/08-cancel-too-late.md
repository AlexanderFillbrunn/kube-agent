A cancel that arrived too late
==============================

Spec: [Cancellation](../PROJECTION.md#cancellation)

U1 ran to its answer, the agent closed it, and the cancel landed afterwards.

    Modes     foreign_arc=elide
    Arc       U2
    Events    U1, R1.1, T1.1.1, TR1.1.1, R1.2, K1x, C1.1, U2

Messages
--------

    1  u(U1)                "how many pods are pending?"

    2  a(R1.1)              "let me check the cluster"
       [t(T1.1.1)]          kubectl_get{kind: pod, status: Pending}

    3  r(T1.1.1:ok)         "6 pods pending, 4 of them in namespace batch"

    4  a(R1.2)              "6 pods are pending. Most are batch jobs
                              waiting on quota."

    5  u(U2)                "also, describe node n1"

Notes
-----

- `K1x` closed the arc and `C1.1` carries no `x`. C1.1 has
  `TerminatedWork: false`, because the arc was already closed. No pending
  calls, no call waiting on a human, no running tasks.

- Always elided, whatever `foreign_arc` says. Projecting it would tell the model
  that a fully answered request was cancelled, and the answer in message 4
  stands.

- `foreign_arc: note` produces the same transcript. This case is not the mode's
  subject. Only a withdrawal is. See
  [10-cancel-withdrawn.md](10-cancel-withdrawn.md).

- R1.2 follows R1.1 because R1.1 requested a call and message 3 settled it.
  K1 follows R1.2 because R1.2 requested nothing and left nothing running, so
  the agent had no reason to be invoked again.
