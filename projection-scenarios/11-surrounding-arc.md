One arc surrounding another
===========================

Spec: [Ordering](../PROJECTION.md#ordering)

U1 opens, U2 opens and finishes inside it, U1 finishes, U3 opens.

    Modes     foreign_arc=keep, pending=stand_in
    Arc       U3
    Events    U1, R1.1, T1.1.1, U2, R2.1, K2x, TR1.1.1, R1.2, K1x, U3

Messages
--------

    1  u(U1)                "how many pods are pending?"

    2  a(R1.1)              "let me check the cluster"
       [t(T1.1.1)]          kubectl_get{kind: pod, status: Pending}

    3  r(T1.1.1:ok)         "6 pods pending, 4 of them in namespace batch"

    4  u(U2)                "also, describe node n1"

    5  a(R2.1)              "n1 is Ready, 14 pods scheduled, 62% memory."

    6  a(R1.2)              "6 pods are pending. Most are batch jobs
                              waiting on quota."

    7  u(U3)                "and the ingress?"

Notes
-----

- Message 3 carries the real result. U2's turn was shown
  `r{T1.1.1:pending}`, because at that cutoff the call had not returned. U3's
  cutoff includes TR1.1.1, so the treatment moves from `stand_in` to `keep`.
  Two consecutive transcripts may disagree about this call.

- Message 6 stays where it is. R1.2 happened after R2.1. R2.1 was written while
  U1 was unfinished and may say so.

- Message 3 is the ordering exception. TR1.1.1 arrived after R2.1, but the
  provider requires a call and its result to be contiguous, so the pair sits
  where the call was made. This hides the fact that U2's whole exchange
  happened while the call was outstanding.

- Messages 5 and 6 are two assistant messages in a row. A provider requiring
  alternation rejects this. Not repaired. Two assistant messages are only
  adjacent when the first one's calls were elided or it had none, because a
  surviving call puts its result between them.
