A refusal that answered the call
================================

Spec: [Interrupts](../PROJECTION.md#interrupts)

A human was asked before the call ran and said no. The call never ran, and the
refusal is what answers it.

    Modes     interrupt=elide
    Arc       U2
    Events    U1, R1.1, T1.1.1, I1.1.1, IR1.1.1-, R1.2, K1x, U2

Messages
--------

    1  u(U1)                 "delete the failed jobs in batch"

    2  a(R1.1)               "12 jobs match. I need approval before
                               deleting."
       [t(T1.1.1)]           kubectl_delete{kind: job, namespace: batch}

    3  r(T1.1.1:refused)     "[a human refused this call: leave the ones from
                               today]"

    4  a(R1.2)               "I left them in place."

    5  u(U2)                 "just delete the ones older than a week"

Notes
-----

- The mode says elide and the refusal is disclosed anyway. No mode reaches it,
  because no mode reaches a tool result: the call has to be answered and this
  is the answer.

- There is no ask note and no decision note. Message 3 carries both facts, and
  a note repeating them would tell the model the same thing twice. Compare
  [12-interrupt-approved.md](12-interrupt-approved.md), where an approval
  needs notes because the call itself records nothing.

- The outcome is `REFUSED`, not `BLOCKED`. A policy refusing a call asks
  nobody, and the model has nobody to go back to; a human refusing one is
  somebody it can ask again with a narrower request, which is what U2 is.

- `IsError` is set, so a provider sees a failed call. The distinction between
  the ways a call can fail lives in `ModelToolMessage.Outcome` and in the
  wording of message 3.

- The human's own words are quoted. `InterruptResponse.Response` was "leave
  the ones from today", which is why U2 asks for a week instead. Without it
  message 3 reads "[a human refused this call]" and the correction in U2 has
  no antecedent.
