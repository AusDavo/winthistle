# Winthistle

A minimalist CLI wizard for opening a batch of Lightning channels in one
on-chain transaction, around **Sparrow and LND**. It does the two things those
two cannot do between them:

1. **Attribute the outputs.** A funding output is a P2WSH 2-of-2 with a peer. No
   signing wallet can tell you which peer it funds, at what amount, or that two
   were not swapped. Sparrow shows you payments to unrecognised addresses.
2. **Hold the I-1 gate.** Get all *n* channels to `chan_pending`, and only then
   let the transaction reach the network.

It builds nothing, holds no keys, selects no coins, derives no addresses, and
opens no socket except to your LND. It assumes you have Sparrow (or comparable),
LND, and a wallet with coins in it — hot, cold, single-sig, multisig. Which of
those does not matter to this program.

Full spec: `docs/design.html`. Direction and rationale for the 2026-08 rewrite:
`docs/replan-2026-08.md`. **This file holds invariants, rules and current state.
The slice-by-slice account of how it got here lives in git history and PR
bodies, and does not belong here.**

## The sequence

```
1  pre-flight            peers reachable? anchor reserve sufficient?    [LND only]
2  open n streams        psbt_shim + no_publish              ← clock A starts
3  print the plan        peer · alias · address · amount · policy
4  build in Sparrow      load the recipients CSV, pick coins and fee, Save PSBT
5  verify + psbt_verify  outputs match the plan; skip_finalize; txid pinned
6  n × chan_pending      ← GATE OPEN. clock A stops, clock B starts
7  sign in Sparrow       no ten-minute pressure
8  txid unchanged? → publish once, via LND
9  watch to confirmation, apply policies
```

**Steps 2–6 are the only part under the peers' ten-minute clock, and they contain
no signing.** That is the whole point of the inversion.

---

## Status

**All six items of the 2026-08 replan are done.** The mainnet cold probe passed
on 2026-08-26 (run `20260826-043441-9a8f28`, two channels, both to
`chan_pending` with nothing signed), and the first live batch published the same
evening: run `20260826-191016-d9407c`, five channels, 9,000,000 sat, txid
`a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90`, five of five
`chan_pending` with nothing signed, published once, confirmed — **signed from a
2-of-3 cold-storage multisig**, which is the mainnet instance behind "which kind
of wallet does not matter"; until then that claim rested on the harness's
simulated `wsh(sortedmulti(2,…))` alone. **That batch was the production run,
not a rehearsal**, and **no issue is open.** What remains is a polish pass, then
the public flip.

### What exists

`winthistle run`, `doctor` and `recover`, plus `print-macaroon-command` and two
`example-*` printers, are the whole command set. All three work against the
cluster in `regtest/`.

`internal/run` imports: `arm` · `plan` · `combine` · `peers` · `reserve` ·
`settle` · `journal` · `methods` · `lnd` · `prose` · `config` · `abort`. Plus
`doctor` and `policy` outside the run path, and `internal/bitcoind` and
`internal/regtestenv/coldwallet` inside the harness only.

`internal/arm` runs the inverted sequence — `skip_finalize` at verify, the *n*
receipts before anything is signed, one publish. There is **no `psbt_finalize`
call anywhere in this build**. It imports `abort` for one call, in
`cancelUnreadable`; `abort` imports only `lnd`, so the direction is the only one
that has ever been available. `run` builds nothing and signs nothing: it prints
the recipients, writes `FILE-recipients.csv`, reads the unsigned transaction back
from `--psbt FILE`, and reads the signed one from `FILE-signed.psbt` or
`FILE-signed.txn`. `internal/combine` is the acceptance check on an inbound
PSBT. `internal/plan` has the batch verifier.

### The version pins, and how to bump them

**The whole build runs on LND v0.21.2-beta**, and every source citation in this
file is against that version. **Three separate pins, and only the first moves by
itself**: `go.mod`, `regtest/.env`, and the citations in comments and copy.

The bump procedure, in order:

1. **`make test` first.** I-1's ordering is the one citation that fails rather
   than waiting to be re-read: `TestTheReceiptIsProvedByForceClosingTheChannel`
   force-closes an armed channel and makes Bitcoin Core check the commitment's
   witness.
2. **`make check-citations`.** Every LND symbol named in a comment or a printed
   sentence must still exist in the module cache at the pinned version, and every
   number transcribed out of LND must still match. Both live in
   `internal/methods`.
3. **Then re-read by hand.** Line numbers move loudly — the citation says `:2718`
   and the function is elsewhere. What moves silently is a renamed symbol, a
   changed constant, and **a new proto field that makes a "this is unreadable"
   claim false**. The first two are what step 2 catches; the third is not
   mechanically detectable and is what the v0.21.2-beta bump missed.

### Settled behaviour, with no issue left open

- **`arm.open` cancels the shim it cannot journal, and only there.** On its two
  "LND sent something unreadable" branches LND has not errored, so nothing on its
  side releases the reservation and a hang-up does not either
  (`TestAShimSurvivesItsStreamBeingHungUp`). `cancelUnreadable` hangs the stream
  up and *then* cancels the shim — that order, because it is the one that test
  measured — and journals nothing, because a stream that never opened has no
  channel row to hang an id on. `ErrNoShim` is success; a cancel that genuinely
  fails is reported alongside LND's own complaint, with the pending channel id in
  it and no claim about what LND still holds. **The `Recv` error path is excluded on purpose**: LND cancels its own
  refusals (`funding/manager.go:5301` → `:5308` → `lnwallet/wallet.go:1488`), and
  the commonest way to reach that branch is Ctrl-C, where a cancel on a dead
  context would print a sentence about a shim nobody needs to chase.
- **The armed window reports the refusal it observed, never a journalling
  complaint about it.** `arm.Open` returns its `Streams` on a first-channel
  failure too, so `NewChannels()` is empty there and `journal.Begin` — which
  rightly refuses an empty batch — must not be called. When `Begin` genuinely
  must run and fails, `unjournalledStreams` returns both causes and prints the
  pending channel ids, because that is the last moment anything knows them.
- **The count at verify grows under the batch, and the top-up is what covers
  it.** `AtVerify` (`RequiredReserve(additional=1)`) is the figure the *first*
  `psbt_verify` uses and the floor for the rest; the *n*th can be judged against
  `AfterBatch`, because an earlier channel's `CompleteReservation` calls
  `SyncPending` (`lnwallet/wallet.go:2534`) before `chan_pending` and nothing
  orders that against a later verify. Measured at *n* = 3 by
  `TestALaterVerifyCountsAnEarlierChannelInTheBatch`: the figure moved 10,000 →
  20,000 sat mid-batch. **`plan.ReserveTopUp` aiming at the larger figure is
  therefore load-bearing, not an economy** — a top-up output inside the batch is
  credited by `CheckReservedValue` at every verify, including the last. Do not
  "optimise" it down to `ShortfallAtVerify`.
- **`Options.Chain` is the harness's seam and may not be deleted.** The
  application fills it on no path, so `Confs == -1` is the rule; `internal/
  regtestenv`'s Core fills it, and deleting the field would take
  `State.ObservedDepth` and a live peer's `minimum_depth` read *from above* with
  it. Since #47 that reading is no longer the only one — `State.PeerDepth` is
  the peer's own figure and needs no `Chain` — which is what makes the observed
  one worth keeping: two numbers from two sources, checked against each other on
  a running node by
  `TestTheSettlementPassPoliciesAChannelAndLearnsItsDepth`.

---

## The four invariants

These are the product, not preferences. Each carries a source citation so you can
verify it rather than trust this file, and I-1's ordering carries a test that
executes it.

**If you believe an invariant is wrong, say so and stop. Do not work around one,
and do not weaken one to make a test pass.**

### I-1 · Publish only once every channel is already recoverable

`no_publish` **and** `skip_finalize` MUST be set on **every** channel in a batch.
Verify all *n*, wait for all *n* `chan_pending`, then publish exactly once via
`WalletKit.PublishTransaction`.

Why it holds, in source: in `funding/manager.go`, **`funderProcessFundingSigned`**
(`:2718`) calls `CompleteReservation(nil, commitSig)` at `:2813` — storing the
peer's commitment signature — *before* the broadcast block at `:2829`, which is
guarded by `completeChan.ChanType.HasFundingTx()`, and emits `chan_pending` at
`:2897`, after it. So each `chan_pending` is a receipt that the channel is
recoverable by force-close. `no_publish` sets `NoFundingTxBit`
(`lnwallet/reservation.go:415`), which clears `HasFundingTx()`.

**And that consequence is executed rather than inferred, since issue #16.**
`TestTheReceiptIsProvedByForceClosingTheChannel` in
`internal/arm/force_close_regtest_test.go` arms one channel through `drive()`,
publishes, mines the batch to confirmation, and force-closes the channel through
`regtest/bin/lncli` — out of band of the Go client, because `CloseChannel` is
never-listed and `TestEveryLNDCallSiteIsRegistered` scans `_test.go` too. The
commitment reaches the mempool and then a block, spending the outpoint
`chan_pending` named, with the four-element P2WSH witness of a 2-of-2. **Bitcoin
Core is the judge, not LND**: consensus checked both signatures and this node
holds one of the two keys, so the peer's was stored before the receipt arrived.
This is what closes the silent failure — the ordering moving is the one break
that leaves every other assertion in the repository passing.

`NoFundingTxBit` does not skip *only* the broadcast. It gates four things: the
broadcast (`:2829`, publishing at `:2851`), the startup rebroadcast (`:766`,
`:772`, calling at `:769` and `:779`), the funding-input witness verification
inside `CompleteReservation` (`lnwallet/wallet.go:2275`, which has no witnesses to
check), and the transaction label (`:3371`). `CompleteReservation`,
`WatchNewChannel` and the `chan_pending` emission are untouched, which is what
I-1 needs.

Why `skip_finalize` is safe, in source: `PsbtIntent.Verify` ends
(`lnwallet/chanfunding/psbt_assembler.go:293-300`) with, when
`!i.shouldPublish && skipFinalize`, `i.FinalTX = packet.UnsignedTx`,
`i.State = PsbtFinalized`, and a close of `i.PsbtReady` — guarded by the
`signalPsbtReady` `sync.Once` (`:149`) rather than being a bare `close`, which
changes nothing here because the channel still closes exactly once, on the first
`skip_finalize` verify. `funding/manager.go:2314`
reads that channel with a bare `case nil:` — *"Nil error means the flow continues
normally now."* (`:2331`). `CompileFundingTx` still runs
(`lnwallet/wallet.go:1881`) because it "sets the actual funding outpoint in
stone" (`:1880`), and the unsigned
transaction suffices: all inputs are segwit, so witnesses do not move the txid.
LND refuses `skip_finalize` without `no_publish` — `PsbtFundingVerify` checks
`skipFinalize && ShouldPublishFundingTX()` (`lnwallet/wallet.go:764`) *before* it
advances the intent, so the refusal is free and the shim still cancels. Confirmed
on the node, in those words, by
`TestLNDItselfRefusesSkipFinalizeWithoutNoPublish`. **`arm.Verify` asserts the
flag on its own side anyway**, because the combination it prevents is a request to
arm a channel *and* broadcast, and the only other guard for that lives in
somebody else's codebase.

**There is no `psbt_finalize` call in this build.** A `skip_finalize` verify
leaves the intent in `PsbtFinalized`, and both of LND's finalize entry points
require `PsbtVerified`, so the call is refused with "invalid state. got finalized
expected verified". `arm.Receipts` reads the streams; it does not step the
machine. `TestEveryVerifyCarriesSkipFinalizeAndNothingIsFinalized` counts the
calls and requires zero.

**Proved on regtest, 2026-08-25.** `TestSkipFinalizeReachesChanPendingWithNothingSigned`
in `internal/arm/skip_finalize_regtest_test.go`: *n* = 2 streams reached
`chan_pending` at the outpoints of an **unsigned** transaction, with nothing
signed and the mempool clear. Both receipts arrived inside 0.55 s.

**And the receipt cashed on regtest, 2026-08-27** — issue #16.
`TestTheReceiptIsProvedByForceClosingTheChannel` in
`internal/arm/force_close_regtest_test.go`: one armed channel, published, mined,
force-closed through `regtest/bin/lncli`, and its commitment mined. 1.34 s, and
**not gated behind `WINTHISTLE_SLOW`** — that gate is for clock A, which is
wall-clock; this is all mining, and a gated test is a test that does not run.
**Where it stops is a decision**: the issue's shape ended by mining past
`to_self_delay` and asserting the swept output comes back, and that is LND's
sweeper rather than the funding ordering. A test that can fail for two unrelated
reasons names neither. `to_self_delay` is read off `ListChannels` and logged, so
the number it does not wait out cannot rot.

NEVER change this to "all but the last", **even though that is what LND's own
docs recommend** (`docs/psbt.md:643`). That idiom exists because `lncli` has no
way to broadcast afterwards — a CLI limitation, not a protocol constraint. LND's
"DO NOT PUBLISH … OR THE FUNDS CAN BE LOST" warning (`docs/psbt.md:345`) is about
*ordering*, not authorship.

Consequence to preserve: `NoFundingTxBit` also gates `rebroadcastFundingTx`
(`funding/manager.go:769`), so we own rebroadcast. Publish through WalletKit (its
wallet re-broadcasts on startup until the tx confirms), and keep the raw tx in
the journal.

### I-2 · Dissolved — and the reason is recorded, not quietly dropped

I-2 read "the app holds the last signature": signers returned **partial**
signatures, combining and finalizing happened in-app, and no external party could
ever hold a broadcastable transaction. It existed for exactly one reason — so
that nothing could broadcast *before the I-1 gate opened* and defeat it from
outside.

**After the inversion there is no "before the gate opens."** You hold no
signatures until step 7, and the gate closed at step 6. Sparrow holding a fully
signed transaction front-runs nothing: every channel is already recoverable by
force-close. The invariant is not relaxed, it is *unnecessary*. The m−1 signing
dance that assisted mode specified is deleted with it.

**The single-sig caveat becomes moot rather than unmet.** It used to say that a
single-sig wallet returns a complete transaction, so there is no partial-signature
path to hold it up in, and that on single-sig I-2 rested on the operator's setup
rather than on a gate. That was an honest limit on an invariant that now has
nothing to reach: which kind of wallet you sign with stopped mattering to this
program. Do not carry it forward as a weaker caveat.

**This section is not a licence to reintroduce a broadcast path.** I-1 still says
the app publishes once, after *n* receipts. What dissolved is the claim about who
else may hold the bytes.

**And there is one place left where "before the gate opens" still exists.** Step 4
is before it: the operator has a funded transaction and nothing has reached
`chan_pending`. A wallet that signs there — the same visit, two clicks from
Broadcast — could put a transaction on the network that confirms one 2-of-2 output
per channel with no channel behind any of them. So step 4 refuses a signed packet
(`combine.Unsigned`), and the copy says do not sign yet. That refusal is I-1's,
not I-2's, and it is not negotiable for the same reason the gate is not.

**`internal/combine`'s remaining finalized-input refusal is not I-2 either.**
`combine.Merge` still refuses one, because a merge unions partial signatures and
finalization discards them, so a device that finalizes ends a round the others
were still in. `combine.Accept` — the batch's path — expects a complete witness.
Do not re-justify either in I-2's words.

### I-3 · The TXID must not move after verification

LND commits to the funding outpoint at `psbt_verify`. Hash the unsigned tx at
verify and reject any returned PSBT whose unsigned TXID differs, naming the
offending device.

**I-3 now carries the weight I-2 used to.** It is the *only* load-bearing check
on what comes back from the signing wallet.

And it is not a duplicate of LND's own check, which is narrower than it sounds.
`PsbtIntent.FinalizeRawTX` (`psbt_assembler.go:356`) compares the outputs and the
inputs' *previous outpoints* and stops — its own comment says "the fields in the
PSBT part are allowed to change" — so sequence numbers, version and locktime are
unchecked, and each moves the txid. `verifyInputsSigned` only asserts that each
input has *something* attached. Our hash-at-verify is what enforces I-3.

This is also why all inputs must be segwit — see `verifyAllInputsSegWit`
(`psbt_assembler.go:611`), called at `:283` with "risk of malleability".

### I-4 · No RBF on the funding transaction, ever

Replacing the funding tx changes the outpoints and destroys every channel in the
batch. **Enforced by authorship, which is all that ever enforced it:** only we
can sign our inputs, and there is no code path in this repository that replaces a
funding transaction.

**There is no sequence-number refusal, and adding one back would be a lint
wearing an invariant's clothes.** Core 29's full-RBF is unconditional — verified live: `mempoolfullrbf` does not
exist even as a hidden debug option (`bitcoind -help-debug` has no such flag),
and `getmempoolinfo` reports `"fullrbf": true` with no way to turn it off — so a
higher-fee conflict relays regardless of what our sequence numbers signal.
Refusing a transaction over a signal that changes nothing is a lint wearing an
invariant's clothes. `replaceable: false` is a statement of intent, not a
defence.

`plan.Code` has no `Replaceable` member and `InputView.Sequence` records what
each input said so a report can show it, unjudged. The comment where the refusal
used to stand, in `plan.checkInputs`, says why at the one place somebody would
put it back. **What holds I-4 is that only we can sign our inputs**, and I-3's
txid pin is what catches a signer that edited a sequence number — a changed
sequence is a changed txid, which
`TestASignerThatChangedASequenceNumberIsRefused` asserts.

What the invariant covers, and what it does not:

- **The funding transaction: never.** *n* peers hold commitment signatures
  against its outpoints. Replacing it destroys the batch.
- **The CPFP child: always replaceable** — if there were one. Nobody has
  committed to anything about a child, so replacing one moves nothing anyone
  depends on, and a batch needing two lifts gets an ordinary RBF of the child
  rather than a grandchild. **This build makes no child**, and nothing in this
  repository constructs a replaceable transaction of any kind. An operator who
  needs one builds it in their own wallet, and `internal/settle`'s
  funding-horizon screen says so.

If a change ever makes a *funding* transaction replaceable, that is the invariant
breaking and the answer is to stop, not to edit this section.

---

## Rules that bind future work

Each of these was decided once and is not to be re-litigated. Where the reasoning
matters, it is in the PR that made the decision.

**On copy and citations**

- **Do not assert a cause the program cannot know.** A failure may be reported;
  its cause may only be named where the program established it. This is the rule
  the most issues have come from, and the ones that cost something were the two
  that wrote a wrong cause to *disk* rather than to a terminal.
- **A row may only claim what has been established when it is written.** Which
  way a write should move when it is wrong depends on how it is read: a
  `verified` row means *"the call may have landed, go and look"*, so it goes
  early; a `signed` row means *"it did happen"*, so it goes late.
- **Attribute LND's own defaults once per screen.** The eleven minutes and the
  2016 blocks are configuration defaults, not protocol constants.
  `prose.StockLNDNote` is the sentence; `settle`'s `horizonNote` is the model.
  **Recovery copy still names clock B in blocks** — that rule is untouched.
- **Key an assertion on an identifier where one exists.** Go's zero value passes
  a lookup for a name that no longer exists, so a test keyed on a deleted check
  name goes quiet rather than red.
- **No sweeps, and no per-slice copy audit.** That process filed roughly five
  issues per one it closed, for six slices, because each slice wrote explanatory
  prose and the audit's search surface *is* explanatory prose. If a class of
  defect can be found mechanically, write the check; otherwise leave it. Fix a
  wrong comment or citation in place, in the current PR, with no issue.

**On the safety model**

- **`Method.CallSites` is 1** for `WalletKit.PublishTransaction`, and the
  registry enforces it. The never-list is the never-list. **Do not register
  `CloseChannel`** to make a test convenient: the credential's inability to close
  a channel is a product claim, and the force-close test goes out of band through
  `regtest/bin/lncli` for exactly that reason.
- **`SubmitPackage` is not registered**, so package relay is unavailable by
  construction. Adding a call site would be a decision about I-4, not a registry
  edit.
- **Step 4 refuses a signed packet, and that relaxation may never move into
  `combine.Parse`.** Step 7 reads a signed PSBT *or* a finalized raw
  transaction; step 4 reads a PSBT only, because `combine.Unsigned` refuses by
  reading partial signatures and a raw transaction has none to read. The
  acceptance sits at step 7's call site in `combine.SignedFromTX`.
- **Two guards do not make a validator.** `lnd.ReadMacaroon` proves the file is
  a macaroon and stops; `CheckMacaroonPermissions` remains the only authority on
  what a credential may do.
- **The test that proves what the app promises cannot be written with the app's
  own capabilities, and that is the right way round.**

**On what the app will not decide for you**

- **There is no fee rate anywhere in this build**, and none may come back — not
  as a key, not as a flag, not as an estimate. `[fees]` is a retired *section*, so
  a config file that still has one is refused with a sentence.
- **There is no pre-flight.** `testmempoolaccept` went with Core.
  `combine.Accept` executes every input's witness against its own script; what is
  lost is node policy, and `plan.Verify`'s `Verification.Unchecked` names it.
- **The change output is recognised, not named.** `plan.RecogniseChangeIn` reads
  the master fingerprints off the transaction's own inputs and accepts an output
  carrying those on branch 1; `run --change ADDRESS` is the stronger override. Do
  not "simplify" this by trusting any unnamed output — the whole product is the
  check that every output is accounted for.
- **A batch with no change output arms with no further prompt.** `ChangeMissing`
  is a `Finding` on `Verification.Reports`, not a `Problem`, and `Finding` is a
  separate type on purpose so a demoted finding cannot reach `OK()`. Name what is
  missing (the lever); never imply what is not (risk).
- **The recipients CSV is BTC, eight places, no separator, label quoted, header
  row.** Settled by measurement against Sparrow 2.5.3, whose amount unit this
  program cannot see. **Do not add a `--csv-unit` flag.**
- **The app writes nothing it later trusts.** `os.WriteFile` appears in exactly
  one place in `internal/`, and a failure to write the CSV is reported rather
  than returned.
- **The printed recipients table is the attribution and is not the CSV's
  preview.** Both renderings come off one `[]Recipient` in one call.
- **Settlement carries on per member.** One stuck channel is recorded and named
  at the end with its channel point; it never ends the loop for the rest. Do not
  restore an early return.
- **An unconfirmed batch is frozen, not lost.** The coins are ours and unspent,
  so nothing is at risk — but it cannot be abandoned either, and CPFP out of the
  change is the exit rather than a speedup. `docs/design.html` documents the
  out-of-band double-spend an operator may perform with their own tools; no code
  path here builds one.
- **LND will force-close a pending channel if asked**, marking it
  `ChanStatusBorked|ChanStatusCommitBroadcasted` and broadcasting a commitment
  whose parent may exist nowhere. So `internal/abort` abandons rather than
  closes, and the armed screen says outright that closing one recovers nothing.

**On the wait for a file**

- **`run.readWhole` skips an empty file without reading it and stats either side
  of the read.** Two stats and no extra tick, so a file that was already complete
  on the first poll is read on that poll.
- **Do not close the remaining window by retrying on a decode failure.** It would
  work, and it would make a genuinely wrong file — the operator saved the wrong
  transaction, or signed at step 4 — indistinguishable from a slow one. Step 4's
  refusal is I-1's last gate, and a gate that waits instead of refusing is not
  one.
- **Every watched path is printed.** Watching a name the operator is never told
  is the same defect as not watching it.

---

## Rejected approaches — do not reintroduce

- **`base_psbt` chaining.** An `lncli` ergonomic crutch that accumulates outputs
  across sequential opens. Chaining makes LND the assembler, so there is never a
  single moment at which the whole output set is checked against the whole plan.
  Our verifier checks all *n* outputs at once, which is what catches a swapped,
  missing or duplicated one. Chaining defeats the check the app exists for.

- **Any broadcast path that could carry a funding transaction outside the gate.**
  There is **exactly one** call to `WalletKit.PublishTransaction` — `arm.Publish`,
  the funding transaction, behind the I-1 gate — and the count is enforced:
  `Method.CallSites` pins it at 1 and `TestEveryLNDCallSiteIsRegistered` fails on
  a second.

  `arm.Armed` carries the *n* `chan_pending` receipts in an unexported map that
  only `arm.Receipts` fills, and `arm.Publish` re-derives the txid from the bytes
  it is handed and refuses any that do not hash to the pinned one. With one call
  site left there is nothing to keep apart by type, so the number is the whole
  claim — which is a reason to guard `CallSites` harder rather than to relax it.

- **Bumping the funding transaction, by any route.** I-4. Nothing in this
  repository replaces a parent.

  This is not contradicted by the frozen-batch escape in `docs/design.html`. That
  procedure is something an operator performs with their own tools. The line is
  authorship, not knowledge: we may tell an operator what the only way out is,
  and still refuse to be the thing that does it.

- **`AbandonChannel(i_know_what_i_am_doing)`** as the default. Try
  `pending_funding_shim_only` first, and fall back to the blunt flag only on its
  specific rejection, and only with explicit confirmation. **Note that under the
  new sequence the fallback is the normal path, not the exception**: LND infers
  "shim funded" from `ThawHeight > 0` (see the `TODO` at `rpcserver.go:3344`) and
  a plain PSBT open sets none, so it refused every channel in the regtest proof
  with *"is not externally funded or not pending"*. The confirmation still gets
  asked; it just always gets asked.

- **Third-party APIs.** Peer facts come from LND's local gossip graph via
  `GetNodeInfo`. Never a block explorer, fee API, or Lightning explorer — those
  log exactly the amounts, peers and timing we are trying not to leak. The rule
  is "no sockets of our own", not "no network": LND talking to peers is the point.

  **Fee rates came from Core's `estimatesmartfee`, and nothing replaced it,
  because this rule forbids the obvious substitute.** A fee API is handed the
  size of what is being built and the moment it is being built, which together
  are most of what this tool exists not to leak. The operator reads a rate from
  their own wallet, their own node or a block explorer and types it into Sparrow;
  that is theirs to do and this program never learns the number.

---

## Development environments

- **regtest, in `regtest/`** — the inner loop, built by `make harness`. A
  hand-written `docker-compose.yml`: one bitcoind, one "our" node (alice) and
  three peers, so batch sizes up to *n* = 3 are testable. bitcoind is Polar's
  image; LND is **Lightning Labs' own**, because Polar publishes no LND tag past
  `0.20.0-beta` and the harness has to run the version an operator actually runs.
  The harness diverges from Polar's defaults deliberately —
  `maxpendingchannels=200`.

  Lightning Labs' image differs from Polar's in three ways the compose file
  absorbs: its entrypoint is `lnd` itself, so `command` carries flags and not the
  binary name; it runs as root with its data in `/root/.lnd`, which
  `regtest/Makefile`'s `creds` target and `regtest/bin/lncli` both name; and it
  has no `USERID`/`GROUPID` shim, which costs nothing because the state lives in
  named volumes.

  Confirmation depth, stuck transactions, a force-close of an armed channel and
  LND's ~2016-block forget horizon are all testable in seconds. **The peers'
  ten-minute window is not** — that clock is wall-clock and cannot be mined
  forward, so `internal/arm/clock_regtest_test.go` is gated behind
  `WINTHISTLE_SLOW`.

- **Simulated multisig cold wallet** — two key-enabled Core wallets, xpubs
  assembled into `wsh(sortedmulti(2,…))`, imported watch-only, signed via
  `walletprocesspsbt` in each and `combinepsbt`. No hardware, fully scriptable.
  It lives in `regtest/cold-wallet.py`, `internal/regtestenv/cold.go` and
  `internal/regtestenv/coldwallet/`. It is **the stand-in for Sparrow**, since a
  regtest test still needs something to build and sign a funding transaction —
  the harness playing the operator's part, not a back door for a mode where the
  app builds the batch.

  **The harness owns its coin locks.** `walletcreatefundedpsbt` is called with
  `lockUnspents` and the app takes no locks, so `Env.BuildPSBTPaying` registers
  the release itself. A leaked lock does not announce itself: it starves the next
  test in the same binary with "Insufficient funds" against a wallet whose
  balance is fine.

  `Env.SignLikeSparrow` unions and finalizes outside the app, so what a test
  hands to `combine.Accept` is one packet with a complete witness — the input the
  production path will get. It is the only remaining caller of the multi-packet
  path, and the reason `combine.Merge` and its finalized-input refusal stay.
  `Env.RecipientsIn` reads the recipients off the printed step-4 table, which is
  the only test that the table is legible.

- **There is no CI in this repository.** No `.github/`, no configuration of any
  kind. Run it with `make test`.

- **mainnet cold probe** — commissioning only, per `docs/design.html`. Proves
  this node, these peers, these devices, with `winthistle run` stopped before
  step 8 and coins that never move. Passed 2026-08-26. Not a thing to repeat
  casually: every channel in a probe reaches `chan_pending` and is then
  abandoned, so each probed peer holds one of its pending-channel slots for
  ~2016 blocks afterwards.

---

## Repo hygiene

This repo is **private and intended to go public**. Assume every commit will
eventually be public.

- **Never commit a mainnet xpub.** A single one deanonymises the whole cold
  wallet's history, permanently, and git history cannot be un-published. Use
  `tpub`/regtest keys in every fixture — and note `.gitignore` blocks descriptor
  and PSBT *files*, but an xpub pasted inline in a test or a doc example sails
  straight through it.
- No macaroons, certs, cookies, `winthistle.toml`, or `*.db` — all gitignored;
  keep it that way.
- Commit identity is `AusDavo <david@dpinkerton.com>`, not the client
  address.
- **Never `git checkout` an unstaged file.** This repository is worked on with
  everything unstaged. Copy it aside and copy it back.
- **Never a bare `go test ./...`.** Parallel packages share one regtest node and
  it fails like a real bug. `-p 1`, always. Exactly two skips
  (`WINTHISTLE_SLOW`); a third is a finding. A `(cached)` package is not a
  package that ran.
- **`make -C regtest info` must show `peers=3`** before any green is believed: a
  dead harness skips every live test while the package prints `ok`. And
  `TestTheFundingHorizonIsReachedByMining` mines 2016 blocks per run, so a
  long-lived cluster eventually exceeds `bitcoind.DefaultTimeout` on
  `generatetoaddress` — that is a red to fix with `make harness`, not a bug.
- **Changing `docs/design.html` means republishing it in the same slice.** It is
  published as an artifact and editing the file does not update the published
  page, which is the only copy an outside reader sees. Diff the live source
  against the local file rather than assuming the local one is ahead.
- **Never hardcode the macaroon permission list in docs.** Generate it from the
  method registry (`winthistle print-macaroon-command`). It drifted anyway once,
  with both lists the same length, so the count matched while the membership did
  not.

## Style

Match the doc's register in user-facing copy: say what happened and what to do,
never euphemise a risk. Recovery copy is the highest-stakes in the product.

**Recovery must name clock B in blocks.** There are two clocks and they are not
interchangeable. Clock A is the peers' ten minutes, covering steps 2 to 6;
blowing it costs a restart and nothing else. Clock B is ~2016 blocks from
broadcast, covering step 6 to confirmation; blowing it means the peer has deleted
their state, and a funding transaction that confirms afterwards leaves coins in a
2-of-2 with a counterparty who no longer knows about the channel. So say "peers
give up at block 887,412, about 13 days" rather than saying there is time.
`waitForTimeout` counts `lncfg.DefaultMaxWaitNumBlocksFundingConf` (2016) from
the channel's broadcast height, and `fundingTimeout` is "only returned for the
responder" (`funding/manager.go:2940`).
