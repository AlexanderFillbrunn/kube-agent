Open arc, reduced to a note
===========================

Spec: [Foreign arcs](../PROJECTION.md#foreign-arcs)

The log of [01-open-arc-kept.md](01-open-arc-kept.md) with
`foreign_arc: note`. The projector reports the open arc instead of replaying
it.

    Modes     foreign_arc=note
    Arc       U2
    Events    U1, U2

Messages
--------

    1  n{inflight:U1}   "[an earlier request, "how many pods are pending?", is
                          still being worked on. It is not yours to answer and
                          its result is not available yet.]"

    2  u(U2)            "also, describe node n1"

Notes
-----

- The note carries U1's words. A note is synthesized text, so quoting costs the
  same tokens `keep` would have spent and adds the status.

- `keep` and `note` differ in who speaks, not in how much is said. Under
  `keep`, `u(U1)` is a request addressed to the model. Under `note`, the same
  words are a report about somebody else's work.

- The vocabulary puts `keep` above `note` even here, where the note discloses
  more text. `keep` hands over the record. `note` hands over an account of it,
  which can be rewritten.
