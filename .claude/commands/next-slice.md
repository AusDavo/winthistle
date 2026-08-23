---
description: Start the next slice of the web UI, with the decisions already made taken as settled
---

<!--
Written for the slice after "Serve one screen, and make the three decisions
carry guards" (6ac6a52). The framing near the bottom — what to take as settled,
what not to re-derive, the harness notes — outlives any one slice. The seam
signatures and the five difficulties do not: rewrite them when this slice lands,
or delete this file if the UI has moved past it.
-->

Continue item 1 of `HANDOFF.md`'s "Next actions": sub-items **1.1 (the four
callback seams and the `POST` that starts a run)** and **1.2 (the explicit abort
control on the run screen)**. Stop there for review — no transports, no QR, no
countdown, no remaining screens in this slice.

**Take as settled and don't re-litigate.** The three decisions and their guards
are in HANDOFF's "The server, and the three decisions with guards on them", and
`internal/server`'s package comment repeats them. In particular: `run.Do` stays a
straight-line blocking function driven by a goroutine; a closing tab does not
abort; screens are served verbatim in a `<pre>` and `screen()` is the oracle.

**The four seams, with the signatures they already have:**

- `rehearsal.Signer` — `func(ctx, psbtB64 string) (combine.Part, error)`, one per
  `rehearsal.Device`
- `abort.Confirmation` — `func(ctx, abort.BluntRequest) (bool, error)`
- `bump.Approve` — `func(ctx, question string) (bool, error)`
- `setup.Ask` — `func(ctx, coldwallet.AddressCheck) (setup.Answer, error)`,
  three-way

**Five things I expect to be the actual difficulty. Decide each and write down
the guard, the way the last slice did:**

1. *A browser can stop answering.* A seam waiting on a channel must not block
   `run.Do` forever, and the bound must be the clock decision 2 names — the 5:00
   gate `rehearsal.Gate` measured and the peers' ten minutes — not an arbitrary
   transport timeout. Say what stops the second thing becoming the first.
2. *`setup.Ask` is three-way because `NotAnswered` must never be recorded as a
   verdict.* A web form that is abandoned has to produce `NotAnswered`, and
   `NotAnswered` must not reach the `setups` table as an answer. That is the
   highest-stakes seam of the four.
3. *`abort.Confirmation` is a func per channel, and nil means never escalate.* A
   handler must not turn it into a set-once checkbox — the func type exists to
   prevent exactly that. See the comment on it.
4. *The `POST` that starts a run is this repo's first unsafe method.* The guard
   already refuses one that cannot say where it came from, so that part is done;
   decide what refuses a **second** concurrent run, given one journal and one
   armed window.
5. *`server.Registry` gets its first non-test caller.* The run's context must not
   be `r.Context()` — that is decision 2's whole point, and the `doctor` handler
   is the one place a request context legitimately is used.

**Guards to write down explicitly**, because HANDOFF asks for a guard rather than
a promise: what stops a seam handler becoming a third
`WalletKit.PublishTransaction` call site (there are already two locks — the
pinned count and the import ban in `imports_test.go`); what stops `NotAnswered`
being journalled; and what stops the abort control being offered for a run that
reached the publish call (`journal.ErrMayBePublished`, which `run.RecoverOne`
already refuses).

**Don't re-derive:** the countdown is the *peers'* clock, because
`pruneZombieReservations` skips PSBT reservations; `internal/signers` already has
two working transports and the browser is a third rather than a replacement;
`--stop-before-publish` is one `if` between `arm.Finalize` and `arm.Publish`,
read nowhere else.

**Harness.** If regtest tests fail with "Requested wallet does not exist" or
"wallet is not fully synced", the container restarted: `make -C regtest
bootstrap` reloads the cold wallets, then mine a few blocks. But if signing fails
with *"no device added a signature — every device returned the packet it was
given"*, that is a different fault — `cold1`/`cold2`'s keys and `cold-watch`'s
descriptors are from different generations, and only `make -C regtest reset`
(~1 min) fixes it. Bootstrap will not.

**No browser is installed on this machine**, so the cookie-then-redirect flow and
the CSP are currently proven by `curl` and headers rather than by a render.
`npx @playwright/mcp install-browser chrome-for-testing` if you want the render
check; say plainly which of the two you did.
