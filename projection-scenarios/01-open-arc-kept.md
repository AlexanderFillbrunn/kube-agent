Open arc, kept
==============

Spec: [Foreign arcs](../PROJECTION.md#foreign-arcs)

U1 is still being worked on when U2 opens. `foreign_arc: keep` projects an
open foreign arc like any other conversation, and reports its status.

    Modes     foreign_arc=keep
    Arc       U2
    Events    U1, U2

Messages
--------

    1  u(U1)            "how many pods are pending?"

    2  n{inflight:U1}   "[another request is still being worked on: "how many
                          pods are pending?". It is not yours to answer and its
                          result is not available yet.]"

    3  u(U2)            "also, describe node n1"

Notes
-----

- U1 has no response, call or task, so no fine-grained mode has a record to
  govern. The note is the only thing in the transcript about the first request.

- Message 1 on its own is a user turn, unanswered, directly before the request
  the model must answer, and it reads as a request addressed to this turn.
  Message 2 is what separates them. An assistant message belongs to exactly one
  arc, so a model that answers both files the answer to U1 under U2 and leaves
  U1 open with no response of its own.

- `keep` discloses the arc's material and reports its status, here and for an
  arc with material to show. See [02-open-arc-noted.md](02-open-arc-noted.md)
  for the status alone.

- Use `keep` when the model should see the request in the user's own words. Use
  `note` when an account of it is enough.

- Messages 1 and 3 are two user-role messages with a role-less note between
  them. On a provider requiring alternation the encoder merges them, labelling
  each part with its origin. Not repaired by the projection.
