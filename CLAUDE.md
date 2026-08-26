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

**The whole build runs on LND v0.21.2-beta**, in `go.mod` and in the harness,
and every source citation below is against that version. It was pinned at
v0.19.3-beta from the scaffold commit onward with no rationale recorded anywhere
— a tag that was already a year old the day the repository started — while the
node it is meant to arm was two minor releases ahead. Nothing in the safety model
turned out to be wrong at v0.21.2-beta; roughly thirty line numbers were, which
is the same way `handleFundingSigned` happened. **On the next bump, re-cite
before assuming**: `go.mod`, `regtest/.env` and the citations are three separate
pins and only the first moves by itself.

**What is built and exercised against live regtest**, and is the guide to what
exists: `winthistle run`, `doctor` and `recover` work against the cluster in
`regtest/`, and those three plus `print-macaroon-command` and the two
`example-*` printers are the whole command set. `internal/arm` runs the
**inverted** sequence — `skip_finalize` at verify, the *n* receipts before
anything is signed, one publish — and the I-1 gate is observed at *n* = 3 with
nothing signed when it opens. `run` **builds nothing and signs nothing**: it
prints the recipients, reads the unsigned transaction back from `--psbt FILE`,
and reads the signed one from `FILE-signed.psbt` or `FILE-signed.txn`. `internal/combine` has twenty
adversarial tests on inbound PSBTs. `internal/plan` has the batch verifier. None
of that is broken, and none of it should be described as broken.

**All six items of the replan are done, and item 5 deleted 28,267 lines.** The
tree went from 55,670 Go lines to 28,881. Read
`docs/replan-2026-08.md`'s "Item 3, as built", "Item 4, as built" and "Item 5, as
built" for the account; the short version:

- ~~**Item 3** changes `internal/arm` to the new sequence.~~ **Done.**
  `arm.Verify` sends `skip_finalize` on all *n* and pins the txid before the first
  call; `arm.Receipts` collects the receipts and is the gate; `arm.Finalize` is
  gone and there is **no `psbt_finalize` call anywhere in this build**;
  `arm.Publish` takes the signed bytes as a parameter and refuses any whose txid
  is not the pinned one. The journal runs arming → armed → signing → publishing →
  published.
- ~~**Item 4** adds the `--psbt` path to `run` and stops calling `coldwallet`.~~
  **Done.** `run.SigningWallet` is the seam (`Built` at step 4, `Signed` at step
  7), with `run.FileWallet` behind `--psbt FILE`. `combine` is *the acceptance
  check on an inbound PSBT*: `combine.Accept` is the batch's path and takes
  complete witnesses; `combine.Unsigned` refuses a step-4 packet that carries any
  signature, which is I-1 at the last place it can be defeated from outside; and
  the change output is *recognised* rather than named — `plan.RecogniseChangeIn`
  reads the master fingerprints off the transaction's own inputs and accepts an
  output carrying those on branch 1, with `run --change ADDRESS` as the stronger
  override.

  **Step 7 reads two encodings and step 4 reads one, and that gap is
  load-bearing** — issue #3, found by the cold probe and landed 2026-08-26.
  `FileWallet.Signed` takes a signed PSBT *or* a finalized raw transaction, hex
  or binary, because that is what Sparrow's *View Final Transaction* yields and
  what `lncli` is fed at the equivalent prompt; a mainnet batch was torn down
  over the wrapper alone, costing both peers ~2016 blocks.
  `combine.SignedFromTX` lifts the witnesses onto the base and refuses a moved
  txid (I-3) or a transaction with no witnesses, and `combine.Accept` then runs
  unchanged, so there is one acceptance path and not two. **The relaxation is at
  step 7's call site and must never move into `combine.Parse`**, which `Built`
  still calls alone: a raw *signed* transaction at step 4 is exactly what
  `combine.Unsigned` exists to refuse, and `combine.Unsigned` refuses by reading
  a packet's partial signatures — which a raw transaction has none of, so it
  would be accepted in silence by a check with nothing to look at.
  `TestStepFourStillRefusesARawSignedTransaction` is the guard, and
  `TestTheFilePathDrivesTheWholeSequence` runs the whole sequence once per
  encoding against the live node.

  **Step 7 watches two names, because the encoding it accepts has a
  conventional extension it was not looking for** — issue #5, landed
  2026-08-26. `SignedPath` derives its extension from `--psbt`, so it asserted
  `.psbt`, and a wallet asked to save a raw transaction names it `.txn`.
  `FileWallet.SignedPaths` returns both, `waitAny` polls them in order so
  `SignedPath` still wins a tie, and a candidate equal to `Unsigned` or to
  another is dropped — the signed file may never overwrite the one it is
  compared against. **The failure this fixes was silence, not a refusal**: step
  7 has no deadline by design, so a file under the unwatched name left the run
  waiting while the wallet reported it had saved. Every watched path is printed,
  because watching a name the operator is never told is the same defect as not
  watching it.
- ~~**Item 5** deletes the cut packages.~~ **Done, 2026-08-26.** `coldwallet`'s
  setup half, `setup` and the `setups` table, `bump`, `rehearsal`, `signers`,
  `server`, `webrun`, `signet/` and `signetenv`, `fees`, `doctor`'s Core checks,
  the coin-lock machinery, and Bitcoin Core from the application entirely.
  `internal/bitcoind` and the simulated multisig cold wallet survive **inside
  `internal/regtestenv`** as the stand-in for Sparrow — the cold wallet is
  literally there now, at `internal/regtestenv/coldwallet`.
- ~~**Item 6** demotes the fee and change findings to reports, and removes
  `Replaceable`.~~ **Done, 2026-08-26.** `ChangeMissing`, `ChangeTooSmall`,
  `FeeTooLow` and `FeeTooHigh` became `plan.Finding`s on `Verification.Reports`
  — **not** `Unchecked`, which would have made that heading lie — and `OK()` is
  still `len(v.Problems) == 0`. **Three of those four are gone**: issue #2
  removed the declared fee rate later the same day, and `FeeTooLow`, `FeeTooHigh`
  and `ChangeTooSmall` went with the number they judged against. `ChangeMissing`
  is the one that remains, and the `Reports` list it renders under is item 6's
  real legacy. The `Replaceable` code, its refusal and
  `MaxNonReplaceableSequence` are gone. See `docs/replan-2026-08.md`'s "Item 6,
  as built". **All six items are done, and the mainnet cold probe passed on
  2026-08-26** — run `20260826-043441-9a8f28`, two channels, both to
  `chan_pending` with nothing signed.

**Three things item 5 decided, which the code now depends on:**

1. **There is no fee rate anywhere in this build.** Item 5 made it declared
   rather than fetched — `[fees] target_sat_per_vb`, with `run --fee-rate N` —
   and **issue #2 removed it entirely on 2026-08-26**, because the honest
   conclusion of "the app does not choose the fee" is that it should hold no
   opinion about it either. Requiring the same number a second time, in a config
   file, so a report could compare it against the first was theatre.
   `internal/fees` went with Core in item 5; `internal/run/fee.go`, `plan.Fee`,
   `--fee-rate` and `doctor`'s fee check went with issue #2. **Step 5 computes the
   rate and reports it, and nothing grades it.** `[fees]` is a retired *section*
   in `internal/config/toml.go`, so a config file that still has one is refused
   with a sentence rather than a shrug. **Do not add a fee target back** — not as
   a key, not as a flag, and not as an estimate; the no-third-party rule below
   still forbids the last of those independently.
2. **There is no pre-flight.** `testmempoolaccept` went with Core. What survives
   is narrower and is not nothing: `combine.Accept` executes every input's witness
   against its own script. What is lost is node policy — min relay fee,
   standardness, ancestor limits — and `plan.Verify`'s `Verification.Unchecked`
   says so, naming `testmempoolaccept` and saying this build does not run it.
3. **`Method.CallSites` is 1.**

**One `PublishTransaction` call site exists** and `Method.CallSites` pins it at 1.

What survives, and what `internal/run` imports: `arm` · `plan` · `combine` ·
`peers` · `reserve` · `settle` · `journal` · `methods` · `lnd` · `prose` ·
`config` · `abort`. Plus `doctor` and `policy` outside the run path, and
`internal/bitcoind` and `internal/regtestenv/coldwallet` inside the harness only.

**The mainnet cold probe passed on 2026-08-26**, run
`20260826-043441-9a8f28`. The safety model below is verified against LND source,
against a running regtest node, **and now against mainnet**: two real peers, two
of two `chan_pending` over an unsigned transaction, backups exported off pending
channels (7, 4,738 bytes), a signed transaction whose txid had not moved, step 8
withheld, and a teardown that left nothing on this node.

**Three attempts failed first, and none of them failed on the safety model.**
One died unattended before arming; one blew clock A while the operator was in
Sparrow; one reached the gate and then refused the signed file because it was a
raw transaction rather than a PSBT — which became issue #3 and landed the same
day. The model held first time; the ergonomics did not, which is the right way
round and is what a commissioning probe is for.

**The first live batch published on 2026-08-26**, run
`20260826-191016-d9407c`: five channels, 9,000,000 sat, txid
`a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90`, five of
five `chan_pending` with nothing signed, ten backups off pending channels, the
txid unmoved at step 7, published once, confirmed. **Step 8 was taken for the
first time.** `--probe` paid for itself immediately: the graph's smallest
existing channel is a poor proxy for a peer's minimum and was wrong for three
of the five, because a node's smallest channel may be one *it* opened
outbound, which its own inbound minimum never constrained. Only
`accept_channel`'s refusal is authoritative, and it names the figure.

**And then settlement stopped on the one channel that had opened — issue #6,
landed 2026-08-27.** Two defects in `internal/settle`, and the second was the
worse one. Both came from the same assumption: that a batch's members share a
fate. **They share a funding transaction and nothing else.**

- **`UPDATE_FAILURE_UNKNOWN` was classified terminal, and it is LND's
  catch-all rather than a verdict.** A freshly-opened channel with a briefly
  offline peer lands there, and the identical update applied cleanly by hand
  minutes later. **But the reading it replaced was guarding something real** —
  retrying forever is also wrong — so the bound moved rather than went.
  `PolicyOutcome.Terminal()` is `INVALID_PARAMETER` and nothing else, terminal
  on the first refusal, because LND checks the CLTV delta and the inbound fees
  against its own bounds before it looks at the channel at all. `PENDING` and
  `NOT_FOUND` retry with no clock, because both name what they are waiting
  for. `PolicyOutcome.Unexplained()` — `UNKNOWN`, `INTERNAL_ERR` and **any
  reason this build does not recognise** — retries for `settle.RetryWindow`,
  ten minutes from the first refusal of that kind, and is then reported. **An
  unrecognised value is deliberately retried rather than called terminal**:
  treating a value you cannot interpret as a verdict on the policy is exactly
  the mistake `UNKNOWN` was.

  The bound is in *time* rather than attempts, because a count of attempts
  only means minutes at one particular `Options.Interval` and the interval
  belongs to the caller. `Options.RetryWindow` is the seam that makes the
  window testable without injecting a clock, and `Tick` holds the only clock
  there is, so `State.Stuck()` stays a question about a `Result` rather than
  about the moment it is asked.

- **`Settle` returned on the first stuck member, and that is the half that
  cost something.** Four channels had not even opened yet and lost their
  watcher, so each went live at LND's defaults — 1000 msat and 1 ppm — which
  is the drain window the loop exists to close. A stuck member is now
  recorded, the loop carries on for everyone else, and one `ErrStuck` comes
  back at the end naming every one of them **with its channel point**, because
  the operator's next move is one `updatechanpolicy` per stuck channel.
  `Result.finished()` is the exit, and it is neither `Done()` nor "any member
  is stuck". **Do not restore an early return**: with one the batch is only as
  settlable as its unluckiest channel.

The screen said *"That is the policy itself, not the channel"* about a policy
that was fine. It belongs to `INVALID_PARAMETER` alone now, and the rule it
broke is the one worth carrying forward: **do not assert a cause the program
cannot know.**

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
(`:2718`) calls `CompleteReservation(nil, commitSig)` at `:2813` — storing the
peer's commitment signature — *before* the broadcast block at `:2829`, which is
guarded by `completeChan.ChanType.HasFundingTx()`, and emits `chan_pending` at
`:2897`, after it. So each `chan_pending` is a receipt that the channel is
recoverable by force-close. `no_publish` sets `NoFundingTxBit`
(`lnwallet/reservation.go:415`), which clears `HasFundingTx()`.

**The function is `funderProcessFundingSigned`.** This file used to cite
`handleFundingSigned`, which does not exist at any version and never did. The
ordering was right and the name was ungreppable, which is how a citation stops
being checkable.

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

**The `Replaceable` sequence-number refusal was a lint, and item 6 removed it.**
Core 29's full-RBF is unconditional — verified live: `mempoolfullrbf` does not
exist even as a hidden debug option (`bitcoind -help-debug` has no such flag),
and `getmempoolinfo` reports `"fullrbf": true` with no way to turn it off — so a
higher-fee conflict relays regardless of what our sequence numbers signal.
Refusing a transaction over a signal that changes nothing is a lint wearing an
invariant's clothes. `replaceable: false` is a statement of intent, not a
defence.

**Removing it did not relax I-4, and the two are the same diff to a fast
reader.** Nothing was weakened, because the lint never held anything: it judged a
signal that Core ignores. `plan.Code` no longer has a `Replaceable` member,
`MaxNonReplaceableSequence` is gone, and `InputView.Sequence` still records what
each input said so a report can show it. The comment where the refusal used to
stand, in `plan.checkInputs`, says all of this at the one place somebody would
put it back. **What holds I-4 is that only we can sign our inputs**, and I-3's
txid pin is what catches a signer that edited a sequence number — a changed
sequence is a changed txid, which is what
`TestASignerThatChangedASequenceNumberIsRefused` now asserts.

What the invariant covers, and what it does not:

- **The funding transaction: never.** *n* peers hold commitment signatures
  against its outpoints. Replacing it destroys the batch.
- **The CPFP child: always** — when there is one, and this build does not make
  one. `settle.buildChildAt` set `plan.MaxBIP125Sequence` and `replaceable: true`,
  and `internal/bump`'s verifier *required* it: nobody has committed to anything
  about a child, so replacing one moves nothing anyone depends on, and a batch
  needing two lifts gets an ordinary RBF of the child rather than a grandchild.
  Two verifiers enforcing opposite rules was how both rules were expressible at
  once. **Item 5 deleted the child, the second verifier and
  `plan.MaxBIP125Sequence`.** One rule, one verifier, and nothing in this
  repository constructs a replaceable transaction of any kind. An operator who
  needs a child builds it in their own wallet, and `internal/settle`'s
  funding-horizon screen says so.

If a change ever makes a *funding* transaction replaceable, that is the invariant
breaking and the answer is to stop, not to edit this section.

---

## Why the change output is worth having

The batch verifier **reports** a transaction with no change output. It does not
refuse one, and it no longer says anything about how big yours is. Item 6 made
the first of those true; **issue #2 made the second**, deleting `ChangeTooSmall`
along with `ChangeFloor`, `ChildFeeSat` and `Fee.CPFPTarget()`. The argument is
the same one twice: this build constructs no CPFP child, so computing a floor for
one was an opinion about the operator's arrangements dressed as arithmetic, and a
tool that graded a batch over them would be claiming an authority it gave up at
step 4.

**What survives is the reason the change output is worth having**, which is a
fact about I-4 rather than a number: replace the batch and every outpoint moves,
so a child spending the change is the only lever there will ever be on it.
Saying that is informing. Measuring your change against a target and grading it
was judging. The plan document says the first and nothing says the second.

**They went to `Verification.Reports`, not to `Verification.Unchecked`, and the
difference was the one real decision in item 6.** `Unchecked` "names what this
verification could not establish, so that a clean result is not read as a broader
guarantee than it is", and it renders under the heading **"Not checked here"**.
"This transaction has no change output" is something the verifier *did*
establish. Filing it under that heading would cost the heading the only thing it
is for. So `Reports []Finding` sits beside `Problems []Problem`, renders under
**"Reported, not refused"**, and `OK()` is still `len(v.Problems) == 0`. **That
structure outlived three of the four findings it was built for** — which is the
argument for it, not against: the split between refusing and reporting is worth
having expressible even when only one code uses it.

**`Finding` is a separate type from `Problem` on purpose.** `Problem`'s comment
says every problem is a refusal and there is no severity on purpose, and item 6
had to keep that true rather than edit around it. Two types make the split a
thing the compiler knows: a demoted finding cannot be appended to `Problems` by
accident, so `OK()` cannot come to depend on a grade somebody set wrong. The
verifier writes to them through `v.refuse(...)` and `v.note(...)`.

**Four codes stayed refusals, deliberately.** `ChangeAmbiguous` — two outputs
that look like change is an *attribution* failure, the same family as
`UnnamedOutput`. `NoFee` — LND refuses it itself at `psbt_verify`, so reporting
it would arm a batch LND will reject. `Unsizable` — with no size there is no fee
rate to report *about*. `LegacyInput` — I-3 and LND's own requirement, and
nothing pre-excludes a legacy coin now that Sparrow picks them.

**And a batch with no change output now arms with no further prompt.** That is
the item, not a gap in it. Do not add a confirmation gate back.

**Which output is the change, now that the app does not choose it.** Not named —
recognised. `plan.RecogniseChangeIn` reads the master fingerprints off the
transaction's own inputs, and `plan.Change.Recognise` accepts an output carrying
those same fingerprints on derivation branch 1. That is the evidence a hardware
signer uses to call an output its own change, and the verification report marks it
"recognised by key origin rather than by address" because the claim is weaker than
a script. `run --change ADDRESS` names the script instead — stronger, and the only
route for a wallet that writes no key origins at all, where the refusal says so
rather than reporting the plan as broken. **Do not "simplify" this by trusting any
unnamed output**: the whole product is the check that every output is accounted
for.

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
orphan. Covering that case would need package relay — Core's `submitpackage`,
or, since v0.21.2-beta, LND's own `WalletKit.SubmitPackage` — which would be
another path to the network either way. **The reason has not changed now that LND
has one of its own**: `SubmitPackage` is not registered in `internal/methods`, so
the guard refuses it and the baked credential never carries it, and adding a call
site would be a decision about I-4 and the CPFP child rather than a registry
edit. `internal/methods`' never-list says the same where somebody would look.

**The old reason was wrong, and the wrong reason was the dangerous part.** The
gate used to be justified in custody language — as though a stuck batch put coins
at risk. It does not, and an operator who believes it does will reach, under
pressure, for the one thing I-4 forbids.

**That copy is gone, in item 6, along with more of it than the three strings
this file used to name.** The three were `internal/plan/plan.go`'s error,
`internal/plan/report.go`'s plan bullet and `internal/plan/size.go`'s doc
comment. A sweep found the same framing in `internal/plan/verify.go`'s own
`ChangeMissing` detail — the most operator-facing of the lot — plus two more in
`size.go`, `report.go`'s "what a rescue child would cost" table label,
`internal/run/fee.go` and `internal/doctor/doctor.go`. The rule the replacements
follow: **name what is missing (the lever), never imply what is not (risk).**
Nothing is at risk in a stuck batch; what is missing is the exit.

`internal/plan/plan.go`'s refusal survives with a different subject. It no longer
says change is the only way to accelerate a stuck batch; it says the plan has no
way to *identify* the change output, which is an attribution failure and is the
check the program exists for. `run --change ADDRESS` is the answer, and
`TestAPlanWithNoChangeArrangementIsRefused` asserts the wording so the two do not
drift back together.

**Two strings in `internal/regtestenv/coldwallet/build.go` still carry the old
framing.** They are harness-only — the stand-in for Sparrow, refusing to build a
fixture — and item 6 did not falsify them, so they were left alone.

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
  There is **exactly one** call to `WalletKit.PublishTransaction` — `arm.Publish`,
  the funding transaction, behind the I-1 gate — and the count is enforced:
  `Method.CallSites` pins it at 1 and `TestEveryLNDCallSiteIsRegistered` fails on
  a second.

  It was two until item 5. The second was `bump.Publish`, the CPFP child of a
  batch that was already public, and what kept the two apart was the type system:
  each call took a type that exactly one constructor fills. `arm.Armed` still
  carries the *n* `chan_pending` receipts in an unexported map that only
  `arm.Receipts` fills, and `arm.Publish` still re-derives the txid from the bytes
  it is handed and refuses any that do not hash to the pinned one. With one line
  left there is nothing to keep apart, and the number is the whole claim — which
  is a reason to guard `CallSites` harder rather than to relax it.

  `testmempoolaccept` validated without relaying and was the only pre-flight.
  **It is gone with Core, and there is no pre-flight at all.** This file used to
  say its only production caller was `internal/rehearsal`; that was wrong — the
  one on the batch's path was in `run.armWindow`, between `combine.Accept` and the
  publish, and it is the one that mattered. What survives is `combine.Accept`
  executing every input's witness against its own script; what is lost is node
  policy, and `plan.Verify`'s `Verification.Unchecked` names it.

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

  **Fee rates came from Core's `estimatesmartfee`, and item 5 removed Core.**
  Nothing replaced it, because this rule forbids the obvious substitute. Item 5
  made the rate *declared* instead; **issue #2 then removed it altogether**, so
  there is now no fee target in this build at all — see "There is no fee rate
  anywhere in this build" above. **This bullet is why the gap must never be
  filled by asking somebody.** A fee API is handed the size of what is being
  built and the moment it is being built, which together are most of what this
  tool exists not to leak. The operator reads a rate from their own wallet, their
  own node or a block explorer and types it into Sparrow; that is theirs to do
  and this program never learns the number.

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
3. ~~Change `arm` to the new sequence.~~ **Done, 2026-08-26.**
4. ~~Add the `--psbt` path to `run`, stop calling `coldwallet`.~~ **Done, 2026-08-26.**
5. ~~Delete the cut packages.~~ **Done, 2026-08-26.** 28,267 lines deleted, 846
   added, five commits. See `docs/replan-2026-08.md`'s "Item 5, as built".
6. ~~Demote the fee and change findings, remove `Replaceable`.~~ **Done,
   2026-08-26.** See `docs/replan-2026-08.md`'s "Item 6, as built".

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
  batch sizes up to *n* = 3 are testable. bitcoind is **Polar's image**; LND is
  **Lightning Labs' own**, because Polar publishes no LND tag past `0.20.0-beta`
  and the harness has to run the version an operator actually runs. `regtest/.env`
  says so, and the harness deliberately diverges from Polar's defaults anyway —
  `maxpendingchannels=200`, "not the 10 Polar would give you". This file used to
  call the harness "regtest, via Polar (Docker)", which read as though the GUI
  were the inner loop. It is not, and `make harness` is what builds it.

  **Lightning Labs' image differs from Polar's in three ways the compose file
  absorbs**: its entrypoint is `lnd` itself, so `command` carries flags and not
  the binary name; it runs as root with its data in `/root/.lnd` rather than as
  `lnd` in `/home/lnd/.lnd`, which `regtest/Makefile`'s `creds` target and
  `regtest/bin/lncli` both name; and it has no `USERID`/`GROUPID` entrypoint
  shim, which costs nothing because the state lives in named volumes and
  `docker cp` still lands the credentials owned by you.

  Confirmation depth, stuck transactions, CPFP and LND's ~2016-block forget
  horizon are all testable in seconds — the horizon is about ten seconds of
  mining. **The peers' ten-minute window is not**, and this file used to list it
  among them. That clock is wall-clock and cannot be mined forward:
  `internal/arm/clock_regtest_test.go` is gated behind `WINTHISTLE_SLOW` and
  calls itself "the one test in this repository that takes longer than a coffee".

- **Simulated multisig cold wallet** — two key-enabled Core wallets, xpubs
  assembled into `wsh(sortedmulti(2,…))`, imported watch-only, signed via
  `walletprocesspsbt` in each and `combinepsbt`. No hardware, fully scriptable.
  It lives in `regtest/cold-wallet.py`, `internal/regtestenv/cold.go` and
  `internal/regtestenv/coldwallet/`, and it **survived item 5 as a harness
  fixture** — the stand-in for Sparrow, since a regtest test still needs something
  to build and sign a funding transaction. That is the harness playing the
  operator's part, not a back door for a mode where the app builds the batch.
  `internal/coldwallet` is where the builder used to live; item 5 moved
  `build.go` and `coins.go` into `internal/regtestenv/coldwallet` and deleted the
  rest.

  **The harness owns its coin locks now.** `walletcreatefundedpsbt` is called with
  `lockUnspents`, and until item 5 the *application's* abort path released those
  locks, so every fixture got its coins back as a side effect of the thing it was
  testing. The app takes no locks, so `Env.BuildPSBTPaying` registers the release
  itself. A leaked lock does not announce itself: it starves the next test in the
  same binary with "Insufficient funds" against a wallet whose balance is fine.

  **Since item 4 it plays Sparrow properly, which means it combines outside the
  app.** `Env.SignLikeSparrow` collects both halves and unions and finalizes them
  itself, so what a test hands to `combine.Accept` is one packet with a complete
  witness — the input the production path will actually get.
  `Env.SignWithColdWallet` still returns *m* partials and
  `Env.SignLikeSparrow` still calls `combine.Merge` — which is now the *only*
  caller of the multi-packet path, and the reason `combine.Merge` and its
  finalized-input refusal stay.
  `Env.RecipientsIn` reads the recipients off the printed step-4 table the way an
  operator reads them off a terminal, which is also the only test there is that
  the table is legible.

  **It is not run by CI, because there is no CI in this repository.** This file
  used to say "this is what CI uses" and `README.md` said the same. There is no
  `.github/`, no CI configuration of any kind. Run it with `make test`.

- **signet, in `signet/`** — **deleted in item 5.** It was Core-only, for the
  descriptor-import rescan over a chain with real history and for the
  prune-horizon check, and both paths belonged to `coldwallet` and `setup`. There
  is one harness now.

- **mainnet cold probe** — commissioning only, per `docs/design.html`. Proves
  this node, these peers, these devices. **No regtest substitute for it, and the
  replan does not change it:** `winthistle run` stopped before step 8, against
  real peers, with coins that never move. **Run, and passed, on 2026-08-26.**
  It is not a thing to repeat casually: every channel in a probe reaches
  `chan_pending` and is then abandoned, so each probed peer holds one of its
  pending-channel slots for ~2016 blocks afterwards.

---

## Repo hygiene

This repo is **private and intended to go public**. The cold probe was the
stated gate and it passed on 2026-08-26; what remains before flipping it is the
operator's own call — real channels running in production first, then a polish
pass. Assume every commit will eventually be public.

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
responder" (`funding/manager.go:2940`).
