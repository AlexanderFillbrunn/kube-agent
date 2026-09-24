A cancel reached a call that had already been dispatched
========================================================

Spec: [What a cancel settles](../PROJECTION.md#what-a-cancel-settles)

U1 had two calls out when a human ended it. One had reached its tool, the
other had not.

    Modes     foreign_arc=keep
    Arc       U2
    Events    U1, R1.1, T1.1.1, T1.1.2, D1.1.1, C1.1x, U2

Messages
--------

    1  u(U1)                "restart the ingress controller and show me the
                              pods"

    2  a(R1.1)              "restarting it now."
       [t(T1.1.1)]          restart_deployment{name: ingress-nginx,
                                                namespace: kube-system}
       [t(T1.1.2)]          list_pods{namespace: kube-system}

    3  r(T1.1.1:pending)    "[this call was handed to its tool and then
                              stopped before it reported. Whether it took
                              effect is not known.]"

    4  r(T1.1.2:cancel)     "[this call was cancelled before it ran]"

    5  n{stopped:U1}        "[a human stopped this request while it was
                              running. The approach above did not
                              finish.]"

    6  u(U2)                "is it back up?"

Notes
-----

- T1.1.1 was dispatched, so the cancel does not settle it. It stays PENDING in
  the DISPATCHED phase and message 3 is synthesized by `pending: stand_in`,
  not derived from a status.

- T1.1.2 was never dispatched, so the cancel settles it and message 4 is
  derived. The two calls were stopped by the same cancel and end differently.

- Message 3 is the one thing the log cannot say. Rendering it as cancelled
  would tell the model the restart did not happen, and a model told that
  restarts it again.

- If T1.1.1's tool reports later, that result settles the call and message 3
  becomes the result itself. An arc projected before that point still reads
  "not known", which was true when it was projected.

- Under `pending: elide` message 3 and the call that requested it both
  disappear, and the model is told nothing about work that may have changed
  the cluster. `stand_in` is the default for this reason.
