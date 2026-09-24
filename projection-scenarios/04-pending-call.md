A call still running at the cutoff
==================================

Spec: [Repairs](../PROJECTION.md#repairs)

U1 called a tool and the call had not returned when U2 opened.

    Modes     foreign_arc=keep, pending=stand_in
    Arc       U2
    Events    U1, R1.1, T1.1.1, U2

Messages
--------

    1  u(U1)                "how many pods are pending?"

    2  a(R1.1)              "let me check the cluster"
       [t(T1.1.1)]          kubectl_get{kind: pod, status: Pending}

    3  r{T1.1.1:pending}    "[this call has not returned yet]"

    4  n{inflight:U1}       "[the earlier request about pending pods is still
                              being worked on. The call above has not come
                              back, and its answer is not available yet.]"

    5  u(U2)                "also, describe node n1"

Notes
-----

- Message 3 is synthesized and message 2 is not. The log says nothing about
  this call's outcome. The provider requires an answer to every call.

- The stand-in is per call. The note is per arc. Three pending calls under U1
  produce three stand-ins and one note.

- `foreign_arc: keep` gives both the words and the status here, which it
  cannot do in [02-open-arc-noted.md](02-open-arc-noted.md). U1 now has a
  record under it and `pending` speaks for that record.

- `:pending` does not race. U2's cutoff excludes every event after U2's user
  message. The result appears in a later transcript and the treatment moves
  from `stand_in` to `keep`. See
  [11-surrounding-arc.md](11-surrounding-arc.md).
