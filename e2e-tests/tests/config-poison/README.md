# config-poison (kuttl TestCase — SKELETON, owner: OPS-8.4, NEGATIVE, release-gated)

NEGATIVE case (arch §4.1 row 7). Set an invalid LIVE key → `CONFIG SET` fails →
`LiveConfigApplied=False`, blocks progress, NO pod roll, no crash-loop.

Suggested flow:

1. `00`–`02` durable cluster Ready.
2. `03-poison` — patch `spec.config.maxmemory: notanumber` (an invalid live-settable
   value).
3. `03-assert` — the `LiveConfigApplied` condition is `False` with the engine's error in
   the message; NO StatefulSet roll occurs; the operator does not crash-loop.
4. `04-revert` — restore a valid `maxmemory`.
5. `04-assert` — `LiveConfigApplied` clears to `True` on the next reconcile; `state: Ready`.

Asserts the CONDITION and the absence of a roll — not an alert cadence (that retry/paging
policy is OPEN QUESTION #3).
