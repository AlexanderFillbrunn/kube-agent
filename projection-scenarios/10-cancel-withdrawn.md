A request withdrawn before anything happened
============================================

Spec: [Cancellation](../PROJECTION.md#cancellation)

U1 was cancelled before the agent responded.

    Modes     foreign_arc=elide
    Arc       U2
    Events    U1, C1.1x, U2

Messages
--------

    1  u(U2)      "describe node n1"

Notes
-----

- Nothing was attempted, so there is no correction to carry and no approach the
  model might resume. This is the only cancel a mode governs at all.

- Under `foreign_arc: note`:

        1  n{withdrawn:U1}   "[an earlier request was withdrawn before it was
                               acted on]"

        2  u(U2)             "describe node n1"

  The note records that the user asked and changed their mind, which is
  sometimes the context for the next request.

- The three cancels under one mode setting:

        too late    always elided, because it stopped nothing
        stopped     always disclosed, because it stopped something
        withdrawn   the mode decides
