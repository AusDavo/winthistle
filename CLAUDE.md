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

**The whole build runs on LND v0.21.2-beta**, in `go.mod` and in the harness,
and every source citation below is against that version. It was pinned at
v0.19.3-beta from the scaffold commit onward with no rationale recorded anywhere
— a tag that was already a year old the day the repository started — while the
node it is meant to arm was two minor releases ahead. Nothing in the safety model
turned out to be wrong at v0.21.2-beta; roughly thirty line numbers were, which
is the same way `handleFundingSigned` happened. **On the next bump, re-cite
before assuming**: `go.mod`, `regtest/.env` and the citations are three separate
pins and only the first moves by itself. **The one citation that now fails rather
than waiting to be re-read is I-1's ordering** —
`TestTheReceiptIsProvedByForceClosingTheChannel` force-closes an armed channel
and makes Bitcoin Core check the commitment's witness, so `make test` is the
first step of a bump and the re-read is the second.

**What is built and exercised against live regtest**, and is the guide to what
exists: `winthistle run`, `doctor` and `recover` work against the cluster in
`regtest/`, and those three plus `print-macaroon-command` and the two
`example-*` printers are the whole command set. `internal/arm` runs the
**inverted** sequence — `skip_finalize` at verify, the *n* receipts before
anything is signed, one publish — and the I-1 gate is observed at *n* = 3 with
nothing signed when it opens, with what the receipt is *worth* observed
separately by force-closing an armed channel. `run` **builds nothing and signs
nothing**: it prints the recipients, writes them to `FILE-recipients.csv`, reads the unsigned
transaction back from `--psbt FILE`, and reads the signed one from
`FILE-signed.psbt` or `FILE-signed.txn`. `internal/combine` has twenty
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

  **And both waits read a file the wallet was still writing** — issue #8, landed
  2026-08-27. `waitAny` returned any `os.ReadFile` that did not error, including
  the zero-byte one a poll sees between a wallet's open and its write, and that
  prefix went straight to the decoder: the operator got *"it is empty"* for a
  file that was correct a millisecond later. At step 4 that is a failed run
  **inside clock A**, which on a five-channel batch is every peer's reservation.
  It predates issue #5 — `wait()` had the same line at `0e8a050`, and `9c051a9`
  moved it into `waitAny` verbatim while adding the second filename.

  `run.readWhole` is the rule now: an empty file is skipped without being read,
  and a stat either side of the read discards one the writer moved underneath
  it. **Two stats, and no extra tick** — a file that was already complete when
  the first poll found it is still read on that poll. The other shape, requiring
  the size unchanged across two consecutive *polls*, is stronger against a
  chunked writer and costs a whole poll interval, two seconds at `DefaultPoll`,
  on every run. **What it does not cover is stated rather than papered over**: a
  writer that splits the write leaves a prefix sitting still between chunks, and
  a prefix is indistinguishable from a short transaction without decoding it.
  The wide window is the empty one — a wallet saving 1,100 bytes is at zero for
  the whole gap between open and write — and that one is closed outright.

  **Issue #10 asked whether the rest was worth its two seconds, and the answer
  is measured now rather than argued: no.** Sparrow 2.5.3's three save paths,
  read in source and each run under `strace`: the **binary** PSBT save is one
  `write(2)` at any size — an unbuffered `FileOutputStream.write(byte[])`,
  measured at 1.1 KB, 6 KB, 12 KB and 30 KB — while the **base64** PSBT save and
  the **`.txn`** final-transaction save go through an `OutputStreamWriter` and do
  split, into 8,192-byte chunks above 8 KB. So a splitting writer is real and not
  exotic, and `readWhole`'s old claim that a file this size is not one was wrong.
  **What decides it is the gap, not the write count**: between two encoder chunks
  it measures 0.05–0.3 ms under `strace`, with no syscall, no I/O and no operator
  in it, and a poll must land inside one *and* finish its read inside it. Closed
  `wontfix` on 2026-08-27; the measurement is in `readWhole`'s doc comment. The
  source also settles why the empty window is the wide one, more plainly than the
  guess did: **all three paths truncate the file before they compute what to put
  in it**, so it sits at zero for the whole serialization.

  **Do not close the rest by retrying on a decode failure.** It would work, and
  it would make a genuinely wrong file — the operator saved the wrong
  transaction, or signed at step 4 — indistinguishable from a slow one. Step 4's
  refusal of a signed packet is I-1's last gate, and a gate that waits instead of
  refusing is not one. **The wait says when it is holding off on a file**, for
  the reason issue #5 gives directly above: replacing a refusal with silence is
  the same defect wearing better manners.
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

**The application writes a file now, and "it writes none" was a stated
property.** Issue #15, landed 2026-08-27: `FileWallet` writes
`FILE-recipients.csv` at step 4 — address, amount, label, the three columns
Sparrow's *Send to Many → Load CSV* reads — so the operator loads the recipients
instead of typing *n* addresses inside clock A. `docs/design.html` said *"the app
writes none and reads two"* and no longer does. **What replaced it is narrower
and is the rule to hold: the app writes nothing it later trusts.** The CSV
carries no transaction and no signature, it is never read back, nothing
downstream depends on it, and a failure to write it is reported and not returned
— a convenience that can end a batch inside clock A is not one. `os.WriteFile`
appears in exactly one place in `internal/`, and `run.writeRecipients` is it.

- **BTC, eight places, no separator, label quoted, header row.** Settled by
  measurement against Sparrow 2.5.3, not by preference — the reasoning is in
  `recipientsCSV`'s doc comment and in issue #15's comment. The short version:
  Sparrow reads the amount in whatever unit its preference is set to and we
  cannot see which, so one reading is always wrong; sats-in-BTC-mode is silently
  10⁸ too large, BTC-in-sats-mode drops every row and names its own cause. **Do
  not add a `--csv-unit` flag** — it would reopen the question the measurement
  closed.
- **The printed table stays, and is not the CSV's preview.** The table is the
  *attribution*, which is job one; `regtestenv.RecipientsIn` scrapes it and is
  the only test that it is legible. Both renderings come off one `[]Recipient` in
  one call, and `TestTheFilePathDrivesTheWholeSequence` asserts they agree
  against a live batch.
- **The path is derived from `--psbt` and refused if it exists**, like
  `SignedPath` and like `Unsigned`. Refused rather than overwritten, which is
  where it parts from `SignedPaths`: this is a name invented out of the
  operator's stem in the operator's directory, and truncating a file we did not
  create is not a thing to do silently. A stale one was written for another
  batch's addresses.
- **Step 5 is still the whole check**, and the copy says so in those words. A
  10⁸ amount is `WrongAmount`, a dropped row is `MissingOutput`. Nothing about
  the trust boundary moved, and no copy may imply the CSV is *why* the addresses
  are right.

**The `chan_pending` receipt is observed now, and "inferred from source" was a
stated property.** Issue #16, landed 2026-08-27: an armed channel is published,
mined and then force-closed, and its commitment transaction is watched into a
block. `README.md` said no test in this repository force-closes a pending channel
"so recoverability is inferred from the source and never exercised end to end",
and that paragraph is deleted rather than softened. **The rule it leaves behind:
the test that proves what the app promises cannot be written with the app's own
capabilities, and that is the right way round.** `CloseChannel` is never-listed —
one of the ten refusals `doctor` makes LND confirm — and
`TestEveryLNDCallSiteIsRegistered` scans `_test.go` and the harness too, so the
force-close goes through `regtest/bin/lncli` and
`regtestenv.ForceCloseOutOfBand` is the one place it does. **Do not register
`CloseChannel` to quiet the guard**: the credential's inability to close a
channel is a product claim, and trading it for a test's convenience would be
trading the thing for the evidence of the thing. No `docker` control was added
either; the harness shells out to the wrapper an operator would run and nothing
more.

**The credential is proved to be a macaroon before it is presented as one, and
the rule behind it is the one to carry: a failure may be reported, but its cause
may only be named where the program established it.** Issue #13, landed
2026-08-27. `lnd.Dial` read the macaroon with `os.ReadFile` and hex-encoded
whatever came back, so a torn read produced a well-formed *credential* carrying
the wrong bytes — `hex.EncodeToString(nil)` is `""` — which LND then refused on
authentication, and every sentence downstream was about the wrong half of the
setup: the address, the permission list, re-baking. **The failure was never the
defect.** It fails loudly and before step 2, so no peer has been told anything
and no clock is running. The defect is the asserted cause, which is issue #6's
rule verbatim: **do not assert a cause the program cannot know.**

- **`lnd.ReadMacaroon` is the one read**, used by `Dial` and by `doctor`'s
  `checkMacaroon`, and it refuses an empty file outright and anything that fails
  `macaroon.UnmarshalBinary`, naming the file. `ErrMacaroonFile` marks the class
  so a caller may say "this file" and must not say "this node" or "too narrow" —
  nothing has been asked of LND by the time one is returned. **`doctor` reads the
  same file a second time and needed the same guard**: `Dial` succeeding a moment
  ago says nothing about what is on disk now, and a re-bake is exactly what moves
  it.
- **The window is one this build prints the command for.** `winthistle
  print-macaroon-command --save-to PATH | sh` is `lncli bakemacaroon --save_to`
  writing the exact path `Dial` reads. As in issue #8 the wide half is zero
  bytes, for the whole gap between the writer's open and its write — and
  re-baking is the action the old copy recommended, so the diagnosis sent the
  operator back through the race.
- **No retry and no polling, which is where this parts from `run.readWhole`.**
  That transport polls for a file a wallet is still writing, inside clock A, so
  it has to tell "not finished" from "wrong". `Dial` is a one-shot at a moment
  nothing is waiting on: a credential that is wrong must fail on the first look.
- **`gopkg.in/macaroon.v2` was already in the module graph**, so the only
  `go.mod` change is its promotion from indirect to direct. `lnd/macaroons` is
  still not imported, for the reason `internal/lnd/client.go`'s `macaroonCreds`
  comment gives — it drags in kvdb and etcd for fifteen lines.
- **`doctor.tooNarrow` is `doctor.refusedOverMacaroon` now, because the name was
  the claim rather than the observation.** The predicate is unchanged and still
  right: `codes.InvalidArgument` is `CheckMacaroonPermissions` answering about the
  macaroon in the *request*, and an untyped `permission denied` is LND's
  interceptor refusing *our call* — matching on either the code alone or the text
  alone confuses the two. What it establishes is the refusal. **Why** LND refused
  — a permission never baked in, a caveat, or a path pointing at some macaroon
  other than the one the operator baked — is not in the error, and the copy no
  longer picks one. *"which means it was baked before this build"* is gone from
  the report and from the package doc. **Guarding the read closed one route into
  that claim and did not earn the claim**, which is why (3) was taken rather than
  left as covered by (2).
- **Two guards do not make a validator.** Do not add a third check here, do not
  validate the macaroon's caveats or its location, and do not second-guess LND:
  `CheckMacaroonPermissions` remains the only authority on what a credential is
  allowed to do. This proves the bytes are a macaroon and stops.

**#13's audit found three more instances, and issues #20, #21 and #22 closed all
three on 2026-08-27. That list is finished; the class is not, and the difference
is the news.** The rule was recorded at #13. What this slice adds is that
`internal/peers` is now the register the whole build is held to rather than the
one package that got it right — and that a fresh sweep run *after* these three
landed found four more, so **do not write "exhausted" about this rule again**.
A sweep finds what the last sweep's vocabulary could see. It strips LND's misleading
`remote canceled … possibly timed out` prefix, labels *"The peer said"* against
*"This node said"*, and says in terms when a refusal is not a verdict. Doing the
three together was the point: the shared question is *what this program is
entitled to say about a failure it did not diagnose*, and answering it three
times separately would have produced three answers.

- **#20** · `horizonNote` quoted LND's proto hedge and overrode it in the same
  sentence — *"very likely cancelled … and that is what has happened"* — then
  printed `ForgetHorizonBlocks`, **our own constant**, as the blocks *the peer*
  waited. The worst of the three, because it says a channel is dead at the moment
  the remedy is still live. LND's hedge is LND's now, the count is named as ours
  (`funding_expiry_blocks` off `PendingChannels`), and the figure is named the way
  `depthNote` names its own: **LND's default policy, binding stock LND and nobody
  else.** The remedy did not change — it is right either way, which is what made
  all three copy-shaped. `Summary`, both warn branches and the shared trailing
  paragraph carried the same claim and got the same treatment.
- **#21** · `depthNote` blamed a Bitcoin Core removed in item 5, on the branch
  that **cannot not fire**: `Confs` comes only from `Options.Chain`, which is set
  in two tests and nowhere else. It says the design fact now — this build dials no
  Bitcoin node, so it counts no blocks — and *"or it has not seen the
  transaction"*, a cause nothing here asked about, is gone. **And the two
  paragraphs swapped conditionality**, which is the half the issue did not name:
  `State.line` prints *"expect ~6"* unconditionally while the hedge explaining it
  sat on the branch nobody has seen, so the one number an operator reads was the
  one never explained.
- **#22** · `SignedFromTX` returned from inside the lifting loop on the first
  witnessless input, so it established *"input i is short"* and said *"this
  transaction carries no signatures … that is the transaction you built at step
  4"*. It counts first now: none signed keeps the old sentence, some signed names
  the inputs that are short and says **why they are short is not in these bytes**,
  all signed carries on. `ErrNoWitnesses` is **`ErrIncompleteWitnesses`** — its
  text was the over-claim at the sentinel level and its name said the same thing.
  One sentinel, because no caller tells the two apart. Nothing relaxed: a partly
  signed transaction is still refused, and the raw-transaction relaxation stays at
  step 7's call site.

**`Options.Chain` survives, and it is the harness's seam — say so, do not delete
it.** Its old doc comment justified it by *"assisted mode has no Core"*, and
assisted mode dissolved with I-2; the comments now say that the application fills
it on no path and may not, that `internal/regtestenv`'s Core fills it, and that
`Confs == -1` is **the rule rather than the exception**. Deleting it would take
`State.ObservedDepth`, `depthLesson` and
`TestTheSettlementPassPoliciesAChannelAndLearnsItsDepth` with it — a live peer's
`minimum_depth` read from above, one block at a time, which is the only
authoritative reading an initiator can get — and this repository does not delete
node-verified evidence to tidy a shape away. **If a production depth reading is
ever wanted it does not come through this field**: it comes from LND, which this
build already dials (`GetTransactions` reports `num_confirmations` and the
funding transaction is ours), and that is a call site, a registry entry and a
decision. `docs/design.html:808` still says step 9 *observes* `minimum_depth`,
and without a `Chain` no production run ever does — **a real gap, found while
sizing this slice and not closed by it.**

**Four more instances, found by the sweep after this slice's own copy was
written.** Each is verified in source, and each is a decision rather than a typo,
so each wants its own slice. Filed 2026-08-27 as **#24, #25, #26 and #27**, in
that order. **#24, #25 and #27 are closed; #26 remains:**

1. ~~**#24 · `doctor` said unfinished runs had stopped.**~~ **Done, 2026-08-27.**
   `journal.Unfinished` is `state NOT IN (published, aborted)`, and a run is in
   that set from the moment `journal.Begin` records its first stream, so a batch
   being armed in another terminal was reported as dead and pointed at
   `winthistle recover`. **Two decisions, deliberately two commits, so a later
   reader can revert one without the other.** (1) The hedge goes at the callers,
   copying `prose.RecoveryList`'s wording, and `Unfinished`'s doc comment now
   states what the query establishes — narrowing the *function* was rejected
   because the narrower question cannot be answered honestly: a run that died
   mid-arming and one being armed write identical rows, and the journal carries
   no heartbeat, so it would mean inventing a liveness signal or picking a time
   window, and a window is a guess wearing a query's clothes. There are exactly
   two callers and the other already hedged. (2) It is a **`Warn`**, not a
   `Fail` — nothing on the run path consults the journal's other runs before
   arming, so an unfinished run stops no batch, and `Report.OK()` no longer goes
   false against a healthy node.

   **The sweep found more sites than the two the issue named**: `checkJournal`'s
   sentence and doc, `Unfinished`'s doc, `run.Unfinished`'s doc,
   `cmd/winthistle`'s package doc and its `recover` **usage line** — *"list runs
   that stopped"*, the front door to the screen that hedges — and
   `README.md:472`. `docs/design.html` **does not carry this claim and did not
   move**; it says only *"the run journal readable"*.

   **Two `internal/prose` sites were found and deliberately left**, to hold the
   slice to `doctor` and `journal`: `RecoveryList`'s *own doc line*, which sits
   directly above the comment explaining why the claim cannot be made, and — the
   sharper one — `stateMeans`' `StateAborting` copy, *"An abort of this run was
   started and did not finish"*, which is **this defect one state over**.
   `MarkAborting` writes that state before the first RPC, deliberately, so a
   `winthistle recover` running right now in another terminal is described as
   having failed. `journal.go:86` and the design page's state table both hedge it
   correctly; only the screen asserted it. `prose`'s printed recovery header
   remains right and remains the model. **Both of those are closed now**, in the
   #27 + #30 slice below — the second as its first commit and the first as its
   own one-line one.

   **And the check's copy had never been rendered by any test.** The old sentence
   passed `prose.IsAre` where a pronoun belonged and printed *"3 runs stopped
   somewhere **are** should not have"* — ungrammatical at every count, unnoticed.
   `TestTheReportStaysInThePane` hand-built every check out of `r.add` and `say`,
   so the sentences the checks write *themselves* were width-checked by nothing;
   it renders a real `checkJournal` report now.
   `TestAnUnfinishedRunIsNotReportedAsStopped` and
   `TestAnUnfinishedRunDoesNotStopABatch` are **node-free** — `checkJournal`
   takes the journal as a parameter and the test package is internal — and the
   first was verified to fail against the old copy rather than only to pass.
2. ~~**#25 · every step-7 failure was journalled as the signer having
   declined.**~~ **Done, 2026-08-27.** `sign` wrote `journal.SignerDeclined` on
   **any** error out of `SigningWallet.Signed` — a failed `os.Remove` *before the
   wallet is prompted at all*, Ctrl-C, an unreadable file, and `combine`'s
   refusal of a **moved txid, which is an I-3 breach**. And `RecordSigner` upserts
   on `(run_id, label)`, so the true `SignerAwaiting` row written twelve lines
   above was **replaced** by the false one; `prose.signerNote` then rendered it as
   *"1 declined"* on the recovery screen. **It is the only instance of the rule
   that persisted the wrong cause to disk**, which is why it was picked ahead of
   the rest.

   **The fix is to write nothing on the error path**, leaving the row already
   there standing, so the screen says *"1 still awaited"* — which is what
   happened. A new `SignerState` was rejected on two grounds worth keeping: the
   distinction it would draw, *asked and still waiting* against *asked and the
   answer was not usable*, is the difference between a live run and a stopped one,
   and **that is the run's own state rather than a signer's**; and no honest name
   for it could separate the classes anyway, because the `os.Remove` and Ctrl-C
   cases have no answer to call unusable. **Nothing in this build can observe a
   refusal at all** — a file transport has no channel through which a wallet says
   no. So `internal/prose` needed no copy change, and none was made.

   **`SignerDeclined` keeps its constant and gains `SignerPartial`'s treatment**:
   its doc says it has no writer, that a row carrying it is **not** evidence a
   wallet said no, and not to repurpose it. Journals written before this change
   carry the value wherever step 7 failed, and `signerNote`'s switch names it
   explicitly rather than dropping it into the `unknown` bucket — so **the screen
   still renders those historical rows with the old wrong sentence**, which is
   `internal/prose`'s to fix and is noted on #30.

   **Decision 2 was taken separately and changed nothing**, which is the result:
   *"the batch was not signed: %w"* is an outcome the frame established rather
   than a cause, with the cause wrapped inside it, and the surrounding wrappers
   name activities. The `jerr` branch's question went moot with the write.
   `TestAFailedSigningStepIsRecordedAsAwaitedAndNeverAsDeclined` is **node-free** —
   `sign` uses only `d.Out`, `d.Journal` and `d.Signing`, so `Deps.LND` stays nil
   — five error classes keyed on `combine.ErrTXIDMoved`,
   `combine.ErrIncompleteWitnesses` and `context.Canceled` rather than on
   sentences, and verified to fail against the old code on all five. The last of
   the five is the refusal **#22 rewrote one call away from this frame**, and it
   is the instance the issue led with. **`docs/design.html` does not carry this claim and did not move**; its
   four `signer` mentions are about BIP174 and QR scope, and both its `declin`
   hits are LND's.
3. **#26 · `internal/settle/report.go`'s `"open, peer offline"`** is `ListChannels`'
   `Active`, which is **this node's link state** — false while our own node is
   bringing links up, and false by default for any channel point missing from the
   map. `State.Active`'s doc comment says *"whether the peer is currently
   connected"* and carries the same over-claim.
4. ~~**#27 · a refusal said a channel was pending on the peer.**~~ **Done,
   2026-08-27**, with #30, as one recovery-screen slice. `failureLine`'s
   `abort.ErrBluntNotConfirmed` arm said *"It is still pending here and on the
   peer"* where only this node's `PendingChannels` was read. **Two honest answers
   were available and the shape of the screen chose between them.** Showing the
   working — which `recoveryPlan` does 160 lines up, naming clock B in blocks as
   the style rule requires — is **not available here**: that paragraph is on the
   `Recovery` screen and this line is on `RecoveryOutcome`, and
   `TestNoScreenPointsBelowItself` forbids pointing at a section a screen does
   not have. Restating it in a bullet would have put a 2016-block horizon on a
   channel this failure did not touch. So the line says the *mechanism* instead
   of the mechanism's conclusion: this node still has it pending, which is what
   was read; an abandon tells the peer nothing either way, so the refusal changed
   nothing on the peer's side. The other arms were left — `ErrNotPending`'s copy
   was checked in the same sweep and is sound, and the new test asserts it still
   renders.

**PR #29's own audit found six more, which was the third sweep in a row to find
instances the previous one's vocabulary could not see. All six are closed**, with
#27, as one recovery-screen slice on 2026-08-27 — `internal/prose/recovery.go`
plus two doc comments in `internal/journal`, and **no state, value or schema
moved in the journal**. The three decisions, and what each rejected:

1. ~~**Three states a live run also holds were rendered as a run that
   stopped.**~~ `StateAborting` — *"An abort of this run was started and did not
   finish"* — was the sharpest, and is #24 one state over; `StateArming` and
   `StateSigning` said *"streams **were** open"* and *"**had** gone out to be
   signed"* for states a run holds for its whole working life. All three are
   written *before* the work they name, so `run.recoverRun` prints this screen
   from inside the process still tearing the run down, and `winthistle recover
   ID` prints it for a run that may be being armed next door. **The hedge went
   in `stateMeans` rather than at the caller, which is where #24 put its own**,
   on #24's own rule: *can the narrower question be answered honestly?*
   `journal.Unfinished` could not, so its hedge had to go where a sentence could
   carry it; `stateMeans` is handed the state itself and each of the three has an
   honest reading — *"in progress, or one was interrupted partway"* is exactly
   what `aborting` establishes, and it is `journal.go:86`'s own wording. **A
   blanket paragraph at the caller was rejected because it would over-apply**:
   `armed` and `published` are *not* written ahead of their work, and hedging
   them would weaken two sentences that are true. One state renders per screen,
   so a clause in each costs the reader nothing. **`armed` was deliberately left
   alone** — the journal sets it itself after observing the *n* receipts.
2. ~~**#27's arm.**~~ See the numbered item above.
3. ~~**Four comments and docs that said more than their code establishes.**~~
   Taken as one look with a likely yes, and all four over-claimed.
   `channelBreakdown`'s `no channels` comment called it *"a run that stopped
   before `Begin` wrote any"* — `Begin` refuses an empty batch and writes the run
   row and the channel rows in one transaction, and nothing deletes a channel
   row, so **that shape is not reachable through this build at all**, which is
   the reason to render something rather than a blank. `journal.Recover`'s doc
   called its input *"a crashed run"* when it is handed a run id, and its own
   caller says the commonest way in is Ctrl-C. `journal`'s package doc — *"the
   record of what a batch **did**"* — is now *"what a run wrote, in the order it
   wrote it"*, which is the contract #24 wrote over three files. And
   `signerNote`'s *"No signer had been asked for anything when this stopped"*
   kept its first half, because here the narrower question **can** be answered:
   `run.sign` writes the `SignerAwaiting` row before it calls `Signed`, so no row
   at all establishes that step 7 was never entered.

**And `%d declined` was in scope after PR #31, which made it sharper rather than
smaller.** #25 stopped `sign` writing `journal.SignerDeclined` at all, so every
row `signerNote` will ever render it for was written by the defect: it is not
wrong for some rows, it is wrong for **all** of them. It reads *"%d marked
declined"* now, with a paragraph saying where the value comes from, that it never
meant a wallet said no, and what the old frame actually had in hand — a file that
never appeared, one that could not be read, a moved txid, Ctrl-C. **Deleting the
arm was not available**: those rows are on operators' disks and dropping them into
the `unknown` bucket is the defect that function's doc comment was written about.

**And the slice's own audit found six flagged sites out of 127 operator-reaching
copy sites, which was the fifth sweep in a row to find what the previous one's
vocabulary could not see.** The count is the point: #23's found four, #29's found
six, #31's found three, this one six, and **#35's four more, one of them inside
that slice's own fix**. **Do not write that this set is exhausted.** Where the
six went:

- **One was #27's own claim in the sibling function**, and #27's sweep did not
  reach it because it grepped the wording `failureLine` used.
  `RecoveryOutcome`'s **clean** path said *n* channels *"are still pending on the
  other side"*, off `rep.Abandoned`, which establishes only that this node
  abandoned them. **Fixed in the same slice**, on #24's precedent that the extra
  sites of a claim belong with the issue that names it — otherwise #27 closes
  with its own sentence standing sixty lines away on the same screen.
- **One is #26**, with two corrections filed as a comment: the *"false by default
  for a channel point missing from the map"* route **does not exist** — that
  branch requires `s.Open`, and `open` and `active` are filled in one loop over
  the same channels — and `Active == false` has **three** causes rather than two,
  because LND computes it as `peerOnline && link.EligibleToForward()`. What the
  field establishes is *this node's own link is not eligible to forward*.
- **One is the screen half of #32's first item**, filed there and **closed with
  it on 2026-08-27**.
- **Three are one new class and are #33**: a figure that is **LND's own default**,
  handed over as the peer's behaviour with no attribution — the eleven-minute
  reservation and the 2016-block horizon, twice. `settle`'s `horizonNote` is the
  model and says *"it binds a peer running stock LND and nobody else"* about the
  same number, because #20 made it. **A real decision rather than a typo**: the
  style rule requires recovery copy to name clock B **in blocks**, so whether
  hedging the one number that rule insists on is an improvement or a cost is the
  question, and it should be taken once for all three sites. **There is a fourth
  site now**, reported on the issue: #32's new standing-shim paragraph in `RecoveryOutcome`
  names the same 2016 blocks. It was written **unattributed on purpose**,
  matching the paragraph sixty lines above it on the same screen — one screen
  saying the same number two ways would be worse than either way said once, and
  #33 is where all four get the one decision.

**The sweep found nothing else false, and that is worth stating rather than
leaving unmentioned.** `cmd/winthistle`'s package doc and its `recover` usage line
already read *"the runs the journal never saw finish"*, and `README.md:472`
already says *"which includes one being armed in another terminal right now"* —
#24 hedged all three and they still agree with this slice's wording.
`internal/peers`' and `doctor.anyOurs`' *"may be an earlier run of this tool that
did not finish"* are about a **pending channel read from LND**, are hedged with
*"may"*, and are sound. **`docs/design.html` does not carry any of these claims
and did not move**: its state table already hedges `aborting`/`aborted` as *"in
progress, was interrupted partway, or finished"* and uses the present tense for
`arming` and `signing` — so the page was the thing the code was brought up to, as
it has been every slice — and it carries no signer-declined copy at all, its
`declin` hits being LND's own.

**No test in `internal/prose` had ever built a run with signer rows**, so every
sentence `signerNote` writes about a signer was rendered by nothing, widths
included, on the screen that has a pane test. That is #24's `checkJournal`
finding in a second package. `withSigners` fixes it, and **`RecoveryOutcome` went
into the pane test too** — the one screen in the file it did not measure, and the
one whose width is least under the file's control, because every `failureLine`
ends with `abort`'s or LND's own error text appended to a bullet. It fits.

**PR #35's audit found three more of the rule and one hole in the slice's own
fix, and it is the sixth sweep in a row to find what the previous one's
vocabulary could not see.** Denominator: 15 write functions in `internal/journal`,
12 production call sites and 83 test ones. The hole is fixed in the slice, above;
the other three were **#37 and #38**, both filed 2026-08-27. **#37 is closed;
#38 remains.**

- ~~**#37 · `SignerSigned` is journalled for a file nothing checked for
  signatures.**~~ **Done, 2026-08-27.** **#25's mechanism with the polarity
  reversed** — #25 wrote a false *failure* over a true row, this wrote a false
  *success*, and it is the second of the three instances of the rule that
  persisted one to disk. `run.sign` wrote it once `SigningWallet.Signed` returned
  bytes, and on the PSBT branch the only thing that had looked at those bytes was
  `combine.Parse`: five magic bytes in, bytes out, **a sniffer and not a parser,
  as its own doc says**. `RecordSigner` upserts, so the true `SignerAwaiting` row
  written thirty lines above was replaced, and `prose.signerNote` read it back as
  *"1 signed"* for a run where nothing was. **The two step-7 encodings
  disagreed**: the `.txn` route is guarded by `SignedFromTX`'s witness count, the
  `.psbt` route was not, so one operator mistake journalled two ways depending on
  what the wallet saved.

  **Decision 1: the write moved to `armWindow`, one statement after
  `combine.Accept`**, which executes every input's witness against its own
  script. **This is #32's ordering rule with the mechanics reversed and the
  reason unchanged: a row may only claim what has been established when it is
  written.** `MarkVerified` moved *ahead* of its call because that row is read to
  mean *"the call may have landed, go and look"*, so its safe direction is early;
  this one is read to mean *"it did happen"*, so its safe direction is late. A
  crash in the gap now leaves `SignerAwaiting`, which is **one more case falling
  under #25's own reading of that state** — the wallet was asked and nothing
  usable came back. **Three rejections worth keeping.** Weakening the doc to *"a
  file came back that decodes as a PSBT"* (#32 item 2's shape) leaves the screen
  still printing *"1 signed"* for an unsigned run, and *"signed"* is the value's
  own name, which is #22's rule at the sentinel level. Writing nothing on success
  either is wrong in the other direction — a refused publish would render as
  *"1 still awaited"*, sending the operator back to a wallet that did its job.
  And checking for signatures inside `sign()` would make a second, weaker
  authority on one question, which is #13's *"two guards do not make a
  validator"*. **`internal/combine` did not change at all**, which was the stated
  test of whether decision 1 landed in the right place.

  **Decision 2 changed no emitted copy, and the rule it turned on is the one to
  carry: does the operator's next move change if the row is the false one?** It
  does not — a false `signed` row belongs to a run that stopped at `Accept`'s
  refusal, which named the file and the missing signatures on the terminal at the
  time, and nothing on the recovery path consults a signer row. **`declined` was
  different in kind: it pointed at a signing device.** And **the conditionality
  runs the other way from #31's** — every `declined` row was written by the
  defect, while a `signed` row is right whenever the run got past `Accept` and
  stopped afterwards, so a hedge would teach the operator to distrust the one
  signer signal that is right. That is #21's mistake with the arms swapped.
  Conditioning it on the build was not available: no version column, no
  migrations, and the narrower key — this value on a run still in `StateSigning`
  — admits true rows, because `--stop-before-publish` reaches the screen in
  exactly that shape. The control **asserts an absence and says it pins a
  rejection**.

  **The printed line carried the same claim one output earlier**, and it is in
  scope for the same reason #24's extra sites were: `"signed (12s elapsed)"` is
  now `"a file came back after 12s; nothing has checked it for signatures yet."`,
  and the honest version was already printed by `armWindow`, once the check has
  run, as *"signed and checked"*. **Its width was found by rendering the
  transcript and reading it, not by a grep** — step 7's duration belongs to the
  operator, so `1h23m45s` is the ordinary case and the first wording was two
  columns over `PaneWidth`.

  **The positive control already existed and is node-backed**:
  `TestTheFilePathDrivesTheWholeSequence` asserts the row reads `signed` after a
  full run, once per encoding. The new one,
  `TestASuccessfulSigningStepIsNotRecordedAsSigned`, is node-free by #25's route
  and hands back the unsigned packet — the operator mistake in its most ordinary
  form — and was proved to fail against the old code on three assertions.
- **#38 · two docs name a stronger observation than their caller made.**
  `ChanPending`'s *"chan_pending arrived"* and five sibling sites, where
  `receiptFor`'s fallback establishes the same fact by asking `PendingChannels` —
  **the safety conclusion is sound and re-verified at v0.21.2-beta**, and only
  the word for it is wrong. And `RecordFinalizedTx`'s *"stores the signed
  transaction"*, where `arm.Publish` checked the txid and a txid check cannot
  establish signing — witnesses do not move it, which is I-3's own premise.

**One lead was left unverified and is not filed**, on the standing rule that an
agent's report is evidence rather than a finding: `arm.go:437` discards the
pending channel id when `psbt_fund`'s `Recv` fails *after* `cli.OpenChannel`
returned, so a shim LND may already hold would never be journalled and could not
be cancelled. **The open question is whether LND registers the PSBT shim before
the stream's first message** — server-streaming `OpenChannel` returns as soon as
the client stream exists — and answering it means reading `rpcserver.OpenChannel`
at v0.21.2-beta.

**The audit's other half came back clean, and that is worth recording too**: no
test in the tree asserts on a copy string, check name or map key the build no
longer emits. PR #23's `doctor_regtest_test.go` fix held, and every surviving
`Contains` against a dead sentence is a *negative* assertion with a comment
saying so. **It has now come back clean four sweeps running** — #31's covered
61 assertion loops across 30 files, and the #27 + #30 slice's covered **373
assertion points across 29 test files**, including 160 string literals sitting
inside table-driven blocks *away from* their `Contains` call, which the first
pass of that sweep missed. **#35's covered ~303 assertion points across all 50
`_test.go` files**, machine-checking 1,268 literals against a
concatenation-merged blob of every non-test file and adjudicating 410 survivors
by hand. A clean answer to this question is cheap and is evidence.

**Two things #35's pass added that the next one should keep.** **Merge the
concatenation before matching**: fourteen literals — thirteen in
`internal/prose/recovery_test.go` and `config_test.go:161`'s *"no longer holds an
opinion about the fee"*, split across `toml.go:265-266` — match production only
after joining `"…" + "…"`, and a naive grep reports every one of them as stale.
And **match against string literals with comments stripped**, to catch the mode
where an assertion matches only a *doc comment*; that pass found zero, which is
the answer worth having. **One claim the audit corrected about itself**:
`internal/settle`'s `paneCases()` lookups do *not* nil-panic on a stale key —
`report.go:47` returns `"Nothing to settle."` for a nil `*Result`, so the
positive assertions there would fail loudly but the negative `gone` guards would
pass over it in silence. The keys are live; the shape is not self-protecting.
**And `cmd/winthistle` has no test files at all**, so its package doc and its
`recover` usage line — the copy #24 fixed — are asserted by nothing.

**And PR #31's audit found two more of the rule and one of #21's, filed
2026-08-27 as **#32**. All three are closed now** — item 3 in the #27 + #30
slice, items 1 and 2 with the screen half on 2026-08-27. **Item 1 was the only
instance found so far with a real operator cost, and the only one that was a
*write* rather than copy**: every earlier instance put a wrong cause on a
terminal, and this one put it on disk, where the next `Recover` read it back as
fact.

- ~~**`recordAbort` writes `ChanCancelled` for a shim that was already gone**~~
  **Done, 2026-08-27.** The defect as filed is kept below, because the shape of
  it is the thing to carry rather than the diff; what the slice decided is here.

  **The three decisions.** (1) **The write is guarded by what the journal already
  established, and `AlreadyGone` never writes `ChanCancelled`.** The narrower
  question #24 asks — *can it be answered honestly?* — answers **yes** here for
  the first time, and not because the caller has `AlreadyGone` in hand but
  because of what it is paired with: `abort.ErrNoShim` establishes an *absence*
  and nothing more, and what the absence **means** is decided by the row.
  `ChanShimGone` is a new `ChannelState` — a value, not a column, so the
  no-migrations rule permits it — written **only** over `ChanShimRegistered`,
  where psbt_verify never ran, so LND created no channel and nothing is left. A
  `ChanVerified` row is **left exactly as it stands**, because there the same
  absence is what a channel that reached `chan_pending` looks like from here.
  **The two rejected answers each cost the thing that separates the cases.**
  Writing `ChanShimGone` unconditionally would have overwritten the one signal
  that distinguishes benign from worrying — the journal has one state column and
  no migrations, so a row cannot carry both. Leaving *every* already-gone row
  alone would have left a benign run permanently unfinished with no exit through
  this tool.

  (2) **`StateAborted` is gated on `leftStanding()` as well as `Report.Clean()`.**
  `Clean()` establishes that no step failed, which is a fact about the calls and
  not about what they left. `leftStanding` re-reads the run and asks
  **`AbortTarget`** — the same function an abort acts on, deliberately, so
  *"nothing left behind"* cannot drift from *"nothing an abort would touch"*; a
  second list of states here is exactly how it would. **The consequence is a
  permanent nag and it is the right one**: a crashed-verified run stays in
  `aborting` and keeps appearing in `winthistle recover` forever, because nothing
  this program can read will ever say otherwise — and that is the case where a
  peer holds its side until clock B runs out. The benign case clears through
  `ChanShimGone`, so the nag is aimed only at the channel that warrants it, and
  the screen says outright that it will not clear.

  (3) **The screen names the third cause and stops calling the set
  not-a-failure.** `RecoveryOutcome` prints the two halves apart, off
  `alreadyGoneSplit`, which keys the report's shims on the run's own rows — `r`
  is the load taken **before** the abort ran, which both callers already do, so
  those are the states the abort found. A shim whose channel the run does not
  know falls to the **standing** side, because the zero `ChannelState` is not
  `ChanShimRegistered` and an unknown must not be the thing that gets waved away.
  The standing paragraph names the peer's full pubkey — greppable against
  `remote_node_pub`, not `shortKey`'s sixteen characters — and hands over `lncli
  pendingchannels` and `lncli abandonchannel`, because **this build cannot do
  it**: the journal never recorded the outpoint. **Closing that window is still
  not on offer** and was not attempted; `AbortTarget`'s comment says why, and
  saying it honestly was the slice.

  **`internal/abort` did not change at all**, which was the stated test of
  whether decision 1 landed in the right place: the distinction was already in
  `ShimOutcome`.

  **Three controls, all proved to fail against the old code** — nine assertions
  in `internal/journal`, twenty in `internal/prose`.
  `TestACrashBetweenVerifyAndTheReceiptIsNotRecordedAsACancelledShim` is the
  deliverable and it is **node-backed**: it produces the crashed-process shape
  for real — verify with `skip_finalize`, never call `MarkPending`, read the
  receipt only so the *test* knows an outpoint the journal never learned — and
  asserts the channel is **still pending in LND after the recovery**, which is
  the cost the old row denied. Two node-free ones cover both sides of the guard.

  **Two more were found by rendering the screens and reading them, by no grep.**
  `recoveryPlan` printed *"An abort of this run would:"* with no bullets under it
  for a run with nothing left — a heading with nothing under it reads as a
  rendering fault, and `ChanShimGone` makes that shape ordinary rather than rare.
  And `stateMeans` had **no arm for `StateAborted`**, so it fell to the default's
  bare restatement of the value; it says what the state now establishes.

  **And the slice's own audit found the guard resting on a row written after its
  own call, which is the same defect one write earlier.** `ChanShimGone` is
  terminal — it says LND created no channel — and it rests on the row reading
  `shim_registered`, taken to establish that no `psbt_verify` happened. The row
  established something weaker: that no `verified` row was **written**. And
  `MarkVerified` was **the one state write in the batch path made after the call
  it names**, so a process that died in that gap left `shim_registered` for a
  channel whose funding flow LND had completed. **Not only a crash**:
  `MarkVerified` takes `ctx`, so a cancelled context fails it deterministically
  while the RPC has already landed, and `recoverRun` runs the abort on a context
  that deliberately survives the cancel. Nothing else closed it — `Verify` does
  not retry, `arm.Receipts`' `PendingChannels` fallback is never reached because
  `armWindow` returns on `Verify`'s error, and `AbortTarget` re-reads nothing
  from LND. **The write moved ahead of the call**, which is `RecordPinnedTxID`'s
  own stated discipline twenty lines up, so the row can now only be wrong in the
  safe direction. `TestTheVerifyRowIsWrittenBeforeTheCallItNames` is the control.

  **And one README sentence this slice's own node test falsified.**
  `README.md:235` said *"`shim_cancel` still works after a successful
  `psbt_verify`"*. It does not, once the channel goes on to reach `chan_pending`:
  `skip_finalize` completes LND's funding flow, `CompleteReservation` consumes
  the intent, and the cancel comes back with nothing to cancel — which is exactly
  what the new regtest control measures. The paragraph is about blowing clock A,
  where nothing got that far, so the reassurance survives with its scope named.

  <details><summary>The defect, as filed</summary>

- **`recordAbort` writes `ChanCancelled` for a shim that was already gone**, and
  `ChanCancelled` is a cause with an actor in it: *"its shim was cancelled before
  it ever reached pending."* `journal/recover.go:260-263` discards
  `ShimOutcome.AlreadyGone` — whose **own doc comment names this consumer**,
  *"which matters when reading a journal after the fact"* — and `setChannelState`
  is an unguarded `UPDATE`, so the truthful `verified` row is overwritten. **The
  case where the row is false is documented 76 lines above the write**, in
  `AbortTarget`: a crashed process reports `AlreadyGone` for a channel that is
  actually pending. The screen then says *"Nothing of this run is still standing
  in LND"* and `AbortTarget` emits nothing for it, so **a second `Recover` cannot
  pick it up**. `StateAborted` — *"the abort completed with nothing left
  behind"* — is reached the same way, because `Report.Clean()` counts failures
  and an `AlreadyGone` shim is not one. **The sharpest instance found so far and
  the only safety-adjacent one**, and no test covers the path.

  </details>

- ~~**`MarkSigning` at `run/run.go:522`** writes a state defined as *"the
  unsigned transaction is out with the signing wallet"* before the ask is
  made.~~ **Done, 2026-08-27, and it changed no emitted copy — which is the
  finding.** The #27 + #30 slice had already taken the renderer's over-claim out,
  so `prose.stateMeans` says *"That state is written before the wallet is asked
  for anything"* and is right. What was left was **the constant's own doc**, and
  it was silent about the ordering where `StatePublishing`'s doc declares it — a
  gap against the package's own convention, on the one state whose neighbours all
  keep it. It says it now, along with what *is* established at the write (the
  wallet holds the transaction because `SigningWallet.Built` is where the
  transaction came from, at step 4) and what is not (the request to sign, which
  is the next statement and can fail before the wallet is prompted at all).
  **`internal/run` did not change**: `run.go:520`'s comment already says
  *"written before the round rather than after it"* and claims nothing further. A
  doc comment cannot be asserted on, so the control is
  `TestTheSigningScreenSaysTheWalletHasNotBeenAsked`, which pins the rendered
  sentence the doc now points at.
- ~~**`prose/recovery.go:370` still credits Core's lock release**~~ **Done,
  2026-08-27**, in the #27 + #30 slice, because it was a known-false sentence
  three paragraphs from a line that slice was rewriting and `internal/prose` was
  in scope already — #32's own slice is `internal/journal` plus `internal/abort`,
  where this would have made a third package. **#21's class rather than #6's**,
  copy crediting a deleted dependency. `abort.Run`'s own doc names two reasons
  and the screen says two now. **`internal/abort/run.go:106` is the same class
  and was left**, reported on the issue: its `Run` doc still cites *"a lock
  release queued behind an abandon that failed"* as what an early return leaves
  behind. **And #32's first item has a screen half**, also reported there —
  `RecoveryOutcome`'s *"%d shims already gone … It is not a failure"* names two
  causes for `AlreadyGone`, observed neither, and the third is the one where the
  channel is **actually pending** with clock B running.

**And the sweep's other half found a test that could only pass.**
`internal/doctor/doctor_regtest_test.go`'s second loop still named `"Bitcoin
Core"`, `"the cold wallet"`, `"the coins"` and `"the fee rate"` — four checks
item 5 deleted. `byName[name]` returns the zero `Check` and the zero `Status` is
`OK`, so four sevenths of it asserted nothing, eleven lines under a comment
naming those same four as removed. Fixed in this slice, off one shared list used
by both loops. **This is the failure the file itself warns about at
`doctor_regtest_test.go:98`**, and it is the standing reason to key an assertion
on an identifier where one exists: #22's rename broke the build instead.

**`internal/settle` has a pane test now, and it found the report over the pane in
seven of its eight branches** — the worst at 144 columns. `State.line`
hand-rolled a three-column row with no width check and the third column is LND's
own unbounded refusal text. It wraps under the row now, the way `prose.Table`
puts an over-wide note on its own line. **Wrapped rather than truncated, and that
is the decision**: a truncated refusal is a cause the operator cannot read at
all, which is the defect the file was being audited for, while a line this
program wrapped on purpose is only wider than it wanted to be — and the emulator
would wrap it anyway, taking every row's alignment with it. Runes, not bytes.
`TestTheFundingHorizonIsReachedByMining` logged the report on a live channel one
block past the horizon and asserted nothing about it; it asserts now, and it is
the one place that *may* say the peer gave up, because it asked the peer's own
node — which is exactly the evidence the application does not have.

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
against a running regtest node — including the one claim that used to be source
only, see issue #16 below — **and now against mainnet**: two real peers, two
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

**LND will still force-close that promise if asked, and it is the wrong move.**
Measured at v0.21.2-beta while building issue #16's test: `closechannel --force`
on a *pending* channel is not refused. LND marks it
`ChanStatusBorked|ChanStatusCommitBroadcasted` and broadcasts the commitment,
which Core accepts as a child of the unconfirmed funding transaction — and on a
batch that was never published, that commitment's parent does not exist anywhere.
So the ten-minute-window instinct to "close it and start again" destroys the
channel and recovers nothing. `internal/abort` abandons rather than closes, and
`CloseChannel` is never-listed, which is why the app cannot make this mistake on
an operator's behalf.

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

  Confirmation depth, stuck transactions, CPFP, a force-close of an armed
  channel and LND's ~2016-block forget horizon are all testable in seconds — the
  horizon is about ten seconds of mining, and the force-close about one. **The
  peers' ten-minute window is not**, and this file used to list it
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
