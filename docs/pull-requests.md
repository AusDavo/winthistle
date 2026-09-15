# Pull requests

Twenty-nine merged pull requests, archived here as the repository's own record.

The GitHub repository was deleted and recreated to scrub two things out of its
history: a client's name, which should never have been in a file destined to be
public, and the funding txid of the first mainnet batch. Deleting the repository
also deleted the pull request pages, and several of these bodies are the only
written account of a decision -- #65 says so in as many words, and #66 is the
whole of what the second live batch taught. They are reproduced verbatim apart
from the redactions noted below.

**Redactions.** Mainnet funding txids appear as `<txid redacted>`. Two were
removed: the first batch's, which was also scrubbed from the repository, and the
second batch's, which never appeared in the repository at all and survived only
here. Run IDs, dates, channel counts, amounts and LND versions are untouched.

Nothing else is changed. Headings inside each body are demoted two levels to sit
under its entry; the words are as merged.


| PR | Title | Merged |
|---|---|---|
| [#1](#pr-1) | The 2026-08 inversion replan, items 1 to 6 | 2026-08-25 |
| [#4](#pr-4) | Read a raw signed transaction at step 7, and only a PSBT at step 4 | 2026-08-26 |
| [#7](#pr-7) | Settle every member the batch has, not only up to the unluckiest one | 2026-08-26 |
| [#9](#pr-9) | Wait for the wallet to finish writing before reading the file | 2026-08-26 |
| [#12](#pr-12) | Measure the chunked writer instead of asserting it (#10) | 2026-08-26 |
| [#14](#pr-14) | Say that step 8 has been taken, and stop the docs claiming otherwise | 2026-08-27 |
| [#17](#pr-17) | Write the recipients as a CSV, and say the app writes a file now | 2026-08-27 |
| [#18](#pr-18) | Prove the chan_pending receipt by force-closing an armed channel | 2026-08-27 |
| [#19](#pr-19) | Prove the credential is a macaroon, and stop naming a cause for a refusal | 2026-08-27 |
| [#23](#pr-23) | Say only what the program established, in the three places left that did not | 2026-08-27 |
| [#29](#pr-29) | Say what the journal established, not what the runs did | 2026-08-27 |
| [#31](#pr-31) | A step-7 failure is awaited, not declined (#25) | 2026-08-27 |
| [#34](#pr-34) | The recovery screen says what it read | 2026-08-27 |
| [#35](#pr-35) | The journal stops naming a cause for an absent shim | 2026-08-27 |
| [#44](#pr-44) | The signed row is written after the check that establishes it (#37) | 2026-08-27 |
| [#46](#pr-46) | reportArmed names the custodian LND's own code names (#39) | 2026-08-27 |
| [#53](#pr-53) | make check-citations, and the version rot it finds | 2026-08-27 |
| [#54](#pr-54) | Copy that names its source, and sentinels that name their branch | 2026-08-27 |
| [#55](#pr-55) | A peer's minimum_depth is a reading, not a guess | 2026-08-27 |
| [#56](#pr-56) | The count at verify grows under the batch | 2026-08-27 |
| [#58](#pr-58) | The armed window reports the refusal it observed | 2026-08-27 |
| [#59](#pr-59) | The shim with no handle is cancelled where it is made | 2026-08-27 |
| [#60](#pr-60) | The wallet that signed the live batch is named | 2026-08-27 |
| [#61](#pr-61) | The handoff holds hazards, not its own history | 2026-08-27 |
| [#62](#pr-62) | The finished plans go to git | 2026-08-27 |
| [#63](#pr-63) | The review goes with its triage | 2026-08-27 |
| [#64](#pr-64) | The run says where you are and what stopping costs | 2026-08-28 |
| [#65](#pr-65) | The step rail plan goes to git | 2026-08-28 |
| [#66](#pr-66) | The second live batch taught the docs three things | 2026-08-29 |

---


<a id="pr-1"></a>

## #1 — The 2026-08 inversion replan, items 1 to 6

Merged 2026-08-25 from `docs-rewrite-inversion` into `main`.


Sixteen commits carrying `docs/replan-2026-08.md` from a written plan to a
finished one. **All six items are done**; what is left is the mainnet cold
probe, which is not a code slice.

The tree went from **55,670 Go lines to 28,881**.

#### What changed about the product

The sequence is inverted. It used to sign inside the peers' ten minutes and
collect the `chan_pending` receipts afterwards; it now verifies with
`skip_finalize`, collects all *n* receipts with **nothing signed**, and only
then asks for a signature with no clock running.

```
2  open n streams        psbt_shim + no_publish            ← clock A starts
5  verify + psbt_verify  skip_finalize; txid pinned
6  n × chan_pending      ← GATE OPEN, nothing signed
7  sign in Sparrow       no ten-minute pressure
8  publish once, via LND
```

That is possible because of the finding the whole replan rests on: in
`lnwallet/chanfunding/psbt_assembler.go`, `PsbtIntent.Verify` closes
`PsbtReady` itself when `!i.shouldPublish && skipFinalize`, so `skip_finalize`
does **not** skip the gate — it skips handing LND a *signed* transaction, which
LND does not need when `no_publish` is set. `CLAUDE.md`'s rejected list had this
marked non-negotiable for the opposite reason, and that reasoning was wrong.

**Proved on regtest before anything was built** (item 1), then again end to end
at *n* = 3.

The app also stopped building and signing the transaction. Sparrow does both;
`run` prints the recipients and reads two files. What the tool does is the two
things Sparrow and LND cannot do between them — **attribute the funding outputs
to peers**, and **hold the I-1 gate**.

#### The items

| | |
|---|---|
| 1 | Prove the inversion on regtest |
| 2 | Rewrite the docs to this direction |
| 3 | Change `internal/arm` to the new sequence |
| 4 | Add the `--psbt` path to `run`, stop calling `coldwallet` |
| 5 | Delete the cut packages — **28,267 lines** |
| 6 | Demote the fee and change findings, remove `Replaceable` |

#### The invariants

- **I-1 · publish only once every channel is recoverable** — unchanged, and now
  nearly free to hold. One `PublishTransaction` call site, and
  `Method.CallSites` pins it at 1.
- **I-2 · dissolved**, with the reason recorded rather than quietly dropped.
  It existed so nothing could broadcast *before the gate opened*; after the
  inversion there is no "before the gate opens". Step 4 still refuses a signed
  packet, and that refusal is **I-1's**, not I-2's.
- **I-3 · the txid must not move** — now the only load-bearing check on what
  comes back from the signing wallet.
- **I-4 · no RBF on the funding transaction** — enforced by authorship, which is
  all that ever enforced it. Item 6 removed the sequence-number lint; **that is
  not a relaxation**, because the lint judged a signal Core ignores (full-RBF is
  unconditional in Core 29, verified against a running node).

#### What was deleted, and what that cost

Bitcoin Core from the application, `coldwallet`'s setup half, `setup`, `bump`
and the CPFP child, `rehearsal`, `signers`' multi-device round, `server` and
`webrun`, `signet/`, `fees`.

Two consequences are stated in the code rather than left to be discovered:

- **The fee rate is declared, not fetched.** `[fees] target_sat_per_vb`, with
  `--fee-rate N` overriding. Nothing replaced `estimatesmartfee` because the
  no-third-party rule forbids the substitute. Two independent refusals stop it
  defaulting to zero.
- **There is no pre-flight.** `testmempoolaccept` went with Core.
  `plan.Verify`'s `Verification.Unchecked` names it and says this build does not
  run it. What survives is `combine.Accept` executing every input's witness
  against its own script.

`internal/bitcoind` and the simulated multisig cold wallet survive **inside the
harness only**, as the stand-in for Sparrow.

#### Item 6, and the one decision worth reviewing

`ChangeMissing`, `ChangeTooSmall`, `FeeTooLow` and `FeeTooHigh` now **report**
rather than refuse. Your fee and change arrangements are yours; the app does not
build the transaction and cannot size a change output for you.

**They did not go to `Verification.Unchecked`, which the replan and the design
page both specified.** `Unchecked` names what the verification *could not
establish* and renders under "Not checked here" — and "your change is 600 sat
and the floor is 12,350" is something the verifier plainly did establish. Filing
it there would cost that heading the only thing it is for. So `Verification`
grew a third list, `Reports []Finding`, under **"Reported, not refused"**.
`OK()` is still `len(Problems) == 0`.

`Finding` is a **separate type** from `Problem` rather than the same one in a
second slice, because `Problem`'s comment — *"Every problem is a refusal. There
is no severity here on purpose"* — had to stay literally true rather than be
edited around. Nothing grew a severity field.

Four codes stay refusals for reasons that are not interchangeable:
`ChangeAmbiguous` (attribution), `NoFee` (LND's own rule), `Unsizable` (nothing
to report about), `LegacyInput` (I-3).

#### Copy

Four claims that were simply wrong were found by auditing rather than by
working, and corrected: `handleFundingSigned` does not exist in LND v0.19.3-beta
(it is `funderProcessFundingSigned`) and was cited in three documents; there is
no CI in this repository, though two files said there was; the harness borrows
Polar's images rather than being Polar; and the design page's macaroon block
named two methods the build never calls while omitting two it does — with both
lists 17 long, so the count matched while the membership did not.

Item 6's sweep found eight more places carrying custody language about a stuck
batch than the three that had been inventoried. The rule the replacements
follow: **name what is missing (the lever), never imply what is not (risk).**
Nothing is at risk in a stuck batch.

#### Verification

`make check` green. Zero leaked coin locks. Run alone and passing:

- `TestSkipFinalizeReachesChanPendingWithNothingSigned`
- `TestTheBatchPublishesExactlyOnceAndOnlyAfterEveryChannelIsRecoverable`
- `TestTheColdProbeRunsTheRealPathAndWithholdsStepNine`
- `TestTheFilePathDrivesTheWholeSequence`

The last two terminate through the abort path, which is what the mainnet probe
does.

#### Reviewing this

It is large, and the commits are the unit — each one keeps the tool working and
carries its own argument in the message. `docs/replan-2026-08.md`'s four "as
built" sections are the account of what actually moved, and `HANDOFF.md`'s "The
next thing" is the cold probe procedure.

**Note on the diff range.** Item 1's regtest proof was committed to `main`
rather than to this branch. `main` has since been pushed (fast-forward,
`557dfd9..1c7c4a8`), so that commit is in the base — GitHub may still list it
among this PR's commits, which is a display artifact and not a second copy. The
merged result is the same either way.

🤖 Generated with [Claude Code](https://claude.com/claude-code)


<a id="pr-4"></a>

## #4 — Read a raw signed transaction at step 7, and only a PSBT at step 4

Merged 2026-08-26 from `step-7-accepts-a-raw-transaction` into `main`.


Closes #3.

#### What it does

`FileWallet.Signed` now reads a finalised raw transaction — hex or binary — as
well as a PSBT. `FileWallet.Built` still reads a PSBT and only a PSBT.

#### Where the acceptance went, and why not in the sniffer

`combine.Parse` was the one sniffer behind both calls, inside `wait`. Teaching it
about raw transactions would have taught step 4 about them too, and step 4 is
before the gate: `combine.Unsigned` proves a packet is unsigned by reading its
*partial signatures*, and a raw transaction has none — so a raw **signed**
transaction would have been accepted in silence, by a check with nothing to look
at. That is I-1 at the last place it can be defeated from outside.

So `wait` returns the file bytes undecoded and each caller decodes for itself.
`Built` calls `combine.Parse` alone; `Signed` tries it, then falls back to the
new `combine.SignedFromTX`, which pins the txid against the base's unsigned
txid, refuses a witness-less transaction, lifts the witnesses onto a packet built
from the base's own transaction, and hands that to `combine.Accept` **unchanged**
— one acceptance path, not two.

#### One thing worth knowing

`wire.MsgTx.TxHash()` is the non-witness serialization, so it covers every
scriptSig. Txid equality with the base therefore *proves* every input's scriptSig
is empty — which is why `SignedFromTX` lifts witnesses only and has nothing else
to lift. A nested P2SH-segwit input fails the pin rather than slipping through,
and the PSBT path fails it the same way at `Finalize`'s `ErrTXIDMoved`.

#### Invariants

- **I-1** untouched, and guarded: `TestStepFourStillRefusesARawSignedTransaction`.
- **I-3** if anything more directly checkable — a raw transaction's txid is a fact
  about the bytes that would be broadcast.
- **I-4** unaffected; nothing here constructs or replaces a transaction.

#### Tests

All run with `-p 1`, and the regtest ones against a live node:

- `TestStepFourStillRefusesARawSignedTransaction` — binary, hex, and unsigned
- `TestStepSevenTakesAFinalizedRawTransaction` — both encodings reach a
  byte-identical `RawTx`, txid, fee and vsize to the PSBT path
- `TestARawTransactionWhoseTXIDMovedIsRefused` — a changed sequence number,
  naming the pinned txid
- `TestAnUnsignedRawTransactionAtStepSevenIsRefused`
- `TestSomethingThatIsNeitherIsNamedAsNeither` — truncation included
- `TestStepSevenReadsARawTransaction` / `…RefusesATransactionThatIsNotThePinnedOne`
  at the transport
- `TestTheFilePathDrivesTheWholeSequence` is now a table over both step-7
  encodings, driven by the harness's new `Env.ViewFinalTransaction`

#### Docs

`CLAUDE.md`, `README.md`, and `docs/design.html` — the last **republished to its
artifact in this slice**, per CLAUDE.md, so the live page and the file are
byte-identical.

🤖 Generated with [Claude Code](https://claude.com/claude-code)


<a id="pr-7"></a>

## #7 — Settle every member the batch has, not only up to the unluckiest one

Merged 2026-08-26 from `fix-6-settlement-carries-on` into `main`.


Fixes #6, found by the first live mainnet batch.

Two defects in `internal/settle`, and the second was the worse one. Both came
from the same assumption: that a batch's members share a fate. **They share a
funding transaction and nothing else.**

#### 1 · `UPDATE_FAILURE_UNKNOWN` was classified terminal

It is LND's catch-all, not a verdict. A freshly-opened channel with a briefly
offline peer lands there, and the identical update applied cleanly by hand a few
minutes later. **But the reading it replaced was guarding something real** —
retrying forever is also wrong — so the bound moved rather than went:

- `PolicyOutcome.Terminal()` is `INVALID_PARAMETER` and nothing else, terminal on
  the first refusal. LND checks the CLTV delta and the inbound fees against its
  own bounds before it looks at the channel at all.
- `PENDING` and `NOT_FOUND` retry with no clock, because both name what they are
  waiting for.
- `PolicyOutcome.Unexplained()` — `UNKNOWN`, `INTERNAL_ERR` and **any reason this
  build does not recognise** — retries for `settle.RetryWindow`, ten minutes from
  the first refusal of that kind, and is then reported.

An unrecognised value is deliberately retried rather than called terminal:
treating a value you cannot interpret as a verdict on the policy is exactly the
mistake `UNKNOWN` was.

The bound is in **time rather than attempts**, because a count of attempts only
means minutes at one particular `Options.Interval` and the interval belongs to
the caller. `Options.RetryWindow` is the seam that makes the window testable
without injecting a clock, and `Tick` holds the only clock there is, so
`State.Stuck()` stays a question about a `Result` rather than about the moment it
is asked.

#### 2 · `Settle` returned on the first stuck member

The half that actually cost something. Four channels had not even opened yet and
lost their watcher, so each went live at LND's defaults — 1000 msat and 1 ppm —
which is the drain window the loop exists to close.

A stuck member is now recorded, the loop carries on for everyone else, and one
`ErrStuck` comes back at the end naming every one of them **with its channel
point**, because the operator's next move is one `updatechanpolicy` per stuck
channel. `Result.finished()` is the exit, and it is neither `Done()` nor "any
member is stuck".

#### Copy

*"That is the policy itself, not the channel"* asserted something the app cannot
know, and it was false about the valid policy that hit this. It belongs to
`INVALID_PARAMETER` alone now; an unexplained refusal gets its own paragraph
saying LND declined to say, and `run.go` no longer calls a settlement that ran to
the end for everyone else "stopped".

#### Tests

The four the issue names, plus the mid-retry report — the state an operator will
actually see, and the one place the new copy has five substitutions in it.

Full suite green with `-p 1`, including the harness-backed settle tests against
the live regtest node. (The cluster needed `make -C regtest reset` first: alice's
LND wallet was ahead of bitcoind, so every harness-backed test had been skipping
silently.)

#### Docs

`docs/design.html`'s hazard table claimed the policy is "applied at step 9 the
moment each channel goes active" — precisely the claim this bug broke. It now
says the gate is per channel, and the claims list carries the mainnet half of the
`UpdateChannelPolicy` finding. Republished to the same artifact URL in this
slice. `CLAUDE.md` records the first live batch and #6 alongside #2, #3 and #5.

🤖 Generated with [Claude Code](https://claude.com/claude-code)


<a id="pr-9"></a>

## #9 — Wait for the wallet to finish writing before reading the file

Merged 2026-08-26 from `fix-8-partial-file-reads` into `main`.


Fixes #8.

`FileWallet.waitAny` returned any `os.ReadFile` that did not error, including
the zero-byte one a poll sees between a wallet's open and its write. That
prefix went straight to the decoder, and the operator got *"it is empty"* for a
file that was correct a millisecond later. At step 4 that is a failed run
**inside clock A**, which on a five-channel batch is every peer's reservation.

It predates issue #5: `wait()` had the same `case err == nil: return body, nil`
at `0e8a050`, and `9c051a9` moved it into `waitAny` verbatim while adding the
second filename. Both verified against the tree rather than taken on trust.

#### The rule

`run.readWhole`:

- **an empty file is skipped without being read**, whatever it does next — no
  valid transaction is empty in either encoding;
- **a stat either side of the read** discards one the writer moved underneath it.

Two syscalls, and **no extra tick**: a file that was already complete when the
first poll found it is still read on that poll. `TestAFileCompleteOnTheFirstPollIsReadOnTheFirstPoll`
runs at a 3 s poll and fails if a whole interval appears. The 5 ms the other
tests use is the other end of the same assertion.

**Not by retrying on a decode failure.** It would close the same race and it
would make a genuinely wrong file — the operator saved the wrong transaction,
or signed at step 4 — indistinguishable from a slow one. Step 4's refusal of a
signed packet is I-1's last gate, and a gate that waits instead of refusing is
not one. `TestAnInvalidFileStillFailsLoudly` asserts the refusal at both steps
and asserts it is not a context deadline.

**And the wait says when it is holding off on a file**, once, after two
consecutive looks find it present and unfinished. Declining to read replaces a
refusal with a wait, and at step 4 that wait ends by blowing clock A in
silence — which is the defect issue #5 was, wearing better manners.

#### Where this is narrower than the issue reads

A prefix *sitting still* between two chunks of a chunked writer is
indistinguishable from a short transaction. Stat-around-the-read misses it —
and so does the issue's own "stable across consecutive polls", which reads a
static prefix just as happily one tick later. Closing it needs a rule that
costs a whole poll interval on **every** run: two seconds at `DefaultPoll`,
inside clock A, for a file that was finished before anybody looked.

The trade is worth taking because the exposure is lopsided. A wallet saving
1,100 bytes opens the file and then writes it, so the file is **empty** for the
whole gap and then jumps to full length in one write. That is the wide window,
it is where the flake landed, and it is closed outright. `readWhole`'s doc
comment states the residual rather than implying full coverage.

#### Tests

Constructed, not timed:

- a file that never finishes is waited on **by construction** (step 4 and step 7);
- a file that does finish is completed only after the transport has *printed*
  that it looked and declined — a happens-after edge, not a sleep;
- the growing-file case is a 32 MB sparse file extended in a loop with nothing
  in it, so the size cannot fail to move while the read is in flight;
- `readWhole`'s own rules are covered close up in a `package run` test.

Every new test was checked against the previous behaviour with `readWhole`
swapped back to the naive read: all of them fail, except the two that are
constraint guards rather than regression guards.

`make -C regtest info` showed **peers=3** before and after. Full suite
`go test -p 1 -v -count=1 -timeout 20m ./...`: every package `ok`, the only two
SKIPs are the `WINTHISTLE_SLOW` eleven-minute clock tests, and the
harness-backed `TestTheFilePathDrivesTheWholeSequence` and
`TestTheColdProbeRunsTheRealPathAndWithholdsStepNine` show `--- PASS`.

`docs/design.html` needs nothing: it names the two watched paths and the
refusal of a pre-existing `--psbt`, and claims nothing about when a file is
read. No republish.

🤖 Generated with [Claude Code](https://claude.com/claude-code)


<a id="pr-12"></a>

## #12 — Measure the chunked writer instead of asserting it (#10)

Merged 2026-08-26 from `fix-10-chunked-writer-evidence` into `main`.


Closes #10 as `wontfix` in substance: the poll rule is unchanged, and none of the
four options in the issue is taken. What changes is that `readWhole`'s doc comment
now carries a measurement where it carried an assertion — and the assertion was
wrong.

#### What was checked

Sparrow 2.5.3 is the wallet this transport is built around and is the build
installed on the machine this was run from. Its three save paths were read in
source at that tag, and each shape was then run under `strace`.

| path | source | shape |
|---|---|---|
| binary PSBT | `HeadersController.savePSBT:1100`, `AppController.savePSBT:853` | `FileOutputStream.write(byte[])` |
| base64 PSBT | `AppController.savePSBT:849-851` | `PrintWriter(OutputStreamWriter(fos, UTF_8))` |
| `.txn` final tx | `HeadersController.saveFinalTransaction:1422` | `PrintWriter(File, UTF_8)` |

```
file                   writes  byte counts
A-1100.bin                  1  [1100]          A = binary PSBT
A-6000.bin                  1  [6000]
A-12000.bin                 1  [12000]
A-30000.bin                 1  [30000]
B-12000.b64                 2  [8192, 3808]    B = base64 PSBT
B-30000.b64                 4  [8192, 8192, 8192, 5424]
C-12000.txn                 2  [8192, 3808]    C = .txn hex
C-30000.txn                 4  [8192, 8192, 8192, 5424]
```

**The binary PSBT save is one `write(2)` at any size** — unbuffered
`FileOutputStream`, and the JDK issues a single write for the whole array.
**The other two split**, into 8,192-byte chunks, because both go through an
`OutputStreamWriter` whose encoder buffer is 8192 bytes. `.txn` is hex and so
twice the transaction's size, which puts a 2-of-2 batch of about fifteen inputs
over the line.

So a writer that splits the write is real rather than exotic, and the doc
comment's "which for a file this size is not what a wallet does" was wrong.

#### Why the conclusion survives anyway

The write count was the wrong number to ask for. The gap between two chunks is
what decides it, measured with `-ttt -T`:

```
C0.txn   8192  (first)      C1.txn   8192  (first)      C2.txn   8192  (first)
C0.txn   8192  0.286 ms     C1.txn   8192  0.249 ms     C2.txn   8192  0.255 ms
C0.txn   3616  0.152 ms     C1.txn   3616  0.142 ms     C2.txn   3616  0.133 ms
```

0.05–0.3 ms, under `strace`, which inflates it. No syscall in that gap, no I/O
and no operator — it is the encoding of the next 8 KB. A poll must land inside
one of those *and* finish its read inside it. Two seconds on every run, inside
clock A, does not buy that.

#### What the source settles as a bonus

All three paths create and truncate the file **before** they compute what to put
in it — `new FileOutputStream(file)` and then `getForExport().serialize()`, or
the `PrintWriter` and then `Utils.bytesToHex(finalTx.bitcoinSerialize())`. So
the file sits at zero bytes for the whole of the serialization. That is #8's
premise, stated by the source rather than guessed at, and it is why the empty
window was the wide one and the right half to have closed outright.

#### Scope

Comment and `CLAUDE.md` only. No behaviour change, no test change.
`TestAFileCompleteOnTheFirstPollIsReadOnTheFirstPoll` survives, which it could
not have under the two-poll rule. `docs/design.html` claims nothing about when a
file is read, so it needs nothing.

#### Verification

`internal/run` passes in full against the live harness (`peers=3`), `-p 1 -v`,
with no skips — including `TestTheFilePathDrivesTheWholeSequence` on both
encodings and `TestTheColdProbeRunsTheRealPathAndWithholdsStepNine`.

🤖 Generated with [Claude Code](https://claude.com/claude-code)


<a id="pr-14"></a>

## #14 — Say that step 8 has been taken, and stop the docs claiming otherwise

Merged 2026-08-27 from `docs-readme-critique-pass` into `main`.


The README's sharpest claim was false. It said **"No funding transaction has ever been broadcast by this tool on mainnet — step 8 has never been taken outside regtest"**, written before run `20260826-191016-d9407c` published five channels and 9,000,000 sat at txid <txid redacted>, confirmed. The evidence landed in `CLAUDE.md` and the code; the documents kept their pre-run wording.

`docs/design.html` had the same gap in a sharper form: its hazard register already carried the settlement finding from that batch, so the page half-knew about a run its status section said had not happened. It also asserted something the batch disproved — that a node's smallest and median graph capacity are *"a good empirical proxy for what they accept"*. They are not: the smallest existing channel was wrong about the peer's minimum for **three of the five**, because a node's smallest channel may be one *it* opened outbound, which its own inbound minimum never constrained. Only `accept_channel` is authoritative.

Republished in this slice, as the repo rule requires. Live and local were byte-identical beforehand — checked rather than assumed.

The rest is a critique of the README worked through in three passes.

#### Structural

- **Status** is one sentence plus a provenance table: what was proved, where, when, at what *n*, against which LND version. The two mainnet **node** versions are `TODO` rather than guessed.
- **"Do not use" is gone.** An unqualified prohibition surviving a published batch reads as ritual, so the exposure is stated instead: one node, one operator, three runs, no CI.
- The sequence and a quickstart move above the justification.
- **"Why not just build it in Sparrow" argued against a claim nobody made.** It is now "What this is not", making the real case — LND-internals coupling against a wallet's release cadence, the macaroon as a bearer credential, reviewable in an afternoon, two programs that must agree — and conceding that integration would *not* improve hardware-device verification either way.
- Four passages narrating the repo's own git history move to **`docs/superseded.md`**, with the partial-signatures one kept and cut to four sentences.

#### Accuracy

- **The load-bearing property now says it rests on behaviour, not an interface**, and says what would catch a change. The answer is asymmetric and worth stating: a `skip_finalize`/`no_publish` regression is loud, but a *reordering* — `chan_pending` before the commitment signature is stored — would be **silent**. Nothing here force-closes a pending channel, so recoverability is inferred from source and never exercised end to end, and `doctor` prints the node's version while grading it against nothing.
- **Every version-pinned measurement carries a date**: 10m41s on 2026-08-23 at v0.19.3-beta and 10m14s on 2026-08-26 at v0.21.2-beta; the two `lncfg` constants read at v0.21.2-beta; full-RBF checked against bitcoind 29.0 on 2026-08-26.

#### New sections

- **Step 6 in detail** — rendered from `run.go`'s own format strings at `prose.ProseWidth`, not illustrated.
- **Threat model** — assume one box; what is defended is error, not an adversary; and the load-bearing assumption is that **LND is honest**, since the funding addresses are its claim and a compromised LND handing over an address that is not what it says would not be caught here.
- **How many channels** — `--maxpendingchannels` defaults to 1, so the bound is per peer; a peer that never answers costs the whole batch because the gate is *n* of *n*; and the receipt wait has no deadline.
- **The batch file** — where peer selection actually lives.

#### Also

Corrects *"steps 2 to 6 take about a second"*, which read as though clock A were nearly free. Winthistle's own work takes about a second; the ten minutes are spent at step 4, in Sparrow, by the operator.

And `cmd/winthistle/main.go` still called Phase 0 *"the peer pre-flight, **the fee source** and the reserve check"*. There is no fee source. It is the shim probe.

#### Checks

`go build`, `go vet` and `gofmt -l` clean. The test suite was not run — this is documentation plus one usage string, and the harness-backed tests need regtest up with `-p 1`.

🤖 Generated with [Claude Code](https://claude.com/claude-code)


<a id="pr-17"></a>

## #17 — Write the recipients as a CSV, and say the app writes a file now

Merged 2026-08-27 from `csv-15-send-to-many` into `main`.


Closes #15.

Step 4 asked the operator to type or paste *n* addresses inside clock A — the only part of the peers' ten minutes that takes any time, and the only part that grows with *n*. Sparrow's **Send to Many → Load CSV** reads a file; this writes one.

#### The format was settled by measurement

From #15's comment, five deliberately different rows loaded into a real Sparrow 2.5.3. Address, amount, label; **BTC to eight places**; no grouping separator anywhere; the label always quoted; a header row.

The unit is the one decision that matters, and the reason is asymmetry rather than taste:

- a **sat integer in BTC mode** loads 10⁸ too large, silently, under the supply cap, surfacing much later as insufficient funds at coin selection;
- a **BTC decimal in sats mode** fails to parse, drops every row, and makes Sparrow say *"ensure amounts are in sats."*

The second names its own cause. No `--csv-unit` flag — it would reopen the question the measurement closed.

#### The decision the slice turned on

Not the format. **This is the first file the application writes** — `os.WriteFile` and `os.Create` appeared nowhere in `internal/` or `cmd/` outside the harness, and `docs/design.html:515` said *"the app writes none and reads two"*.

**It writes it.** The alternative — print the CSV to stdout and let the operator redirect — keeps "writes nothing" literally true and is worse in every way that matters: it puts the file's name back in the operator's hands at the one step where a wrong name costs silence rather than a refusal (#5), it cannot be refused when stale, and it asks somebody inside clock A to get a shell redirection right. Rejected in the commit message rather than silently.

**The name is derived from `--psbt` and an existing one is refused.** A stale `batch-recipients.csv` was written for funding addresses that are not this batch's, so loading it builds a transaction paying somebody else's outputs — refused at step 5, inside clock A. Refused rather than *overwritten*, which is where it parts from `SignedPaths`: those are names the run has spent its whole length telling the wallet to write and clears one line before watching. This is a name invented out of the operator's own stem in the operator's own directory, and truncating a file we did not create is not a thing to do silently. The suffix carries its own extension, so no `--psbt` value can collide it with `Unsigned` or either signed path.

**What replaced the stated property: the app writes nothing it later trusts.** The CSV carries no transaction and no signature, is never read back, and nothing downstream depends on it — so a failure to write it is reported and not returned. A convenience that can end a batch inside clock A is not one.

#### What did not change

- **The printed table stays, and is not the CSV's preview.** It is the attribution, which is job one, and the only screen on which which-peer-gets-what is legible. Both renderings come off one `[]Recipient` in one call.
- **Step 5 is still what checks it**, in those words, on screen. A 10⁸ amount is `WrongAmount`, a dropped row is `MissingOutput`, a shifted column is both.
- `combine.Unsigned` and step 4's refusal of a signed packet are untouched. `Method.CallSites` is still 1 and no LND call was added.

#### Tests

- A **golden test on the exact bytes** (`internal/run/csv_internal_test.go`): an alias with a comma, an alias with a quote, an amount that is not a round BTC, an amount that is. Plus `btcAmount` at the edges and a row whose alias carries a line break. Its doc comment says what it does *not* claim — `float64` with `%.8f` agrees on every one of those cases, so the integer form is an argument that does not have to be made rather than a bug that was found.
- `TestTheFilePathDrivesTheWholeSequence` now asserts the CSV and the scraped table name the same outputs at the same amounts, against a live batch — the only place both exist at once. Breaking `btcAmount` to emit sats fails it.
- Transport tests for the derived path, the stale-file refusal, and that step 4 writes the file *and names it*.

Full suite green under `-p 1 -v` against a fresh harness (`peers=3`); the only SKIPs are the two `WINTHISTLE_SLOW` clock tests.

#### Docs

Copy swept across `internal/run`, `internal/regtestenv`, `internal/plan`, `README.md` and `docs/design.html` — the paste is no longer the seam, "mis-paste" is no longer the representative step-4 error, and `connect.go`'s *"the two files `--psbt` names"* was the buried one. `docs/design.html` is **republished to the same artifact URL** in this slice.

🤖 Generated with [Claude Code](https://claude.com/claude-code)


<a id="pr-18"></a>

## #18 — Prove the chan_pending receipt by force-closing an armed channel

Merged 2026-08-27 from `force-close-16` into `main`.


Closes #16.

The whole safety argument rests on one sentence: **each `chan_pending` is a receipt that the channel is already recoverable by force-close.** That was read out of `funding/manager.go` — `funderProcessFundingSigned` emits `chan_pending` at `:2897`, strictly after `CompleteReservation(nil, commitSig)` stores the peer's commitment signature at `:2813` — and exercised by nothing.

`TestTheReceiptIsProvedByForceClosingTheChannel` (`internal/arm/force_close_regtest_test.go`) arms one channel through `drive()`, publishes, mines the batch to confirmation, and force-closes the channel. The commitment transaction reaches the mempool and then a block, spending the exact outpoint `chan_pending` named, with the four-element witness of a P2WSH 2-of-2. **Bitcoin Core is the judge rather than LND**: consensus verified both signatures against that script and this node holds one of the two keys, so the peer's was stored before the receipt was emitted. 1.34 s.

#### The shape is forced, and it is the right one

`CloseChannel` is on `internal/methods`' never-list, and `TestEveryLNDCallSiteIsRegistered` scans `_test.go` and the harness too. So the force-close goes through `regtest/bin/lncli`, via one new harness helper, `regtestenv.ForceCloseOutOfBand`. `Method.CallSites` is unchanged and nothing was registered to quiet the guard — the credential's inability to close a channel is a product claim, and the test that proves what the app promises cannot be written with the app's own capabilities. No `docker` control was added.

#### Two decisions

**It stops at "the commitment is in a block."** The issue's step 5 — mine past `to_self_delay`, assert the swept output comes back — is a second claim, about LND's sweeper, and it fails for reasons that have nothing to do with the funding ordering. `to_self_delay` is still read off `ListChannels` and logged, so the number the test does not wait out cannot rot.

**The obvious negative control does not exist, and this says so rather than implying a check that was never made.** Attempting the force-close before the funding transaction confirms was expected to be refused. Measured at v0.21.2-beta: LND accepts it, marks the channel `ChanStatusBorked|ChanStatusCommitBroadcasted`, and broadcasts a commitment that Core takes as a child of the unconfirmed parent. So confirming first is not what makes the test valid — it is what makes it a test of the state the claim is about. Worth knowing beyond the test: on an *unpublished* batch that same close produces a commitment whose parent does not exist anywhere, which is one more reason `internal/abort` abandons rather than closes.

Not gated behind `WINTHISTLE_SLOW`. That gate is for clock A, which is wall-clock; this is all mining.

#### Copy

- **`README.md`** — the paragraph saying recoverability is "inferred from the source and never exercised end to end" is **deleted**, not softened. The section names the test, the provenance table gains a row, and the per-release check becomes `make test` and a re-read, in that order.
- **`docs/design.html`** — the "Claims — verify first" list had **no entry at all** for the claim the page rests on: asserted in prose thirteen times, badged nowhere. It gains one, `proven on regtest`, beside the `skip_finalize` claim whose trailing clause used to smuggle the source inference in under a regtest badge. I·1 gains the observation; the 2026-08-25 proof block gains its sibling; the commissioning card no longer says the harness does not vouch for the funding flow. **Republished to the same artifact URL in this slice.**
- **`CLAUDE.md`** — a paragraph on what changed about the evidence, the two rules above, and the pending-channel force-close finding.

#### Verification

`make -C regtest info` showed `peers=3` before and after. Full suite, `-p 1 -count=1 -v`: every package `ok`, no failures, two skips and both are the `WINTHISTLE_SLOW` clock tests. `TestEveryLNDCallSiteIsRegistered`, `TestTheReportsFitThePane` and `TestTheFilePathDrivesTheWholeSequence` all pass — the first is the guard this slice deliberately works around rather than through.

🤖 Generated with [Claude Code](https://claude.com/claude-code)


<a id="pr-19"></a>

## #19 — Prove the credential is a macaroon, and stop naming a cause for a refusal

Merged 2026-08-27 from `macaroon-13` into `main`.


Closes #13.

`lnd.Dial` read the macaroon with `os.ReadFile` and hex-encoded whatever came back, so a torn read produced a well-formed *credential* carrying the wrong bytes — `hex.EncodeToString(nil)` is `""`. LND refused it on authentication, and every sentence downstream was about the wrong half of the setup.

**The failure was never the defect.** It fails loudly and before step 2, so no peer has been told anything and no clock is running. The defect is the asserted cause — issue #6's rule verbatim, **do not assert a cause the program cannot know** — and the recommended action, re-bake, is the one that reopens the window the torn read came through.

#### What landed

- **`lnd.ReadMacaroon`** refuses an empty file outright and anything that fails `macaroon.UnmarshalBinary`, naming the file. `ErrMacaroonFile` marks the class so a caller may say "this file" and must not say "this node" or "too narrow" — nothing has been asked of LND by the time one is returned.
- **`doctor`'s `checkMacaroon` uses the same guard**, because it reads the same file a *second* time and can tear independently: `Dial` succeeding a moment ago says nothing about what is on disk now, and a re-bake is exactly what moves it.
- **No retry, no polling.** `Dial` is a one-shot at a moment nothing is waiting on. `readWhole`'s stat-either-side shape is not wanted here — that one is inside clock A and has to tell "not finished" from "wrong".
- **`go.mod`:** `gopkg.in/macaroon.v2` promoted indirect → direct. That is the whole module diff; `lnd/macaroons` is still not imported.

#### The decision on fix 3, and the rejected alternative

**Taken, not left as covered by fix 2.** The rule was: does the string match still assert something the program has not established? It does. What the predicate establishes is that LND refused the call over the credential. *Why* — a permission never baked in, a caveat, or a path pointing at some macaroon other than the one the operator baked — is not in the error. **Guarding the read closed one route into the claim and did not earn the claim.**

So the predicate is unchanged and still right (InvalidArgument is the answer about the request's macaroon; untyped `permission denied` is the interceptor refusing us; matching either alone confuses them), but:

- `tooNarrow` → `refusedOverMacaroon`, because the name was the claim rather than the observation.
- *"which means it was baked before this build"* is gone from the report **and** from the package doc.
- Both renderings now name the two readings and say re-baking only fixes one.
- `checkLND`'s stat loop offered the bake command when *either* file was missing, so a missing `tls.cert` printed a command that cannot help. The fix now follows whichever path failed.

#### Deliberately not changed

- **`internal/lnd/client.go:65`'s `no certificate found in %s`** — it names the file and asserts nothing beyond what `AppendCertsFromPEM` returned. That is the standard this slice held the macaroon path to.
- **`internal/bitcoind/client.go`'s cookie read** — same shape, harness-only since item 5.
- **`docs/design.html` does not move**, so there is no republish in this slice. Its paragraphs on checking the credential by asking and on the two refusals that look alike describe the InvalidArgument / untyped split, which is unchanged, and neither claims a cause for the untyped one.

#### The audit, and the answer to the question it was asked

**#13 is the second of at least four, not the last.** An audit of every place this build reads a file another process writes, or renders a cause it did not establish, found three more — none touched here, all worth their own issue:

| Where | Claims | Has in hand |
|---|---|---|
| `internal/settle/report.go:296` | the peer **has** cancelled the funding, overriding LND's own hedge in the same sentence | one negative block count off `PendingChannels`; nothing observed the peer's side |
| `internal/settle/report.go:237` | "Core is not connected, or it has not seen the transaction" | `run.settlePhase` passes no `Chain` at all since item 5, so `Confs` is always `-1` and this branch **always** fires, at a bitcoind this build never dials |
| `internal/combine/rawtx.go:165` | "That is the transaction you built at step 4, not the one you signed" | it returns on the *first* witnessless input, so a partially-signed transaction gets told it saved the unsigned file |

`internal/settle/report.go:296` is the worst of the three under pressure: it tells an operator their channel is already dead at the moment the remedy — confirm it anyway — is still live.

**`internal/peers` is the package the rest should be held to.** It strips LND's misleading `remote canceled … possibly timed out` prefix, labels *"The peer said"* against *"This node said"*, and says in terms when a refusal is not a verdict.

The audit also confirmed there are no other unguarded reads of a file another process writes in `internal/` or `cmd/`: `config.Load`'s TOML read is the only remaining one, and nothing but the operator's editor writes those.

#### Verification

- **A node-free table test on the bytes** — empty, one byte, a truncated prefix of a real macaroon, and a whole one. The first three must be refused naming the file, marked `ErrMacaroonFile`, and free of "permission denied" / "not authorised" / "too narrow" / "re-bake"; the fourth must return the bytes unchanged so `hex.EncodeToString` gets the same input as before. A truncated V2 macaroon does fail `UnmarshalBinary` — measured, not assumed.
- Full suite green at `-p 1` against a live harness (`peers=3`), with only the two `WINTHISTLE_SLOW` clock-A tests skipped. `TestEveryLNDCallSiteIsRegistered`, `TestTheReportStaysInThePane` and `TestTheReportsFitThePane` all pass.
- `doctor_regtest_test.go` keyed a `Contains` on the copy this slice replaced, which would have become a check that can only pass; it is re-keyed, with a comment saying so.
- `CLAUDE.md` gets the rule, and `HANDOFF.md`'s stale `doctor.tooNarrow` reference is corrected.

🤖 Generated with [Claude Code](https://claude.com/claude-code)


<a id="pr-23"></a>

## #23 — Say only what the program established, in the three places left that did not

Merged 2026-08-27 from `asserted-cause-20-21-22` into `main`.


Closes #20, closes #21, closes #22.

Three renderings of one rule — **do not assert a cause the program cannot know**,
from #6, re-broken by #13. Two of them were in the same file, in adjacent
functions. One commit per issue, so each decision is readable and revertable on
its own; a fourth for the sweep, which belongs to none of them.

The shared question is *what this program is entitled to say about a failure it
did not diagnose*, and the answer is `internal/peers`': label who said it, keep
somebody else's hedge as theirs, and name a figure of ours as ours. The remedies
did not change in any of the three — each is the right next action whether or not
the asserted cause is true, which is what made all three copy-shaped. Only the
certainty changed.

##### The four decisions

**1 · The pane test, and the over-wide third column.** `internal/settle` had no
pane test while rendering more operator-facing prose than `plan`, `doctor` or
`prose`, each of which has one. Written first, and it failed on its first run:
**seven of eight report branches were past the 78-column pane, the worst at 144
columns.** `State.line` hand-rolled a three-column row with no width check and
the third column is LND's own unbounded refusal text.

Wraps under the row now, the way `prose.Table` puts an over-wide note on its own
line — **wrapped rather than truncated**, because a truncated refusal is a cause
the operator cannot read at all, which is the defect this file was being audited
for, and because the emulator would wrap it anyway, taking every row's alignment
with it. Runes, not bytes.

**2 · `Options.Chain` survives.** Its doc comment justified it by *"assisted mode
has no Core"* and assisted mode dissolved with I-2, so it was stale twice over,
and it is set in two tests and nowhere else. **Rejected: deleting it.** Deletion
takes `State.ObservedDepth`, `depthLesson` and
`TestTheSettlementPassPoliciesAChannelAndLearnsItsDepth` with it — a live peer's
`minimum_depth` read from above, one block at a time, which is the only
authoritative reading an initiator can get — and this repository does not delete
node-verified evidence to tidy a shape away. It would also have had to edit
`docs/design.html`'s step-9 claim and republish it.

So it is **the harness's seam** and the comments say so: the application fills it
on no path and may not, `internal/regtestenv`'s Core fills it, and `Confs == -1`
is the rule rather than the exception. The third option — leaving the stale
comment — was not available.

**3 · How much may be said about the peer's horizon.** Named the way `depthNote`
names its own prediction: LND's default policy, binding stock LND and nobody
else. LND's proto hedge stays LND's; the count is named as ours.

**4 · One sentinel, not two.** No caller tells the two apart — `FileWallet.Signed`
refuses — so `ErrNoWitnesses` became `ErrIncompleteWitnesses`. Its text was the
over-claim at the sentinel level and its name said the same thing.

##### What did not change

No invariant, no gate, no call site. `Method.CallSites` is still 1. Nothing
relaxes at step 4: a partly-signed transaction is still refused and the
raw-transaction relaxation stays at step 7's call site. No Bitcoin Core and no
substitute; no new RPC. `settle.Settle` still carries on past a stuck member.
`docs/design.html` does not move — its `funding_expiry_blocks`, horizon and
`minimum_depth` claims all read the same after this — so it is not republished.

##### Verification

`go test ./... -p 1 -count=1 -v` green, with only the two `WINTHISTLE_SLOW`
wall-clock tests skipping. `TestTheFundingHorizonIsReachedByMining` drives a real
channel one block past the horizon against the live harness and now **asserts**
the report rather than logging it — and it is the one place that *may* say the
peer gave up, because it asked the peer's own node, which is exactly the evidence
the application does not have.

##### The sweep found more, and CLAUDE.md says so

Four further instances of the same rule, verified in source and left for their
own slices: `doctor.go:611`'s *"runs stopped"* off `state NOT IN (published,
aborted)`; `run.go:602` journalling `SignerDeclined` for every step-7 error
including #22's own refusal; `settle`'s *"peer offline"* off this node's link
state; and `recovery.go:388`'s *"pending here and on the peer"*. **So the claim
that the set is exhausted is not made** — #13's list is finished, the class is
not.

And one test that could only pass: `doctor_regtest_test.go`'s second loop named
four checks item 5 deleted, eleven lines under a comment naming those same four
as removed. Fixed here.

🤖 Generated with [Claude Code](https://claude.com/claude-code)


<a id="pr-29"></a>

## #29 — Say what the journal established, not what the runs did

Merged 2026-08-27 from `hedge-the-journal-check-24` into `main`.


Closes #24.

`doctor`'s journal check read `journal.Unfinished` — `state NOT IN (published, aborted)` — and reported the result as *"%d runs stopped somewhere they should not have"*. A run is in that set from the moment `journal.Begin` records its first stream, so **a batch being armed in another terminal right now was in the list, called stopped, and pointed at `winthistle recover`**. Sixth instance of #6's rule, and the sharpest: it is wrong in the direction that costs something, firing against a healthy node — often one that is healthy *because* a batch is running on it.

#### Two decisions, two commits

Deliberately separable: the copy fix and the `Fail`/`Warn` change are different claims, and a later reader must be able to revert one without the other.

**1 · The hedge goes at the callers.** `Unfinished`'s doc comment now states what the query establishes; `prose.RecoveryList`'s wording is copied rather than a new register invented.

*Rejected: narrowing `Unfinished` itself.* The narrower question cannot be answered honestly — a run that died mid-arming and one being armed right now write identical rows, and the journal carries no heartbeat. Narrowing means inventing a liveness signal (a schema decision; the journal grows by tables, never columns) or picking a time window, which is a guess wearing a query's clothes. Verified with `grep -rn "Unfinished("`: exactly two callers, and the other already hedged. `StatePublishing` runs stay in the result set.

**2 · It is a `Warn`.** `Fail` is *"this has to be fixed before a batch can be opened"*; nothing on the run path consults the journal's other runs before arming, so an unfinished one blocks nothing. `Report.OK()` no longer goes false against a healthy node.

The `winthistle recover` fix line stays: with no run id that command *lists*, and the screen it prints is the one that already hedges.

#### Scope: `doctor` and `journal`

Plus comment-only touches on the same call path in `internal/run` and `cmd/winthistle`. **`internal/prose` is deliberately untouched** — its printed recovery header is right and is the model this copies.

The sweep found the claim past the two sites the issue named: `run.Unfinished`'s doc, `cmd/winthistle`'s package doc **and its `recover` usage line** (*"list runs that stopped"* — the front door to the screen that hedges), and `README.md:472`.

**`docs/design.html` does not carry this claim and does not move** — its only mention is *"the run journal readable"*. Nothing republished, stated explicitly.

#### Tests

Both new tests are **node-free**: `checkJournal` takes the journal as a parameter and the test package is internal, so a temp database with `Begin`-ed runs is the whole fixture.

- `TestAnUnfinishedRunIsNotReportedAsStopped` — **verified to fail against the old copy** rather than only to pass.
- `TestAnUnfinishedRunDoesNotStopABatch` — the status and `Report.OK()`.
- `TestTheReportStaysInThePane` now renders a **real** `checkJournal` report. Every check in it was hand-built out of `r.add`/`say`, so the sentences the checks write *themselves* were width-checked by nothing.

That gap had already cost something: the old sentence passed `prose.IsAre` where a pronoun belonged and rendered **"3 runs stopped somewhere *are* should not have"** — ungrammatical at every count, and unnoticed because nothing had ever rendered this check.

#### The audit found six more instances, none fixed here

Run after this slice's own copy was written. Recorded in `CLAUDE.md`, **not** called a complete list. The sharpest is `prose.stateMeans`' `StateAborting` — *"An abort of this run was started and did not finish"* — which is this defect one state over: the state is written before the first RPC, so a `recover` running right now is described as one that failed. Two of the six are the `internal/prose` sites this slice deliberately left.

Its other half came back clean: **no test asserts on a copy string, check name or map key the build no longer emits**. PR #23's `doctor_regtest_test.go` fix held.

#### Verification

`go test -p 1 ./...` green across every package. `-v` shows **exactly two skips**, both the `WINTHISTLE_SLOW` wall-clock tests. `TestTheReportStaysInThePane`, `TestDoctorReadsTheWholeSetup`, `TestAWarningIsNotAFailure` and `TestEveryLNDCallSiteIsRegistered` all pass. Nothing in the safety model moved: no invariant, no gate, no call site, no registry entry.

🤖 Generated with [Claude Code](https://claude.com/claude-code)


<a id="pr-31"></a>

## #31 — A step-7 failure is awaited, not declined (#25)

Merged 2026-08-27 from `sign-does-not-say-declined-25` into `main`.


Closes #25. The seventh instance of #6's rule — **do not assert a cause the program cannot know** — and the only open one that **persisted** the wrong cause to disk.

`internal/run`'s `sign` wrote `journal.SignerDeclined`, the one `SignerState` that names an intent, on **any** error out of `SigningWallet.Signed`. And `RecordSigner` upserts on `(run_id, label)`, so the truthful `SignerAwaiting` row written twelve lines above was **replaced** by the false one; `prose.signerNote` read it back later as *"1 declined"* on the recovery screen. An operator who reads that looks at their signing device.

Everything that lands there, from `internal/run/wallet.go`:

- `:335` — a failed `os.Remove`, **before the wallet has been prompted at all**.
- `:354` — `waitAny` returning on context cancellation, which is **Ctrl-C**.
- `:358` — `decodeSigned`: a file that is neither encoding, **or `combine`'s refusal of a moved txid, which is an I-3 breach** and the one failure where sending the operator to their device rather than to the file costs the most.

Nothing in this build can observe a refusal: a file transport has no channel through which a wallet says no.

#### Decision 1 — write nothing on the error path

Option (a) of the two the issue names. What the frame established is that the wallet was asked and nothing usable came back, and `SignerAwaiting` is that in its own words, so the row already on disk is the right one. The screen now says *"1 still awaited"*, which is what happened.

**A new `SignerState` was rejected, on two grounds:**

1. The distinction it would draw — *asked and still waiting* against *asked and the answer was not usable* — is the difference between a **live run and a stopped one**, and that belongs to the run's own state rather than to a signer's.
2. No honest name for it could separate the classes anyway: the `os.Remove` and Ctrl-C cases have **no answer** to call unusable.

The cause is in the returned error, on the terminal the step is already printing to. A value every future reader must handle forever — no CHECK constraint, no migration table — buys nothing here.

**`internal/prose` therefore needed no copy change, and none was made.** That was the check the issue asked for and the answer is no.

##### The second site

`SignerDeclined` keeps its constant and gains `SignerPartial`'s treatment: its doc now says it has no writer, that a row carrying it is **not** evidence a wallet said no, and not to repurpose it. Journals written before this change carry the value wherever step 7 failed, whatever the reason, and `signerNote`'s switch names it **explicitly** rather than dropping it into the `unknown` bucket — checked, per the exhaustive-switch trap.

#### Decision 2 — taken separately, and it changed nothing

That is the result rather than an omission. `sign`'s own *"the batch was not signed: %w"* is an **outcome** the frame established, not a cause, with the cause wrapped inside it. Both `"journalling the signing step"` wrappers establish exactly what they say; the call site at `run.go:527` returns the error bare; `MarkSigning`'s wrapper and `combine.Accept`'s name activities rather than causes. The `jerr` branch's question went moot — decision 1 deleted the write it guarded.

#### The test

`TestAFailedSigningStepIsRecordedAsAwaitedAndNeverAsDeclined`, and it is **node-free**: `sign` uses only `d.Out`, `d.Journal` and `d.Signing`, so `Deps.LND` stays nil, and `journal.Begin` reaches a run in a temp database. Five error classes, keyed on `combine.ErrTXIDMoved`, `combine.ErrIncompleteWitnesses` and `context.Canceled` rather than on sentences — the last of them is the refusal **#22 rewrote one call away from this frame**, and the instance the issue led with. **Verified to fail against the old code on all five subtests** before landing — the defect was restored, the test run, the file copied back.

#### Copy sweep

Third commit, kept separate so a revert of the fix does not take it along. `grep -rn "declin" --include=*.go --include=*.md --include=*.html .` found one site this slice makes false — `CLAUDE.md`'s own #25 entry, now struck through with what was decided. Everything else is LND declining, the abort path's safe flag, or a still-accurate historical illustration: `internal/prose/recovery.go:539` and `docs/replan-2026-08.md:494` both quote *"0 returned a partial, 0 still awaited, 0 declined"* as the **old** no-default-branch bug.

**`docs/design.html` does not carry this claim and did not move.** Its four `signer` mentions are BIP174, the transport table and QR scope; both its `declin` hits are LND's own. No republish.

#### What is left for #30

The **consumer** side. `signerNote` still renders *"%d declined"*, and after this change every row it will ever see was written by the defect — so the sentence is now wrong for **all** of them, not just some. That is `internal/prose`'s to fix and belongs with the recovery-screen slice; commented on #30 rather than pulled into this diff, which stays at two packages.

#### Verification

`go test -p 1 -v ./...` — all 16 packages `ok`, **exactly two skips**, both `WINTHISTLE_SLOW`. `make -C regtest info` showed `peers=3` first. `internal/prose`, `internal/journal` and `internal/run` re-run with `-count=1` to make sure nothing came back cached.

Nothing in the safety model moved: no invariant, no gate, no call site, `Method.CallSites` still 1. I-3's refusal is untouched — a moved txid still fails the run loudly; this slice is only about what gets **recorded** about it.

#### The audit

Run read-only after this slice's own copy was written, asked two questions, and forbidden from tests and the harness.

**Question 1 — does any other journal write record a cause the caller had not established?** It enumerated the nine exported writers plus the three inside `Recover`, and every call site: **13 write sites, 11 sound.** The two that are not are **filed as #32**, not fixed here, and both were verified in source before filing:

- **`journal/recover.go:260-263`** writes `ChanCancelled` — *"its shim was cancelled before it ever reached pending"* — for a shim that was **already gone**, discarding `ShimOutcome.AlreadyGone`, **whose own doc comment names the journal as the consumer the distinction exists for.** The case where the row is false is documented **76 lines above the write**, in `AbortTarget`: a crashed process reports `AlreadyGone` for a channel that is *actually pending*. The screen then says *"Nothing of this run is still standing in LND"*, `AbortTarget` emits nothing for it, and **a second `Recover` cannot pick it up** while the peer holds its side until clock B runs out. **The sharpest instance of this rule found so far and the only safety-adjacent one.**
- **`run/run.go:522`** — `MarkSigning` writes *"the unsigned transaction is out with the signing wallet"* before the ask is made. Narrower than it looks, and filed anyway: the wallet does hold the transaction, having built it at step 4; what has not happened is the request to sign.

One extra of #21's class came with them: `prose/recovery.go:370` still credits *"Core's lock release"* in the safe-to-call-twice list, in a build that removed Core and every coin lock the app took.

**That makes four sweeps in a row that each found more** — #23's four, #29's six, #31's three. `CLAUDE.md` records the count and draws no conclusion from it.

**Question 2 — does any test assert on copy the build no longer emits?** **Clean**, and that is the second time (PR #29's was too). 61 assertion loops and ~150 `Contains`/`HasPrefix` sites across 30 files; PR #23's `doctor_regtest_test.go` fix held, and its `byName` map is guarded by an existence loop *before* the status loop. One fixture label (`doctor_test.go:240`'s `"the coins"`) names a removed check but is never asserted on, so it cannot pass for the wrong reason.

🤖 Generated with [Claude Code](https://claude.com/claude-code)


<a id="pr-34"></a>

## #34 — The recovery screen says what it read

Merged 2026-08-27 from `recovery-screen-says-what-it-read-27-30` into `main`.


Closes #27. Closes #30.

One recovery-screen slice, `internal/prose/recovery.go` plus two doc comments
in `internal/journal`. Two packages, no state, value or schema moved in the
journal, and nothing in the safety model: no invariant, no gate, no call site,
`Method.CallSites` still 1.

Every instance is the rule from #6, closed eight times over now: **do not
assert a cause the program cannot know.** This time they were all on one
screen — the one this file's own comment calls the highest-stakes copy in the
build, read by somebody who has just found a run that did not finish and is
deciding what to do with *n* pending channels.

#### The three decisions

**1 · What may the screen say about a state that a live run also holds?**
(#30 items 1 and 2, one question rather than two.)

`StateAborting`, `StateArming` and `StateSigning` are all written *before* the
work they name, deliberately, so that a crash leaves artifacts rather than
mystery. `stateMeans` rendered all three as though they were over: *"streams
**were** open"*, *"**had** gone out to be signed"*, and worst, *"an abort of
this run was started and did not finish"* — #24 one state over.
`journal.go:86` hedges that last one correctly (*"an abort is in progress, or
one was interrupted partway"*) and so does the design page's state table; only
the screen asserted it.

It is not hypothetical on any of the three. `run.recoverRun` prints this very
screen from inside the process still tearing the run down, and `winthistle
recover ID` prints it for a run that may be being armed in another terminal.

**The hedge went in `stateMeans` rather than at the caller, which is where #24
put its own**, on #24's own rule: *can the narrower question be answered
honestly?* `journal.Unfinished` could not — a run that died mid-arming and one
being armed write identical rows, and the journal has no heartbeat — so its
hedge had to go where a sentence could carry it. `stateMeans` is handed the
state itself and each of the three has an honest reading.

**Rejected: a blanket paragraph at the caller**, because it would over-apply.
`armed` and `published` are *not* written ahead of their work, so hedging them
would weaken two sentences that are true. One state renders per screen, so a
clause in each costs the reader nothing.

**Rejected: hedging `armed`.** The journal sets it itself after observing the
*n* receipts, so *"every channel reached chan_pending"* and *"the publish never
happened"* are both established.

**Noted and left to #32:** the new signing sentence says the transaction *"is
out with the signing wallet"*, which is `journal.go:65`'s own definition,
rather than *"had gone out to be signed"*, which implied the ask had been made.
#32's item 2 is that `MarkSigning` is written before `sign()` asks. This copy no
longer asserts the ask; the state's own definition is #32's to settle.

**2 · #27's arm: show the working, or say only what was read?**

`failureLine`'s `abort.ErrBluntNotConfirmed` arm said the channel *"is still
pending here and on the peer"*. `internal/abort` read **this node's**
`PendingChannels` and the abandon did not run, so the first half is exactly
that; the second was an inference, sound, asserted bare.

**Showing the working is not available here.** The paragraph that shows it —
and names clock B in blocks, as the style rule requires — is on the `Recovery`
screen, and this line is on `RecoveryOutcome`; pointing at it would break the
rule `TestNoScreenPointsBelowItself` enforces, and restating it in a bullet
would put a 2016-block horizon on a channel this failure did not touch.

So the line says the *mechanism* instead of the mechanism's conclusion: this
node still has it pending, which is what was read; an abandon tells the peer
nothing either way, so the refusal changed nothing on the peer's side. **No new
call** — there is no way to ask a peer what it holds and no route to one.

**Unchanged: the other arms.** `ErrNotPending`'s copy was checked in the same
sweep and is sound, and the new test asserts it still renders.

**3 · The comments and docs, as one look with a likely yes.**
(#30 items 3, 4, 5 and 6.) All four over-claimed.

- **`signerNote`'s zero branch** kept its first half, because here the narrower
  question *can* be answered: `run.sign` writes the `SignerAwaiting` row before
  it calls `Signed`, so no row at all establishes step 7 was never entered.
  Only *"when this stopped"* had to go.
- **`channelBreakdown`'s comment** called `no channels` *"a run that stopped
  before `Begin` wrote any"*. `Begin` refuses an empty batch and writes the run
  row and the channel rows in one transaction, and nothing deletes a channel
  row, so that shape is **not reachable through this build at all** — which is
  the reason to render something rather than a blank, the same reason
  `stateColumn` renders `(no state)`. The emitted copy was always fine.
- **`journal.Recover`'s doc** called its input *"a crashed run"*. It is handed a
  run id, and its own caller says the commonest way in is Ctrl-C.
- **`journal`'s package doc** — *"the record of what a batch **did**"* — is now
  *"what a run wrote, in the order it wrote it"*, which is the contract #24
  wrote over three files.

#### And `%d declined`, which PR #31 made sharper rather than smaller

#25 stopped `sign` writing `journal.SignerDeclined` at all, so **every row
`signerNote` will ever render it for was written by the defect**: the sentence
is not wrong for some rows, it is wrong for all of them. It reads *"%d marked
declined"* now, with a paragraph saying where the value comes from, that it
never meant a wallet said no, and what the old frame actually had in hand — a
file that never appeared, one that could not be read, a moved txid, Ctrl-C.

**Deleting the arm was not available**: those rows are on operators' disks, and
dropping them into the `unknown` bucket is the defect that function's doc
comment was written about. `journal.SignerDeclined` keeps its constant and its
documentation; the screen was what was left.

#### Two things found on the way

**No test in `internal/prose` had ever built a run with signer rows**, so every
sentence `signerNote` writes about a signer was rendered by nothing — widths
included, on the screen that has a pane test. That is #24's `checkJournal`
finding in a second package. `withSigners` fixes it.

**`RecoveryOutcome` was the one screen in this file the pane test never
measured**, and it is the screen whose width is least under the file's control:
every `failureLine` ends with `abort`'s or LND's own error text appended to a
bullet. It went in while #27's bullet was being lengthened, and it fits.

#### Plus one item of #32, and why it is here

**`RecoveryOutcome`'s "safe to call twice" list credited Core's lock release**
— #32's third item, #21's class rather than #6's. Item 5 removed Bitcoin Core
and every coin lock the app took; `abort.Run`'s own doc names two reasons and
the screen now says two. Taken here because it is a known-false sentence three
paragraphs from a line this slice was rewriting and it costs no third package;
#32's own slice is `internal/journal` plus `internal/abort`, where this would
have made three. **Found by rendering the screens and reading them**, not by
the grep, which was looking for asserted causes. #32's items 1 and 2 are
untouched and remain open.

#### One more, found by the audit — #27's claim in the sibling function

`RecoveryOutcome`'s **clean** path said *n* channels *"are still pending on the
other side"*, off `rep.Abandoned`, which establishes only that this node
abandoned them. **#27's own sweep missed it because it grepped the wording
`failureLine` used.** Fixed here on #24's precedent — the extra sites of a claim
belong with the issue that names it — because otherwise #27 closes with its own
sentence standing sixty lines away on the same screen. It shows the mechanism
now, and clock B stays in blocks.

#### The sweep

- `cmd/winthistle`'s package doc and its `recover` **usage line** already read
  *"the runs the journal never saw finish"*, and **`README.md:472`** already
  says *"which includes one being armed in another terminal right now"*. #24
  hedged all three and they still agree with this slice's wording.
- `internal/peers`' and `doctor.anyOurs`' *"may be an earlier run of this tool
  that did not finish"* are about a **pending channel read from LND**, hedged
  with *"may"*, and sound.
- **`docs/design.html` does not carry any of these claims and did not move.**
  Its state table already hedges `aborting`/`aborted` as *"in progress, was
  interrupted partway, or finished"* and uses the present tense for `arming`
  and `signing`, so the page is what the code was brought up to — as it has
  been every slice — and it carries no signer-declined copy at all, its
  `declin` hits being LND's own. Nothing was republished.
- `CLAUDE.md`: #27 is struck off the numbered list, #30's six-item list is
  replaced by the three decisions and what each rejected, and #32's item 3 is
  struck with the reason it was taken here.

#### The audit

Run read-only after this slice's copy was written, and asked two questions with
an enumeration method rather than a hunch. **Six flagged sites out of 127
operator-reaching copy sites** across `internal/prose` and
`internal/settle/report.go` — the **fifth sweep in a row** to find what the
previous one's vocabulary could not see. #23's found four, #29's six, #31's
three. **Every finding was verified in source before being acted on**, and
nothing here says the set is exhausted.

Where the six went:

- **One is fixed above** — #27's claim in the sibling function.
- **One is #26**, with two corrections filed as a comment. The *"false by
  default for a channel point missing from the map"* route **does not exist**:
  that branch requires `s.Open`, and `open` and `active` are filled in one loop
  over the same channels. And `Active == false` has **three** causes, not two —
  LND computes it as `peerOnline && link.EligibleToForward()`, so it is false
  while our own node is bringing links up, and the copy names the only one of the
  three that is about the peer.
- **One is the screen half of #32's first item**, filed there because it pairs
  with that item's write: *"%d shims already gone … It is not a failure"* names
  two causes for `AlreadyGone` and observed neither, and the third —
  documented at `journal/recover.go:183-192` — is a channel that is **actually
  pending**, with clock B running.
- **Three are a new class, filed as #33**: a figure that is LND's own default,
  handed over as the peer's behaviour with no attribution — the eleven-minute
  reservation and the 2016-block horizon, twice. `settle`'s `horizonNote` says
  *"it binds a peer running stock LND and nobody else"* about the same number
  because #20 made it. **A decision rather than a typo**, and the style rule
  pushes the other way here: recovery copy must name clock B in blocks.

**Its second question came back clean for the third sweep running** — 373
assertion points across 29 test files, none keying on a copy string, check name
or map key the build no longer emits. Worth recording that the sweep's own first
pass missed 160 string literals sitting inside table-driven blocks *away from*
their `Contains` call; that is the enumeration trap for the next audit's prompt.

#### Verification

- `go test -count=1 -p 1 -v ./...` — all sixteen packages green, **exactly two
  skips**, both the `WINTHISTLE_SLOW` wall-clock tests. `internal/settle` at
  118 s is the whole of the wall time.
- **The new controls are node-free.** `prose.Recovery` takes a `*journal.Run`
  and returns a string, so each is a run built in the state under test and
  rendered.
- **Every one was verified to fail against the old copy** — copy the file
  aside, restore the sentence, run the one test, restore. Six subtests on
  decision 1, four assertions on #27, both `signerNote` branches, and the Core
  clause.
- Assertions are keyed on the **shape** of each old claim rather than on a
  word, because the replacements still contain most of the words.


<a id="pr-35"></a>

## #35 — The journal stops naming a cause for an absent shim

Merged 2026-08-27 from `the-journal-stops-naming-a-cause-for-an-absent-shim-32` into `main`.


Closes #32 — items 1 and 2, plus the screen half of item 1 filed in the issue's comment. Item 3 landed in the #27 + #30 slice.

Item 1 is #6's rule for the ninth time: **do not assert a cause the program cannot know.** Every earlier instance was copy. This one is a **write** — it put the wrong cause on disk, over a verified row, where the next `Recover` read it back as fact and `AbortTarget` then emitted nothing for it ever again, while the peer held its side until clock B ran out. **No test covered the path.**

---

#### Decision 1 — what the journal may write for a shim that was already gone

`recordAbort` wrote `ChanCancelled` for every entry in `rep.Cancelled`, discarding `ShimOutcome.AlreadyGone` — the field whose own doc comment names this consumer, *"which matters when reading a journal after the fact"*. `ChanCancelled` is a cause with an actor in it and an ordering claim on top: *"its shim was cancelled before it ever reached pending."* `abort.CancelShim` established neither; `ErrNoShim` is matched off LND's own text and says only that LND holds no funding intent under that id.

**#24's question — *can the narrower question be answered honestly?* — answers yes here for the first time, and not because the caller has `AlreadyGone` in hand.** It is what the flag is paired with. The absence means opposite things on either side of `psbt_verify`, and the row is what says which side this is.

- **`ChanShimGone` is written only over `ChanShimRegistered`.** `psbt_verify` is what starts LND's funding flow after the inversion, so a channel this run never verified is one LND never created: nothing was made, nothing is left. That pair is terminal, which is what lets a benign run settle.
- **A `ChanVerified` row is left exactly as it stands.** There the same absence is what a channel that reached `chan_pending` looks like from here, because `CompleteReservation` consumed the intent on the way.

A value and not a column, so [the no-migrations rule](https://github.com/AusDavo/winthistle) permits it. `setChannelState` keeps its shape; the guard is a `channelStateIn` read inside the same transaction, with `MarkPending`'s outpoint guard as the precedent.

**Two answers rejected, each for what it cost.** Writing `ChanShimGone` unconditionally overwrites the one signal that separates the two cases — the journal has one state column and no migrations, so a row cannot carry both. Leaving *every* already-gone row alone leaves a benign run permanently unfinished with no exit through this tool.

##### The run state, one line up

`StateAborted` is gated on **`leftStanding()`** as well as `Report.Clean()`. `Clean()` establishes that no step failed, which is a fact about the calls and not about what they left. `leftStanding` re-reads the run and asks **`AbortTarget`** — the same function an abort acts on, deliberately, so *"nothing left behind"* cannot drift from *"nothing an abort would touch"*; a second list of states here is exactly how it would.

**The consequence is a permanent nag, and it is aimed at the one channel that warrants it.** A crashed-verified run stays in `aborting` and keeps appearing in `winthistle recover` forever, because nothing this program can read will ever say otherwise. The benign case clears through `ChanShimGone`. **Closing the window is not on offer and was not attempted** — `AbortTarget` has no client and the journal cannot map a pending channel point back to a pending chan id. Saying it honestly is the change.

**`internal/abort` did not change at all**, which was the stated test of whether this landed in the right place: the distinction was already in `ShimOutcome`.

#### Decision 2 — what the screen may say

`RecoveryOutcome` called every already-gone shim *"not a failure"* and named two causes for it, **neither observed**. The third is the one where it is a channel the operator must go and look at.

`alreadyGoneSplit` keys the report's shims on the run's own rows. `r` is the load taken **before** the abort ran — both callers already do that — so they are the states the abort found. **A shim whose channel the run does not know falls to the standing side**: the zero `ChannelState` is not `ChanShimRegistered`, and an unknown must not be the thing that gets waved away.

*"Nothing of this run is left on this node"* now prints only when nothing is. The standing paragraph says what was read, why an absence there is what a `chan_pending` channel looks like, and that **this build cannot abandon it** — the journal never recorded the outpoint. It hands over `lncli pendingchannels` and `lncli abandonchannel`, and names the peer's **full pubkey** rather than `shortKey`'s sixteen characters, because that is the string matched against `remote_node_pub`. It says outright that the run will keep appearing.

The 2016 blocks are named **unattributed on purpose**, matching the paragraph sixty lines above on the same screen. **#33 now has a fourth site**, reported there; one screen saying the same number two ways would be worse than either way said once.

#### Decision 3 — `MarkSigning`, and it changed no emitted copy

The #27 + #30 slice had already taken the renderer's over-claim out, so `prose.stateMeans` says *"That state is written before the wallet is asked for anything"* and is right. What was left was **the constant's own doc**, silent about the ordering where `StatePublishing`'s doc declares it — a gap against the package's own convention, on the one state whose neighbours all keep it.

So *"is out with the signing wallet"* does not claim the ask and does not deny it, and the **silence** is the defect. The doc says both halves now: the wallet holds the transaction because `SigningWallet.Built` is where it came from at step 4, and the request to sign is the next statement, with `FileWallet.Signed`'s `os.Remove` running before the prompt is even printed. **`internal/run` did not change.**

---

#### Two more, found by rendering the screens and reading them

Neither by any grep.

- **`recoveryPlan` printed `"An abort of this run would:"` with no bullets under it** for a run with nothing left, two lines under `Recovery`'s own *"nothing of this run is still standing"*. A heading with nothing under it reads as a rendering fault, and `ChanShimGone` makes that shape ordinary rather than rare.
- **`stateMeans` had no arm for `StateAborted` at all**, so it fell to the default's bare restatement of the value. It says what the state now establishes.

#### And one README sentence this slice's own node test falsified

`README.md` said *"`shim_cancel` still works after a successful `psbt_verify`"*. It does not, once the channel goes on to reach `chan_pending`: `skip_finalize` completes LND's funding flow, `CompleteReservation` consumes the intent, and the cancel comes back with nothing to cancel — which is exactly what the new regtest control measures. The paragraph is about blowing clock A, where nothing got that far, so the reassurance survives with its scope named.

#### Verification

- **`TestACrashBetweenVerifyAndTheReceiptIsNotRecordedAsACancelledShim` is the deliverable and it is node-backed.** It produces the crashed-process shape for real — verify with `skip_finalize`, never call `MarkPending`, read the receipt only so the *test* knows an outpoint the journal never learned — and asserts the channel is **still pending in LND after the recovery**, which is the cost the old row denied. It abandons out of band on cleanup.
- Two node-free journal controls cover both sides of the guard; four `internal/prose` controls cover the screen. **All seven were proved to fail against the old code**, on 29 assertions between them.
- `channelBreakdown` carries six channel states, `halfAborted` builds all six, and `RecoveryOutcome`'s widest already-gone shape is in the pane test.
- Full `go test ./... -p 1 -count=1 -v`: green, **exactly the two `WINTHISTLE_SLOW` skips**.
- **`docs/design.html` does not carry any of these claims and did not move.** Its state table already hedges `aborting`/`aborted` as *"an abort is in progress, was interrupted partway, or finished"*, and `StateAborted`'s meaning got stronger rather than different.

---

#### The audit found a hole in this slice's own fix, and it is fixed here

**`ChanShimGone` rested on a row that was written after the call it names.** The state is terminal — it says LND created no channel and there is nothing to abandon — and it rests on the row reading `shim_registered`, taken to establish that no `psbt_verify` happened. **The row established something weaker: that no `verified` row was *written*.**

`MarkVerified` was **the one state write in the batch path made after the call it names**. A process that died between `FundingStateStep` returning and `MarkVerified` left `shim_registered` for a channel whose funding flow LND had completed — which goes on to reach `chan_pending` on its own, with the peer holding its side. **That is this issue's item 1 one write earlier, on the branch the fix had just added.**

**Not only a crash.** `MarkVerified` takes `ctx`, so a cancelled context fails it *deterministically* while the RPC has already landed — Ctrl-C during a multi-channel verify is exactly that, and `recoverRun` runs the abort on a context that deliberately survives the cancel. Nothing else closed the gap: `Verify` does not retry, `arm.Receipts`' `PendingChannels` fallback is never reached because `armWindow` returns on `Verify`'s error, `AbortTarget` re-reads nothing from LND, and `CancelShim` consults nothing but its own answer.

**The write moved ahead of the call** — `RecordPinnedTxID`'s own stated discipline twenty lines up: *"Before, not after, and that is the same discipline MarkPublishing follows for the same reason."* The row can now only be wrong in the safe direction: a verify that never happened, or was refused, leaves a channel journalled as verified whose shim LND still holds and which cancels normally. The reverse leaves a pending channel journalled as one that never started.

`TestTheVerifyRowIsWrittenBeforeTheCallItNames` refuses the first verify at the stub and asserts the row already says verified, which is what a crash between the two statements looks like from the journal's side. **Proved to fail against the old ordering.**

#### Three more the audit found, filed rather than fixed

Denominator: **15 write functions in `internal/journal`, 12 production call sites and 83 test ones**, plus every `State`, `ChannelState` and `SignerState` constant checked against its writer.

- **#37 · `SignerSigned` is journalled for a file nothing checked for signatures** — **#25's mechanism with the polarity reversed**, a false success where #25 was a false failure. `combine.Parse` checks magic bytes and returns; `RecordSigner` upserts over the true `SignerAwaiting` row; `combine.Accept`, which refuses an unsigned packet with `ErrNoSignatures`, runs *after* `sign()` returned. The `.txn` route is guarded and the `.psbt` route is not, so the same operator mistake journals differently depending on what their wallet saved.
- **#38 · two docs name a stronger observation than their caller made** — `ChanPending`'s *"chan_pending arrived"* across six sites, where `receiptFor`'s fallback establishes the same fact by asking `PendingChannels` (**the safety conclusion is sound and was re-verified at v0.21.2-beta**; only the word is wrong), and `RecordFinalizedTx`'s *"the signed transaction"*, where a txid check cannot establish signing because witnesses do not move the txid — I-3's own premise.
- **#36 · `abort.Run`'s doc still cites a Core lock release** as what an early return would leave behind. #21's class, already on record in this issue's comment, filed separately so it does not close with #32.

**One lead is deliberately not filed**, on the rule that an agent's report is evidence rather than a finding: `arm.go:437` discards the pending channel id when `psbt_fund`'s `Recv` fails *after* `cli.OpenChannel` returned. Whether LND has registered the shim by then is unanswered, and answering it means reading `rpcserver.OpenChannel` at v0.21.2-beta.

#### The stale-assertion half came back clean, four sweeps running

**~303 assertion points across all 50 `_test.go` files**, with 1,268 literals machine-checked against a concatenation-merged blob of every non-test file and **410 survivors adjudicated by hand**. No positive assertion and no lookup key resolves to anything the build no longer emits.

Two things worth keeping for the next one. **Merge the concatenation before matching**: fourteen literals match production only after joining `"…" + "…"`, and a naive grep calls every one of them stale. And **match against string literals with comments stripped**, to catch an assertion that matches only a doc comment — zero found, which is the answer worth having.

The audit corrected itself on one point, recorded because the shape matters: `internal/settle`'s `paneCases()` lookups do **not** nil-panic on a stale key — `report.go:47` returns `"Nothing to settle."` for a nil `*Result`, so the positive assertions there would fail loudly but the negative `gone` guards would pass over it in silence. The keys are live; the shape is not self-protecting. And **`cmd/winthistle` has no test files at all**, so the copy #24 fixed there is asserted by nothing.

🤖 Generated with [Claude Code](https://claude.com/claude-code)


<a id="pr-44"></a>

## #44 — The signed row is written after the check that establishes it (#37)

Merged 2026-08-27 from `the-signed-row-is-written-after-the-check-that-establishes-it-37` into `main`.


Closes #37.

**#25's mechanism with the polarity reversed, and the second of the three instances of the asserted-cause rule that persisted a wrong claim to disk.** #25 wrote a false *failure* over a true row; this wrote a false **success**, and `prose.signerNote` read it back on the recovery screen as *"1 signed"* for a run where nothing was.

`run.sign` wrote `journal.SignerSigned` the moment `SigningWallet.Signed` returned bytes. On the `.psbt` branch the only thing that had looked at those bytes was `combine.Parse` — five magic bytes in, bytes out, **a sniffer and not a parser, as its own doc says**. `RecordSigner` upserts on `(run_id, label)`, so the write did not add a row: it replaced the truthful `SignerAwaiting` one written thirty lines above. And the two step-7 encodings disagreed — the `.txn` route is guarded by `SignedFromTX`'s witness count, the `.psbt` route was not — so one operator mistake journalled two ways depending on what their wallet saved.

#### Decision 1 · the write moves to after the check that establishes it

`armWindow`, one statement after `combine.Accept`, which executes every input's witness against its own script. Shape (a) of the three the issue named.

**#32's ordering rule with the mechanics reversed and the reason unchanged: a row may only claim what has been established when it is written.** `MarkVerified` moved *ahead* of its call in PR #35 because that row is read to mean *"the call may have landed, go and look"*, so its safe direction is early. This one is read to mean *"it did happen"*, so its safe direction is late. A crash in the gap now leaves `SignerAwaiting` — **one more case falling under #25's own reading of that state**, the wallet asked with nothing usable back.

Rejected, and recorded in the commit:

- **Weakening `SignerSigned`'s doc** to *"a file came back that decodes as a PSBT"* (#32 item 2's shape). It leaves the screen still printing *"1 signed"* for an unsigned run, so it costs the operator nothing less — and *"signed"* is the value's own name, which is #22's rule at the sentinel level.
- **Writing nothing on success either.** Wrong in the other direction: a refused publish would render as *"1 still awaited"*, sending the operator back to a wallet that did its job.
- **Checking for signatures inside `sign()`.** A second, weaker authority on one question — #13's *"two guards do not make a validator"*. `combine.Accept` stays the only one.
- **A new `SignerState`**, not considered: #25 rejected one and the reason holds.

**`internal/combine` did not change at all**, which was the stated test of whether this landed in the right place.

**The printed line carried the same claim one output earlier**, and it is in scope for #24's reason — the extra sites of a claim belong with the issue that names it. `"signed (12s elapsed)"` is now `"a file came back after 12s; nothing has checked it for signatures yet."`, and the honest version was already printed by `armWindow`, once the check has run, as *"signed and checked: … Every witness executed against its own script."*

#### Decision 2 · the screen keeps saying `%d signed`, and the rejection is recorded

**No emitted copy changed.** The rule it turned on is the one the issue asked for: *does the operator's next move change if the row is the false one?* It does not. A false row belongs to a run that stopped at `Accept`'s refusal, which named the file and the missing signatures on the terminal at the time, and nothing on the recovery path consults a signer row — only `prose.signerNote` reads them at all. **`declined` was different in kind: it pointed at a signing device, which is a move an operator makes.**

**And the conditionality runs the other way from #31's.** Every `declined` row this build will render was written by the frame #25 removed, so that paragraph fires only where it is needed. A `signed` row is right whenever the run got past `Accept` and stopped afterwards — a refused publish, `--stop-before-publish`, Ctrl-C — so a hedge here would teach the operator to distrust the one signer signal that is right, on the highest-stakes screen in the product. **That is #21's conditionality mistake with the arms swapped.**

Conditioning it on the build was not available: the journal has no version column and no migration table, and the narrower key that suggests itself — this value on a run still in `StateSigning` — admits true rows, because a batch armed with `--stop-before-publish` reaches the screen in exactly that shape.

So the change is two comments, and a control that **asserts an absence and says in its own comment that it pins a rejection rather than a behaviour**. `"%d still awaited"` gained a third true reading from decision 1 and its comment now names all three.

#### Controls

- **`TestASuccessfulSigningStepIsNotRecordedAsSigned`** — node-free by #25's route (`sign` uses only `d.Out`, `d.Journal` and `d.Signing`, so `Deps.LND` stays nil). The stub hands back the packet it was given, which is the operator mistake in its most ordinary form. **Proved to fail against the old code on three assertions**: the journal row, the printed line, and the absence of the honest sentence.
- **The positive control already existed and is node-backed.** `TestTheFilePathDrivesTheWholeSequence` asserts the row reads `signed` after a full live run, once per step-7 encoding. Both still pass, which is what proves the moved write still lands.
- `internal/prose`'s new `signed` subtest keys its negative assertion on the *shape* of the hedge (`a journal an earlier build wrote`), not on a bare word.

#### Verification

`go test ./... -count=1 -v -p 1`: every package `ok`, **exactly two skips**, both the `WINTHISTLE_SLOW` wall-clock tests. `make -C regtest info` showed `peers=3` before any of it was believed.

**The step-7 transcript was rendered and read**, which is how the one thing this slice got wrong the first time was found: the new line measured 74 columns in a test, where the elapsed time is two characters — and step 7's duration belongs to the operator, so `1h23m45s` is the ordinary case and that is 80, two over `prose.PaneWidth`. Reordered rather than wrapped, and it is 75 at the widest now. Its own commit.

**Noted and not fixed:** the line below it, `"signed and checked: … Every witness executed against its own script."`, is 108 columns and predates this slice. It is a width, not a claim — it says exactly what `combine.Accept` established — so it is not #37's to move.

#### Docs

**`docs/design.html` does not carry this claim and did not move**, and neither does `README.md`: neither says anything about what a signer row means. Checked, so **no republish**. The page has been unmoved since #13, and five slices have now checked it.

`CLAUDE.md` loses #37. **Nothing is written about the asserted-cause set being exhausted.**

#### The slice's own audit

**Seventh sweep in a row to find what the previous one's vocabulary could not see.** Denominator: **160 operator-reaching sites** across `internal/run` and `internal/arm` — the *printed* lines, which PR #35's journal-focused audit did not cover. Five findings, **each verified in source before filing** (the LND half of the sharpest one re-read at v0.21.2-beta rather than taken on report), filed as **#39–#43**:

- **#39 · `reportArmed` puts the commitment signature on the peer, and says the funds come back without this node.** The sharpest found outside a write: `commitSig` comes off *the peer's* `funding_signed` and *this node* stores it, and *"even if this node vanished"* is denied by the backup paragraph eighteen lines below it on the same screen. Nothing renders this screen and asserts on it — #24's finding in a third package.
- **#40 · `arm.Verify`'s doc still describes the ordering PR #35 reversed** — a hole at the other end of the write that slice moved.
- **#41 · `ErrPublishRefused` says the node declined**, on the branch that catches `context.Canceled` and a dropped socket — and it defeats `mayBePublic`'s deliberate *"refused or did not answer"* hedge by being interpolated into it.
- **#42 · a first-channel `Open` failure is reported as a journalling failure**, with the peer's own refusal discarded. **The only one with a functional cost.**
- **#43 · *"is still being written"* printed from an observation of zero bytes**, on a branch the build's own strace measurement says is reached by files that are *not* being written.

Two comments widen existing issues: **#38's denominator is at least seventeen, not six**, three of them printed; **#33 gains two `internal/run` sites**.

**The other half came back clean for a fifth sweep running.** No test asserts on a copy string, check name or map key the build no longer emits: 390 assertion call sites, 560 deduped literals, 105 assertion-position misses adjudicated one at a time, zero stale. It adds a fourth thing for the next sweep to keep — **walk upward to the enclosing field marker rather than grepping near the `Contains`**, which is what separates those 105 from 176 fixture-position literals. All three literal-keyed map lookups checked, all three keys live.

**And half of the standing unverified lead is read now, which does not settle it.** `rpcserver.OpenChannel` runs `newPsbtAssembler` before `server.OpenChannel`, and the reservation is created asynchronously inside the funding manager — so a client-side failure after `cli.OpenChannel` returned does not establish that no shim exists. Still unfiled, per the standing rule; #42 names it as the dependency its second half rests on. What remains unread is whether a cancelled stream tears the reservation down.


<a id="pr-46"></a>

## #46 — reportArmed names the custodian LND's own code names (#39)

Merged 2026-08-27 from `reportarmed-names-the-custodian-39` into `main`.


Closes #39.

The armed screen — the summary the operator reads with one call left — said the
**peer** had stored its commitment signature, and that a force-close would get
the funds back **even if this node vanished**. Both halves are wrong, and the
same function denied the second one eighteen lines later.

#### The LND half, re-read at v0.21.2-beta

`funding/manager.go`'s `funderProcessFundingSigned` takes `msg *lnwire.FundingSigned`
— **the peer's** message — parses `commitSig` from it at `:2805`
(`msg.CommitSig.ToSignature()`), and hands it to **this node's**
`resCtx.reservation.CompleteReservation(nil, commitSig)` at `:2813`, before
`chan_pending` is emitted at `:2897`. The responder's mirror,
`CompleteReservationSingle` (~`:2558`), is what stores the *initiator's*
signature — so the fix does not invert it the other way.

So this node stored the peer's signature, in this node's channel database. That
is what `CLAUDE.md`'s I-1 citation says, what `internal/arm/arm.go:756-757` says
on the fallback route, and what `README.md:165` and `docs/design.html:686` say.
**The screen never matched a citation the rest of the repository already
carried.**

#### Decision 1 — name the custodian, delete the other clause

> All 3 channels reached chan_pending before anything was signed, so every one
> of them is already recoverable: **this node has stored the peer's commitment
> signature** against an outpoint in this transaction, and a force-close would
> get the funds back. Nothing is in any mempool.

One clause, copying `arm.go:756-757`, which is the model rather than the thing
to edit — **`internal/arm` did not change at all**, which was the test of
whether the slice stayed on its screen.

**Saying nothing about custody was the other available answer and was
rejected.** The backup paragraph depends on the custody fact, so a screen that
never states it leaves *"already recoverable"* as a bare assertion and makes the
paragraph below it a non-sequitur.

*"even if this node vanished"* is **deleted rather than rescoped**. It is the
clause an operator acts on, and it says the backup is optional at the one moment
the copy is telling them to store it off the box.

**I-1 did not move.** *"a force-close would get the funds back"* stands, and it
is still executed against Bitcoin Core by
`TestTheReceiptIsProvedByForceClosingTheChannel`.

#### Decision 2 — the two paragraphs stay apart, and decision 1 dissolved the question

The backup paragraph is **conditional** — it renders only when the export
carried single-channel backups — so the old screen made an unhedged custody
claim with its correction on a branch, which is #21's mistake in its original
form. **The answer is not to merge them but to make the unconditional paragraph
true standing alone**, which decision 1 does. Merging would have put the custody
clause back on the branch.

**Whether that branch can be false is now stated rather than assumed**, in
`reportArmed`'s doc: `armWindow` guards `MultiChanBackup` and **that guard does
not reach the singles this screen keys on** — LND's `createBackupSnapshot` packs
the multi from the same slice, and `Multi.PackToWriter` writes a version byte
and a count even for zero backups, so an empty export still yields a non-empty
multi. In practice the branch is true: every channel here reached `chan_pending`,
which is `CompleteReservation` having marked it pending in the channel database,
and `FetchStaticChanBackups` reads all open channels *including pending open*.
In practice is not the same as checked.

That commit changes **no emitted copy**.

#### The control is the fix

`reportArmed` was printed into two live transcripts and asserted on by neither —
#24's `checkJournal` finding in a third package. `internal/run/report_internal_test.go`
is **node-free**: every field `reportArmed` reads is exported or in this package,
so an `arm.Armed` and a `*prepared` can be built by hand.

- It keys on the **shape** of the old claim (`peer has stored its commitment`,
  `even if this node\b`), not on a bare word, because the true sentence contains
  most of the false one's words.
- It renders **both sides of the conditional**, so the standing-alone property
  decision 2 rests on is asserted rather than argued.
- It **measures the pane** at n = 1, 3 and 12, which nothing had done for this
  screen.

**Five assertions fail against the old copy** — verified by restoring the defect,
running the one test, and restoring the file.

#### What did not move, and was checked rather than assumed

- **`docs/design.html` is right and was not republished.** `:686` (*"Every
  channel therefore holds its peer's commitment signature … our own node is lost
  and the exported backup plus the peer's data-loss protection recovers them"*),
  `:726` and `:748` all say what LND does. This was the first slice where the
  page was a live candidate to be wrong; it is not.
- **`README.md` is right.** `:165`, `:209` and `:345` all attribute correctly.
- **`internal/arm` did not change.** **`mayBePublic` was read and not edited** —
  its custody claims (*"every peer holds a commitment signature against the [old
  outpoints]"*, *"every channel is recoverable and the batch simply needs to
  confirm"*) are sound; its sentinel text is #41's.
- **The `chan_pending` wording is #38's** and was left alone.
- The single site of *"even if this node vanished"* in the tree was this one.

#### Verification

`go test ./... -p 1 -v -count=1` — green, **exactly two skips**
(`TestWhoOwnsTheTenMinuteClock` and
`TestOnePeersWindowExpiringLeavesTheRestOfTheBatchArmed`, both `WINTHISTLE_SLOW`).
Harness live at `peers=3`. The screen was rendered and read as one screen, at
n = 2 off the live cold-probe transcript and at n = 1/3/12 in the pane test.


#### The audit

Pointed **one layer out** this time: not the printed lines, which PR #36's audit
turned, but **every place this build describes a mechanism inside LND, Bitcoin
Core or a peer's node**, checked against source at v0.21.2-beta, with **doc
comments as the unturned ground**. Denominator: **8,964 comment lines across
105+ files**, ~220 sites making checkable external claims, **153 verified
correct**, ~50 unverifiable because Core is not vendored.

**Five re-read in source before filing** — #47, #48, #49, #50, #51 — plus #52
batching six smaller citations and three comments stale against this build. And
#45 from this slice's own screen.

- **#47** · a peer's `minimum_depth` **is** readable at v0.21.2-beta, via
  `PendingChannels.confirmations_until_active`, and five printed sentences say it
  is not. `PendingChannels` is already registered, already called and already in
  the baked macaroon. **It changes #28's answer**, and #28 now carries a comment
  saying so.
- **#48** · `policy.MinTimeLockDelta` is 18, `routing.MinCLTVDelta` is 24. **The
  only finding with a direct operator cost**, and `TestPolicyValidateMatchesLNDsBounds`
  keys both arms on our own constant, so it can only pass.
- **#49** · `INVALID_PARAMETER` is described as the one refusal that is *not*
  about the channel, and it is the one that is entirely about the channel — at
  the site of issue #6's own justification.
- **#50** · `handleFundingOpen` does not exist. **This is `handleFundingSigned`
  again**, fundee side, eight sites, two printed.
- **#51** · `internal/reserve`'s doc reasons from `psbt_finalize`, which the
  inversion removed, and its conclusion rests on it — with a possible cost
  **inside clock A**.

**The dominant cause is the version bump**, and it goes in the bump procedure:
re-citing after v0.19.3-beta caught roughly thirty line numbers and missed a new
proto field, two constants, a renamed symbol and a dependency's formatting
change. **Line numbers move loudly; values do not.**

**The stale-assertion half came back clean for the sixth sweep running.** 2,511
comment-free, concatenation-folded literals across 56 non-test files; 260
assertion call sites plus 43 comparison-position literals; **100 misses
adjudicated by hand, zero stale**. All four literal-keyed map lookups checked,
every key live. Trap (iv) clean for the third sweep: 16 assertion-position
literals match production only inside comments, and every one is a documented
negative assertion, a runtime-formatted value or a fixture.


<a id="pr-53"></a>

## #53 — make check-citations, and the version rot it finds

Merged 2026-08-27 from `citations-and-constants` into `main`.


Closes #48. Closes #50. Closes #52.

Four of the last seven issues were version rot — a comment naming an LND
symbol that no longer exists, or a constant transcribed from LND that no
longer matches. Both are mechanically detectable, so this turns the most
productive audit class into a build failure and fixes everything the two
new checks flag.

#### `make check-citations`

**`TestEveryCitedLNDSymbolResolves`** compares three sets: every identifier
this module writes as code, every name the pinned LND and btcsuite modules
declare in the module cache, and every identifier-shaped word in a comment
or a printed string. A word in the third and neither of the first two is a
wrong citation. It reads the module cache rather than importing, because
most of what this repo cites is unexported and `.proto` field names are not
Go symbols at all. String literals are scanned as well as comments, since
two of #50's eight sites were sentences an operator reads.

**`TestTranscribedConstantsMatchLND`** reads the pinned source with
`go/types` and compares fourteen pairs. `routing` was cheap to load this
way (~1 s for six packages) and half the table is unexported —
`lnwallet.minRequiredConfs` among them — so export data would not have
done.

`make check` runs both before `test`: three seconds of citation failure
beats three minutes of suite before the same failure.

#### What they found

**#48, and it is worse than filed.** `policy.MinTimeLockDelta` was 18;
`routing.MinCLTVDelta` is 24. A delta of 18–23 passed `Validate()`, armed,
published, and was then refused by `validateChannelPolicyTimeLockDelta`
(`rpcserver.go:7504`) as a gRPC error for the *whole* `UpdateChannelPolicy`
call — leaving every channel in the batch at 1000 msat / 1 ppm.

**A second constant, not filed anywhere.** `settle.MinDepth` was 3;
`lnwallet.minRequiredConfs` is 1. LND moved the scaling into
`lnwallet.ScaleNumConfs`, which clamps into `[1, 6]`. Verified on the
harness, not only in source:
`TestTheSettlementPassPoliciesAChannelAndLearnsItsDepth` now logs
`opened at 1 confirmations (as predicted)` where it used to log the
mismatch and pass anyway.

**#50.** `handleFundingOpen` does not exist at any version this build has
pinned. It is `fundeeProcessOpenChannel`, `funding/manager.go:1438`. Eight
sites in code plus one in `docs/design.html`. Re-read while renaming: every
limit check does run before `InitChannelReservation` (:1490–:1690 against
:1705), and `accept_channel` goes out at :1975, so both printed ordering
claims hold and `--probe` is unchanged. One claim did *not* hold —
`env.go` attributed the initiator's "channels cannot be created before the
wallet is fully synced" to the fundee, which refuses with
"Synchronizing blockchain" instead.

**#52.** `btcutil.Amount.String()` *re-adds* trailing zeros — `Format`
pads with `%.8f` whenever the trimmed form still has a decimal point — so a
peer sends `0.00020000 BTC`, not `0.0002 BTC`. Three comments said the
opposite and no fixture used the real form; measured, and both shapes are
now pinned. `already connected to peer` is
`errPeerAlreadyConnected.Error()` at `server.go:166`, not an `fmt.Errorf`
in `rpcserver.go`; text matching stays because the type does not cross
gRPC. `no funding intent found for pendingChannelID(...)` is
`PsbtFundingVerify`'s, `lnwallet/wallet.go:757`, not `chanfunding`'s. The
tag is `integration`, not `dev` — `lncfg/dev.go` is built when
`integration` is *absent*, and LND has a separate `dev` tag driving the log
level, which is the trap. The force-close broadcast ordering was backwards:
`ForceCloseContract` broadcasts and *then* `rpcserver` sends
`ClosePending`, so `awaitMempool` is justified by `ErrDoubleSpend` and
`ErrMempoolFee` being swallowed at `channel_arbitrator.go:1170` rather than
by the broadcast being late. Plus `PublishTransaction` is pinned at 1 call
site (one comment said 2, not two), and there has been no pre-flight of
ours since item 5.

**And three tests that could only pass.**
`TestPolicyValidateMatchesLNDsBounds` keyed both arms on our own constant,
so it asserted `Validate` agreed with itself — deleted, since
`policy_test.go` already covers the behaviour. `config_test.go`'s literal
pin on `18` deleted for the same reason. `TestExpectedDepthReproducesLNDs
DefaultPolicy` had three of its seven cases sitting on the clamp.

**Three names that resolved nowhere**, all fixed in place with no issue: a
doc header naming a test whose name lost a suffix, a comment naming
`signedWith` where the field is `signed`, and a garbled `theyThey`.

#### Notes

- `docs/design.html` moved and was republished to the same URL.
- The exception list holds words that resolve nowhere and are not defects:
  Core RPC arguments, Sparrow's Java classes, prose, and symbols that
  genuinely existed and genuinely do not now. No name that could plausibly
  be reintroduced is on it — an entry is a hole in the check for exactly
  that name, which is why `handleFundingOpen` appears nowhere in the file.
- `make check-citations` and full `make test` green; exactly two
  `WINTHISTLE_SLOW` skips.

🤖 Generated with [Claude Code](https://claude.com/claude-code)


<a id="pr-54"></a>

## #54 — Copy that names its source, and sentinels that name their branch

Merged 2026-08-27 from `copy-that-names-its-source` into `main`.


Closes #26. Closes #33. Closes #36. Closes #38. Closes #40. Closes #41.
Closes #43. Closes #45. Closes #49.

Nine issues, one answer each, no decision records. Stacked on #53 — the
base retargets to `main` when that merges.

#### The mechanisms, re-read in LND at v0.21.2-beta

**#49** was the sharpest, because that sentence is the whole justification
recorded for issue #6's fix. `INVALID_PARAMETER` is not LND checking the
figures ahead of the channel; both emitting sites are `updateEdge` failing
(`routing/localchans/manager.go:122`, `:273`) and `updateEdge` measures the
HTLC bounds against the channel's **negotiated** `LocalChanCfg`
(`:448-467`). It is entirely about the channel. `Terminal()` survives on
the better argument — negotiated bounds are fixed when the channel opens.
The figures LND *does* check ahead of any channel produce no
`failed_updates` entry at all: they fail the whole call as a gRPC error,
which is why `policy.Validate` refuses them before a batch is armed.
Whether a `FetchChannel` failure is reachable there is not settled, and the
comment says so.

**#26** · `Active` is `peerOnline && link.EligibleToForward()`, so a false
one says *this node's link is not eligible to forward* — three causes, not
one. The column reads `open, not forwarding` (twenty characters, which is
the column's width; `link not forwarding` overflowed it and shifted the
third column on that row alone) and a new `linkNote` says once per screen
what the field is.

**#38** · "chan_pending arrived" is one of two routes to that row —
`receiptFor` falls back to `PendingChannels`, and presence there is
`CompleteReservation` having run, which is the same fact. Said once on
`ChanPending` itself. Plus `RecordFinalizedTx`, which did not store "the
signed transaction": a txid check cannot establish signing, since witnesses
do not move a txid.

**#36** · `abort.Run`'s doc still credited a Core lock release. **#40** ·
`arm.Verify`'s doc still described the ordering PR #35 reversed; deleted
rather than synced, since `MarkVerified` owns that write.

#### The sentinels and the states

**#41** · `ErrPublishRefused` fired on `codes.Unavailable`, a dead
transport and a cancelled context, and its text asserted the one thing the
operator must not conclude — `run/report.go:277` interpolated
"lnd refused to publish" straight into its own "refused or did not answer"
hedge. It is `ErrPublishUnanswered` now; `ErrPublishRefused` keeps its name
for `publish_error`, which is a refusal LND stated. The table test carries
the sentinel per case.

**#43** · `readWhole` returns `fileEmpty` / `fileGrew` / `fileWhole`, and
the screen names which. By the time the line prints, empty-and-staying-empty
is the likelier state: issue #10 measured Sparrow's inter-chunk gap at
0.05–0.3 ms and this waits two consecutive polls.

#### The copy

**#33** · `prose.StockLNDNote` is `settle`'s `horizonNote` sentence
factored out — *"it binds a peer running stock LND and nobody else"* —
called **once per screen** with flags for which figures that screen
printed, across `prose`, `peers`, `run` and the `--probe` usage line. The
block counts stay untouched.

**#45** · Option 2. The shorthand stays; the counter-instruction goes
beside it, once on the armed screen and once at step 6. LND does not refuse
a force-close on a pending channel — measured while building #16's test —
so closing one before the transaction confirms destroys the channel and
recovers nothing.

#### Notes

- Assertions went into the render tests that already exist
  (`report_internal_test.go`, `recovery_test.go`, `settle_test.go`,
  `report_test.go`, `wallet_internal_test.go`) rather than nine new
  controls. `paneCases()` gained an open-but-not-forwarding row so
  `linkNote` is measured.
- `docs/design.html` carried #49's sentence; republished to the same URL.
- **`make check-citations` from #53 caught one of this PR's own new
  fixtures** — `mSAT`, LND's spelling of the unit in its error text — which
  is the first thing it found that was not pre-existing.
- **Harness note, not a code failure**:
  `TestTheFundingHorizonIsReachedByMining` mines 2016 blocks per run, so a
  long-lived cluster eventually exceeds `bitcoind.DefaultTimeout` (2 min)
  on `generatetoaddress`. It failed at height ~46,700 and passed in 53 s
  after `make harness`. Worth knowing before believing a red.
- Full `make test` green on a fresh cluster; exactly two `WINTHISTLE_SLOW`
  skips.

🤖 Generated with [Claude Code](https://claude.com/claude-code)

#### Also in this PR

**`CLAUDE.md` goes from 1,761 lines to 573**, in one commit. What went is the
slice-by-slice narrative, the sweep counters, and the instruction not to call
the audit set exhausted — which is what made non-termination a goal. All of it
is in git history and in the PR bodies. What stays: the product and the
sequence, the four invariants with their citations untouched, the
rejected-approaches list, current state with the three pins and the bump
procedure, hygiene and style — plus one new section, the rules that bind future
work, one sentence each.

`HANDOFF.md` was checked and left alone: it holds hazards of this repository,
harness and machine, and carries none of the claims this slice changed.


<a id="pr-55"></a>

## #55 — A peer's minimum_depth is a reading, not a guess

Merged 2026-08-27 from `the-peers-own-minimum-depth` into `main`.


Closes #47. Closes #28.

**The decision: take the reading.** A peer's `minimum_depth` is readable at
v0.21.2-beta, and nine places in this build said it is not. The reading costs
no call site, no registry entry and no permission — `PendingChannels` is
already registered, already called on the batch path, already in the baked
macaroon — and it closes the gap `docs/design.html` already claimed step 9
closed. The copy had to change either way; taking the reading is the cheaper
half of the two.

**Where it goes: `settle.Tick`, off the `PendingChannels` call it already
makes.** Not the armed window, which was the other candidate. Two reasons. An
extra RPC there lands inside clock A, and the number changes nothing about
whether to publish — the operator publishes either way. In settlement it
replaces the guess the operator was actually looking at with the peer's own
figure, at the moment they are waiting on it.

**The mechanism, and the one constraint.** `confirmations_until_active` is two
different numbers either side of the first confirmation. While
`OpenChannel.ConfirmationHeight` is zero, `calcRemainingConfs` returns
`NumConfsRequired` verbatim — for an initiator, the `min_accept_depth` the peer
sent, floored at 1 where the peer sent 0. Once a confirmation height exists it
counts down to the target and shrinks every block. So `State.PeerDepth` is
taken once, only while `confirmation_height == 0`, and never revised: a late
reading is smaller than the peer's number and looks exactly like a peer that
asked for less. `TestTickReadsThePeersOwnMinimumDepthWhilePendingAndNeverAfter`
walks the countdown down through plausible values, because a wrong reading here
would not look wrong.

**Three figures now, and the report names which one it is showing.** The peer's
own ("peer asked for 3 confirmations"), the prediction ("expect ~6"), and the
depth observed from above ("opened at 3"). `depthNote` renders one paragraph per
kind the rows above actually carried — a batch can hold both, since a channel
whose transaction confirmed before the first poll has no reading and no second
chance at one.

**`depthLesson` now prints on a production run.** Its only row came from
`ObservedDepth`, which is read off `Confs`, and no production run has a `Chain`
— so "what this batch learned" taught a real operator nothing, on every batch
they have run. The peer's own figure needs no `Chain`.

**And one assertion nothing in this repository had ever made.**
`settle.ExpectedDepth` reimplements `lnwallet.ScaleNumConfs`. The regtest
control test now checks it against a live stock peer's own answer, arrived at
over the wire in `accept_channel` — one reimplementation, one real execution,
and a hard failure if they disagree. The observed depth stays as the third
check: an upper bound on the same number, never shallower.

**`Options.Chain` stays, with a changed job.** It is no longer the only route to
a peer's depth; it is the harness's independent cross-check on the reading LND
hands over. `CLAUDE.md`'s bullet says so now.

`docs/design.html:808` flips from *settled in source / No* to *proven on
regtest / Yes*, and is republished to the same URL in this PR — that is #28.
`HANDOFF.md`'s standing fact and `internal/methods`' two `Why` strings are
corrected in place.

`make check` green on a live harness (`peers=3`), including the settle regtest
pass.


<a id="pr-56"></a>

## #56 — The count at verify grows under the batch

Merged 2026-08-27 from `the-count-grows-under-the-batch` into `main`.


`internal/reserve`'s package doc reasoned from `psbt_finalize`, which the inversion deleted, and concluded that *"every verify in a batch sees the same pre-batch count."* Both halves are wrong in this build.

#### What was measured

A `skip_finalize` `psbt_verify` is what completes LND's funding flow, so `CompleteReservation` runs during `arm.Verify`, and its `SyncPending` (`lnwallet/wallet.go:2534`) writes the channel into the channel database *before* `chan_pending` is emitted. Nothing orders that against a later channel's `psbt_verify`.

`TestALaterVerifyCountsAnEarlierChannelInTheBatch` is the measurement #51 asked for, at *n* = 3, arranged to be deterministic rather than raced: channel 1's `chan_pending` is read before channels 2 and 3 are verified, so the count `CurrentNumAnchorChans` reads has definitely grown by then.

On a fresh harness node — under LND's 100,000-sat cap, so the figure has room to move:

```
pending opens before the first verify: 0
channel 1 to 02b6d5d5 is at chan_pending at 973b221c…:0
pending opens before the second verify: 1
the figure at verify moved from 10,000 sat to 20,000 sat while the batch
  was half-verified: Finding.AtVerify is the figure the FIRST verify uses,
  and channel 2's is larger
channel 2 was not in the database yet when channel 3 was verified, so
  back-to-back the loop wins the race on this harness
3 of 3 verified against a channel database that grew under them
```

So the conclusion is false, and the concrete form of it is the pre-flight's own number going stale inside clock A. The back-to-back margin is real and is only a latency measurement — not something to rest a pre-flight on.

#### What already covered it, and why that matters

`plan.ReserveTopUp` aims at the **larger** of the two figures, and `CheckReservedValue` credits an output paying into the node's own wallet — so a top-up inside the batch counts at every verify, including the last. The batch was never exposed.

But `ReserveTopUp`'s own comment justified that choice as an economy (*"since an output is being built either way"*) and named the smaller figure as *"what psbt_verify will actually demand"*. The safety margin existed by accident. It is now the stated reason, and `CLAUDE.md` says not to optimise it back down.

#### Copy

Three places asserted a cause the program cannot know — that the batch will verify:

- `reserve.Summary` and `reportShortAfter` (*"Nothing will refuse the batch"*) now say the **first** verify clears, and name the mid-batch refusal.
- `doctor`'s `ShortAfterBatch` warning likewise, and it now says that `winthistle run` pays this as a top-up output while `doctor` builds nothing.
- `reportRefused`'s aim-at-the-larger paragraph no longer explains itself with the deleted ordering.

`AtVerify` is the floor, `AfterBatch` is the ceiling, and a batch has to clear the ceiling. `Blocking()` is unchanged: nothing in the run path consults it, and the plan pays the shortfall either way.

One limit of the harness, stated in the test: alice accumulates channels across a suite run, and above ten announced anchor channels `RequiredReserve` is pinned at the cap and cannot move. The test asserts the figure moving only when the node is under the cap, and logs loudly when it is not.

`make check`: green, exactly two `WINTHISTLE_SLOW` skips.

Fixes #51


<a id="pr-58"></a>

## #58 — The armed window reports the refusal it observed

Merged 2026-08-27 from `the-refusal-the-run-observed` into `main`.


**Stacked on #56.** Base is `the-count-grows-under-the-batch`, because both slices edit `CLAUDE.md`'s known-gaps section. Merge #56 **without** `--delete-branch`, or retarget this to `main` first — deleting a base branch closes the child PR and a closed PR cannot be retargeted.

---

`arm.Open` hands back its `Streams` on the first channel's failure too, so that whatever shims exist can be released. On that path `Streams.All` is empty, `NewChannels()` is a zero-length slice, and `journal.Begin` refuses one outright. `armWindow` returned *that* complaint and never returned `err` at all.

What the operator used to see when their first peer refused:

```
The armed window failed: journalling the run: run 20260827-…: a batch with
no channels in it is not a batch
```

What they see now:

```
The armed window failed: opening the batch's funding streams (0 already
open, and cancellable): channel 1 of 2: waiting for psbt_fund from
02b6d5d5552db7db…: rpc error: code = Unknown desc = channel is too small,
the minimum channel size is: 20000 SAT
```

#### The fix, both halves

`Begin` refusing an empty batch is right; calling it with one is what was wrong. So it is called only when there is something to journal.

And when `Begin` genuinely must run — some streams did open — and fails, both causes matter and neither may be dropped: the refusal names the remedy, the journalling failure is why `winthistle recover` will not see the shims. `unjournalledStreams` returns both, wrapped so `errors.Is` finds either, and it prints the pending channel ids — because that is the last moment anything knows them, and a shim outlives the stream it came on.

`recoverRun`'s `ErrNoRun` comment asserted a cause from a row's absence: *"which means arm.Open never returned a stream."* `Begin` writes the run row and the channel rows in one transaction, so `ErrNoRun` is also what a successful `arm.Open` with a failed `Begin` looks like — the shape `arm.Open`'s own doc exists for. It now says what it knows, and points at where those ids are printed instead.

#### The dependency, measured

#42 named one unread question: whether a cancelled stream tears down the reservation LND may hold. It does not — `TestAShimSurvivesItsStreamBeingHungUp` in `internal/abort` opens a stream, hangs it up, waits two seconds and cancels the shim successfully. `rpcServer.OpenChannel`'s update loop returns on the first `updateStream.Send` failure with a bare `return err`; nothing there cancels the reservation.

But LND cleans up its own refusals. A peer's `lnwire.Error` reaches `Manager.handleErrorMsg` (`funding/manager.go:5301`) → `cancelReservationCtx` (`:5308`) → `ChannelReservation.Cancel`, whose handler deletes the intent (`lnwallet/wallet.go:1488`). So the `recv.Recv()` failure — the realistic first-channel refusal — leaves nothing behind, and the discarded id costs nothing there.

**What is left is the two branches after it**, where LND has not errored, the reservation is live and the hang-up is ours. Filed as #57 rather than fixed here: returning the id is easy, making it recoverable is a journal or abort-path decision, and the issue argues for cancelling inline. Recorded in the source at the discard site so nobody re-reads LND for it.

#### Tests

- `TestAFirstChannelRefusalIsReportedAsTheRefusal` — live, the previous test's batch inverted so the refused channel is first. Asserts LND's own wording survives and that neither `"a batch with no channels in it is not a batch"` nor `"journalling the run"` appears.
- `TestAJournalFailureNeverSwallowsTheRefusalItArrivedWith` — unit, on `unjournalledStreams`: both causes in the text, both surviving `errors.Is`, both ids named, and the no-refusal case still naming the open shims.
- `TestAShimSurvivesItsStreamBeingHungUp` — live, the measurement above. The two-second wait is load-bearing: without it an asynchronously-cleaned shim would still be in the map and the test would pass proving nothing.

`make check`: green, exactly two `WINTHISTLE_SLOW` skips.

Fixes #42


<a id="pr-59"></a>

## #59 — The shim with no handle is cancelled where it is made

Merged 2026-08-27 from `the-shim-with-no-handle-is-cancelled` into `main`.


Closes #57.

`arm.open` generated the pending channel id, called `OpenChannel`, and on failure hung the stream up and returned only an error. On the two branches after the `Recv` — `fund == nil`, and a `psbt_fund` naming no output — **LND has not errored**, so nothing on its side releases the reservation, and hanging up does not either: `TestAShimSurvivesItsStreamBeingHungUp` closes a stream, waits, and then cancels the shim successfully. The id went out of scope one return later, and it is the only handle there is.

#### What changed

`cancelUnreadable` in `internal/arm`. It hangs the stream up and *then* calls `abort.CancelShim` — that order, because it is the one that test measured; cancelling an intent out from under a live stream is not something anyone here has watched LND do.

**Nothing is journalled.** A stream that never opened has no funding address, no amount and no channel row to hang an id on, and `journal.Begin` takes `[]NewChannel` — so recording this would mean a new table for a shim with no channel behind it. There is no need: the process is alive, the id is in hand, and it is one call. The issue argued for this over the table and it still looks right.

**`ErrNoShim` is success** — LND registered no intent, or already dropped one, which is the end state this is aiming at.

**A cancel that genuinely fails is reported alongside LND's own complaint**, not in place of it, with the pending channel id in the sentence because that is the last moment anything knows it. It claims nothing about what LND still holds: the cancel failing is exactly the case where that is unknown.

**The `Recv` error path is untouched, on purpose.** LND cleans up its own refusals — `Manager.handleErrorMsg` (`funding/manager.go:5301`) → `cancelReservationCtx` (`:5308`) → `ChannelReservation.Cancel`, whose handler deletes the intent (`lnwallet/wallet.go:1488`) — and the commonest way to reach that branch is Ctrl-C, where a cancel on a dead context would print a sentence about a shim nobody needs to chase.

`internal/arm` now imports `internal/abort`. `abort` imports only `internal/lnd`, so that direction was the only one available.

#### Tests

`internal/arm/open_unreadable_test.go`, against a stub. These branches cannot be reached with a real LND — the subject is not whether LND sends unreadable updates, it is that the id gets used before it goes out of scope.

- both branches cancel, and cancel **the id they opened with** rather than merely cancelling something
- `ErrNoShim` produces no second sentence
- a failed cancel names both causes, the id, and the out-of-band route
- the `Recv` refusal path cancels **nothing**
- a 3-channel batch failing at channel 2 cancels exactly that stream's shim and still hands back the one that opened, for the caller to journal

`make check` green, exactly the two `WINTHISTLE_SLOW` skips, `peers=3` on the harness throughout.

#### `CLAUDE.md`

"Known gaps, with issues open" is now "Settled behaviour, with no issue left open" — **no issue is open on this repository.**

🤖 Generated with [Claude Code](https://claude.com/claude-code)


<a id="pr-60"></a>

## #60 — The wallet that signed the live batch is named

Merged 2026-08-27 from `the-wallet-that-signed-the-batch` into `main`.


Every record of the 2026-08-26 live batch said "five channels, 9,000,000 sat, confirmed" — `CLAUDE.md`, `README.md`, `docs/design.html` and the handoff — and **not one said what signed it.** It was a 2-of-3 cold-storage multisig.

#### Why that is evidence, not trivia

The harness's stand-in for Sparrow is a *simulated* `wsh(sortedmulti(2,…))`, and `CLAUDE.md` is explicit that it exists only because a regtest test needs something to build and sign a funding transaction. So the page's claim — "hot, cold, single-sig or multisig — which it is does not matter to this program" — rested on reasoning plus a fixture. A real 2-of-3 signing a five-channel mainnet batch is the instance behind it.

**It evidences the multisig half only.** Nothing here says anything about single-sig on mainnet; that is still the operator's call, and I·2's dissolution already says why.

#### Where it went

- **`docs/design.html:566`** — the "A wallet" row in the prerequisites table, which is *where the claim is made*. This is the site that matters: the fact was going into the batch record everywhere and never into the claim it supports.
- **`docs/design.html:505`** and **`:798`** — the commissioning paragraph and the claims table's mainnet paragraph, where the batch is described.
- **`README.md:20`** — the evidence table's step-8 row.
- **`CLAUDE.md:49`** — the status paragraph, with the reason it is worth a sentence.

**Not `docs/design.html:728`.** That row is "a bug in LND", and what signed the transaction has no bearing on it. Adding it there would have been filling in a checklist rather than putting a fact where it does work.

#### One stale line dropped

`CLAUDE.md` still listed "real channels running in production" as remaining work. **That batch was the production run**, two days before this. What remains is a polish pass, then the public flip.

#### design.html

Republished to the same URL, in this slice. Live and local were byte-identical before the edit (checked, not assumed), and all 827 lines of the live copy were read before publishing.

🤖 Generated with [Claude Code](https://claude.com/claude-code)


<a id="pr-61"></a>

## #61 — The handoff holds hazards, not its own history

Merged 2026-08-27 from `the-handoff-holds-hazards-not-history` into `main`.


695 lines to 428. The narrative went, and five stale claims went with it — each checked against the tree rather than read.

#### Deleted

- **The banner** about the file's own deletion history.
- **"Where the build actually is"** — duplicated `CLAUDE.md`'s status and had already drifted from it.
- **"The three collisions found while rewriting the docs"** — all three settled. The one live fact inside it, the two-slot update buffer, was already a bullet further down.
- **"Going public"** — it happened.
- **"Docker on this machine"** — its premise is that this login session predates the docker group. It no longer does; `make harness` ran without `sg docker` this session.

#### Stale, not merely narrative

Each verified against the tree:

| Claim | Reality |
|---|---|
| "`[fees] target_sat_per_vb` must be set or the config will not load" | There is no fee rate anywhere in this build. `[fees]` is a retired section |
| "`bump.Locate` consults the journal", "a bump has three tables of its own" | There is no `bump` package |
| "Still live for the CPFP child" | There is no CPFP child |
| "the exclusion report in `Coins.Report`" | It is `coldwallet.Coins.Excluded` |
| "`TestMempoolAccept` … so our own pre-flight enforces that same ceiling" | There is no pre-flight; it went with Bitcoin Core |

Dangling cross-references fixed too: "Phase 0 above", "see finding 6", "finding 2 above" — none of those sections still exist.

#### Fixed in place, per the no-issue rule

Three Go comments cited **"HANDOFF.md, finding 5"** and **"finding 6"** — numbers deleted in an earlier pass. `internal/regtestenv/wallet.go`, `internal/reserve/reserve_regtest_test.go`, `internal/settle/settle_regtest_test.go`.

#### What is kept

- **The runbook**, rewritten as one. It was phrased as a description of a probe that had not happened yet; a probe and a live batch are the same run, so it now reads as instructions. The three peer costs are a table, and the gossip-graph finding from the first live batch is in it.
- **The two live code gaps** — unbounded step 4/7 waits, and the unbounded `chan_pending` wait whose fallback dies with the context. Both still true; `run.FileWallet.wait` and `arm.receiptFor` both checked.
- **The hazards**, grouped: this repository · the harness · LND · Bitcoin Core as the harness uses it.

**Bullets `CLAUDE.md` already owns verbatim are dropped rather than said twice** — `peers=3`, no bare `go test ./...`, the 2016-block horizon test, `git checkout` on unstaged work, and the design.html republish rule. The header now says outright that `CLAUDE.md` holds the rules and git holds the history.

#### Not touched

`docs/replan-2026-08.md` and `docs/review-2026-08-triage.md` still reference sections of `HANDOFF.md` that this PR deletes. Both are records of completed processes rather than live documents, and rewriting them is a separate call about what the public repo should carry.

`make check` green, exactly the two `WINTHISTLE_SLOW` skips, `peers=3` throughout.

🤖 Generated with [Claude Code](https://claude.com/claude-code)


<a id="pr-62"></a>

## #62 — The finished plans go to git

Merged 2026-08-27 from `the-finished-plans-go-to-git` into `main`.


Deletes `docs/replan-2026-08.md` and `docs/review-2026-08-triage.md` — 1,404 lines between them. Both are records of processes that finished: the replan's six items all landed on 2026-08-26, and the triage classified a review whose findings were acted on.

**The durable half of the replan already lives in `docs/superseded.md`** — why each removed design went, which is what stops one being reintroduced by somebody who only sees the gap. The slice-by-slice account is git history and PR bodies, where `CLAUDE.md` says it belongs.

#### Nine inbound references repaired

- **`README.md`** called the replan *"the current direction … Read this first"*, which it had not been since the day it finished. `design.html` takes that line, and the "see the plan that got it there" clause in the intro goes.
- **`CLAUDE.md`**'s pointer now goes to `docs/superseded.md`, which is the part of the replan worth keeping a pointer to.
- **`internal/run/connect.go`**, **`internal/arm/skip_finalize_regtest_test.go`**, **`regtest/README.md`** cited "item 5 of `docs/replan-2026-08.md`" or "`docs/replan-2026-08.md` is void"; they now cite the rewrite and the sequence.
- **`docs/superseded.md`** says the plan existed, finished, and was deleted, rather than linking it.

#### One comment deleted rather than repaired

`internal/run/run_regtest_test.go` carried a tombstone for `TestARejectedWalletStopsTheRunBeforeAnythingIsAsked` saying the gate is *"still tested, in internal/setup"* — a package the **next sentence** says item 5 deleted. The test is gone, `internal/setup` is gone, and the doc it cited is gone with this PR. Every referent gone, so the comment goes with them.

#### `docs/review-2026-08.md` is kept

It now says at the top that it is a record rather than the work order it reads as — its Phase 0 block instructs a reader to produce the triage this PR deletes and then *"stop there … and wait"*, which is actively misleading in a file that survives.

What it is worth is its provenance: **written from `README.md` alone, by a reviewer with no source access.** Where it guesses wrong is a map of where the public documents mislead, which is the one thing in it that does not expire. Say the word if it should go too — `docs/` would then be `design.html` and `superseded.md`.

`make check` green, exactly the two `WINTHISTLE_SLOW` skips, `peers=3` throughout.

🤖 Generated with [Claude Code](https://claude.com/claude-code)


<a id="pr-63"></a>

## #63 — The review goes with its triage

Merged 2026-08-27 from `the-review-goes-with-its-triage` into `main`.


Deletes `docs/review-2026-08.md`, the external review that #62's deleted triage was classifying. **No inbound references anywhere in the repo**, so nothing to repair.

It was written from `README.md` alone, and what it was worth was where a reader with no source access guessed wrong — a map of where the public documents mislead. That map was read and acted on, and the documents it pointed at have been rewritten twice since. Git has it.

`docs/` is now `design.html` and `superseded.md`: the design, and why each removed design went.

**Checks run:** `go build`, `go vet`, `make check-citations`. Not the full suite — the change removes one markdown file with no inbound references and touches no Go file, so there is nothing for a regtest run to say about it.

🤖 Generated with [Claude Code](https://claude.com/claude-code)


<a id="pr-64"></a>

## #64 — The run says where you are and what stopping costs

Merged 2026-08-28 from `the-run-says-where-you-are` into `main`.


Nine numbered steps in three acts, one standing line per act saying what it costs
to stop there, and a step 8 that says what it checked.

#### What was wrong

The run printed two vocabularies at once, and neither was complete:

```
Phase 0 — the peers
Phase 0 — the shim probe
Phase 0 — the anchor reserve
Phase 1 — the armed window
Step 4 — build the transaction in your wallet
The batch plan
Step 5 — does it match the plan?
Step 7 — sign it
Phase 2 — settlement
```

Steps 1, 2, 3, 8 and 9 had no heading. **Step 6 had none at all** — the gate, the
moment the whole inversion exists to produce, went past unnamed. An operator
reading down the transcript could not say where they were or how much was left.

#### The rail

```
Act I — nothing has been asked of anyone
Step 1 — who you are opening to
Step 1 — this node's anchor reserve

Act II — the peers' ten minutes, and nothing is signed
Step 2 — asking each peer to hold a slot
Step 3 — the plan
Step 4 — build it in your wallet, and do not sign
Step 5 — does it match the plan?
Step 6 — every channel becomes recoverable

Act III — past the gate, and every channel is already recoverable
Step 7 — sign it
Step 8 — publish, once
Step 9 — watch it confirm, and apply the policies
```

Numbered as `docs/design.html` numbers them, so the app and the design teach one
model. `=` for acts, `-` for steps; no colour and no glyph carries meaning.
`Armed`, `Step 8 was not made` and `Taking the batch apart` stay off the rail —
they are reports and the abort path, not positions in the sequence.

**The acts are not the phases.** Phase 1 is `armWindow`, which runs from step 2 to
step 7; the acts break between 6 and 7, because the gate is what changes what
stopping costs. Both cuts are real, and the phase vocabulary stays in the
comments and never prints.

#### What numbering the screens exposed

**The run counted 4, then 3, then 5.** The recipients table printed under step
4's heading, and the batch plan — which cannot be assembled until the packet
exists, because until then there is nothing to say about the change output —
printed under an unnumbered heading after it. Unnumbered, the contradiction was
invisible. The table is step 3, because the table is the attribution; the
instruction and the wait are step 4; and the plan document opens step 5, which is
what it is. `regtestenv.RecipientsIn` scrapes that table and its anchor moved
with it.

**`section` underlined by `len(title)`,** and every heading has an em dash in it —
three bytes, one column — so every rule in the product was two characters long.
It counts runes now.

**The cold probe's test was named `…WithholdsStepNine`** while asserting on *"Step
8 was not made"*.

#### The standing line

`prose.StoppingHere(Stage, n)`. Its answer changes exactly three times in a run,
and each change is a thing the operator has to understand — so the rail teaches
the gate rather than describing it.

| After | The line says |
|---|---|
| step 1 | nothing is lost; no peer has been asked for anything |
| `arm.Open` | the batch is taken apart for you; each peer holds its slot until its own sweeper releases it |
| `arm.Receipts` | the batch is abandoned; *n* channels recoverable, but an abandon tells the peer nothing |
| `arm.Publish` | nothing stops; `winthistle recover` refuses to abort a published run |

**Printed where the value changes, not under every heading.** Steps 2 to 5 share
one answer. And a line under a heading would print before the thing it claims —
the Act II banner runs before `arm.Open`, so a line there would say the shims are
cancellable before any shim exists. Each stage prints immediately after the call
that makes it true.

**No stage line names an LND default.** The eleven minutes and the 2016 blocks
stay on the screens that own them, where `StockLNDNote` attributes them; a figure
repeated on four more screens would owe four more attributions, and the rule
exists to stop noise rather than license it.
`TestNoStageLineRepeatsAnLNDDefault` is the check.

**The unknown-`Stage` default is loud.** Falling through to `""` would render as a
screen with no standing line, which reads exactly like a screen where stopping is
free. It says it cannot say, and asserts nothing about the batch, because at that
point it knows nothing about the batch.

#### Two copy decisions

**Step 6's heading is future tense and prints before the wait.** Nothing bounds
that wait — a silent peer parks it until Ctrl-C — so a heading printed afterwards
leaves the operator watching a blank screen with no idea what for. And *"every
channel is now recoverable"*, printed before the receipts arrive, would assert the
one thing the step exists to establish. What happened is the paragraph below it,
which counts the channels itself.

**Step 6's paragraph lost "the teardown abandons",** which the standing line now
says four lines below. It keeps the key and the hazard that is its own: LND does
not refuse a force-close on a pending channel.

#### Step 8 says what it checked

The three checks between that heading and the RPC all run inside `arm.Publish`
and none of them prints anything when it passes: the bytes are hashed again and
refused if they are not the txid pinned at `psbt_verify` (I-3), the transaction
goes on disk before the call because `NoFundingTxBit` gates
`rebroadcastFundingTx` and we own rebroadcast, and `MarkPublishing` refuses the
call unless the journal counts every channel at `chan_pending`.

Not reassurance — the difference between a program that publishes what it was
handed and one that publishes only what it pinned, and step 8 is the last screen
on which that distinction can still be read.

#### Gaps, stated

**No test takes `run.Do` past the publish,** so the step 8 and step 9 headings and
the `StagePublished` line are unexercised — the two tests that arm set
`StopBeforePublish`, and the other three fail inside the armed window on purpose.
That gap predates this pass.

Step 8's paragraph stays inline in `run.go` rather than moving into `prose` to
become testable: the gap is not the wording, so an assertion on the string would
answer a question nobody asked and read like coverage.

#### Also

`docs/step-copy-2026-08.md` is a live plan and gets deleted when it lands, per
the finished-plans rule. Slice 3 closed with nothing in it — it was "give step 6
the heading and the line", and slices 1 and 2 had already given it both.

`make test` green, `gofmt` and `go vet` clean, `make check-citations` clean,
exactly two `WINTHISTLE_SLOW` skips. Four tests added in `internal/prose`. No
invariant moved: no new LND call site, no `psbt_finalize`, and the publish count
is still 1.

🤖 Generated with [Claude Code](https://claude.com/claude-code)


<a id="pr-65"></a>

## #65 — The step rail plan goes to git

Merged 2026-08-28 from `the-step-rail-plan-is-finished` into `main`.


Deletes `docs/step-copy-2026-08.md`, 201 lines. A record of a process that
finished: the rail, the standing line and step 8's paragraph all landed in #64,
and slice 3 closed with nothing in it.

**Nothing cited it** — unlike `docs/replan-2026-08.md`, which had nine inbound
references when it went. `docs/` is back to `design.html` and `superseded.md`.

#### Where the durable half lives

In the code, at the places somebody would need it:

- `act()`'s own comment carries the acts-are-not-the-phases decision, and why
  the phase vocabulary stays in comments and never prints.
- Step 6's heading is argued where the heading is printed — future tense, before
  the wait, and why either alternative is worse.
- `prose.StoppingHere`'s type comment says why no stage line names an LND
  default, and its `default` arm says why silence would be the dangerous render.
- The two ordering defects the rail exposed — the run counting 4, then 3, then 5,
  and every rule in the product being two characters long — are in the commit
  that fixed them.

#### One thing outlives the plan

Not code, so it goes to `HANDOFF.md` rather than nowhere:

> **No test takes `run.Do` past the publish, so steps 8 and 9 print unexercised.**
> The two regtest tests that arm a batch set `StopBeforePublish`, and the other
> three fail inside the armed window on purpose — so `arm.Publish`'s screen,
> `Published <txid>`, `prose.StagePublished` and the whole of `settlePhase` have
> never rendered under `go test`. A green suite is not evidence that the last two
> screens of a live batch are legible, and the one time they ran for real was the
> mainnet batch on 2026-08-26.

Filed under *Watch out for → This repository*, beside the other things a green
suite does not prove. Closing it means a regtest run that publishes, which
nothing prevents; it has simply never been written.

🤖 Generated with [Claude Code](https://claude.com/claude-code)


<a id="pr-66"></a>

## #66 — The second live batch taught the docs three things

Merged 2026-08-29 from `the-batch-taught-the-docs-three-things` into `main`.


Two Lightning Network+ swap channels of 3,000,000 sat each, opened on 2026-08-29 in one transaction — run `20260829-093343-3c9684`, txid <txid redacted>, both open, active and policied in 9m20s, signed from the 2-of-3 cold multisig.

**The code was right every time.** Step 6 held (2 of 2 `chan_pending`, nothing signed), I-3 held end to end (one txid at `psbt_verify`, at the signed-file read, and re-derived from the bytes at the publish), and the two refusals before it tore themselves down without help. This slice is the three places the *documentation* was wrong, all found by doing it.

#### 1 · HANDOFF told the operator to run a call the credential refuses

`getchaninfo --chan_point TXID:N` comes back `permission denied`: `GetChanInfo` is not in `internal/methods`, and `ListChannels`, which is registered, carries no routing policy. So with the baked macaroon there is no graph read-back of a policy at all.

The check that actually proves a policy landed is `settle`'s own read of `failed_updates` — the refusal `UpdateChannelPolicy` hides inside a successful response. The line now says that, names `admin.macaroon` for the read-back on top, and says **not** to register `GetChanInfo` to make itself runnable. The registry is the credential; widening it so a document's advice works is the wrong way round.

#### 2 · "The gossip graph is a poor proxy" never said what to use instead

It named the smallest channel as misleading and stopped there. The median against the ask is the signal, step 1 already prints it, and this batch measured it both ways:

| Peer | Median | Smallest | Asked | Result |
|---|---|---|---|---|
| PiCube | 5,000,000 | 250,000 | 3,000,000 | refused |
| Die-Tor-Node | 2,500,000 | 150,000 | 3,000,000 | accepted |

Both smallest channels were far below the ask and said nothing useful.

The ordering consequence goes on the `arm.Open` bullet, since fail-fast means a batch ordered by median descending has its refusals for free. This batch was ordered by which peer looked likeliest to be *unreachable* — the wrong axis — and paid one shim for it. Reordered, the next refusal cost `0 already open, and cancellable`: no shim, no journal row, no files.

The same peer then refused from *above* an hour after refusing from below (`maxchansize` of 1,000,000 after a `minchansize` of 4,500,000), so the bullet also says that a peer who has just moved a limit for you has not necessarily moved the one in the way.

#### 3 · The armed screen told you to store something it never gives you

> the backup plus the peer's data-loss protection is what recovers them if the database is lost. **Store it off this box.**

"It" is the export, which lives in memory to gate the publish and is never written — the single `os.WriteFile` in `internal/` is the recipients CSV. The screen now says what is actually storable: the node's own `channel.backup`, the file LND maintains at `backupfilepath`. It names the artifact without asserting anything about the operator's setup, which the program cannot know.

#### Checks

`make lint vet check-citations test` clean, harness at `peers=3`. The `internal/abort` package failed on the first pass with *"channels cannot be created before the wallet is fully synced"* — the documented two-hour unsync, not this change — and passed after `make -C regtest mine N=6`.

Fixed in place with no issue filed, per CLAUDE.md.
