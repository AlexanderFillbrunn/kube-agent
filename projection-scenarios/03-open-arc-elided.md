Open arc, elided
================

Spec: [Foreign arcs](../PROJECTION.md#foreign-arcs)

The same log with `foreign_arc: elide`.

    Modes     foreign_arc=elide
    Arc       U2
    Events    U1, U2

Messages
--------

    1  u(U2)      "also, describe node n1"

Notes
-----

- The transcript is what it would have been if U1 had never been asked.

- U1 is never disclosed, so no message the model produces can depend on it,
  and every later projection drops it too. One mode governs U1 whether it is
  open, dead or withdrawn, so nothing about its status can change that. See
  [05-dead-arc-elided.md](05-dead-arc-elided.md) for the same setting on an arc
  that died.

- A user whose request fails quietly may never be told. Eliding open arcs is a
  decision about the whole session.
