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

Direction and rationale: `docs/replan-2026-08.md`. Full spec: `docs/design.html`.

## The sequence

```
1  pre-flight            peers reachable? anchor reserve sufficient?    [LND only]
2  open n streams        psbt_shim + no_publish              ← clock A starts
3  print the plan        peer · alias · address · amount · policy
4  build in Sparrow      enter the recipients, pick coins and fee, Save PSBT
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

**What is built and exercised against live regtest**, and is the guide to what
exists: `winthistle setup`, `run`, `bump`, `doctor`, `recover` and `serve` all
work against the cluster in `regtest/`. `internal/arm` holds the I-1 gate,
observed at *n* = 3. `internal/combine` has seventeen adversarial tests on
inbound PSBTs. `internal/plan` has the batch verifier. `winthistle serve` carries
a loopback bind, a startup token, strict `Origin` and `Host` checks and no CORS,
and can open a batch, run a setup and a bump, and serve the journal read-only.
The signet harness is two bitcoinds and no LND. None of that is broken, and none
of it should be described as broken.

**What the replan cuts, and which item deletes it.** Nothing has been deleted
yet — this is a description of intent, not of the tree:

- **Item 3** changes `internal/arm` to the new sequence: `skip_finalize` at
  verify, receipts before signing, the journal's reordered states.
- **Item 4** adds the `--psbt` path to `run` and stops calling `coldwallet`.
  Note `run` today has `--psbt-dir`, which is the file handshake for a signer
  with no command. It is a different flag; do not conflate them.

  **A collision to settle before writing that path.** `internal/combine`
  survives, and `combine.ErrAlreadyFinalized` (`combine.go:94`) refuses a device
  that returns a *finalized* input — with I-2 named in the comment and the error
  text saying "Only partial signatures may leave a signer". Step 7 is "sign in
  Sparrow", which returns exactly that. **The surviving code refuses the new
  happy path's own input, citing a dissolved invariant.** Decide it explicitly;
  do not delete the check silently. The base-packet guard at `combine.go:249` is
  a different check and should stay.
- **Item 5** deletes `coldwallet`, `setup` and the `setups` table, `bump`,
  `rehearsal`, `signers`' multi-device round, `server`, `webrun`, `signet/`,
  `doctor`'s Core checks, and `internal/bitcoind` from the application.
  `internal/bitcoind` and the simulated multisig cold wallet survive **inside
  the harness** as the stand-in for Sparrow.
- **Item 6** demotes the fee and change findings to reports, and removes
  `Replaceable`.

**Two publish call sites exist today** and `Method.CallSites` pins the count at
2. The replan takes it to 1, *after* `internal/bump` is deleted in item 5. Do not
change the pin before then.

What survives: `arm` · `plan` · `combine` · `peers` · `reserve` · `settle` ·
`journal` · `methods` · `lnd` · `prose` · `config`.

**What is still missing is the mainnet cold probe.** The safety model below is
verified against LND source *and* against a running node — but never against
mainnet, which is what the probe is for.

---

## The three invariants

These are the product, not preferences. Each carries a source citation so you can
verify it rather than trust this file.

**If you believe an invariant is wrong, say so and stop. Do not work around one,
and do not weaken one to make a test pass.**

### I-1 · Publish only once every channel is already recoverable

`no_publish` **and** `skip_finalize` MUST be set on **every** channel in a batch.
Verify all *n*, wait for all *n* `chan_pending`, then publish exactly once via
`WalletKit.PublishTransaction`.

Why it holds, in source: in `funding/manager.go`, **`funderProcessFundingSigned`**
(`:2694`) calls `CompleteReservation(nil, commitSig)` at `:2789` — storing the
peer's commitment signature — *before* the broadcast block at `:2805`, which is
guarded by `completeChan.ChanType.HasFundingTx()`, and emits `chan_pending` at
`:2884`, after it. So each `chan_pending` is a receipt that the channel is
recoverable by force-close. `no_publish` sets `NoFundingTxBit`
(`lnwallet/reservation.go:402`), which clears `HasFundingTx()`.

**The function is `funderProcessFundingSigned`.** This file used to cite
`handleFundingSigned`, which does not exist at v0.19.3-beta and never did. The
ordering was right and the name was ungreppable, which is how a citation stops
being checkable.

`NoFundingTxBit` does not skip *only* the broadcast. It gates four things: the
broadcast (`:2805`), the startup rebroadcast (`:753`, `:760`), the funding-input
witness verification inside `CompleteReservation`
(`lnwallet/wallet.go:2274`, which has no witnesses to check), and the
transaction label (`:3243`). `CompleteReservation`, `WatchNewChannel` and the
`chan_pending` emission are untouched, which is what I-1 needs.

Why `skip_finalize` is safe, in source: `PsbtIntent.Verify` ends
(`lnwallet/chanfunding/psbt_assembler.go:290-304`) with, when
`!i.shouldPublish && skipFinalize`, `i.FinalTX = packet.UnsignedTx`,
`i.State = PsbtFinalized`, and `close(i.PsbtReady)`. `funding/manager.go:2308`
reads that channel with a bare `case nil:` — *"Nil error means the flow continues
normally now."* `CompileFundingTx` still runs (`lnwallet/wallet.go:1880`) because
it "sets the actual funding outpoint in stone" (`:1879`), and the unsigned
transaction suffices: all inputs are segwit, so witnesses do not move the txid.
LND refuses `skip_finalize` without `no_publish` (`lnwallet/wallet.go:764`).

**Proved on regtest, 2026-08-25.** `TestSkipFinalizeReachesChanPendingWithNothingSigned`
in `internal/arm/skip_finalize_regtest_test.go`: *n* = 2 streams reached
`chan_pending` at the outpoints of an **unsigned** transaction, with nothing
signed and the mempool clear. Both receipts arrived inside 0.55 s.

NEVER change this to "all but the last", **even though that is what LND's own
docs recommend** (`docs/psbt.md:643`). That idiom exists because `lncli` has no
way to broadcast afterwards — a CLI limitation, not a protocol constraint. LND's
"DO NOT PUBLISH … OR THE FUNDS CAN BE LOST" warning (`docs/psbt.md:345`) is about
*ordering*, not authorship.

Consequence to preserve: `NoFundingTxBit` also gates `rebroadcastFundingTx`
(`funding/manager.go:757`), so we own rebroadcast. Publish through WalletKit (its
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

### I-3 · The TXID must not move after verification

LND commits to the funding outpoint at `psbt_verify`. Hash the unsigned tx at
verify and reject any returned PSBT whose unsigned TXID differs, naming the
offending device.

**I-3 now carries the weight I-2 used to.** It is the *only* load-bearing check
on what comes back from the signing wallet.

And it is not a duplicate of LND's own check, which is narrower than it sounds.
`PsbtIntent.FinalizeRawTX` (`psbt_assembler.go:342`) compares the outputs and the
inputs' *previous outpoints* and stops — its own comment says "the fields in the
PSBT part are allowed to change" — so sequence numbers, version and locktime are
unchecked, and each moves the txid. `verifyInputsSigned` only asserts that each
input has *something* attached. Our hash-at-verify is what enforces I-3.

This is also why all inputs must be segwit — see `verifyAllInputsSegWit`
(`psbt_assembler.go:611`), called at `:281` with "risk of malleability".

### I-4 · No RBF on the funding transaction, ever

Replacing the funding tx changes the outpoints and destroys every channel in the
batch. **Enforced by authorship, which is all that ever enforced it:** only we
can sign our inputs, and there is no code path in this repository that replaces a
funding transaction.

**The `Replaceable` sequence-number refusal is a lint, and it is slated for
removal in item 6. It is still there today** (`internal/plan/verify.go:302`), and
until item 6 it still refuses. Core 29's full-RBF is unconditional — verified
live: `mempoolfullrbf` does not exist even as a hidden debug option
(`bitcoind -help-debug` has no such flag), and `getmempoolinfo` reports
`"fullrbf": true` with no way to turn it off — so a higher-fee conflict relays
regardless of what our sequence numbers signal. Refusing a transaction over a
signal that changes nothing is a lint wearing an invariant's clothes.
`replaceable: false` is a statement of intent, not a defence.

What the invariant covers, and what it does not:

- **The funding transaction: never.** *n* peers hold commitment signatures
  against its outpoints. Replacing it destroys the batch.
- **The CPFP child: always.** `settle.buildChildAt` sets `plan.MaxBIP125Sequence`
  and `replaceable: true`, and `internal/bump`'s verifier *requires* it. Nobody
  has committed to anything about a child, so replacing one moves nothing anyone
  depends on. **`bump` goes in item 5**, and with it the second verifier; until
  then both rules are stated at once by having two verifiers rather than one
  verifier with a flag on it.

If a change ever makes a *funding* transaction replaceable, that is the invariant
breaking and the answer is to stop, not to edit this section.

---

## Why the change output is required

The batch verifier refuses a transaction with no change output, or with change
too small to fund a child that lifts the package to `Fee.cpfpTarget()`.

**That gate is real today, and item 6 demotes it to a report.** `ChangeMissing`,
`ChangeTooSmall`, `FeeTooLow` and `FeeTooHigh` move to `Verification.Unchecked`,
which exists "so that a clean result is not read as a broader guarantee than it
is". The reasoning below survives the demotion intact — it is why the fact is
worth *saying*. Your fee and change arrangements are yours, and after the
inversion the app does not build the transaction and so cannot size a change
output; it can only tell you yours is too small.

**Nothing is at risk while the batch is unconfirmed.** The coins are ours,
unspent, in a transaction only we could have signed. Every channel reached
`chan_pending`, which makes it recoverable by force-close *once the funding
transaction confirms* — before that there is no channel yet, only a promise.

**But it cannot be abandoned either.** An unconfirmed funding transaction never
becomes safe to abandon on its own: its inputs stay unspent, so it stays valid
indefinitely, and eviction from mempools does not invalidate it. `run.RecoverOne`
therefore refuses any run that reached the publish call
(`journal.ErrMayBePublished`), because abandoning a pending channel whose funding
transaction *later* confirms strands its funds with no force-close path. A batch
that never confirms leaves its coins **frozen**: not abortable, not safely
spendable, waiting on the mempool.

**So CPFP is the exit from that state, not a speedup.** Confirm the batch, then
close the *n* channels normally if you no longer want them.

**And it cannot rescue an evicted parent.** A child of an absent parent is an
orphan. Covering that case would need Core's `submitpackage` for 1p1c relay,
which would be another path to the network.

**The old reason was wrong, and the wrong reason was the dangerous part.** The
gate used to be justified in custody language — as though a stuck batch put coins
at risk. It does not, and an operator who believes it does will reach, under
pressure, for the one thing I-4 forbids.

**That copy is still in the code.** Three strings still say it, two of them
operator-facing: `internal/plan/plan.go:395` ("change is the only way a stuck
batch can be accelerated", an error message), `internal/plan/report.go:101` (the
same claim, in a plan-report bullet) and `internal/plan/size.go:246` ("a batch
with nothing to rescue it", a doc comment). Fixing them is item 6's business,
alongside the demotion. Until then this file and that copy disagree, and this
file is right.

**The escape this build does not implement.** If a batch is frozen anyway, the
only route out is an out-of-band double-spend of one of its inputs, performed by
the operator with their own tools. `docs/design.html` documents the procedure and
the ordering that keeps it from losing funds: keep every channel's state until
the replacement is deeply confirmed, and abandon only then. Documenting it is not
a relaxation of I-4. No code path here builds one, and none may be added.

---

## Rejected approaches — do not reintroduce

- **`base_psbt` chaining.** An `lncli` ergonomic crutch that accumulates outputs
  across sequential opens. **Its old justification was "we build the tx
  ourselves", and that is no longer true** — Sparrow builds it. The reason that
  survives is better: chaining makes LND the assembler, so there is never a
  single moment at which the whole output set is checked against the whole plan.
  Our verifier checks all *n* outputs at once, which is what catches a swapped,
  missing or duplicated one. Chaining defeats the check that the app exists for.

- **Any broadcast path that could carry a funding transaction outside the gate.**
  There are **exactly two** calls to `WalletKit.PublishTransaction`, and the count
  is enforced: `Method.CallSites` pins it at 2 and
  `TestEveryLNDCallSiteIsRegistered` fails on a third.

  1. `arm.Publish` — the funding transaction, behind the I-1 gate.
  2. `bump.Publish` — the CPFP child of a batch that is already public. It has no
     gate to sit behind and needs none: by the time a child can be built the
     parent is in a mempool and every channel reached `chan_pending` before that.

  What keeps them apart is the type system. `arm.Publish` takes an `*arm.Armed`
  and `bump.Publish` takes a `*bump.Signed`; each has its raw transaction in an
  unexported field that exactly one constructor fills. Neither line can be handed
  the other's bytes.

  **Item 5 takes this to one.** Change `CallSites`, this bullet and
  `docs/design.html` in the same commit as the deletion, or do not change it.

  `testmempoolaccept` validates without relaying and has been the only pre-flight.
  **Its only production caller is `internal/rehearsal`, which item 5 deletes, and
  it needs Core, which item 5 also removes** — so after item 5 there is no
  pre-flight at all. That is a real consequence to decide on, not a detail to
  paper over.

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
  "shim funded" from `ThawHeight > 0` (see the `TODO` at `rpcserver.go:3236`) and
  a plain PSBT open sets none, so it refused every channel in the regtest proof
  with *"is not externally funded or not pending"*. The confirmation still gets
  asked; it just always gets asked.

- **Third-party APIs.** Peer facts come from LND's local gossip graph via
  `GetNodeInfo`. Never a block explorer, fee API, or Lightning explorer — those
  log exactly the amounts, peers and timing we are trying not to leak. The rule
  is "no sockets of our own", not "no network": LND talking to peers is the point.

  **Fee rates came from Core's `estimatesmartfee`, and item 5 removes Core.** The
  replan does not say what replaces it, and `internal/fees` appears on neither
  its survive list nor its goes list. Sparrow picks the fee at step 4, but
  `plan.Fee.TargetSatPerVB` still needs a number to judge against, and this rule
  forbids the obvious substitute. **Open question — decide it, do not let a
  third-party call in by default.**

- **Hardcoding the macaroon permission list in docs.** Generate it from the
  method registry (`winthistle print-macaroon-command`) so it cannot drift. It
  drifted anyway: `docs/design.html`'s illustrative block named `WalletBalance`
  and `SubscribeChannelEvents`, which the build does not call, and omitted
  `CheckMacaroonPermissions` and `GetNodeInfo`, which it does. Both lists were 17
  long, so the count matched while the membership did not.

---

## Build order

From `docs/replan-2026-08.md`, which is the document that describes the future.

1. ~~Prove the inversion on regtest.~~ **Done, 2026-08-25.**
2. ~~Rewrite the docs to this direction.~~ **Done.**
3. Change `arm` to the new sequence.
4. Add the `--psbt` path to `run`, stop calling `coldwallet`.
5. Delete the cut packages.
6. Demote the fee and change findings, remove `Replaceable`.

Modify in place, on a branch, keeping the tool working at every commit. The
packages that survive are the ones that were expensive to get right and are
verified against a running node; rewriting them to avoid deleting the peripheral
code would discard the only thing this repository has that a new one would not,
which is evidence.

**Abort and recovery still ship before the happy path**, because the
commissioning cold probe runs the real production flow and *terminates via the
abort path*.

---

## Development environments

- **regtest, in `regtest/`** — the inner loop. A hand-written
  `docker-compose.yml`: one bitcoind, one "our" node (alice) and three peers, so
  batch sizes up to *n* = 3 are testable. It **borrows Polar's images** and says
  so in `regtest/.env`, and deliberately diverges from Polar's defaults —
  `maxpendingchannels=200`, "not the 10 Polar would give you". This file used to
  call the harness "regtest, via Polar (Docker)", which read as though the GUI
  were the inner loop. It is not, and `make harness` is what builds it.

  Confirmation depth, stuck transactions, CPFP and LND's ~2016-block forget
  horizon are all testable in seconds — the horizon is about ten seconds of
  mining. **The peers' ten-minute window is not**, and this file used to list it
  among them. That clock is wall-clock and cannot be mined forward:
  `internal/arm/clock_regtest_test.go` is gated behind `WINTHISTLE_SLOW` and
  calls itself "the one test in this repository that takes longer than a coffee".

- **Simulated multisig cold wallet** — two key-enabled Core wallets, xpubs
  assembled into `wsh(sortedmulti(2,…))`, imported watch-only, signed via
  `walletprocesspsbt` in each and `combinepsbt`. No hardware, fully scriptable.
  It lives in `regtest/cold-wallet.py` and `internal/regtestenv/cold.go`, and it
  **survives item 5 as a harness fixture** — the stand-in for Sparrow, since a
  regtest test still needs something to build and sign a funding transaction.
  That is the harness playing the operator's part, not a back door for directed
  mode.

  **It is not run by CI, because there is no CI in this repository.** This file
  used to say "this is what CI uses" and `README.md` said the same. There is no
  `.github/`, no CI configuration of any kind. Run it with `make test`.

- **signet, in `signet/`** — Core only, for the descriptor-import rescan over a
  chain with real history and the prune-horizon check. **Slated for deletion in
  item 5**, because both paths belong to `coldwallet` and `setup`, which go.
  Measured costs are in `signet/README.md`; do not restate them here.

- **mainnet cold probe** — commissioning only, per `docs/design.html`. Proves
  this node, these peers, these devices. **No signet or regtest substitute for
  it, and the replan does not change it:** `winthistle run` stopped before step
  8, against real peers, with coins that never move. This is the one thing still
  missing.

---

## Repo hygiene

This repo is **private and intended to go public** once the cold probe passes on
mainnet. Assume every commit will eventually be public.

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
  it fails like a real bug. `-p 1`, always.
- **Changing `docs/design.html` means republishing it in the same slice.** It is
  published as an artifact and editing the file does not update the published
  page, which is the only copy an outside reader sees. The two drifted five
  statements apart before anyone checked. Diff the live source against the local
  file rather than assuming the local one is ahead.

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
responder" (`funding/manager.go:2914`).
