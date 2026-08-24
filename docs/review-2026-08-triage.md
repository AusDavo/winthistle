# Phase 0 triage of `docs/review-2026-08.md`

Read against the source at `c6037ff`. Nothing in this document was
implemented; it is the classification the review's Phase 0 asks for and
stops there.

The review is honest about its provenance, and the provenance shows. It was
written from a README whose status blockquote is **stale** — it says the web
interface "does not exist yet" while `internal/server` is 4,558 lines and can
open a batch — so the review consistently underestimates what is built and
occasionally proposes a second implementation of something that already exists
with tests. That is a defect in the README before it is a defect in the review,
and it is the single highest-value item in the whole document (item 6, bullet 7).

Two of the review's own scoping rules turn out to do most of the work:
"does this make it harder to audit" kills item 4 as specified, and the
implementation limit on item 2b is aimed at code that already exists.

---

## The two load-bearing assumptions

### 1. Phase structure — **partly wrong, and the error matters**

The review assumes `build → arm (finalize n, collect n receipts) → publish`.
The real sequence is in `run.Do` (`internal/run/run.go`), and it differs in two
ways that change several findings.

The actual shape:

| | what | where |
|---|---|---|
| **Phase 0** — no clock running | cold-wallet setup answer read back; `GetInfo`/chain-sync; peer pre-flight; optional shim probe + hold gate; fee estimate; coin selection + fence; change address; anchor reserve + top-up; **dress rehearsal + gate** | `run.prepare`, steps 0–6 |
| **Phase 1** — the armed window | `arm.Open` (n streams, n peer clocks start) → `journal.Begin` → `finding.StillApplies` → `streams.Plan` → `coldwallet.Build` → `RecordLocks` → our verifier → `arm.Verify` (LND `psbt_verify`, all n) → **signing round** → `combine.Complete` → txid recheck (I-3) → `testmempoolaccept` → `arm.Finalize` | `run.armWindow` |
| the gate | `arm.Publish`, once | `internal/arm/publish.go` |
| **Phase 2** | settle, confirm, apply policy | `run.settlePhase` |

**Correction A — the build and the signing round are *inside* the armed
window, not before it.** `coldwallet.Build` and `run.sign` are both called from
`armWindow`, after `arm.Open` has started n peers' ten-minute clocks. The
review's ordering puts the build before arming. This is not an accident of
implementation: LND issues the funding addresses at step 2, so there is nothing
to build until the shims are open. The review actually states this correctly in
item 6 ("LND issues funding addresses before the PSBT can be built, so shims are
open while signers assemble") and then contradicts it in its own phase model.

Consequence: **item 1's "before cold storage is touched" is not achievable, and
should not be the acceptance criterion.** Phase 0 already touches cold storage
four times on purpose — `setup.Check` reads the descriptors, `SelectCoins` and
`FenceOff` lock coins, `ChangeAddress` derives one — and then deliberately runs a
**full signing round** in `rehearsal.Run`, mirroring the batch's amounts, so that
a signing session slower than `limits.abort_after_signing_seconds` is discovered
before any clock starts (`rehearsal.Gate`). The correct criterion is "before any
shim opens", which is exactly where the existing pre-flight already sits.

**Correction B — `arm.Finalize` interleaves; it does not finalize n then collect
n.** `internal/arm/arm.go:545` loops `finalizeOne` per stream, and `finalizeOne`
(`:594`) does `FundingStateStep` *and then waits for that channel's receipt*
before the next channel is finalized. The n-of-n property is preserved elsewhere
— `MarkPending` counts, and `Finalize` re-reads the run and refuses unless the
journal says `StateArmed` (`:565`) — but the review's picture of the interleaving
is wrong.

Consequence: **item 3's headline scenario is the code's natural failure mode, not
an exotic one.** "Peer disconnects between finalize 2 and 3" is simply
`finalizeOne` failing on stream 3 with 1 and 2 already pending, which is what
`TestRunAbortsAPartiallyArmedBatch` and `TestAFailureInsideTheArmedWindowIsTakenApart`
already drive.

**What the review's model omits entirely:** the dress rehearsal (Phase 0), and
step 8's other half — `ExportAllChannelBackups` at `arm.go:576`, which runs after
the last receipt and before publish, and refuses to proceed on an empty backup.
Neither appears anywhere in the document.

### 2. What `doctor` already covers — **substantially built**

`doctor.Run` (`internal/doctor/doctor.go:180`) makes ten checks, never stops at
the first failure, and prints a runnable `Fix` line rather than prose for each:
`winthistle.toml`, LND, the macaroon (via `CheckMacaroonPermissions`, including
the never-list), Bitcoin Core, the cold wallet (including the round-trip address
answer), the coins, **the anchor reserve**, the fee rate, **the peers**, and the
run journal.

`--batch` and `--connect` already exist (`cmd/winthistle/main.go:269`, `:271`).
With `--batch`, `checkPeers` (`:810`) resolves every peer in the real batch via
`peers.Check`, and `checkReserve` (`:729`) computes the anchor reserve for the
actual batch with three distinct verdicts. `--connect` is off by default on the
stated grounds that a diagnostic must not change the node.

So item 1's acceptance clause "same checks available read-only via
`doctor --batch`" is **already satisfied** for every check that exists.

---

## Findings

| # | Verdict | One line |
|---|---|---|
| 1 | **PARTIAL** | Pre-flight exists as a Phase 0 gate; one cheap check missing, most of the rest is unknowable without paying for a probe |
| 2a | **PARTIAL** | The sheet exists as `Plan.Document`; it is missing the peer *alias* |
| 2b | **DONE** | Fully implemented with 17 adversarial tests; the review's spec-only limit is moot |
| 3 | **PARTIAL** | The house test pattern is already "assert an empty mempool"; five named scenarios genuinely absent |
| 4 | **WRONG** (premise) | The web UI exists; `ratatui`/`crossterm` are Rust and this is Go; small real core survives |
| 5 | **BLOCKED** | As specified it is a funding-transaction replacement authored by this repo — the rejected list forbids it. Needs your decision, not a spec |
| 6 | **VALID** (6 of 8) | Real docs gaps; one bullet is wrong, one badly understates a stale claim |
| 7 | **DONE** | Already out of scope and already agrees with `CLAUDE.md`; one docs sliver |
| 8 | **WRONG** (artifact) / **DONE** (substance) | There is no run directory — it is SQLite — and "version it" is explicitly rejected by design |

---

### 1. Peer preflight — **PARTIAL**

The phase the review proposes already exists and is already a gate.
`run.prepare` steps 1–5 run before `arm.Open`, fail closed, and name the peer:
`peers.Check` → `for _, f := range facts { if !f.Usable() { return ... } }`
(`run.go`, and `unusable()` renders which check failed).

Already covered:

- **reachable/connected** — `peers.Check`, `Facts.Connection`, `Facts.Usable()`.
  Plus a pubkey check nobody asked for: `ValidatePubkey` rejects a well-formed
  33 bytes that is not a point on secp256k1, which is the residue after hex and
  length pass.
- **amount within min/max** — as far as it can be. `Facts.SmallestSat`/`MedianSat`
  from the gossip graph, surfaced as `BelowSmallest()`, explicitly labelled a
  proxy: **there is no gossip field for minimum channel size**
  (`internal/peers/peers.go`, tier 2).
- **local anchor reserve after all n channels exist** — `reserve.Check(ctx, …,
  arm.BatchOf(p.chans))`, with `WouldBeRefused` / `ShortAfterBatch` /
  `NotApplicable`, and `finding.StillApplies(streams.Batch())` re-checked against
  the streams that actually opened. This one is **DONE**, and it is item 1's
  "locally" clause verbatim.
- **read-only via `doctor --batch`** — done, see above.

Not covered, and honestly not coverable: **`max_pending_channels` headroom,
remote reserve, dust limits, accepted commitment type.** These are enforced
conversationally in `accept_channel` and published nowhere. The only way to learn
them is to open a funding stream — which is what `--probe` does, and a probe the
peer *accepts* costs one of that peer's pending-channel slots for
`HoldUpperBound` (11 minutes) because `shim_cancel` sends the peer nothing.
`peers.ReadyToArm` exists precisely so a run that probed cannot immediately
collide with its own probes. So this half of item 1 is not a gap; it is a cost
the design already priced and defaulted to *not* paying.

**The one genuinely missing, genuinely cheap check: "no competing pending open."**
`PendingChannels` is already in the method registry
(`internal/methods/methods.go:319`) and already called from `internal/abort` and
`internal/bump`, but nothing asks it during the peer pre-flight. Our *own* pending
channels toward a target peer are locally readable, cost nothing, and a collision
there is refused on the clock at step 2 for a reason we could have seen for free.
**VALID, small, and the right first thing to build.**

Priority note: the review calls item 1 "highest" on the grounds that partial
arming is the dominant mainnet failure. Partial arming is already handled — it
aborts, through the same code `winthistle recover` runs, with tests. The
pre-flight reduces its *frequency*; it was never unhandled.

### 2a. Verification sheet — **PARTIAL**, one field missing

`Plan.Document()` (`internal/plan/report.go:16`) is printed in `armWindow`
before `coldwallet.Build` and long before any signer sees anything. It already
carries: per-funding-output label with peer pubkey prefix (16 hex chars,
`shortPeer`), address on its own line, exact amount, forwarding policy, total
out, the reserve top-up and why it counts, change address, change floor, fee
target with accepted range, RBF-off, SegWit-only, minimum confirmations, the
exact list of allowed coins, and the list of *excluded* coins with reasons —
that last one because the operator's wallet balance and the plan will otherwise
disagree.

Against item 2a's list: output index → present as channel number (note
`Plan.Outputs()` sorts by kind, so the label is the channel's ordinal, not the
literal `vout`); peer pubkey prefix, amount, address, change address, fee,
feerate, input count → all present. Change *amount* is absent from the plan
because Core chooses it; it appears in `Verification.Report()` once the
transaction exists.

**Missing: the peer alias.** `plan.Channel` carries `Peer` (pubkey) and no
alias, and `peers.Facts.Alias` is fetched in Phase 0 and then dropped. This is
the whole of item 2a's residue, and it is the field most useful to a human
reading a sheet against a signer screen — `bitrefill` beats
`02a1b2c3d4e5f6a7…`. **VALID, small: thread `Facts.Alias` into `plan.Channel`
and into the label.**

The review's framing of *why* is correct and worth keeping: funding outputs are
P2WSH 2-of-2 scripts no signing wallet can attribute, so the sheet is the only
place the mapping exists. That is a good paragraph for the README (item 6's
"why not Sparrow").

### 2b. Signed-PSBT validation — **DONE**

The review marks this SPEC ONLY on the grounds that "a plausible-looking but
subtly wrong validator is worse than none." The validator exists, and every
acceptance bullet is already a named failure code.

Return path, in order, all before `arm.Finalize` — i.e. before any
`FundingStateStep(psbt_finalize)`:

1. `combine.Merge` (`internal/combine/combine.go:193`) refuses:
   `ErrDifferentTransaction` (I-3, at the merge), `ErrConflictingSignature` (two
   sigs for one key — refused rather than chosen between),
   `ErrConflictingField`, `ErrAlreadyFinalized` (**I-2**: a finalized input means
   that device held a broadcastable transaction), `ErrNoSignatures`. Every
   refusal names the device and the input.
2. `combine.Finalize` executes each witness locally against its input's script,
   and counts signatures first so a 2-of-3 carrying three partials gets a
   sentence about the wallet rather than btcd's "Unsupported script type".
3. `Finalized.Recheck` (`finalize.go:217`) re-runs the *full* plan verifier over
   the **extracted transaction** rather than the packet, then requires the
   verifier's vsize to equal the real vsize.
4. `run.armWindow` compares `final.TxID != built.TxID` explicitly and names I-3.
5. `testmempoolaccept` — validate without relaying, the only pre-flight.

The verifier's codes (`internal/plan/verify.go:54`) map onto item 2b one for one:
`UnnamedOutput`/`MissingOutput`/`DuplicateOutput`/`WrongAmount` (outputs and
amounts), `ChangeMissing`/`ChangeAmbiguous`/`ChangeTooSmall` (change to a
verified branch), `InputNotAllowed`/`DuplicateInput`/`MismatchedUTXO`/`NoUTXOInfo`
(no input added, removed or swapped), `NoFee`/`FeeTooLow`/`FeeTooHigh` (fee
tolerance) — **plus two the review does not ask for**, `LegacyInput` (I-3
malleability) and `Replaceable` (I-4).

The test vectors it asks for also exist — `internal/combine/combine_test.go`
has 17 cases, of which these are deliberately corrupted PSBTs: a device that
changed the transaction, a signer that made it replaceable, two signatures for
one key, an identical repeat that is *not* a conflict, a returned finalized
input, a device rewriting what an input spends, a later packet overwriting an
earlier witness script, over-signing, and a witness that does not satisfy its
script. Plus `TestTheVerifierStillGetsTheLastWord`.

**No work here.** The implementation limit the review places on 2b should be
lifted rather than honoured, because honouring it means writing a spec for
shipped, tested code.

### 3. Adversarial harness — **PARTIAL**

The review's acceptance criterion is already the house pattern: `env.InMempool`
assertions appear in `internal/abort`, `internal/journal`, `internal/run` and
`internal/settle`, including — `abandon_regtest_test.go:332` — a mempool check
after *every* finalize including the last, which is the one that actually tests
I-1. And the second half it says "matters as much" (a state `recover` can act
on) is done: `journal.Recover` is the same code path `winthistle recover` runs,
exercised by `recover_regtest_test.go`. 388 test functions across 61 files.

Scenario by scenario:

| Scenario | Status |
|---|---|
| Peer disconnects between finalize 2 and 3 | **covered** — `TestRunAbortsAPartiallyArmedBatch`, `TestAFailureInsideTheArmedWindowIsTakenApart`; and per Correction B this is the natural shape of the loop |
| `PublishTransaction` fails at the gate | **partly** — `TestPublishRefusesABatchTheJournalDoesNotCallArmed`, `TestMarkPublishingRefusesAPartiallyArmedBatch`, `TestACrashMidPublishLeavesTheTransactionOnDisk`. The *refusals* are tested; a real mempool rejection (already-in-mempool, insufficient fee) is not |
| Peer never responds to `FundingStateStep` | **absent** |
| LND restarts mid-batch after some receipts | **absent** |
| Funding timeout expires with one receipt outstanding | **absent** — `TestWhoOwnsTheTenMinuteClock` measures the clock (10m41s against bob) but does not drive this |
| Duplicate receipt, or a receipt for a non-member channel | **absent** — `TestWritesToAnUnknownRunAreRefused` is the nearest and is about runs, not channels |
| bitcoind unreachable at publish time | **absent** |

Five genuinely missing scenarios. **VALID**, and the cheapest of them
(duplicate/non-member receipt) is a unit test against the journal, not a harness
test. Note `make test` uses `-p 1` deliberately — two harness-backed packages
through one alice fail with LND's reserved-value error, which says nothing about
the real cause — so new regtest tests must respect that.

### 4. Operator surface — **WRONG** on the premise, small **VALID** core

Three separate errors, then something real.

- **"The largest unbuilt thing in the repo."** `internal/server` exists: 4,558
  lines, routes at `server.go:252` — index, doctor screen, run attach, start
  run, answer, abort screen, abort, `/recover`, `/recover/{id}`. It carries the
  security shape the design asks for (loopback bind, startup token, strict
  `Origin` and `Host`, no CORS) and can open a batch. The README says otherwise;
  the README is wrong.
- **`ratatui` + `crossterm` are Rust crates.** This is Go (`go 1.24.4`). The
  proposal is not portable to this codebase as written.
- **"The actual payoff is the single struct holding whole-batch state."** That
  struct exists: `journal.Run`, with per-channel state, outpoints and txid — and
  it is *already rendered as the table the review sketches*, by
  `prose.RecoveryList` (`internal/prose/recovery.go:42`) and `prose.Recovery`
  (`:130`), which print per-run state, a channel breakdown and the txid on its
  own line. So item 4's premise that state is "spread across the run loop" is
  wrong, and its estimate of the remaining work is far too high.

The review's own audit constraint finishes the argument: building a second front
end, in a second language, to display state a Go web UI already serialises, is a
large spend against "small enough to read end to end." **Recommend rejecting the
TUI outright.**

What survives, and it is worth doing: the **attach screen is a transcript, not a
state table** (`internal/server/screens.go:224` — it renders `run.State()`'s
prose transcript plus the pending question). It does not render `journal.Run`.
And there is **no countdown** — the only deadline shown is the *signing gate's*,
as a static timestamp, and `CLAUDE.md` already lists the countdown as missing.
So the real item 4 is: *render `journal.Run` per channel on the attach screen,
and add the countdown* — one screen, in the front end that already exists, over
a struct that already exists, with a renderer that already exists for `/recover`.
That is a fraction of what the review scoped.

Two of its details are worth keeping verbatim: the receipt column with
`funding tx HELD` as the live assertion of I-1 (`journal.Run.State` is the
authority — the server must keep asking rather than holding a second copy of the
rule), and "do not overload one key for abort." The abort control is already a
link to a screen that says what stopping costs rather than a one-click button,
which is the same instinct.

### 5. Pre-signed abort — **BLOCKED, and it needs your decision**

Flagging rather than specifying, per `CLAUDE.md`'s instruction to say so and stop.

A pre-signed abort spends the batch's own inputs to a different destination. That
is a **replacement of the funding transaction, built and signed by this
repository** — which is the exact thing the rejected-approaches list forbids, in
two places:

> **Bumping the funding transaction, by any route.** I-4. `winthistle bump`
> builds a child; nothing in this repository replaces a parent.

> The only route out is an out-of-band double-spend of one of its inputs,
> performed by the operator with their own tools. … Documenting it is not a
> relaxation of I-4. **No code path in this repository builds one, and none may
> be added.**

The review reaches this from the other direction and lands on the same discomfort
— "two conflicting signed transactions in the world", "a griefing vector", "it
breaks the cleanest sentence in the README" — without noticing it has proposed
the named-and-rejected approach. Its stated threat model is also narrower than
the ban: it defends a *compound* failure (the signed transaction leaks **and**
the pending channels were since abandoned). Note that the second half is already
hard to reach — `journal.Run.AbortTarget` refuses any run in `StatePublishing`
or `StatePublished` with `ErrMayBePublished`, so this tool will not abandon
channels whose transaction might be live.

So: **not DEFER.** Writing the spec the review asks for would be documenting a
code path `CLAUDE.md` says may not be added, and I am not doing that on my own
judgement. If you want it reconsidered, that is an explicit change to the
rejected list and to I-4's authorship line, and it should be made deliberately
and in one commit with `docs/design.html` — not arrived at via a spec.

### 6. README and docs — **VALID**, 6 of 8

| Bullet | Verdict |
|---|---|
| No install step | **VALID.** "Trying it" opens with a `winthistle` invocation; nothing says Go, nor `make build` |
| No requirements/topology section | **VALID.** Where Winthistle runs relative to bitcoind and LND, what it needs from each, and which signers are known-good, are nowhere |
| Qualify the no-RBF claim | **VALID, and the material is already written.** `CLAUDE.md` has the paragraph: verified against Core v29, `mempoolfullrbf` does not exist even as a hidden debug option, `getmempoolinfo` reports `"fullrbf": true` with no way off. What enforces I-4 is that only we can sign our inputs and nothing here builds a replacement. The nSequence to state is `plan.MaxNonReplaceableSequence` = `0xfffffffe` (`internal/plan/verify.go:24`). Lift it into the README |
| No-change batches break the CPFP story | **WRONG as a code gap.** Already guaranteed: `plan.ChangeFloor` (`internal/plan/size.go:247`), `DefaultCPFPMultiple` = 3.0, enforced by `checkChange`/`checkChangeSize` via `ChangeMissing` and `ChangeTooSmall`, in both the outbound verify and the `Recheck`. The review's "guarantee a change output above a threshold, **or** document the failure" — the first branch is done. Docs sliver only |
| The signing session has its own clock | **PARTIAL, and the review's framing is slightly off.** It says "read the value out of the vendored LND version rather than trusting any doc" — already done: `internal/peers/peers.go` derives the hold from `chanfunding.DefaultReservationTimeout` + `lncfg.DefaultZombieSweeperInterval`, notes neither is adjustable in a release build, and `TestWhoOwnsTheTenMinuteClock` measured 10m41s. The correction: **the clock is the *peer's*, not ours** — our node never expires a PSBT reservation. And the mitigation already exists and is unmentioned in the README: the Phase 0 dress rehearsal times a real signing round and `rehearsal.Gate` refuses to arm if it was too slow. **VALID as a docs gap** |
| Add "why not just use Sparrow" | **VALID.** "Sparrow signs a transaction; Winthistle knows what the transaction means" is the right sentence, and item 2a's P2WSH-unattributable paragraph is the evidence for it |
| Split the status blockquote | **VALID, and badly understated.** This is not a run-on to break in two: the clause is **false**. "the web interface described in the design does not exist yet" contradicts `internal/server`, and the command list omits `winthistle serve` entirely. Highest-value item in the document, and the reason the review under-reads the repo |
| `print-macaroon-command` vs `make macaroon` | **VALID.** They are one door: the Makefile's `macaroon` target is literally `go run ./cmd/winthistle print-macaroon-command`. Cross-reference or drop one |

### 7. In-house QR — **DONE**

Recorded as out of scope, and it agrees with the repo: nothing here does QR, and
`CLAUDE.md` lists the transports (file up/down, animated QR) as unbuilt. The
analysis is sound and no decision is needed.

One sliver is **VALID** and belongs with item 6: state the contract, not the app
— any wallet that round-trips BIP174 with the descriptor works. Naming Sparrow
as the only path is wrong for SD-card signers and wrong for a headless
deployment beside LND. Note this sits in slight tension with item 6's "which
signers are known-good for multisig change", which asks for named wallets; the
resolution is to name known-good examples while stating the contract as the rule.

### 8. Freeze the run-directory format — **WRONG** on the artifact, **DONE** in substance

**There is no run directory.** State is a SQLite journal (`internal/journal`),
schema at `journal.go:262`: `runs`, `channels`, `signers`, `locks`, `bumps`,
`bump_signers`, `bump_locks`, `setups`. Item 8 is asking to freeze a file format
that does not exist.

On the substance, the three things it asks for:

- **Enumerate every state a run can be found in** — done, as typed constants
  with a documented meaning each: `StateArming`, `StateSigning`, `StateArmed`,
  `StatePublishing` (written *before* the RPC), `StatePublished`,
  `StateAborting`, `StateAborted`.
- **Specify `recover`'s behaviour in each** — done, in one function:
  `Run.AbortTarget` (`internal/journal/recover.go:141`) refuses
  `StatePublishing`/`StatePublished` with `ErrMayBePublished`, splits pending
  channels (abandon by outpoint) from unfinished shims (cancel by pending chan
  id), leaves already-cleaned channels out so a second recovery reports
  honestly, and refuses a channel journalled pending with no outpoint. `Unfinished`
  (`:95`) is the list, and it is careful not to claim a listed run has stopped.
- **Version it** — **explicitly rejected by design.** `journal.go:233`: one
  `CREATE TABLE IF NOT EXISTS` block, no version table, no migration machinery,
  so growth is by new tables and never new columns. Adding a version table now
  would be reversing a recorded decision, not freezing a format.

So item 8 should **not** be done as specified, and its claim to be a prerequisite
for everything else does not hold. The honest residue is documentation: the seven
states and `AbortTarget`'s behaviour live in code and are not enumerated as a
table in `docs/design.html`, which mentions the journal and the state machine but
does not list them. **Small VALID docs item** — and it is item 6's territory, not
a blocker on 1 and 3.

---

## Recommended order, revised

The review's order is `8 → 1 → 3 → 2a → 2b spec → 6 → 4 → 5 spec`. Two of those
are moot (2b, 8-as-specified), one is blocked (5), and one is much smaller than
scoped (4). What is left, cheapest-first:

1. **Item 6, the README.** Start with the stale status blockquote, because every
   under-estimate in this review descends from it. Then the install step,
   requirements/topology, the qualified no-RBF claim with the nSequence, the
   signing-session clock budget (and the dress rehearsal that already gates it),
   "why not Sparrow", the BIP174 contract from item 7, and the
   `make macaroon` cross-reference. Independent of everything else and the
   review is right that it can run in parallel.
2. **Item 1's one real check** — no competing pending open, via `PendingChannels`
   in the peer pre-flight, surfaced in `doctor --batch` too.
3. **Item 2a's one real field** — the peer alias into `plan.Channel` and the
   sheet.
4. **Item 3's five missing scenarios**, cheapest first: duplicate / non-member
   receipt (unit, against the journal), then bitcoind unreachable at publish,
   mempool rejection at publish, peer never responds, LND restart mid-batch.
   Respect `-p 1`.
5. **Item 8's docs residue** — the state table in `docs/design.html`.
6. **Item 4, reduced** — render `journal.Run` as a live per-channel table on the
   attach screen, plus the countdown. Reject the TUI.

Not doing: 2b (built), 8 as specified (no such artifact; versioning rejected),
4 as specified (second front end, wrong language, fails the review's own audit
test), 5 (collides with the rejected-approaches list — needs your decision
first).

Nothing above touches `arm.Publish`, the I-1 gate, or the pinned call-site count
of 2.
