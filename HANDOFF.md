# Winthistle — handoff

> ## ⚠ Direction changed on 2026-08-25. `docs/replan-2026-08.md` is the plan, and as of 2026-08-26 **every item on it is done.**
>
> **This file used to be ~3,960 lines of design record for a design that is
> being replaced.** Most of it has been deleted, deliberately, in the docs
> rewrite (replan build-order item 2). Git has it if you need it —
> `git log --follow HANDOFF.md`. What is kept below is the part that was never
> about the old design: hazards of this repository, this harness and this
> machine, which are as true after the replan as before.
>
> **The plan is now:** a CLI wizard around Sparrow and LND doing the two things
> those two cannot do between them — attribute the funding outputs to peers, and
> hold the I-1 gate. `CLAUDE.md` carries the invariants and the
> do-not-reintroduce list. `docs/replan-2026-08.md` carries the sequence, the
> two clocks, and the build order.

## Where the build actually is

**All six replan items are done.** The inversion is proved on a running node,
the armed window is built around it, `run` neither builds nor signs the
transaction, everything on the cut list is deleted, and since 2026-08-26 the fee
and change findings report rather than refuse and the `Replaceable` lint is gone.
`docs/replan-2026-08.md`'s four "as built" sections are the account.

**There is no code slice waiting, and the mainnet cold probe passed on
2026-08-26** — run `20260826-043441-9a8f28`. That was the last item on the build
order. What is written up below is now the account of a probe that ran, kept
because a real batch needs the same operational knowledge: the costs, the pass
criteria, and how to read the journal afterwards.

**What is outstanding is issues, not milestones.** #2 (remove the declared fee
rate, and `ChangeTooSmall` with it) and #5 (step 7 waits on one filename
asserting `.psbt`, so a `.txn` is never seen — and the failure mode is silence,
not a refusal).

**The tree.** ~29,100 Go lines, down from 55,670 before item 5 (28,881 at the
end of item 5; item 6 put ~250 back). Packages:
`arm` · `plan` · `combine` · `peers` · `reserve` · `settle` · `journal` ·
`methods` · `lnd` · `prose` · `config` · `abort` · `doctor` · `policy` · `run`,
plus `internal/bitcoind` and `internal/regtestenv/coldwallet` **inside the
harness only**. Four commands: `run`, `doctor`, `recover`,
`print-macaroon-command`, and two `example-*` printers.

**Four decisions the code now depends on, from items 5 and 6:**

1. **The fee rate is declared.** `[fees] target_sat_per_vb`, or `run --fee-rate
   N`. Nothing estimates it and nothing may be asked to. `config.Load` and
   `plan.Build` each refuse a missing or non-positive one, independently — and
   **that is a different thing from the fee findings**, which report. A missing
   rate is still a refusal at load time; a batch that pays the wrong one is a
   report at step 5.
2. **There is no pre-flight.** `combine.Accept` executes every input's witness
   against its own script; what is lost is node policy, and
   `plan.Verify`'s `Verification.Unchecked` names `testmempoolaccept` and says
   this build does not run it.
3. **`Method.CallSites` is 1.**
4. **The verifier refuses and reports out of two different types.**
   `Verification.Problems []Problem` are refusals and `OK()` is
   `len(Problems) == 0`; `Verification.Reports []Finding` are things it
   established and does not stop for — `ChangeMissing`, `ChangeTooSmall`,
   `FeeTooLow`, `FeeTooHigh` — rendered under **"Reported, not refused"**.
   `Unchecked []string` is the third and is not interchangeable with the second:
   it is what could not be established at all. **Do not add a severity field to
   `Problem`**; the split is which list a finding lands in, and two types are
   what keeps `OK()` from depending on a grade somebody set wrong.

---

## The mainnet cold probe — passed 2026-08-26

**Not a code slice.** Everything it needed existed, and composing it forced
exactly one branch — an `if` before `arm.Publish`. It is the real run with the
final call withheld, not a second path to the same place. The specification is
`docs/design.html`'s **Cold probe** section (phase C).

**What it proved,** run `20260826-043441-9a8f28`, two peers at 1,000,000 sat
each: 2 of 2 `chan_pending` **with nothing signed**; channel backups exported off
pending channels (7, 4,738 bytes); a signed transaction whose txid had not moved
and whose every witness executed against its own script; `Step 8 was not made`;
and a teardown that abandoned both channels via the blunt flag and left nothing
on the node. Journal: `state=aborted`, txid pinned, **`raw_tx` NULL**.

**Three attempts failed first, none on the safety model.** One died unattended
before arming and left no journal row. One blew clock A while the operator was in
Sparrow. One reached the gate and then refused the signed file for being a raw
transaction rather than a PSBT — issue #3, which landed the same day and made the
fourth attempt possible. **Funding addresses do not repeat between runs**; every
run derives fresh ones on both sides, so there is no pre-staging a transaction
against a previous run's addresses.

The rest of this section is what to know before running it **again** — which a
first real batch is, in every respect except that step 8 is taken.

### What it is

```
winthistle run --batch BATCH.toml --psbt FILE --stop-before-publish
```

`--probe` is worth adding the first time you meet a peer, and worth leaving off
afterwards: it shim-probes every peer before arming and then **waits out the
~11-minute hold it just created**, because otherwise the probe collides with the
open. `run --help` says so.

Steps 1 through 7 exactly as production runs them, against **real peers on
mainnet**, with **real coins that never move**. Step 8 is simply not taken. It
proves the one thing no harness can: *these* peers, *this* node, *this* wallet
and *these* devices.

**Why it costs nothing on-chain.** Nothing is at risk until broadcast. The coins
stay unspent in a transaction that is never published, and there is no fee and no
footprint.

**It is not free of the peers, though, and this is the thing to plan around.**
Every channel in a probe reaches `chan_pending` and is then abandoned — and
`AbandonChannel` touches only *our own* database. The peer, as responder, keeps
its side pending until `fundingTimeout` fires: `waitForTimeout` counts
`lncfg.DefaultMaxWaitNumBlocksFundingConf` = **2016 blocks**, about a fortnight
on mainnet, from the channel's broadcast height. Nothing shortens that and
nothing tells the peer otherwise. So **a probe spends one of each probed peer's
pending-channel slots for roughly two weeks**, and `withheld()`'s own screen says
so at the moment it happens.

### Before you start

- **Decide which peers you are willing to burn, and decide it first.** This is
  the only choice in the probe that cannot be taken back, because of the two-week
  hold above. Against a peer running LND's default `--maxpendingchannels=1`,
  **probing a peer and then opening a real channel with that peer are a fortnight
  apart.** Two ways to read that, and both are defensible: probe with the peers
  you actually want, which is what proves *these* peers and costs you the wait;
  or probe with two you do not mind, which proves the code path against mainnet
  and leaves the real batch free to go the same day. `--maxpendingchannels` is
  not in gossip and cannot be read, so assume 1 unless the peer has told you
  otherwise.
- **`winthistle doctor` clean**, against the mainnet node, with the baked
  macaroon rather than `admin.macaroon`. It checks the credential by asking
  `CheckMacaroonPermissions` rather than by calling anything, so a clean doctor
  is evidence and not a rehearsal.
- **`winthistle print-macaroon-command`**, and bake from *that* rather than from
  `docs/design.html`'s illustrative block — which has rotted before, with the
  count matching while the membership did not.
- **Two channels, at the smallest size the chosen peers accept.** The probe is
  commissioning, not an allocation. The first *live* batch afterwards should be
  the same shape.
- **Know each peer's minimum, and find it out for free.** Every limit check runs
  *before* the peer creates a reservation, so a probe the peer **refuses** costs
  nothing at all and you may probe downwards as often as you like. Worth doing
  before the run: a size a peer rejects at step 2 is rejected on the clock.

  **Three costs, and they are not the same size.** A refused probe: nothing. A
  *shim* probe the peer accepts (`run --probe`, or a stream that reaches
  `accept_channel` and is then cancelled): one slot for **about eleven minutes**,
  because `shim_cancel` deletes an entry in our own wallet and sends the peer
  nothing. A channel that reached `chan_pending` and was abandoned — **which is
  every channel in a probe**: one slot for **~2016 blocks**. The peer counts live
  reservations *plus* pending channels with no thaw height against the same
  `--maxpendingchannels` budget, so all three draw on one number. This file used
  to name only the eleven-minute one here, which is the smallest of the three and
  not the one a probe actually incurs. See the "probe is not free" bullet under
  "Watch out for" for the reservation half.
- **A declared fee rate.** `[fees] target_sat_per_vb` must be set or the config
  will not load. The number does not matter much here — nothing is published —
  but a wrong one now produces a report at step 5, not a refusal, so it will not
  stop you and you should not expect it to.

### What to actually watch

1. **Step 6 is the assertion.** *n* of *n* `chan_pending`, **with nothing
   signed**. That is the whole safety argument, observed on mainnet for the first
   time. Everything before it is reversible at no cost.
2. **The channel backups export while the channels are pending.** Step 6 does
   this, before publish and before signing, and it is the one place the probe
   proves something the harness cannot fully vouch for.
3. **Sign for real at step 7.** The point is to exercise the devices and the
   descriptor, not to satisfy the tool. Then confirm the txid has not moved —
   I-3, and the only load-bearing check on what comes back.
4. **The teardown.** `--stop-before-publish` clears up itself rather than
   suggesting it: `AbandonChannel` for each pending channel and `shim_cancel` for
   any stream that never verified. **Expect `pending_funding_shim_only` to be
   refused and the blunt `i_know_what_i_am_doing` flag to be asked for, once per
   channel.** That is the normal route here, not a warning sign — LND infers
   "shim funded" from `ThawHeight > 0` and a plain PSBT open sets none.
5. **`--stop-before-publish` exits non-zero if the teardown does not finish.** A
   probe that proved the sequence and then left channels pending is not a
   success. **Check the exit code.**
6. **You cannot walk away from the teardown, but it will wait for you.** `--yes`
   skips the arming prompt and **not** the blunt-abandon confirmation, which
   `abort.AbandonPending` asks *per channel, at the moment of the rejection*. So
   the probe is attended — plan to sit with it, or pipe the answers in;
   `confirmBlunt` accepts a piped answer deliberately and refuses only
   end-of-input.

   **There is no longer a clock on you while you decide.** There was until
   2026-08-26: `run.TeardownBudget` put five minutes around the whole teardown,
   prompts included, so an operator who deliberated past it got the abandon
   refused with a deadline *after* answering yes. The clock is per LND call now
   — `abort.CallBudget`, 30s — and the call that runs after the confirmation
   takes neither the parent's deadline nor its cancellation, because once
   somebody has authorised `i_know_what_i_am_doing` the worst outcome available
   is not finishing. `TestTheOperatorIsNotOnTheTeardownClock` reproduces the old
   defect and fails on it.
7. **Separately, deliberately let one stream lapse without verifying**, to
   observe a real peer's timeout rather than trusting the ten-minute figure.
   Regtest measured 10m41s against a stock LND at v0.19.3-beta and 10m14s at
   v0.21.2-beta; a CLN or Eclair peer has its own.

### Reading the probe's journal row afterwards

**A probe that withheld the publish leaves no `raw_tx` on disk, and that is
correct.** The signed bytes reach the journal immediately before the publish RPC
and nowhere else, so `raw_tx` means "we may owe a rebroadcast" — and a probe owes
nothing. The pinned txid is there.

**Publish is step 8, not step 9.** It moved when the signing round moved, and the
withheld-publish screen says *"Step 8 was not made"*. A screen or a note that
calls it step 9 is pre-inversion copy.

### What would make it a failure

- A `chan_pending` that does not arrive, and `PendingChannels` not explaining it.
  Note that **nothing bounds this wait** — see "Two live code gaps" below — so a
  silent peer parks the run until `Ctrl-C` rather than timing out.
- A txid that moved between step 5 and step 7.
- A teardown that leaves a channel pending, i.e. a non-zero exit.
- Anything the verifier **refuses** at step 5. What it *reports* there is not a
  failure of the probe: a change output too small or a fee outside tolerance is
  the operator's business and the run continues past it deliberately.

### Going public

The probe was the stated gate and it passed. **Flipping the repo is now the
operator's call, not a milestone's** — real channels running in production first,
then a polish pass. Assume every commit will eventually be public, and re-read
the "Repo hygiene" section of `CLAUDE.md` before flipping it — particularly
**never commit a mainnet xpub**, which `.gitignore` does not protect you from
when one is pasted inline in a test or a doc example.

---

## The three collisions found while rewriting the docs, and where they stand

**All three are settled.**

1. **`combine.ErrAlreadyFinalized` — settled in item 4, and its last caller went
   in item 5.** `combine` is the acceptance check now, `combine.Accept` takes the
   complete witness Sparrow produces, and `Merge`'s refusal survives with a
   mechanical reason: a merge unions partial signatures, finalization discards
   them, so a packet that arrives finalized has nothing left to union with. It has
   **no production caller** — the CPFP child was the last one — and the harness's
   `Env.SignLikeSparrow` is what still exercises it. That is a fact about the
   package comment, which says so, not a reason to delete it. Nothing about I-2 is
   cited as live anywhere.

2. **The receipt buffer — settled, and it fits.** `req.Updates` is
   `make(chan *lnrpc.OpenStatusUpdate, 2)` (`lnd/server.go:5309`), and the
   funding manager blocks when it is full. Steps 5→6 verify all *n* and then
   collect *n* receipts, which is what `arm.Receipts` does, and this flow produces
   exactly two updates per stream before confirmation: `psbt_fund` (read in
   `Open`) and `chan_pending`. Re-verified against LND v0.21.2-beta on
   2026-08-26: every send into an update channel in `funding/manager.go` is at
   `:2241` (`psbt_fund`), `:2908` (`chan_pending`) and `:4483` (`ChanOpen`, which
   is after confirmation), and there is no fourth. At v0.19.3-beta the same three
   sends were `:2217`, `:2884` and `:4265`; before that this file cited `:2873`
   and `:4254`, which were the `upd :=` construction and the `if updateChan != nil`
   guard — a few lines short of the sends, and the kind of citation that stops
   being checkable.
   There is room for the receipt and no room for anything else. **A third update
   per stream, or a change that stops reading promptly, breaks this and the
   failure looks like a dead peer.** `arm.Receipts`' doc comment says so where it
   would be read.

3. **The custody-language change-output copy — fixed in item 6**, along with
   eight more places carrying the same framing that the three-string inventory
   had missed, including the most operator-facing one of the lot:
   `plan.checkChange`'s own `ChangeMissing` detail text. The rule the
   replacements follow: **name what is missing (the lever), never imply what is
   not (risk).** Nothing is at risk in a stuck batch; what is missing is the
   exit. Three strings in `internal/regtestenv/coldwallet/build.go` still carry
   the old framing and were left alone — harness-only, and item 6 did not
   falsify them. Two are error messages (`:131`, `:197`); the third (`:109`) is a
   comment about the CPFP child, which item 5 deleted from the application, so it
   is stale for a second reason.

**Four claims that were wrong**, found by auditing rather than by working:
`handleFundingSigned` does not exist in LND at any version (it is
`funderProcessFundingSigned`, `funding/manager.go:2718` at v0.21.2-beta) and was cited in three
documents; there is no CI in this repository, though `CLAUDE.md` and `README.md`
both said there was; the harness is a hand-written compose file that borrows
Polar's images rather than Polar itself; and `docs/design.html`'s macaroon block
named two methods the build never calls while omitting two it does, with both
lists 17 long so the count matched. Check before writing "we do", not only
before writing "we cannot".

**Three more stale strings found in item 6's sweep and deliberately not fixed**,
because item 6 did not falsify them and the slice had a scope: the "directed
mode" / "assisted mode" vocabulary in `plan.Change.Address`'s doc comment and in
`plan.checkOutputs`, and `plan_regtest_test.go`'s "the app's own builder", which
has been the harness's builder since item 4. All three are comments. Fix them in
whatever slice next touches those functions.

## Two live code gaps, and neither is a safety failure

**Nothing bounds step 4 or step 7, and that is deliberate rather than
overlooked.** `run.FileWallet.wait` polls until the context ends, and the two
waits have different clocks above them that are not the transport's to enforce:
step 4 is inside the peers' ten minutes, where a deadline of ours would abort a
batch the peers were still holding, and step 7 has no deadline at all now that the
gate is open. What ends either is the operator, or the run's own context. The
browser path used to bound both, because `server.Run.Ask` required a deadline;
that path is deleted and its bounds went with it, which changes nothing about the
reasoning. **If a countdown is ever added to the CLI, step 4 is the one it is
for** — it is the only step inside clock A that takes any time at all.

**Nothing bounds the `chan_pending` wait, and the fallback dies with the context
that ends it.** `arm.receiptFor` blocks in `Recv` with no deadline of its own,
and `winthistle run` builds its context from `signal.NotifyContext` and nothing
else — so a peer that accepts and never answers parks the armed window until
`Ctrl-C`, with the rest of the batch already armed behind it. Then, because the
context that ended the wait is the one `isPending` is asked on, the lookup that
decides abandon-versus-cancel fails too, and the operator is told "this channel's
state is unknown" about a channel LND could still have answered for.

Not a safety failure: the batch is unpublishable and abortable throughout, and
`TestAPeerThatNeverAnswersLeavesTheBatchUnarmed` pins both as current behaviour
rather than as correct behaviour. The `isPending` half is much smaller than the
deadline half and could be taken on its own — a `context.WithoutCancel` plus a
short timeout would let the one question that matters still be asked.

**This matters more after the inversion, not less.** Step 6 is now the gate and
the only thing left under clock A, and it is the step this gap sits inside.

**Its cousin, which item 3 named rather than closed.** A *crashed* process — one
that died between a channel's `psbt_verify` and its receipt — leaves a journal row
saying `verified` for a channel LND has probably already created, because
`skip_finalize` makes the verify complete the funding flow rather than park it.
`Run.AbortTarget` then lists it as a shim to cancel, and `abort.CancelShim`
reports `AlreadyGone` — a *success* — for a channel still pending on the peer's
side until clock B runs out. Inside a run this cannot happen: `arm.Receipts` asks
`PendingChannels` whenever a receipt does not arrive and journals what it finds.
**The pre-inversion sequence had exactly the same gap between `psbt_finalize` and
its receipt, unchanged in kind and in size**, which is why item 3 did not treat it
as new. Closing it needs a lookup `AbortTarget` cannot make — it has no client,
and the journal cannot map a pending channel point back to a pending channel id.
The comment in `internal/journal/recover.go` says all of this where somebody
editing that switch would read it.

## Docker on this machine

The login session predates the docker group, so `docker` in a fresh shell gets
`permission denied ... /var/run/docker.sock`. Group membership is fixed at
process start, so wrap it: `sg docker -c "make -C regtest reset"`. A re-login
fixes it permanently. On a new machine: `sudo addgroup --system docker; sudo
adduser $USER docker; sudo snap disable docker && sudo snap enable docker`.

## Watch out for

**Every bullet below is true of the repository today.** Twenty went in item 5,
with the machinery they were about — the browser's `Origin: null`, the startup
token, the device transports, the CPFP child's replaceability and sequence
arithmetic, `estimatesmartfee`'s two moods, the server's screens. Git has them:
`git log --follow HANDOFF.md`. What is left is about Go, LND, Bitcoin Core as the
harness still uses it, this harness and this repository's own habits.

Two are worth reading in the new light rather than skimmed. The legacy-address
bullet: **nothing pre-excludes a legacy coin** now — Sparrow picks them, and
`plan.Verify`'s `LegacyInput` refusal is the only thing that notices. And the
2-of-3-with-three-signatures bullet, because Sparrow can over-sign and
`internal/combine` is what meets it.


- **`docs/design.html` is published as an artifact, and editing the file does not
  update the published page.** The two drifted five statements apart before
  anyone checked, across several slices, and the published page is the only copy
  an outside reader sees — the reviewer who wrote `docs/review-2026-08.md` worked
  from public documents alone. Five statements it was still making: that
  `replaceable: false` protects against RBF and should be surfaced as a UI
  toggle; that assisted single-sig is gated before the mode is selectable (that
  gate was never built — see the residual table); that a download hands the PSBT
  to Sparrow, as the only path; that animated QR was coming; and that a camera is
  a prerequisite. It was also missing "A batch that never confirms" and the
  seven-state recovery table entirely, which are the two sections an operator
  would most need under pressure.

  **So: any slice that changes `docs/design.html` republishes it in the same
  slice.** The artifact URL is not in this repository — `/artifacts` in Claude
  Code lists it, or ask the owner. Two things about doing it: the tool refuses a
  publish from a session that has not read the live version, which is the
  safeguard against clobbering and not an obstacle to route around; and *diff*
  the live source against the local file rather than assuming the local one is
  ahead. That diff is what found the five.

- **A browser is installed on this machine.** Two handoffs said there was not,
  and that claim was never checked; `which chromium google-chrome firefox` finds
  three. Render before believing a page works. Three rendering sessions have now
  produced twelve defects between them, most of which no test would have caught,
  and four of which were false statements in operator copy.

- **`git checkout <file>` discards unstaged work, and this repository is worked
  on with everything unstaged.** One `git checkout internal/prose/recovery.go`,
  used to undo a two-line experiment, threw away six reapplied edits and cost
  fifteen minutes. Copy the file to the scratchpad and copy it back.

- **A `bool` that means "it failed" cannot also mean "we never tried".** The
  dress rehearsal's `Accepted` was both, and the report read the second as the
  first: a rehearsal that died before it asked announced that `testmempoolaccept`
  had refused it. Both the field and the package are deleted; the rule is not.
  When a report renders a verdict, check that the zero value is not a verdict.
  `prose.signerNote`'s missing `default` arm was the same defect in a `switch`.

- **A Core container restart unloads the harness wallets and the tests reload
  nothing.** Symptom: `Requested wallet does not exist or is not loaded`, from
  `getwalletinfo`. `./bin/bcli listwallets` will show only the
  `winthistle-*-test` wallets that earlier test runs created — `miner`,
  `cold-watch`, `cold1` and `cold2` are gone. `./bin/bcli loadwallet NAME` for
  each of the four is the quick fix; `make -C regtest bootstrap` is the blunt
  one. Distinct from the generation mismatch below, which bootstrap cannot fix.

- **`getaddressinfo`'s `parent_desc` can name the wrong descriptor, and it is not
  a gate.** A Core wallet can hold two descriptors that derive the same address —
  the state a corrected setup leaves behind — and Core credits the address to the
  key manager created first, active or not. Measured: three of five addresses of a
  *correct* pair attributed to a rejected one. The app no longer imports
  descriptors or asks the address question — that went with `winthistle setup` —
  but the harness's cold wallet does exactly this at `make harness`, so a
  bootstrap that looks wrong may be this rather than a broken descriptor.

- **Core deactivates the descriptor it replaces and keeps its coins.** One active
  external and one active internal per wallet, no RPC to remove either, and
  `listunspent` still lists a deactivated descriptor's coins. So a corrected setup
  is a new wallet name, not a second import.

- **`bitcoind.Client.TestMempoolAccept` passes no `maxfeerate`**, so our own
  pre-flight enforces that same ceiling. Do not "fix" that by passing zero: the
  pre-flight should apply the ceiling the broadcast will, and the earlier,
  better-worded refusal is what an operator should hit first.

- **The harness owns its coin locks now, and a leaked one is invisible.**
  `walletcreatefundedpsbt` is called with `lockUnspents`, and until item 5 the
  *application's* abort path released those locks — so every fixture got its coins
  back as a side effect of the thing it was testing. It does not, so
  `Env.BuildPSBTPaying` registers the release itself and a test calling
  `coldwallet.Build` directly has to call `Env.ReleaseLocksAtCleanup`. The symptom
  names nothing: `walletcreatefundedpsbt: Insufficient funds` from a wallet whose
  `getbalances` is fine and whose `listunspent` is empty. Check
  `./bin/bcli -rpcwallet=cold-watch listlockunspent`, and
  `lockunspent true` frees the lot. **A clean `make check` ends with zero locked
  outputs**, which is the thing to assert if this recurs.

- **Core hides locked outputs from `listunspent`**, so a change output an
  unfinished bump is holding looks exactly like one that has already been spent —
  and the two call for opposite actions. `bump.Locate` consults the journal to
  tell them apart and names the holding bump. Nothing else can.

- **`journal.Open` has no migrations.** One `CREATE TABLE IF NOT EXISTS` block, no
  version table. A new column would silently not reach a journal an earlier build
  wrote, so schema growth means new tables — which is why a bump has three of its
  own rather than columns on `signers` and `locks`.

- **The peers do not forget an aborted batch, until something mines.** See
  finding 6: `AbandonChannel` touches only our own database. If the harness
  starts refusing opens with *"Number of pending channels exceed maximum"*, that
  is what it is — and the cheap cure is `make -C regtest mine N=2016`, about ten
  seconds, which times every stale pending channel out on every peer at once.
  `make harness` also works and is slower. `internal/plan`'s and
  `internal/peers`' regtest tests add to the pressure from the other side: they
  open shim streams and cancel every one without finalizing. A cancelled shim
  costs *us* nothing, but the peer has already sent `accept_channel` and holds
  its reservation until its own timeout, so a tight run of `make test` can still
  crowd a peer for ten minutes.

  **On mainnet this bullet is not an annoyance, it is a fortnight**, and there is
  no `mine N=2016` to reach for. The same fact — the peer keeps an abandoned
  channel pending until `fundingTimeout` — is what makes the cold probe cost one
  pending-channel slot per peer for ~2016 blocks. See "The mainnet cold probe"
  above.
- **A shim probe that succeeds is not free.** The peer holds a reservation for
  about eleven minutes and `shim_cancel` does not tell it otherwise. Probing all
  *n* peers and then arming collides with itself against any peer running LND's
  default `--maxpendingchannels=1`. `peers.ReadyToArm` is the gate; the long
  version is in "Phase 0" above. A probe that is *refused* costs nothing at all,
  because every limit check runs before the peer creates a reservation.

- **`UpdateChannelPolicy` reports failure inside a success.** Nil error,
  `failed_updates` populated. Reading only `err` records a policy that was never
  applied. `settle.ApplyPolicy` is the only place this build calls it.

- **`minimum_depth` is readable, in one window.** It is in `accept_channel` and
  in `OpenChannel.NumConfsRequired`, and `PendingChannels` reports it back as
  `confirmations_until_active` for as long as the funding transaction is
  unconfirmed. `settle.State.PeerDepth` takes that reading once;
  `ExpectedDepth` is the prediction that stands in when the window was missed,
  and `ObservedDepth` is the harness's check from above. Once the transaction
  confirms the same field is a countdown, so a reading taken late is not the
  peer's number.

- **`gettransaction` on a client with no wallet scope returns -19, not -5.**
  "Multiple wallets are loaded. Please select which wallet to use...".
  `bitcoind.Client.Confirmations` falls back to `getrawtransaction` on both, plus
  on a node built without wallet support — otherwise a node-level client can
  never report a confirmation count.

- **Mining and then acting needs a wait.** LND refuses to open a channel while
  its wallet is behind the chain — *"channels cannot be created before the wallet
  is fully synced"* — and one block is enough to trigger it. `regtestenv.Mine`
  now blocks until alice has caught up, which is why it takes a `*testing.T` and
  can fail.

- **`chan_pending` txids are chainhash bytes**, i.e. reversed relative to every
  txid a human or Core sees. `lnd.ChannelPointFromPending` handles it; hex-encoding
  those bytes directly yields a plausible txid that matches nothing.
- **`lnd@latest` resolves to `v0.0.2`**, a retracted tag — lnd's real versions are
  pre-releases. Pin the exact tag, matching `regtest/.env`; it is `v0.21.2-beta`.
- **lnd needs a forked protobuf, and the restatement does not follow the bump.**
  `lnrpc` uses `protojson`'s `UseHexForBytes`, which only exists in Lightning
  Labs' fork. A dependency's `replace` does not apply transitively, so `go.mod`
  restates it — and because it is a restatement, `go get` on lnd leaves it where
  it was. It sat at `v1.30.0-hex-display` against an lnd that replaces at
  `v1.33.0-hex-display`. **Read lnd's own `go.mod` on every version bump** and
  match the line. Do not remove it.
- **grpc rides lnd's pin, and it moved.** It was v1.59, which has no
  `grpc.NewClient`, so `internal/lnd.Dial` used `DialContext` on purpose. lnd
  v0.21.2-beta pins v1.79, where `DialContext` is deprecated and `NewClient` is
  the call — `Dial` uses it now. Nothing was lost: `DialContext` without
  `WithBlock` never dialled either, so its 15-second context bounded nothing, and
  the `GetInfo` probe was always the thing that proved the connection.
- **`journal/` was an unanchored .gitignore pattern**, so `internal/journal/`
  matched it and the whole package was silently un-committable. Now `/journal/`
  and `/runs/`, anchored to the repo root where they were meant to be. Worth
  remembering the shape: unanchored gitignore patterns match at every level.
- **The call-site check sees typed client calls, not `conn.Invoke`.**
  `TestEveryLNDCallSiteIsRegistered` resolves method calls through go/types, so it
  finds a call on `lnrpc.LightningClient` and a call on a narrow interface that
  one satisfies — but a raw `conn.Invoke(ctx, "/lnrpc.Lightning/…", …)` with the
  path as a string is invisible to it. `internal/methods/bake_regtest_test.go`
  does exactly that on purpose, to test LND's own enforcement rather than ours;
  it is the one place that should.
- **A macaroon refusal is not `codes.PermissionDenied`.** It is
  `bakery.ErrPermissionDenied` — a plain `errgo.New("permission denied")` from
  gopkg.in/macaroon-bakery.v2 — passed through LND's interceptor with no gRPC
  status attached, so it arrives as `codes.Unknown` with that text. `winthistle
  doctor` has to match the text; matching the code would never fire. Proved in
  `TestTheBakedMacaroonWorksAndIsNarrow`, which logs if LND ever starts typing it.
- **`internal/reserve`'s tests lease every coin alice has**, on purpose, and give
  them back in `t.Cleanup`. If a later test fails with LND's reserved-value error
  and nothing explains it, check for a leaked lease: `lncli --network regtest
  wallet listleases`, and release with `wallet releaseoutput`. `-p 1` is what
  stops another package seeing that node mid-test.
- **btcd is now a direct dependency, at lnd's own pins.** `internal/plan` parses
  PSBTs and classifies scripts with `btcutil/psbt` and `txscript`, and it must be
  the same code LND runs or the verifier's answer stops predicting
  `psbt_verify`'s. `go mod tidy` only promoted the existing indirect entries; do
  not bump them independently of lnd.
- **A single send to a legacy address poisons a Core wallet for batching.** Core
  derives change of the same type as the payment, so paying one P2PKH address
  leaves a P2PKH change output behind — and the next batch built from that wallet
  fails at `psbt_verify` with *"not all inputs are SegWit spends"*, naming an
  input that has nothing to do with whatever produced it. A fixture that needed a
  legacy coin did exactly this to the miner wallet and broke every abort test.
  Two things now stop it: that fixture keeps its legacy coins inside its own
  wallet, and `regtestenv.BuildFundingPSBT` fences off the funding wallet's
  non-SegWit coins with `coldwallet.FenceOff` before handing over to
  `coldwallet.Build`. Worth knowing past the harness — a real cold wallet with any
  legacy history is in the same position, which is what the exclusion report in
  `Coins.Report` is for.

- **Locking is the only way to exclude a coin in Core.** There is no
  "do not spend these" option on `walletcreatefundedpsbt`, and Core does skip
  locked outputs. So `coldwallet.FenceOff` is `lockunspent`, and it inherits
  everything finding 2 above says about it.

- **A 2-of-3 with three signatures does not finalize.** btcd's
  `checkIsMultiSigScript` requires the number of partial signatures to equal the
  number the script demands, so asking one more device to sign "just in case"
  breaks the batch. `internal/combine` catches it before `MaybeFinalizeAll` and
  says so with the counts and the device labels; without that the operator gets
  "Unsupported script type". Still live for the CPFP child, and note
  `checkSignatureCounts` now **skips an input that already carries a complete
  witness** — the batch's own path, where the signatures were counted by whatever
  finalized them and what checks the result is `executeWitnesses`. A wallet that
  over-signs and then finalizes is Sparrow's problem, not this build's; one that
  over-signs and hands back partials still gets the counts and the labels.

- **A finalized inbound PSBT is refused by `combine.Merge` and expected by
  `combine.Accept`, and the two are right for opposite reasons.** This used to be
  one rule and it used to be I-2: a finalized input is a complete witness, so that
  device held a broadcastable transaction. I-2 is dissolved. What survives is
  mechanical — a merge *unions* partial signatures and finalization discards them,
  so a packet that finalizes on its own leaves the rest of the round nothing to
  add to. **No production caller reaches `Merge` any more** — the CPFP child was
  the last, and item 5 deleted it — and the harness's `Env.SignLikeSparrow` is
  what exercises it. That is a fact about the package comment, which says so,
  rather than a reason to delete a rule about merging. The batch's wallet goes
  through `Accept`, where a
  complete witness is the expected input and the check on it is
  `executeWitnesses` rather than a signature count. **If you find yourself
  re-justifying either in I-2's words, stop**: `CLAUDE.md`'s I-2 section is the
  record of why that reasoning is gone.

- **`internal/arm`'s publish test leaves open channels and spends cold coins.**
  It is the only test that publishes, and it has to, because "the transaction
  reaches the network on exactly one line" is not a claim a dry run can make.
  Nothing can close what it opened — `CloseChannel` is on the never-list — so
  `make harness` is the reset. **"Insufficient funds" from the cold wallet is
  usually this**, and occasionally a leaked coin lock: see the next bullet.

- **A stream must be read promptly, or the funding manager waits — and the new
  sequence spends the buffer.** `funderProcessFundingSigned` sends `chan_pending`
  on `resCtx.updates`, a channel with a buffer of 2 (`server.go:5309`), and blocks
  on `f.quit` if it is full. One slot is spent on `psbt_fund`, read in
  `arm.Open`. `arm.Verify` then verifies all *n* streams and `arm.Receipts` reads
  the *n* receipts afterwards, so at the worst moment every stream is holding one
  unread `chan_pending` in one free slot. That fits exactly, with nothing spare.
  It is safe because this flow produces exactly two updates per stream before
  confirmation and there is no third emitter — `funding/manager.go:2241`,
  `:2908`, `:4483` are every send site, at v0.21.2-beta. **Add a third update per stream, or stop
  reading promptly, and the funding manager blocks; the failure reads as a dead
  peer.** `arm.Receipts`' doc comment carries this where it would be read.

- **A macaroon refusal has two shapes and they mean opposite things.**
  `codes.InvalidArgument` from `CheckMacaroonPermissions` is the answer about
  the macaroon in the request; an *untyped* error carrying the same "permission
  denied" text is the interceptor refusing the caller. Match both, as
  `doctor.refusedOverMacaroon` does — the code alone confuses them and the text
  alone does too. (It was `tooNarrow`; issue #13 renamed it, because what it
  establishes is the refusal and not its reason.)

- **`ExportAllChannelBackups` is node-wide.** The snapshot covers every channel
  the node has, not the batch's. A test may assert "at least the batch"; on the
  harness it comes back with 33.

- **`internal/run`'s regtest tests open real channels and abort them.** Four
  tests now: two channels for the cold probe, two for the cancellation test, two
  for the below-minimum failure test and one for the `--psbt` file-path test. All
  cancelled or abandoned, so they add to the pending-channel pressure every abort
  test creates. Same cure: `make -C regtest mine N=2016`. The file-path test is
  *n* = 1 deliberately, because it proves a transport rather than a batch size.

- **The composition's failure path aborts without asking.** `run.Do` tears the
  batch down on any failure between `arm.Open` and the publish, because nothing
  has been broadcast and the answer is always the same. It does ask before using
  LND's blunt abandon flag — `prose.BluntConfirmation` — and that prompt is
  asked on **every** abort of a channel that reached `chan_pending`, because
  LND's safe flag rejects every channel this app opens.

  A piped answer is accepted; end of input is not. The first live CLI probe ran
  with stdin closed, refused its own teardown, and left two channels pending —
  correct by the old rule ("not a terminal, so no") and useless, because the
  operator then had to recover by hand from a run that had done everything
  right. The rule is now about whether anybody answered rather than about what
  kind of file stdin is: `echo y | winthistle recover <id>` works, EOF is no,
  and the guard that actually matters is unchanged — `abort.AbandonPending`
  asks `PendingChannels` itself and offers no confirmation at all for a channel
  that is not pending.

- **`--stop-before-publish` exits non-zero if the teardown does not finish.**
  The probe proving the sequence and then leaving two channels pending is not a
  success, and a probe is usually run from a terminal somebody walks away from.

- **The config reader refuses unknown keys, which makes it strict about its own
  history too.** Renaming a key is a breaking change to every operator's file,
  and the failure is loud rather than silent. That is the intent — but "[fees]
  has no key \"mode\"" reads like a typo rather than like the change it is, so a
  removed setting goes in `retiredKeys` or `retiredSections` in
  `internal/config/toml.go` and gets a sentence saying what happened to it.
  Item 5 retired eleven keys and three sections that way. **Add the entry in the
  same commit as the removal**, or the refusal is the shrug.

- **Waiting out the peers' window on a batch gives you no receipts, not one.**
  The obvious way to drive "a funding timeout expires with one receipt
  outstanding" is to open the batch, wait eleven minutes and finalize. It does
  not work: every peer's clock starts at its own `accept_channel`, so a batch
  opened together lapses together, and what you get is *n* dead reservations and
  a `psbt_verify` that already failed before any finalize. What produces one
  survivor is a **stagger** —
  `TestOnePeersWindowExpiringLeavesTheRestOfTheBatchArmed` opens the doomed
  stream six minutes before the other, verifies both while both are alive
  (`psbt_verify` is local and touches no timer, which is why it passes minutes
  before one member is gone), watches the first peer give up rather than sleeping
  past it, and finalizes afterwards. `WINTHISTLE_SLOW=1`, ~12 minutes, and it
  holds alice's streams and the cold wallet's coin locks for all of it — so do
  not run anything else harness-backed alongside it.

- **LND calls itself unsynced when regtest's tip is more than two hours old, and
  a long test can cross that line mid-run.** btcwallet's `IsSynced`: "if the
  timestamp on the best header is more than 2 hours in the past, then we're not
  yet synced" — exactly two hours; `isCurrentDelta` in btcwallet's
  `chain/interface.go`, still `2 * time.Hour` under lnd v0.21.2-beta. `OpenChannel` then refuses
  with `channels cannot be created before the wallet is fully synced`, which
  reads like a node problem and is a *clock* problem: nothing has been mined.
  It bit the funding-timeout test on its first run, where the stream opened at
  t+0 and the one at t+6m did not. `env.Mine(t, 1)` at the start buys two hours
  of headroom and confirms nothing but itself. Any test that spans minutes on an
  idle cluster wants it.
