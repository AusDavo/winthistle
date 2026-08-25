# Winthistle — handoff

Read this first. `CLAUDE.md` is loaded automatically and carries the four
invariants and the do-not-reintroduce list; treat those as settled.

**State: every function in this repository now has a non-test caller.**
`winthistle setup` builds the watch-only wallet from the cold wallet's
descriptors and ends by asking a human to compare addresses; `winthistle run`
drives Phase 0 — peer pre-flight, fee rate, anchor reserve, dress rehearsal —
then Phase 1's armed window and its single publish, then Phase 2's confirmation
watch and policy pass; `winthistle bump` builds, verifies, signs and broadcasts
the CPFP child of a batch that went out too cheap; `winthistle doctor` runs
every pre-flight in order and prints the command that fixes each failure;
`winthistle recover` is the recovery screen and the abort behind it. All of it
is configured by `winthistle.toml`, a batch file and a descriptor file, and all
of it is exercised against the live regtest node.

**The server can now open a batch.** `winthistle serve` puts `internal/server`
on a loopback socket with the security shape `docs/design.html` asks for — a
startup token printed once, strict `Origin` and `Host` checks, no CORS at all —
and it serves three screens and three unsafe methods: start a run, answer the
four questions a run asks, stop one. The three decisions HANDOFF asked for before
any handler was written are made and each carries a guard; the four callback
seams are wired, and each of the five things that turned out to be hard about
them carries a guard too. See "The server, and the three decisions with guards on
them" and "The four seams" below.

A browser-driven cold probe runs end to end against the harness: two signing
rounds of two devices each, base64 out in a `Question` and back in a form post,
through `internal/combine` and `internal/arm`'s verifier untouched, then the
teardown's per-channel blunt confirmations, then `StateAborted` with nothing in
the mempool. `TestABrowserDrivenColdProbeRunsTheRealPathAndWithholdsStepNine`.

**And it has now been driven in a real Chromium, which found a bug that made
every form in the UI unusable.** See "The bug the first render found" below —
`Origin: null` — and note the correction it carries: a browser *is* installed on
this machine, and the previous two handoffs said otherwise.

What is missing is the **mainnet cold probe**, and at this level only that. This
sentence used to name four other things, and every one of them had been built by
the time it was read: transport selection per device
(`TestTheTransportIsPerDeviceAndTheSameInBothRounds`), the setup screen
(`GET /setup`), the bump screen (`/bump/<run>`) and the signet harness
(`signet/`). That is the fourth handoff in a row to carry a "missing" that was
not — check the code before writing one. **The countdown is built**; see the
review section below. **In-house animated QR is out of scope as of 2026-08-24**, by the
owner's decision, and it is out of scope rather than unbuilt: see "The QR decision
and the file transport" below.

**An external review arrived, and triaging it was most of a session.**
`docs/review-2026-08.md` was written from `README.md` alone by a reviewer who
never saw the source, and it carried its own mandatory Phase 0: classify every
finding against the code before implementing any of it. The triage is
`docs/review-2026-08-triage.md` and it is the thing to read rather than the
review. About half the findings were already built — item 2b asked for a *spec*
for signed-PSBT validation that exists in `internal/combine` with seventeen
adversarial tests — and the half that was wrong turned out to be a map of where
the public docs mislead. Item 4 called the web UI "the largest unbuilt thing in
the repo" because the README's status blockquote said so. Item 8 asked to freeze
a "run-directory format" because nothing public said the journal is SQLite.

So **the README was the actual defect**, and it was rewritten: the stale status
blockquote, an install section, a requirements-and-topology section, the no-RBF
claim qualified with what actually enforces I-4, the change-output guarantee, the
peers' eleven-minute clock and the dress rehearsal that gates it, "why not just
sign it in Sparrow", and the BIP174 contract in place of naming Sparrow. Plus the
three things whose absence produced the review's wrong guesses: `doctor`'s ten
checks, the journal's seven states, and the inbound PSBT checks.

**Four small items came out of it, one commit each**, and two of them found
things:

- The peer pre-flight now reads whether this node already has a channel pending
  open with a batch peer — free, local, and the same answer a probe pays a
  peer-slot for. A warning and not a gate: `--maxpendingchannels` is the peer's
  own and published nowhere. It is honest in one direction only, because
  `AbandonChannel` is local-only, so an empty answer is not proof of a free slot.
- The peer's alias reaches the batch plan, beside its key and never instead of
  it. An alias is self-declared, non-unique gossip; a test asserts the verifier
  produces identical scripts and amounts whether it is right, wrong or absent.
- Receipt tests, which **found a defect**: a second `chan_pending` receipt naming
  a *different* outpoint silently overwrote the first, and that outpoint is what
  an abort abandons. `MarkPending` now refuses with `ErrOutpointMoved`. An
  identical repeat is still accepted.
- The journal's seven states and what `recover` does in each are now in
  `docs/design.html`, and a stale single-sig claim there was removed.

**Then the countdown and the live state, which was item 4 reduced to its core.**
`prose.Progress` renders one row per channel, the receipt count, whether the
funding transaction is still held, and what is left of the peers' window; the
attach screen shows it *above* the transcript, because a transcript grows without
bound. It reaches the server through `Launcher.Progress` — decision 1 means
`internal/server` cannot import `internal/journal` — as text rather than rows,
for the reason `Unfinished` is text. The TUI the review proposed was not built;
see "Held open" below.

**Four things from that work worth carrying forward.**

1. **`journal.loadChannels` orders by `pending_chan_id`, which is 32 random
   bytes.** So a run's channels come back in an order unrelated to the batch
   file's. This was found by rendering the live screen in a browser and seeing
   channel 1 hold the third channel's amount. Numbering rows on that screen would
   have invited an operator to match row 2 against the plan document's "channel 2
   to bitrefill" and get a different channel, so the numbers were dropped.
   Fixing the order properly needs a position column, and the journal grows by
   new tables rather than new columns.
2. **`Run.Reply` returns before the asking `Ask` has cleared `r.pending`.** The
   answer is buffered and `Ask` clears the slot on its way out, so anything that
   treats `Reply` returning as "that question is finished" is racing. A test in
   `internal/webrun` was doing exactly that and flaked about one run in four once
   the package's test binary grew; it now awaits each device, which is what
   `run.sign` does in production. Production was never affected — but a new test
   here will hit it again.
3. **Harness state still leaks across packages even under `-p 1`.**
   `internal/bump`'s `TestTheChildsArithmeticAgreesWithCoresOnARealStalledParent`
   failed once in a full-suite run and passes alone and on repeat. `-p 1` stops
   two test binaries sharing alice concurrently; it does not undo what an earlier
   package left in the mempool.
4. **Two of the three item-3 scenarios this list called un-writable were
   ordinary test-writing jobs, and one still is not.** This entry said an LND
   restart mid-batch and a peer that accepts then goes silent both needed
   container stop/start that `internal/regtestenv` does not have. They did not:
   both are failures of the *counterparty* arriving at `arm.Client`, and a stub
   client returns either one at exactly the chosen channel, every time — which is
   a better test of our handling than a container stopped at the right moment
   would be. Both shipped on 2026-08-25 in `internal/arm/mid_batch_test.go`. The
   lesson is the one the triage audit taught twice in the same slice: **check
   what a scenario actually needs before recording that it cannot be reached.**

   The third is still open and its reasoning still holds. **Core unreachable at
   the `testmempoolaccept` pre-flight inside the armed window** needs a seam
   where `run.Deps` holds a concrete `*bitcoind.Client`, and Phase 0's fee
   estimate uses the same client, so a dead one fails earlier and tests a
   different thing. Note this is *not* the triage's "bitcoind unreachable at
   publish time" row, which is covered — nothing calls Core at publish, so that
   one arrives as LND's transport error. This is the pre-flight, several steps
   earlier, with *n* streams already open.

**That decision was owed before the transports slice and it has been made.** See
"The QR decision and the file transport" below. In short: review item 7 was
arguing against a *planned* feature rather than recording a settled one — three
documents listed animated QR as coming — and the owner's answer on 2026-08-24 was
to take it out of the design rather than leave it there unbuilt. All three
documents changed in the same commit as the download leg.

**Held open — set aside, not rejected.** Three ideas are deferred by David rather
than settled, and a later pass must not quietly convert them into "no". A
**pre-signed abort** (review item 5) is blocked on an invariant decision rather
than on a spec: as specified it puts a funding-transaction replacement in this
repository, which the rejected list forbids and I-4 scopes by *authorship*. A
**TUI** is held on audit surface rather than merit — a second front end is a
large spend against "small enough to read end to end", and the review's
`ratatui`/`crossterm` are Rust — but the reason to want one survives for a
headless box beside LND, and `journal.Run` plus `prose.RecoveryList` already
supply the state and the renderer. And a **no-change / spend-all batch**, which
is David's own question: currently refused by the verifier (`ChangeMissing`), and
the cost is the one the gate exists for — no change output means no CPFP lever,
so a slow batch is frozen with no exit this build implements. If it is taken up,
the shape is an explicit per-batch opt-in that fails loudly and says what it
costs, not a lowered floor and not a switch on the verifier.

**I-2 lost a clause it could never have kept.** It used to end "Impossible for
single-sig, which therefore requires a genuinely air-gapped signer — enforce that
in the UI, don't just document it". Nothing enforced it, and nothing can: a
single-sig wallet that signs at all returns a complete transaction, and no check
in a program can establish that a device is air-gapped. That is David's call —
single-sig runs work and nothing refuses one — and it is recorded in `CLAUDE.md`,
`README.md` and `docs/design.html` as a limit on I-2's *reach* rather than a
relaxation. Nothing may hand a *multisig* signer enough to broadcast. The claim
was found by fact-checking the README's own new prose, which is worth repeating
as a habit.

**Two functions have no production caller, and they are named rather than left to
be found.** `internal/server`'s `Registry` used to be the one and `POST /runs`
calls it now; what replaced it on the list is `webrun.Ask` and `webrun.Approve` —
the adapters for `setup.Ask` and `bump.Approve`. Both are built and unit tested,
and their POSTs arrive with the setup and bump screens (item 1.6). That is a
deliberate exception, made for `setup.Ask` in particular because the guard on
"a comparison nobody made is never recorded" is worth having before its screen
rather than after.

**The rule that said "there must be no other publish call site" has changed,
deliberately, and it is now enforced rather than asserted.** There are two calls
to `WalletKit.PublishTransaction` — the funding transaction behind the I-1 gate,
and the CPFP child of a batch that is already public — `Method.CallSites` pins
the count at 2, and the call-site test fails on a third. See "`winthistle bump`"
below for why the second one is allowed to exist and what stops it carrying a
funding transaction.

`winthistle run --stop-before-publish` is the cold probe, and composing it
forced **exactly one branch** — an `if` between `arm.Finalize` and
`arm.Publish`. That was the thing worth watching: the design's claim is that the
probe is the real run with the final call withheld rather than a second path to
the same place, and it survived. See "The composition layer" below.

All four of `docs/design.html`'s open questions are now answered. The two that
were still open both sat past the broadcast line, and both are closed by Phase 2:
`UpdateChannelPolicy` on a pending channel, and the settlement pass as a whole.
The second is closed on regtest only — the first small live batch is still the
thing that settles it on mainnet.

**Three of the design's own claims turned out to be wrong**, and each is written
up below: the shim probe is not free, `minimum_depth` cannot be read by the
initiator at all, and the peers' 2016-block horizon is reachable on regtest in
about ten seconds of mining rather than being out of reach.

Private repo at `AusDavo/winthistle`, goes public once the cold probe passes on
mainnet and the abort paths work.

## What exists

| | |
|---|---|
| `docs/design.html` | the spec — invariants, RPC sequence, phases, hazards, hosting |
| `CLAUDE.md` | invariants as rules, with LND source citations |
| `regtest/` | working cluster: bitcoind + alice + 3 peers + simulated 2-of-2 cold wallet. `make reset` rebuilds and self-tests in ~1 min |
| `internal/lnd` | gRPC client, `PendingChanID`, `ChannelPoint` |
| `internal/bitcoind` | Core JSON-RPC client, coin-lock lock/release, and the descriptor-wallet calls directed mode needs |
| `internal/coldwallet` | directed mode's setup: create the watch-only wallet, checksum and import the descriptors, read back what landed, and end in the round-trip address check. Also the segwit-only coin filter, and step 4's `walletcreatefundedpsbt` |
| `internal/setup` | `winthistle setup`: the install, the resume, and the one question the program cannot answer for itself. Records the answer, keyed on the descriptors it was about |
| `internal/plan` | the batch plan and its verifier — the check LND does not make |
| `internal/prose` | the operator-facing copy primitives: one column, one way to render a satoshi |
| `internal/abort` | `CancelShim`, `AbandonPending`, `Run` — the three abort paths |
| `internal/journal` | the run journal: SQLite, pure Go, `Recover` turns a crashed row into an abort |
| `internal/methods` | the registry of LND RPCs this build calls; `make macaroon` prints the bake command from it, and a gRPC interceptor refuses anything it does not list |
| `internal/reserve` | the anchor-reserve pre-flight: predicts `psbt_verify`'s verdict before a signature is asked for, and says what to do about it |
| `internal/combine` | I-2's only real code: merge the signers' partials, finalize in-app, execute every witness, re-verify against the plan |
| `internal/arm` | the armed window — steps 2 to 9, and one of the two places in the repo that can broadcast |
| `internal/fees` | the batch's fee rate, from Core's `estimatesmartfee` and nowhere else — with a relay floor, a configured floor, and a refusal to guess |
| `internal/peers` | Phase 0's peer pre-flight: the key check, the local gossip graph, and the shim probe that is the only authoritative answer — plus what that probe costs |
| `internal/rehearsal` | Phase 0's dress rehearsal and the measurement the 5:00 abort gate compares against |
| `internal/settle` | Phase 2: the confirmation watch, `UpdateChannelPolicy` polled unconditionally, LND's funding horizon, and the CPFP child |
| `internal/policy` | one channel's forwarding policy: chosen in Phase 0, shown in the plan document, applied by Phase 2. Its own package because those are three packages, and `internal/settle` already imports `internal/plan` |
| `internal/config` | `winthistle.toml`, the batch file and the cold wallet's descriptor file, read by a strict reader that refuses every key it does not know — and refuses `allow_rbf` even when spelled correctly |
| `internal/signers` | how a base64 PSBT reaches a device and a partial comes back: a command, or a file handshake |
| `internal/doctor` | every pre-flight in order, each failure with the command that fixes it. Also the credential check, which asks LND rather than calling things |
| `internal/run` | the composition: Phase 0, the armed window, Phase 2, and the abort that any failure ends in |
| `internal/bump` | `winthistle bump`: find the parent in Core's mempool, build the child, verify it twice, sign it from cold storage, and broadcast it. The second and last publish call site |
| `internal/server` | `winthistle serve`: the loopback socket, the startup token, the `Origin`/`Host` guard, the run-attach registry, the four callback seams' channel, and the three unsafe methods |
| `internal/webrun` | the browser's side of the four seams, and the goroutine that drives `run.Do` behind them |
| `internal/regtestenv` | test support: drives funding streams, signs with the cold wallet's two halves, aborts what it opened, and writes a `winthistle.toml` and a batch file pointing at the harness |

`internal/prose` is a lift-and-shift out of `internal/reserve/report.go`, which
had the only copy of the wrapper and the satoshi formatter. Nothing about the
reserve reports changed; there are just three more screens now that have to line
up in the same pane.

`make check` lints, vets and runs everything. `make test-unit` (`-short`) is the
subset that needs no harness; the harness-backed tests skip with a reason when
regtest is down, so a bare `go test ./...` is honest either way. A fresh clone
needs `make harness` first — `regtest/creds/` is gitignored, so until it exists
every harness-backed test skips.

`make test` passes `-p 1`, and that is load-bearing rather than tidy. Eight
packages now drive the harness — `abort`, `arm`, `bump`, `coldwallet`, `combine`,
`journal`, `plan` and `reserve` — and `go test ./...` runs package binaries
concurrently by default, which puts several test processes through the same
alice. `internal/reserve` makes this sharper than it was: its tests deliberately
lease *every* coin alice has, so a concurrent package would see a node with no
balance and fail over the reserve. Use `make test`, not a bare `go test ./...`,
when the harness is up.

`internal/arm`'s publish test is the first one that spends real regtest coins and
leaves three *open* channels behind. It is no longer the only test that
broadcasts — `internal/bump`'s does too — but it is still the only one that
leaves anything behind; see "`winthistle bump`" below. Nothing can close them — `CloseChannel` is
on the macaroon never-list — so `make harness` is the only way back to a clean
node, and running `make test` repeatedly accumulates open channels and eats the
cold wallet's UTXOs. Both are fine for a while (8 BTC in four coins, ~0.75 BTC a
run) and neither is subtle when it bites.

There is one test that `make test` deliberately does not run:
`TestWhoOwnsTheTenMinuteClock` takes eleven minutes of wall clock and skips
unless `WINTHISTLE_SLOW=1`. It is the measurement behind the countdown, and it
only needs re-running when lnd's reservation timeout might have changed.

`make lint` refuses a tracked executable with no shebang. That is not a style
rule: `/bin/sh` does not decline a file it cannot understand, it interprets it,
which is how `regtest/verify.py` fork-bombed this machine. Recipes now name their
interpreter so nothing depends on a shebang, and the lint stops the next such
file being committed.

`.claude/settings.json` allowlists this repo's loop (`go`, `gofmt`, `make`,
`docker compose`, `sg docker`). Note it is committed and therefore shared, and
`make`/`go` run code from the repo — which is no more than anyone working here
would do by hand, but it is a deliberate choice rather than an oversight.

## Docker on this machine

The login session predates the docker group, so `docker` in a fresh shell gets
`permission denied ... /var/run/docker.sock`. Group membership is fixed at
process start, so wrap it: `sg docker -c "make -C regtest reset"`. A re-login
fixes it permanently. On a new machine: `sudo addgroup --system docker; sudo
adduser $USER docker; sudo snap disable docker && sudo snap enable docker`.

## What the harness runs proved

Seven things, all verified against a running node rather than inferred, not one
of them from documentation. The findings from the happy-path work are in
"Combining in-app" and "The armed window" below, because they are about LND's
enforcement rather than about the harness.

1. **`regtest/verify.py` had no shebang.** It is executable and invoked by path,
   so `/bin/sh` got it, read the backticks around `` `make verify` `` in the
   module docstring as command substitution, and forked ~7100 shells before it
   was killed. `make reset` had therefore never passed, despite commit 704330e
   claiming it was verified end to end. Fixed with a shebang, and the Makefile
   now calls `python3` explicitly so no recipe depends on one again.

2. **Core validates the whole `lockunspent` list before applying any of it**, and
   rejects an entry already in the requested state. One stale outpoint therefore
   frees *nothing* — and since Core's locks are memory-only, a Core restart
   mid-run makes every journalled outpoint stale at once. So the release that
   exists to clean up after a restart was precisely the one that would fail.
   `ReleaseLocks` now filters against the live lock set and reports what it
   actually freed; `LockForRun` does the same on the way in.

3. **Core does not auto-load non-default wallets.** After any bitcoind restart
   every wallet call fails with "Requested wallet does not exist or is not
   loaded", which reads like data loss and is not. `bootstrap.sh` reloads them
   and the tests load what they need, because a real node behaves the same way
   and the abort path has to run on a node that has just come back up.

4. **`shim_cancel` does still succeed after a successful `psbt_verify`.** That
   was an open question in `docs/design.html` and the whole re-arm story depended
   on it. `TestCancelShimAfterVerify` covers it. The intent lives in
   `LightningWallet.fundingIntents`; `psbt_verify` only attaches a verified
   packet to it, so cancelling still finds and clears it.

5. **`psbt_verify` can be refused over the node's own on-chain balance** — see
   the second operations finding below. Observed, with LND's numbers.

6. **`AbandonChannel` is local-only, so the peer keeps its side pending.** After
   `TestBatchArmsEveryChannelBeforeAnythingIsPublished` aborts its three
   channels, alice is clean and bob, carol and dave each still report one pending
   open. `fundingTimeout` in `funding/manager.go` only fires for the responder
   after `DefaultMaxWaitNumBlocksFundingConf` = 2016 blocks from the funding
   height — and on regtest, where nothing mines, that is never. Every abort test
   therefore consumes one of each peer's pending-channel slots, which is why
   `--maxpendingchannels` is now 200 rather than Polar's 10. Consequence past the
   harness: after an abort, a re-arm against the same peer costs another of *its*
   slots, and a peer running the LND default of 1 will refuse. "Re-arming is
   free" holds for cancelled shims, not for channels that already reached
   `chan_pending`.

   **Corrected by the Phase 2 work: "permanently" and "`make reset` is the actual
   cure" were both wrong.** "Where nothing mines" was the load-bearing clause,
   and mining is cheap — 2016 blocks takes about ten seconds on this machine, and
   every peer then times every stale pending channel out at once. Observed:
   bob went from five pending to zero. See "The funding horizon" below.

7. **`make -C regtest info` never worked.** The recipe's inline Python used `\"`
   inside an f-string replacement field; Python 3.12+ parses that backslash as a
   line continuation and raises `SyntaxError`, so the target failed for every
   node. Rewritten with `%`-formatting, which needs no escaping inside the
   recipe's single quotes.

## Two findings that change operations

### `pending_funding_shim_only` declines every channel this app opens

`rpcserver.go` infers "shim funded" from `ThawHeight > 0`, and a plain PSBT open
sets no thaw height, so the safe flag rejects with *"channel … is not externally
funded or not pending"* on the normal path. Observed, not predicted — see the
log line in `TestAbandonPendingShimFundedChannel`.

So the blunt-flag fallback is not an edge case; it is the standard route, and the
operator will be asked to confirm on every abort. That makes the guard around it
the real safety mechanism, not the LND flag:

- `AbandonPending` re-establishes the missing half of LND's check itself, by
  asking `PendingChannels` whether the channel is pending.
- If it is not pending, the call is refused and **no confirmation is offered** —
  there is nothing a human could usefully authorise. `ErrNotPending`.
- `Confirmation` is a func, not a bool, so it cannot be set once and forgotten;
  nil means never escalate.

`TestAbandonRefusesAConfirmedChannel` opens a real channel, confirms it, and
proves a confirmation that says yes still cannot remove it. That test is the one
to keep working — it stands where the design's worst outcome would be.

### `psbt_verify` is judged against the node's own on-chain wallet

`LightningWallet.PsbtFundingVerify` does not stop at verifying the packet. After
`PsbtIntent.Verify` returns it calls `enforceNewReservedValue`, which runs
`CheckReservedValue` over the *now known* inputs and outputs — and that function
sums `ListUnspentWitnessFromDefaultAccount`, which **excludes leased coins**, and
compares it against `RequiredReserve(anchor chans + 1)`: 10,000 sat per public
anchor channel, capped at 100,000.

So step 5 of the sequence can be refused for a reason that has nothing to do with
the batch. The batch's money comes from cold storage; this check is about alice's
own hot wallet. Reproduced deliberately by leasing alice's only UTXO and running
the flow:

```
[DBG] LNWL: Reserved value=0.00010000 BTC above final walletbalance=0 BTC
             with 1 anchor channels open
```

The operator sees only *"reserved wallet balance invalidated: transaction would
leave insufficient funds for fee bumping anchor channel closings"* — which says
nothing about the cold wallet being perfectly fine, and arrives after the cold
wallet has been brought out.

**`internal/reserve` is now the pre-flight for it,** and building it corrected
several things this file used to say. The corrections matter, because each of them
would have produced a pre-flight that disagreed with LND:

- **Not `WalletBalance`.** That RPC sums `ConfirmedBalance` over *every* account
  and reports leased coins as a separate field; `CheckReservedValue` sums
  `ListUnspentWitness` over the **default account only**. On a node with an
  imported account, `WalletBalance` is the larger number, so a pre-flight built on
  it would wave through a batch that verify then refuses. Use
  `WalletKit.ListUnspent(min_confs=0, max_confs=MaxInt32, account="default")`,
  which wraps the very same `ListUnspentWitness` call with the very same
  arguments.
- **Zero confirmations, not confirmed.** `CheckReservedValue` calls
  `ListUnspentWitnessFromDefaultAccount(0, math.MaxInt32)`, and btcwallet's
  `ListUnspent` counts a credit at zero confirmations. A top-up therefore counts
  the moment it hits the mempool. `docs/design.html` said "confirmed on-chain
  balance"; that would have had an operator waiting for a block they do not need.
- **The blocking figure is +1, not +n.** At verify time no member of the batch is
  in the channel database — `CompleteReservation` runs when the peer's
  `funding_signed` arrives, which is after `psbt_finalize` — so every verify in a
  batch sees the same pre-batch count. The +n figure is real but it is not what
  refuses the batch; it is what the node needs afterwards, and below it LND
  declines further on-chain spends and public channel opens. `internal/reserve`
  reports both and blocks only on the first.
- **Private channels are not checked at all.** `enforceNewReservedValue` returns
  before it counts anything when `!isPublic`, and `CurrentNumAnchorChans` skips
  unannounced channels when counting. So the count to pass to `RequiredReserve`
  is the number of *public* members, and an all-private batch is never judged.
  That also answers a `docs/design.html` open question: `RequiredReserve`
  blindly adds `additional_public_channels` to `CurrentNumAnchorChans()` and has
  no idea whether the channels you are naming are public — passing `n` for a
  mixed batch overstates the requirement.
- **A top-up output inside the batch counts at verify.**
  `CheckReservedValue` credits transaction outputs paying to an address the wallet
  owns (`IsOurAddress`), so the design's remedy needs no second transaction and no
  wait. Worth knowing *why* it works, since the alternative reading is that a
  top-up must confirm first.
- **The earlier steps cannot catch it.** `enforceNewReservedValue` is skipped at
  reservation time for a PSBT funder — `enforceNewReservedValue = !isPsbtFunder`,
  `lnwallet/wallet.go` — which is exactly why `OpenChannel` succeeds on a node
  that cannot clear the reserve and the refusal waits for step 5, with the cold
  wallet out and the windows open.

`TestCheckPredictsWhatVerifyDoes` proves the prediction in both directions
against the live node: lease every coin alice has, watch `Check` say
`WouldBeRefused` and `psbt_verify` refuse with LND's own wording; release the
leases, watch both flip back. A check that only ever said "fine" would pass a
one-directional test.

The harness was fragile for the same reason the app was. `bootstrap.sh` gave each
node one 5 BTC UTXO, so anything that leased it dropped the reported balance to
zero. It now funds 5 × 1 BTC, which is also closer to a real node.

## I-1, observed — and now at n = 3

`armBatch` in `internal/abort/abandon_regtest_test.go` opens one funding stream
per peer, builds ONE unsigned transaction carrying all three funding outputs plus
change, runs `psbt_verify` against every stream, then `psbt_finalize` on all
three. It funds from the watch-only cold wallet and signs through the real
two-partials-combined-in-app path (`SignAndCombine`), so the n=3 assertion is
made about the production flow rather than about a fixture shortcut. `TestBatchArmsEveryChannelBeforeAnythingIsPublished` asserts three
`chan_pending` receipts, one shared TXID with three distinct output indices, and
an empty mempool — checked after *every* finalize, including the last, which is
precisely where LND's own "all but the last" idiom would have published. It then
takes the whole armed batch apart again through `internal/abort`.

It behaved exactly as the source reading predicts, and the reading re-checked
clean against `v0.19.3-beta`. `handleFundingSigned` calls
`CompleteReservation(nil, commitSig)`, then the broadcast block gated on
`completeChan.ChanType.HasFundingTx()`, then `WatchNewChannel`, then the
`chan_pending` emission. Two further details make the batch case work rather than
merely not break:

- `PsbtIntent.Verify` locates its own output with `psbt.TxOutsEqual` and requires
  only that the input sum exceed the *total* output sum. It never asserts its
  output is the only one, so n streams can each verify the same n-output
  transaction and each commits to the same unsigned TXID (I-3).
- `handleFundingCounterPartySigs` deletes the funding intent when the reservation
  completes, so by the time `chan_pending` is emitted there is no shim left to
  cancel. The journal's `AbortTarget` relies on that: a pending channel goes to
  `Target.Channels` only, never also to `Target.Shims`.

`armOneChannel` is now the n=1 case of the same fixture, so the single-channel
abort tests and the batch assertion cannot drift apart.

## The run journal

`internal/journal`, SQLite via `modernc.org/sqlite` — pure Go, because the design
promises a single static binary and cgo would break that. Four tables: `runs`,
`channels`, `signers`, `locks`, all `STRICT`. `journal_mode=wal` plus
`synchronous=full`, because "a crash mid-publish leaves artifacts" is exactly the
claim that a committed write reached the disk before the publish RPC went out.

No key material: no seeds, no xprvs, no xpubs, no descriptors, no PSBTs. A signer
is a label and one of `awaiting` / `partial` / `declined`. The finalized raw
transaction *is* stored, because I-1 leaves rebroadcast to us and losing it after
the peers hold their commitment signatures is the worst outcome available.

Two gates live in the journal rather than in the caller, because the journal is
the only component that knows whether every `chan_pending` actually arrived:

- `MarkPending` counts rows and flips the run to `armed` itself. No caller gets
  to assert that the batch is fully armed.
- `MarkPublishing` refuses anything that is not `armed`, and refuses an armed run
  with no finalized transaction on disk. It is the write that must land before
  `WalletKit.PublishTransaction` — if it is not there, the publish did not happen.

`abort.Target` is the seam, as intended. `Run.AbortTarget()` splits the batch the
way `Target` already models it: channels that reached `chan_pending` are
abandoned by outpoint, streams that did not are cancelled by pending channel id,
and unreleased coin locks come along. It **refuses** a run in `publishing` or
`published` with `ErrMayBePublished` — that transaction may be in a mempool or a
block, and abandoning a pending channel whose funding transaction then confirms
strands its funds with no force-close path.

`Journal.Recover` is the entry point: read the row, build the target, write
`aborting` *before* the first RPC, run `internal/abort`, record what happened,
and only reach `aborted` if nothing was left behind. A failed abort stays in
`aborting` so `Unfinished` still lists it. `TestRecoverAbortsACrashedRunFromItsJournalRow`
does this against the live harness, closing and reopening the file first so the
recovery genuinely has nothing but the row.

## Directed mode: the watch-only wallet

`internal/coldwallet` is the whole lifecycle — pre-flight, checksum, create,
import, read back, derive — and it deliberately does not end in a success
message. `Install`'s only successful verdict is `AwaitingAddressCheck`, because
nothing the app can check distinguishes a correct descriptor from a plausible
wrong one.

Four things came out of building it that were not in `docs/design.html`, and
three of them are Core behaving in a way the obvious code would have got wrong.
Four more came out of building its caller — see "`winthistle setup`" below.

### The wrong descriptor does not show a zero balance. It shows a partial one.

The design says a `multi()`-where-you-wanted-`sortedmulti()` wallet "imports
cleanly, shows a zero balance, and tells you nothing about why". Against the
harness's own 2-of-2 it is worse than that.

`sortedmulti` sorts the *derived* pubkeys, so the two descriptors agree at every
index where the keys already happen to be in ascending order — about half of
them for two keys. Measured on two independently generated harness fixtures: 13
of the first 20 addresses agreed on one, 10 on the other, and **index 0 agreed on
both**. So:

- an operator who compared one address would have passed a wrong descriptor;
- the wrong wallet is not empty. `TestSortedMultiAndMultiAgreeOftenEnoughToFoolYou`
  builds it through the real `Install` and it finds **6 of the cold wallet's
  8 BTC**, on both fixtures — the cold wallet's coins sit at low indices, where
  agreement is as likely as not. A plausible, wrong, partial balance is a far
  better disguise than zero.

`DefaultSampleSize` is 5 rather than the design's "first few" for exactly this
reason: five leaves roughly a three per cent chance of a whole sample agreeing
for a 2-of-2, and less for larger quorums.

### `getaddressinfo`'s `ischange` is not the import's `internal` flag

It looks like the read-back and it is not. Core's `IsChange` means "an output of
ours with no address-book entry", so it is true for *any* address that has never
received — external branch included — and false for an internal one that has.
Observed on the harness: receive addresses 0-3 report `ischange: false` because
the cold wallet's coins landed on them, and receive address 4 reports
`ischange: true` because nothing ever did. The first version of the round-trip
check used it and failed on a correct wallet.

`listdescriptors` is the authoritative read-back, and `Confirm` uses it.

While there: `getaddressinfo` answers with the singular `parent_desc` and
`listunspent` with the plural `parent_descs`. `bitcoind.AddressInfo.Parents()`
covers both.

### Core grows a descriptor's range, and then refuses to shrink it

Import with `range: [0,50]` and Core tops the keypool up on its own; a second
import of the same descriptor then fails with *"new range must include current
range = [0,1003]"*. The gap limit is therefore a floor, not a setting — and
`Install` has to be re-runnable, because a half-finished setup is exactly the
state you want to resume. `Import` reads `listdescriptors` first and widens to
whatever Core has grown to.

### The rescan and the prune horizon, and what signet settled

Regtest has no history. A wallet imported with the right birthday and one
imported with a wrong one find precisely the same nothing, and the node cannot be
made meaningfully pruned. `RunPreflight` dates Core's `pruneheight` by reading
that block's header and compares it against the birthday, and `Config.Validate`
refuses a birthday it was not given — both were written and neither was proved.
The regtest tests said so in a named constant rather than by omission, and that
constant now names the file that proves them instead.

**`signet/` is that file's harness**, and it is two bitcoinds: one unpruned for
the rescan, one `-prune=550` so `PrunedPastBirthday` has a horizon to fire on.
No LND — `setup.Deps` has no LND field and `doctor`'s prune warning is
`getblockchaininfo` — so no channels, no peers and no coins of our own.
`internal/signetenv` is the wiring, `WINTHISTLE_SIGNET=1` is the switch, and
`make test` stays green on a machine that has never downloaded signet.

Four tests, in `internal/coldwallet/coldwallet_signet_test.go` and
`internal/doctor/doctor_signet_test.go`. **Three have passed; the fourth is
written and has not yet run — see "One test has not run yet" below.**

1. *Passed.* A birthday before the cold wallet's first coin finds it. That is
   `coldwallet.Import`, blocking for the whole rescan, which is the first thing
   an operator does on mainnet with their real descriptors. Measured: 17s over a
   thirty-day span against 2,000 watched scripts.
2. *Written, not yet run.* **A birthday after it finds nothing, and is otherwise
   indistinguishable.** Same verdict, same derived addresses, same passing
   address check, no error and no warning that could be called a refusal. That is
   not a defect and it is not fixable: Core cannot know a wallet's real birthday,
   and a zero balance is not evidence because a correct wallet that has never
   been paid shows the same zero. It is why `Install` ends in the round-trip
   address check. The test's comment is written for whoever arrives believing
   they have found a hole — read it before "fixing" anything there.
3. *Passed.* `PrunedPastBirthday` fires, for the first time ever. Both directions
   are pinned: the same pruned node is `Ready` for a wallet born after its
   horizon, so the check is a comparison rather than a refusal of pruning.
   Measured horizon: block 318,427, dated six days behind a 319,253-block tip.
4. *Passed.* `doctor` reports a prune horizon, also for the first time.

### One test has not run yet, and the reason is arithmetic

`TestATooLateBirthdayFindsNothingAndLooksExactlyTheSame` needs a birthday that is
both **later than the coin plus Core's two-hour `TIMESTAMP_WINDOW`** and **still
in the past** — `Config.Validate` refuses a future birthday, correctly. The
faucet coin confirmed at 2026-08-25 05:31 UTC, so no such birthday existed during
the slice that funded it.

`signetenv.RequireClearOfTheTimestampWindow` skips rather than running it early,
because a margin that is too thin does not make this test *fail* — it makes it
pass for the wrong reason, which is the worst outcome available for a test whose
whole subject is an indistinguishability. The threshold is the window plus an
hour.

**To finish it**, with the harness up and the coin more than three hours old:

```sh
WINTHISTLE_SIGNET=1 go test -p 1 -count=1 -run TooLate ./internal/coldwallet/
```

If it skips, read the skip message: it says how old the coin is. If it *fails*,
that is a real finding and the test's comment says what to suspect. Nothing else
in the slice depends on it, and `make check` is green without it.

Two things about the harness are easy to get wrong. The coins come from a
**faucet**, because default signet cannot be self-mined — its blocks need the
challenge key — so that step has a human in it; a custom signet was considered
and declined, because a chain we mined ourselves would not test the one thing the
real one does. And **a coin must be more than three hours old** before a birthday
can be placed after it: Core winds a rescan back `TIMESTAMP_WINDOW`, two hours,
from the import timestamp, so a birthday "after" a fresh coin still reaches over
it and the test would prove the opposite of what it says.
`signetenv.OldestCoin` skips rather than allowing that.

## `winthistle setup`, and the four things Core does that the obvious code gets wrong

`internal/coldwallet` was the last written-and-uncalled package in the
repository. `internal/setup` is the caller, and it is a thin composition over
`Install`, `Read`, `DeriveCheck` and one journal table — but building it turned
up four more pieces of Core behaviour, one of which would have made the command
fail on a wallet that was entirely correct.

### It cannot end in success, so the interface is "ask again"

`Install`'s only successful verdict is `AwaitingAddressCheck`, and that is not a
placeholder. Nothing the program can check separates a correct descriptor from a
plausible wrong one: Core parses both, the import succeeds for both, the
read-back is self-consistent for both, and the balance does not separate them
either — the multi()-where-you-wanted-sortedmulti() wallet finds 5.45 BTC of the
harness cold wallet's coins, on a wallet that is wrong.

So the command has two shapes and they are the same screen:

- `winthistle setup --descriptors cold.toml` installs. Idempotent, because
  `createwallet` accepts a wallet that exists and `Import` widens to whatever
  range Core has grown to.
- `winthistle setup` reads the wallet back — `coldwallet.Read`, new — derives the
  addresses from what is actually in it, and asks again.

`deriveaddresses` has no side effect, so the second form can be run as often as
it takes. That is the whole answer to "the operator has gone to find a hardware
wallet": nothing is lost by walking away, and the same addresses are there
tomorrow.

**There is no `--yes`, and there must not be.** Every other prompt in this tool
guards a decision the operator made by running the command. This one is the
operator *supplying evidence* — that they looked at a device and saw the same
addresses — and a flag that answered it would fabricate the evidence. The prompt
is three-way instead: yes, no, and anything-else-means-not-yet. A stray keypress
cannot confirm a wallet nobody looked at, and it cannot condemn a working one
either.

### The answer lives in a new journal table, and it is keyed on what it was about

`setups`, alongside `bumps`: **new tables are the only growth this journal
supports**, because `Open` runs one `CREATE TABLE IF NOT EXISTS` block with no
version table and no migrations, so an `ALTER` would silently not reach a
journal an earlier build wrote.

A file was the alternative and it is worse. This journal is already the tool's
only durable state, already opened by every command, already the one thing the
design tells an operator to back up, already `STRICT` so a typo cannot land as an
integer, and already the home of the other record that must never be rewritten
(`BumpSuperseded`). A second store would be a second format, a second set of
permissions and a second way to be half-written, for nothing.

"Nothing at all" was the other alternative, and it fails on a consumer: `doctor`'s
cold-wallet check previously had to warn unconditionally, because it had no way to
know whether anybody had ever looked. It can only stop warning if somebody wrote
down *which descriptors* were compared.

Which is the load-bearing part. The row stores the descriptors as
`listdescriptors` reports them, the sample size, and index 0 of each branch —
every field a read-back rather than an input. `Setup.Describes` then makes the
record self-invalidating: a confirmation from before a re-import describes a pair
this wallet no longer derives from, and `doctor` says exactly that rather than
believing it or ignoring it. Four verdicts, all tested against the live node:

| journal says | doctor says |
|---|---|
| nothing | **warn** — nobody has compared this wallet's addresses |
| confirmed, this pair | **ok** — confirmed on *date*, *n* per branch |
| answered, a different pair | **warn** — it says nothing about these addresses |
| rejected, this pair | **FAIL** — this wallet must not fund a batch |

The rows are append-only and the *latest* wins whatever it says. A reader that
searched for the newest confirmation would find one from before the descriptors
were corrected and report a wallet as checked when the last thing a human said
about it was no.

Rejected is a `Fail` and unanswered is only a `Warn`, deliberately: a wallet
imported by hand before this command existed is a working wallet that has not
been checked, and failing on it would break a setup that works.

### `winthistle run` reads it too, first, before LND is asked anything

`setup.Check` is the gate and it is now step 0 of Phase 0 — ahead of `GetInfo`,
ahead of the peer pre-flight, ahead of the coin fence. It refuses on **one**
condition: the latest recorded answer is a rejection *and* it describes the exact
descriptor pair the wallet holds right now. Everything else — nobody asked, an
answer about a pair this wallet no longer derives from, a confirmation — is a
line on the screen and not a stop, which is what makes the gate unable to refuse
a wallet somebody has just fixed.

It is first because a refusal there has cost nothing: no stream, no reservation,
no coin lock, and no peer has been told a channel is coming. The regtest test
asserts that as well as the refusal — it fails if the output ever reaches
"Phase 0 — the peers", because a peer that has been asked holds a
pending-channel slot for about eleven minutes whether or not the batch goes on.

There is no flag that overrides it, for the same reason there is no `--yes` on
the prompt. The way past a rejection is to answer the question again
(`winthistle setup`, if the "no" was a slip) or to set up under a new wallet
name; a switch that let a run proceed against a wallet a human had already
disowned would be a switch on the only check in this build that a node cannot
make.

`setup.Check` also refuses everything `coldwallet.Read` refuses — private keys
enabled, a legacy wallet, no active pair, no internal branch — and that is
widening the gate on purpose rather than by accident. Each of those is a wallet
that cannot fund a batch, and each of them otherwise surfaces later and worse:
a missing internal branch becomes "Core cannot derive change", which becomes no
change output, which becomes no CPFP, which is the only acceleration I-4 leaves
available.

### The descriptors and the birthday come from a file, and the birthday is in it

`winthistle example-descriptors` prints it: `[cold]` with `receive`, `change` and
`birthday`. Three required keys, no defaults, and the same strict reader
`winthistle.toml` uses, so a misspelled key is a refusal naming its line.

Not flags. A descriptor is three hundred characters of extended public key, and
`CLAUDE.md`'s rule is the reason: one of them deanonymises the whole cold
wallet's history, permanently. A flag puts it in the shell history of every
machine it is typed on and in `ps` output for every other local user for the
length of the rescan, which on mainnet is hours.

Not `winthistle.toml`. That file is read by every command on every run and holds
the *paths* to secrets rather than secrets. This one is read once, by one
command, and then the descriptors live in Core where they are needed.

The birthday is in the same file rather than beside it because it belongs to the
same wallet — a birthday supplied separately is how the right descriptor gets
paired with the wrong date. One format, `YYYY-MM-DD`, or the literal `genesis`.
`03/04` is refused rather than guessed at: it is two different days depending on
where it was written, and a rescan from the wrong one finds part of the wallet and
reports it as all of it.

`[bitcoind] wallet` is where the wallet name comes from — not a flag of its own.
The wallet setup builds has to be the wallet the run spends from, and a second
place to name it is a second thing to get wrong.

**`.gitignore` grew four lines for it, and two more that were already missing.**
It blocked `descriptors*.json` and `*wallet-export*`, which a file called
`cold.toml` is neither — and `cold.toml` is what the README, the design and
`doctor`'s fix line all tell an operator to create. `batch.toml` went in at the
same time: it is not key material, and it is exactly the set of facts — which
peers, how much, when — that the no-third-party-APIs rule exists to keep off
other people's servers, so it does not belong in a repo either. Nothing tracked
matched either pattern.

### A multipath descriptor is refused, because Core silently keeps half of it

Most wallet software now exports the whole wallet as one line with a `<0;1>`
derivation step. Core 29's `getdescriptorinfo` on one of those returns a
`multipath_expansion` array with both branches, a `checksum` for the multipath
form — and a `descriptor` field holding **the first expansion alone**, under a
different checksum:

```
in:         wsh(sortedmulti(2,…/<0;1>/*,…/<0;1>/*))
checksum:   pq9yk6vg          ← for what was passed in
descriptor: wsh(sortedmulti(2,…/0/*,…/0/*))#pdl90q54   ← what you get back
```

So the obvious use of it — paste the one descriptor into both fields — imports
the receive branch twice, once where it belongs and once as the wallet's change
branch, and the wallet then derives change to the addresses the batch is also
funded from. `importdescriptors` would have refused the multipath form outright
(*"Cannot have multipath descriptor while also specifying 'internal'"*), but it
never sees it: `Prepare` sends what `getdescriptorinfo` returned.

`Config.Validate` refuses it locally, before anything is created, and says what
Core would have done with it. It is fatal rather than a warning because there is
nothing to weigh.

### Core deactivates the descriptor it replaces, and keeps its coins

This is the one that changes what to tell an operator. A Core descriptor wallet
holds **one** active external descriptor and **one** active internal one. A second
active import does not replace the first — it *deactivates* it and leaves it in
the wallet. Observed on Core 29:

```
active= False internal= None  #pdl90q54   ← was active; note `internal` is gone
active= True  internal= False #huwq79j5
```

And the deactivated descriptor's coins are still in `listunspent`. Measured on the
same wallet: 3 coins and 6 BTC before the deactivation, 3 coins and 6 BTC after.
There is no RPC that removes a descriptor from a Core wallet.

So the ordinary recovery path — compare, find them wrong, fix the descriptor,
re-run — leaves a wallet whose balance is partly the wallet the operator meant and
partly the one they rejected, with coin selection unable to tell which found
what. The advice after a rejection is therefore **a new wallet name**, which is
one line of `winthistle.toml`, and `coldwallet.StaleWarning` says so at the point
it is read. `doctor` warns about inactive descriptors for the same reason.

### The round-trip check can fail on a wallet that is entirely correct, twice over

Both are now closed, and the second one is the reason
`AddressCheck.Consistent()` is no longer a gate.

**A gap limit below the sample size.** The import covers `[0, gap limit]` and the
check derives `0` to `sample-1`. An address outside the imported range is not in
the wallet at all: `getaddressinfo` for index 5000 of a wallet imported to
`[0,1132]` answers `ismine: false`, `solvable: false`, no parent descriptor — the
same three flags a wrong descriptor produces, on the same screen, with nothing to
tell the operator which they are looking at. `Config.Validate` now refuses it
before anything is created.

**`parent_desc` is singular, and Core names the wrong descriptor.** A wallet can
hold two descriptors that derive the same address — which is exactly what a
corrected setup leaves behind, because Core cannot remove the old one and
sortedmulti and multi agree wherever the derived keys were already in order. Core
then credits the address to one of them, and on Core 29 that is the key manager
created *first*, active or not.

Measured: a wallet that held a rejected `multi()` pair and then imported the
correct `sortedmulti()` pair reported **three of five receive addresses** as
belonging to the rejected descriptor. `FromImported` was false on a pair that was
entirely correct, whose addresses the real cold wallet owns.

`Consistent()` was going to be the gate in front of the question. It would have
refused to ask about the corrected wallet — the exact case this command exists
for, and the failure that would make an operator distrust a working setup. So:

- `AddressCheck.Recognised()` — `ismine` and `solvable` for every address — is
  the gate. That is the property that has to hold, because an address the wallet
  disowns is not part of the wallet a batch would be built from.
- `AddressCheck.Attributed()` is reported, never gated on. A false one means
  "another descriptor in this wallet derives this address too", and
  `Elsewhere()` names it.
- `Consistent()` is the two together, unchanged for existing callers.

`TestACorrectedSetupCanStillBeConfirmed` walks the whole path against the live
node and asserts the corrected pair is confirmable with attribution broken.

## The batch-plan verifier, and the gap it fills

`internal/plan` emits the plan and checks what comes back. The claim it rests on
is not that LND's `psbt_verify` is weak — it is that `psbt_verify` is *narrow on
purpose*, and the same narrowness that makes the n-of-n batch possible leaves the
rest of the transaction unpoliced.

`PsbtIntent.Verify` at `v0.19.3-beta`:

- finds its own output with `psbt.TxOutsEqual` and sets a flag. It never asserts
  its output is the only one, and it never counts — so an output nobody named
  passes, and so does a funding output paid twice;
- requires only that the input sum exceed the *total* output sum, with the
  comment "we don't want to dive into fee estimation here". Any fee above zero
  passes;
- runs `verifyAllInputsSegWit`, whose first case is `case in.WitnessUtxo != nil:`
  with no look at the pkScript inside it. **Attaching a `WitnessUtxo` to a P2PKH
  input satisfies LND's malleability check.** Only the `NonWitnessUtxo` branch
  reads the script. `plan.IsSegwitSpend` reads it either way.

`TestLNDAcceptsAnOutputTheVerifierRefuses` is the argument, run live: a batch
transaction paying **400,000 sat to an address nobody named** is accepted by all
three `psbt_verify` calls and refused by the plan. `TestTheVerifierAndLNDAgreeOnARealBatch`
is the other half — both say yes to the same bytes, so a clean verification is a
prediction of step 5 rather than a second opinion about it.

Two more checks are ours alone:

- **A `NonWitnessUtxo` that is not the input's previous transaction.**
  `psbt.SumUtxoInputValues` — the function LND uses — reads the attached
  transaction without checking it belongs to the input, so a wrong one makes the
  input total, and therefore the fee, a fiction LND would accept.
- **Replaceability.** I-4 at the byte level: any input below sequence
  `0xfffffffe` is refused. `0xfffffffe` itself is not replaceable and is
  accepted, which is what a wallet uses when it wants `nLockTime` honoured.

### Change, in the mode that cannot name it

Directed mode picks the change address, so the plan names the exact script.
Assisted mode cannot — Sparrow chooses its own — so `plan.Recognition` accepts an
unnamed output as change only when its `PSBT_OUT_BIP32_DERIVATION` entries carry
*every* one of the cold wallet's master key fingerprints, all on the change
branch. That is the same evidence a hardware signer uses to decide an output is
its own change, and it is strictly weaker than naming the script — the report
says so on the line.

### I-4's arithmetic, not I-4's slogan

"Change sized so a CPFP child stays viable" is checked as an actual sum. The
verifier estimates the parent's vsize (upper bound, so the fee rate is a floor —
the conservative direction when there is no RBF), sizes a one-in one-out child
spending the change script, and requires

    change >= (parentVsize + childVsize) * bumpTo - parentFee + dust

with `bumpTo` defaulting to three times the plan's target. On the live three-
channel batch that comes out at 11,270 sat.

**The reasoning behind that sum was wrong, and has been corrected in place.** The
gate is unchanged — the verifier still refuses a batch with no change output or
with change below the floor — but the copy around it used to be written in custody
language, and that was the dangerous part.

`internal/plan` called an undersized change output "a batch with nothing to rescue
it"; the plan report called change "the only way a stuck batch is ever
accelerated". Both true, and both reading as though a stuck batch put coins at
risk. It does not, and an operator who believes it does will reach, under
pressure, for the one thing I-4 forbids.

What is actually true, and now written down in `CLAUDE.md` under "Why the change
output is required" and in `docs/design.html` under "A batch that never confirms":

- **Nothing is at risk while the batch is unconfirmed.** The coins are ours,
  unspent, in a transaction only we could have signed. Every channel reached
  `chan_pending`, so each is recoverable by force-close *once the funding
  transaction confirms* — before that there is no channel, only a promise. What a
  stuck batch costs is the **ceremony**.
- **But it cannot be abandoned either.** An unconfirmed funding transaction never
  becomes safe to abandon on its own: its inputs stay unspent, so it stays valid
  indefinitely, and eviction does not invalidate it. That is why `run.RecoverOne`
  refuses any run that reached the publish call (`journal.ErrMayBePublished`). So a
  batch that never confirms leaves its coins **frozen** — not abortable, not safely
  spendable.
- **CPFP is therefore the exit from that state, not a speedup.** Confirm the batch,
  then close the channels normally. That is what makes the gate proportionate: the
  lever is cheap and what it buys — the ceremony, and an escape from the freeze —
  is not.
- **And it cannot rescue an evicted parent.** `bump` already refuses with
  `ErrParentMissing`; covering that case would need Core's `submitpackage` for 1p1c
  relay, which would be a third path to the network. So the gate protects the case
  a child can actually address.

`docs/design.html` now also documents the only route out of a frozen batch — an
out-of-band double-spend — and the ordering that keeps it from losing funds, with
*do not abandon anything first* as step one. It is documented rather than built on
purpose: **the line is authorship, not knowledge.** No code path here builds a
funding-transaction replacement and none may be added.

**Verified in the same pass, not remembered: `replaceable: false` buys nothing at
the relay layer.** Against the `polarlightning/bitcoind:29.0` container this repo
runs, `mempoolfullrbf` is absent even from `bitcoind -help-debug` — the only RBF
option left is `-walletrbf`, about what the wallet *signals* when sending — and
`getmempoolinfo` reports `"fullrbf": true` with no way to turn it off. Full-RBF is
unconditional, so a higher-fee conflict relays regardless of our sequence numbers.
Both `CLAUDE.md` and `docs/design.html` previously implied the flag protected
something; the flag stays set as a statement of intent, and what actually enforces
I-4 is that only we can sign our inputs and nothing here builds a replacement.

**Considered and rejected in the same session:** making the gate opt-in, and
allowing a no-change "spend all" batch. Both were built, tested green, and
reverted. The gate's cost is small, the freeze has no supported exit, and this
build has not yet opened a real channel — a looser default is not the thing to
ship ahead of the cold probe. What survived is the reasoning above, which is what
the session was actually worth.

### The reserve top-up, proved

`plan.ReserveTopUp` turns an `internal/reserve` finding into an output, aiming at
the larger of the two figures — verify needs the smaller, the node needs the
larger, and the output is being built either way. It returns nothing for an
all-private batch, because `enforceNewReservedValue` never runs for one.

`TestTheReserveTopUpCountsAtVerify` proves the design's remedy against the live
node in both directions: lease every coin alice has, watch `psbt_verify` refuse
with LND's own wording, then add a top-up of **exactly** the shortfall paying a
fresh **p2tr** address the node minted, and watch the same call succeed — with
nothing confirmed and no second transaction. That settles two things the design
asserted: that `CheckReservedValue`'s output credit works from the mempool, and
that it works for a v1 witness program (`ExtractPkScriptAddrs` handles taproot,
and btcwallet's `HaveAddress` recognises it).

## Combining in-app, and what LND does not check

`internal/combine` is I-2's only piece of real code. btcd's psbt package has no
`Combine`, so the merge is ours; what makes it worth reading is that it trusts
neither side. Every returned packet must carry the same unsigned transaction as
the base (I-3, and the refusal names the device); partial signatures are unioned
by pubkey and two different signatures for one key are **refused rather than
chosen between**; witness scripts, redeem scripts, sighash types, derivations and
attached UTXOs are carried forward and a later packet may not overwrite an
earlier one's; and a packet that arrives already finalized is refused outright,
because a finalized input is a complete witness and that device therefore held a
broadcastable transaction.

Then `MaybeFinalizeAll`, `Extract`, **execute every input's witness against its
own script**, and re-run `internal/plan`'s verifier on the transaction that came
out — not on the packet that came in, because the second is what n channels will
depend on. With every witness present the verifier's size is exact rather than an
upper bound, so `Recheck` also asserts that its vsize equals the real one; a
disagreement means one of the two is wrong about the bytes and neither answer is
usable.

### `psbt_finalize` is not a second opinion on I-3

This is the finding that most changes how the earlier sections read.
`PsbtIntent.FinalizeRawTX` compares the outputs with `psbt.VerifyOutputsEqual`
and the inputs' *previous outpoints* with `psbt.VerifyInputPrevOutpointsEqual`,
and stops — the comment says "the fields in the PSBT part are allowed to change".
Sequence numbers, version and locktime are in the wire transaction rather than in
the PSBT part, and none of them is compared. `CompileFundingTx` then takes the
channel point from `i.FinalTX.TxHash()`: the transaction it was just handed.

So a returned transaction whose sequence numbers changed has a different TXID and
**LND would adopt it** — including one that is BIP-125 replaceable, which is
exactly what I-4 exists to prevent. LND does not check the signatures either:
`verifyInputsSigned` only asserts that each input has *something* attached. Both
gaps are ours to close, and both are: the merge refuses a moved unsigned TXID,
and the verifier refuses any input below sequence `0xfffffffe`. The proto's "no
inputs or outputs can change, only signatures can be added" describes LND's
intent, not the extent of its enforcement.

### The finalizepsbt question, answered by not asking it

`docs/design.html` asked whether Core's `finalizepsbt` handles every signer's
output for the descriptor in use, or whether a fallback finalizer is needed.
Nothing in the funding path calls Core: the merge is ours and btcd's
`MaybeFinalizeAll` assembles the witness. `TestOurFinalizerAgreesWithCoresByteForByte`
is the corroboration rather than the answer — on the harness's
`wsh(sortedmulti(2,…))` the in-app result is byte-identical to Core's
`combinepsbt` plus `finalizepsbt`, which it should be, since both derive the
witness order from the witness script and the signatures are deterministic.

btcd's finalizer is narrower than Core's in two ways, and both are checked here
first so the operator hears a sentence about their wallet rather than one about
btcd:

- a P2WSH input's witness script must be a **bare** *m*-of-*n* multisig.
  `getMultisigScriptWitness` → `checkIsMultiSigScript` starts with
  `GetScriptClass(script) != MultiSigTy`. `wsh(sortedmulti)` and `wsh(multi)`
  qualify; a miniscript policy with a timelock does not, and would need a
  finalizer this build does not have.
- it wants **exactly** *m* signatures, not more. `checkIsMultiSigScript` requires
  `numSigs == len(pubKeys) == len(sigs)`, so a 2-of-3 carrying three partials
  fails — with "Unsupported script type", which tells an operator nothing. This
  matters operationally: on a 2-of-3, asking a third device to sign as a
  belt-and-braces measure *breaks the batch*. The refusal now names the counts
  and the devices.

While there: btcd's *deserializer* already runs `PartialSig.checkValid` —
`ParsePubKey` plus `ParseDERSignature` — on every partial signature it reads, and
refuses two records with the same pubkey inside one input. So a malformed partial
cannot reach the merge from parsed bytes.
`TestBtcdRefusesAMalformedPartialSignatureBeforeTheMergeSeesIt` pins that,
because the merge's own length guard is what stands between a hostile packet and
`checkSigHashFlags` reading `sig[len(sig)-1]` if it ever stops being true.

The unit tests need no harness and no Core at all: they build a real *m*-of-*n*
P2WSH spend out of generated keys and sign it, so the claim under test is "this
witness satisfies this script", not "the merge moved bytes around".

## The armed window, and the single publish

`internal/arm` is steps 2 to 9 and one of two places in this repo that can
broadcast — `internal/bump` is the other, and the count is now enforced rather
than asserted; see "`winthistle bump`" below.
Three things enforce I-1 there, deliberately not the same thing said three times:

1. `no_publish` is set in `Open` with no parameter that changes it.
2. `Publish` takes an `*arm.Armed` and nothing else, and `Armed`'s raw
   transaction lives in an **unexported** field that only `Finalize` fills. "There
   is no path to the publish call that skips the gate" is therefore a fact about
   the type system rather than a convention.
3. The journal refuses. `MarkPublishing` declines a run that is not armed, and it
   is the journal that decided the run was armed, by counting its own rows in
   `MarkPending`. This is the one that would still hold if `internal/arm` were
   wrong about everything else, and it is the one
   `TestPublishRefusesABatchTheJournalDoesNotCallArmed` exercises.

`Publisher` is a separate one-method interface rather than a fifth method on
`Client`, because broadcasting is the only action in the sequence that cannot be
taken back and the narrowest way to say so is a type. `bump.Publisher` is
structurally identical and deliberately a *different* type, which is what stops
either publish path being handed the other's bytes.

Two details inside `Finalize` are choices rather than defaults:

- The finalized transaction is journalled **before the first** `psbt_finalize`,
  not after the last. From the moment LND holds it the peers start storing
  commitment signatures against its outpoints, and `no_publish` also gates
  `rebroadcastFundingTx`, so losing the bytes after that point is the worst
  outcome available.
- Channels are finalized **one at a time**, each waiting for its own receipt.
  Issuing all n finalizes and collecting afterwards is marginally faster and
  leaves up to n channels in the state "LND has the transaction and we do not
  know whether it armed". Sequentially there is at most one, and the error says
  which. If a receipt does not arrive, `PendingChannels` is consulted rather than
  guessed at: presence in `pending_open_channels` means the channel is in the
  channel database, which happens in `CompleteReservation` — the same call that
  stores the peer's commitment signature and the one `chan_pending` is emitted
  after. Same fact, different route.

The receipt is also *checked*, not believed: each stream's funding address is
resolved to its output in the transaction and compared against the outpoint
`chan_pending` reports. It costs nothing and it is the only independent
confirmation that LND put the channel where the plan says it is.

`ExportAllChannelBackups` works on pending channels, which the design listed as
something the cold probe would have to verify. It goes through
`chanbackup.FetchStaticChanBackups` over `ChannelStateDB.FetchAllChannels`,
documented as "all open channels ... including pending open". Observed: a
1,808-byte multi-channel backup covering three singles, taken while all three
were pending and before anything was broadcast.

### `PublishTransaction` is still not on the never-list, and now that is a decision

The registry's never-list promises the operator that the baked credential cannot
send coins, send payments, close a channel, sign a message, or widen itself.
`WalletKit.PublishTransaction` is none of those: `walletkit_server.go`
deserializes the bytes, hands them to the wallet, and returns — there is no
signing step in it. So a credential holding it can relay a transaction that is
*already* fully signed and nothing more, and producing one needs a signature this
credential cannot obtain (`SendCoins`, `SendMany`, `SendOutputs`, `SignPsbt` and
`FundPsbt` are all on the list). I-1 is why it has to be there at all, and
WalletKit rather than Core is the route because it also puts the transaction in
LND's wallet-level rebroadcaster — which is what restores the property
`no_publish` took away.

`OpenChannel` moved from `InHarness` to `InApp` with this work, exactly as its
registry entry said it would. `ExportAllChannelBackups` is new. The macaroon the
tool prints is wider by three methods than it was, and every one of them has a
production call site the type-checker can see.

**`winthistle bump` widened the credential by nothing at all.** Both methods it
needs — `PendingChannels` for the countdown and `PublishTransaction` for the
child — were already in the registry for other reasons, so a whole new command
came in without asking the operator for one more permission. What it did add is
`Method.CallSites`, which pins how many production call sites a method may have
and is set only for `PublishTransaction`.

### `SignWithMiner` is gone

`regtestenv.SignWithMiner` signed with one key so the abort fixtures had a
complete transaction, and its own doc comment said it did not model I-2. Now that
`internal/combine` exists there is no reason to keep a weaker path around for
tests to lean on, so it was replaced by `SignAndCombine`: both halves of the
simulated cold wallet return partials, the app merges and finalizes them, and
`testmempoolaccept` confirms the result without relaying. `internal/abort` and
`internal/journal`'s fixtures now fund from the cold wallet rather than the miner
and go through that path, which means the I-1-at-n=3 test is running the real
signing flow rather than a shortcut.

`regtestenv.BuildFundingPSBT` is likewise now a thin wrapper over
`coldwallet.Build` rather than its own `walletcreatefundedpsbt` call, and
`internal/plan`'s test fixture is too. A fixture with its own builder is a fixture
that can drift from the thing it is meant to be testing, and the options that are
not negotiable — `replaceable: false`, `lockUnspents`, `bip32derivs` — now live in
the app, once.

## Phase 0, and the two things the design got wrong about it

`internal/peers`, `internal/fees` and `internal/rehearsal` are the untimed end
before the clock starts. Everything in them is free and repeatable — except one
thing, which is the first finding below.

### The shim probe is not free, and probing then arming collides with itself

`docs/design.html` calls the shim probe "free and abortable" and puts it in Phase
0 for that reason. It is free of fees and free of risk — nothing is signed,
nothing is broadcast, and `shim_cancel` still works after a successful
`psbt_verify` — but it is not free of the peer's patience, and that turns out to
be the constraint that matters.

Reading `handleFundingOpen` at `v0.19.3-beta`, in order: the peer counts its live
reservations for us plus its pending channels with no thaw height, and refuses
with `ErrMaxPendingChannels` if that count already reaches
`--maxpendingchannels`. **LND's default for that flag is 1.** Then it checks max
chan size, min chan size and the channel acceptor. Only after all of those does
it call `InitChannelReservation` and send `accept_channel`.

Two consequences, in opposite directions:

- **A refused probe costs nothing.** Every limit check runs *before* the peer
  creates anything, so a refusal leaves no reservation behind. Probing downwards
  to find a peer's minimum is therefore free, however many times you do it.
- **An accepted probe costs one of that peer's pending-channel slots, and
  `shim_cancel` does not give it back.** `CancelFundingIntent`
  (`lnwallet/wallet.go`) deletes an entry in our own wallet's map, calls
  `intent.Cancel()`, and sends the peer *nothing at all*. The peer's own zombie
  sweeper releases it, after `DefaultReservationTimeout` with up to
  `DefaultZombieSweeperInterval` of slack — ten minutes plus one.

So a Phase 0 that probes all *n* peers and then immediately arms is a Phase 0
that collides with its own probes. Against a peer running the default, step 2 is
refused with *"Number of pending channels exceed maximum"* — on the clock, with
the cold wallet out, for a reason we created ourselves a minute earlier.

**What that means for the flow**, and it is a change to the phase model rather
than a caveat on it:

- **Do not probe as part of arming.** A successful probe *is* step 2 with the
  answer thrown away: same RPC, same `no_publish` shim, same reservation, same
  ten minutes. `peers.Do` is `arm.Open` plus `abort.CancelShim`, which is exactly
  why. If the batch is ready to arm, arm it.
- **Probe as a separate, earlier act**, when a peer's minimum is genuinely
  unknown and the graph proxy is not good enough — then wait out
  `Probe.HoldsUntil` before arming. `peers.ReadyToArm` is the gate, and it
  refuses on the conservative reading because the peer's own limit is not
  observable from here.
- The probe therefore **starts a clock**, which is the one property Phase 0 was
  defined by not having. That is worth saying out loud in the UI rather than
  discovering.

`TestAnAcceptedProbeIsAuthoritativeAndCostsAPeerSlot` asserts both halves against
the live harness, including that the same peer accepts a *second* reservation
straight away — this harness runs `--maxpendingchannels=200`, so the gate is a
judgement about the peer's configuration and not a lock.

### The peer's refusal survives, under a claim that it timed out

`failFundingFlow` forwards the peer's real text only for
`lnwallet.ReservationError`, `lnwire.FundingError` and
`chanacceptor.ChanAcceptError`; everything else becomes *"funding failed due to
internal error"*. So a peer's minimum *does* reach us — `chan size of 0.00001 BTC
is below min chan size of 0.0002 BTC` — which is what makes the probe worth
running at all.

It arrives under two prefixes, and the outer one is a lie. `handleErrorMsg` wraps
**every** peer error in `chanfunding.ErrRemoteCanceled` when the reservation is a
PSBT one — *"if this was a PSBT funding flow, the remote likely timed out because
we waited too long"* — so a peer that refused in 31 milliseconds because the
channel was too small arrives claiming it *"possibly timed out"*. `peers.Unwrap`
strips both prefixes and classifies what is left, and the report says how long
the peer actually took, so the operator is never told a timeout happened when one
did not.

One more thing worth knowing about the amounts: they render through
`btcutil.Amount.String()`, which is `strconv.FormatFloat(v, 'f', -8, 64)` and
therefore **trims trailing zeros**. LND's own `MinChanFundingSize` of 20,000 sat
prints as `0.0002 BTC`, not `0.00020000 BTC`. A parser that assumed eight decimal
places reports a minimum of zero.

### `estimatesmartfee` never answers on regtest, and it is not a fault

There was no fee source in the repo at all; `plan.Fee.TargetSatPerVB` came from
the caller and every test hardcoded 10. `internal/fees` is it, and Core is the
only source — a fee API is handed the size of what is being built and the moment
it is being built, which is most of what this tool exists not to leak.

`estimatesmartfee` does not fail on a node with no block history. It *succeeds*,
returns no `feerate` field at all, and puts `Insufficient data or no feerate
found` in an `errors` array. A client that read the absent field as a rate would
build the batch at zero sat/vB; one that read the array as an RPC failure would
refuse to build on a node that is working perfectly well. Both are wrong, and the
state is the ordinary one for regtest, for a freshly synced node, and for one
that has been offline.

Note what the harness's `-fallbackfee=0.0002` does **not** do here: it is a
wallet setting, consulted by Core's own coin selection, and `estimatesmartfee`
never looks at it.

So the rate is the largest of three figures, and `Rate.Source` says which won:
Core's estimate, the node's relay floor (the higher of `mempoolminfee` and
`minrelaytxfee`), and a configured floor from `winthistle.toml`. **A node with no
estimate and no configured floor is an error, not a guess** — I-4 forbids
replacing the funding transaction, so a rate chosen badly costs a CPFP child and
another cold-wallet session.

`CONSERVATIVE` is the default mode, which is not the usual choice. The two modes
differ in how willing they are to believe a recent fall in fees; for an ordinary
payment the economical mode is right because a payment that lags can be replaced.
This transaction cannot be.

### The dress rehearsal, and the gate that existed only in the config block

`limits.abort_after_signing_seconds = 300` was in `docs/design.html`'s config
block and nowhere else. `internal/rehearsal` is the measurement and
`rehearsal.Gate` is the refusal.

The decoy mirrors the batch's *shape*, not merely its value: one output per
member at the same amount, paying fresh addresses of the cold wallet's own, at
the same fee rate and the same confirmation floor. Both halves of what a hardware
device spends its time on — inputs to sign, outputs to display — therefore come
out the same, which is what makes the measurement a prediction rather than a
stopwatch reading.

Two choices in it are deliberate:

- **The measured number is the signing round alone**, from handing the first
  device the PSBT to the last partial coming back. The build and the merge happen
  off the peers' clock and must not count against a gate that exists to protect
  the window.
- **`Measurement` carries no transaction.** By the end of `Run` there is briefly
  a fully signed transaction spending the coins the real batch is about to use.
  It pays them straight back to cold storage, so publishing it would lose
  nothing — but it would spend the batch's inputs, and the batch would then fail
  at build time or, worse, at `psbt_verify` with the cold wallet already out. So
  the bytes do not leave the package, the way `arm.Armed` keeps its raw
  transaction unexported, and Core's coin locks are released on every path.

`rehearsal.Gate` refuses on three counts, not one: a device that did not return a
usable partial, a decoy `testmempoolaccept` would not have taken, and a round
slower than the limit. The first is the more important — a signer that produces
malformed witnesses is one of the two failures the phase exists to catch.

### The reserve check needs the announced count, and now gets it

`internal/reserve` was already Phase 0's reserve half and already knew that
private members are invisible to LND's check twice over. What was missing was the
wiring: `arm.Streams.PublicCount()` was exported and nothing called it, and no
test opened a private channel, so the whole private path was written and
unexercised.

It is wired now. `arm.BatchOf` counts a planned batch the way `reserve.Check`
needs it, `Streams.Batch()` counts the streams that actually opened the same way,
and `reserve.Finding.StillApplies` compares the two: a Phase 0 finding is about a
particular count of announced channels, and a batch that changed shape between
the check and the arm has a finding that no longer describes it. That comparison
costs no RPC and the armed window is where a stale figure would first do damage.
`TestAPrivateMemberIsInvisibleToTheAnchorReserve` opens a mixed batch of three
against the live node and asserts the counts on both sides of it.

## Phase 2, and what LND will not tell an initiator

`internal/settle` is the settlement pass: from the single publish to
active-and-policied. Nothing in it can lose money — the transaction is public and
every channel is already recoverable — so what it watches is fees, time, and the
one hazard past the end.

### `UpdateChannelPolicy` on a pending channel does not fail

`docs/design.html` left this open — "will it accept a channel point that is
pending but not yet active, or must Phase 2 poll?" — and defused it by polling.
The instinct was right and the answer is worse than either option the question
offered.

The call **succeeds**. `localchans.Manager.UpdatePolicy` walks the graph's
outgoing edges, finds no edge for the channel, falls through to `FetchChannel`,
sees `IsPending`, and appends a `FailedUpdate` with reason
`UPDATE_FAILURE_PENDING` and the text `not yet confirmed`. `rpcserver.go` then
returns that list inside a `PolicyUpdateResponse` **with a nil error**.

So a caller that checked only `err` would record a policy it had not applied, and
go on routing at LND's defaults — 1000 msat base and 1 ppm, from
`chainreg.DefaultBitcoinBaseFeeMSat` and `DefaultBitcoinFeeRate` — believing
otherwise. `settle.ApplyPolicy` exists as a function rather than an inline call
for exactly this: `Applied` is only ever true when `failed_updates` came back
empty. `TestUpdateChannelPolicyRefusesAPendingChannelInsideASuccessOK` asserts it
against the live node, and asserts LND's side of it first so the finding is about
LND rather than about us.

`UPDATE_FAILURE_PENDING`, `NOT_FOUND` and `INTERNAL_ERR` are retried;
`INVALID_PARAMETER` is not, because waiting cannot make a CLTV delta of 4 valid
and a loop that retried it forever would leave a channel routing at 1 ppm with a
spinner beside it.

### `minimum_depth` cannot be read by the initiator at all

This one contradicts the design in two places. Phase 0 says to "check min and max
channel size, accepted commitment type, and `minimum_depth`" and to "show the
user *usable after k confirmations* per channel, up front". Phase 2 says to
"track depth against each peer's `minimum_depth`". Neither is possible.

The peer states `min_depth` in `accept_channel`. LND stores it as
`OpenChannel.NumConfsRequired` and exposes it over **no RPC**: grepping
`lnrpc/*.proto` at `v0.19.3-beta`, `min_accept_depth` appears exactly once, on
`ChannelAcceptResponse` — the *responder's* side of somebody else's channel.
`PendingChannels` does not carry it either; its `reserved 2` is a former
`confirmation_height` field and there is a standing `TODO(roasbeef): need to
track confirmation height`.

Three things stand in, in descending order of certainty:

1. **The observed depth.** When a channel first appears in `ListChannels`, the
   number of confirmations it had at that moment is the peer's `minimum_depth`,
   from above. It is authoritative, it is an upper bound because the loop polls,
   and it arrives far too late to plan with — which makes it exactly the right
   thing to record for the *next* batch. `settle.State.ObservedDepth` is written
   once and never revised: on a later pass the transaction is merely older.
2. **`settle.ExpectedDepth`,** which reproduces the `NumRequiredConfs` closure
   wired up in `server.go`: 6 above `MaxFundingAmount`, otherwise
   `6 * stake / MaxFundingAmount` clamped into `[3, 6]`. It is a prediction about
   a peer running stock LND and the reports say so. For the harness's 250,000 sat
   fixtures it predicts 3, and the live test observed 3.
3. Nothing else. There is no gossip field, and no third party may be asked.

The depth count itself comes from Core, not LND — `gettransaction` falling back
to `getrawtransaction`. Without Core the settlement still works, because a
channel leaving `pending_open_channels` is the authoritative signal and needs
nobody's help; the operator just sees "not yet" instead of "2 of an expected 3".

### The funding horizon, and how cheap it is to reach

A funding transaction that never confirms is not a stalemate, it is asymmetric.
After `lncfg.DefaultMaxWaitNumBlocksFundingConf` = 2016 blocks from the broadcast
height, the **responder** gives up: `waitForFundingWithTimeout` starts
`waitForTimeout` only `if !ch.IsInitiator && !ch.IsZeroConf()`, and `fundingTimeout`
then closes its side as `FundingCanceled`. Our node — the initiator — waits
forever, by design and with a `TODO` next to it. If the transaction then confirms
we hold an open channel the peer has forgotten, and the only way out is a
force-close.

LND publishes the countdown, which the design did not know: `PendingChannels`
reports `funding_expiry_blocks`, computed in `rpcserver.go` as
`waitBlocksForFundingConf + pendingChan.BroadcastHeight() - currentHeight`, and
its own proto says *"a negative value means the channel responder has very likely
canceled the funding"*. `settle.State.ExpiryBlocks` is that number and the report
turns it into a warning at 432 blocks and an urgent one at 144.

**Reachable on regtest, and quickly.** `TestTheFundingHorizonIsReachedByMining`
arms one channel, never publishes it, and mines 2016 blocks: about ten seconds on
this machine. Measured, to the block:

- at exactly 2016 blocks `funding_expiry_blocks` is **0**, not 1 — the boundary
  is inclusive on both sides, since `waitForTimeout` fires on
  `epoch.Height >= maxHeight`. Zero already means gone.
- one block later it is **-1**, and bob has dropped the channel entirely.
- alice still lists it as a pending open, and still will. The test asserts that
  too, because if the initiator ever *did* start timing out, the hazard this
  whole section describes would have changed shape.

The side effect is the correction to finding 6 above: mining past the horizon is
also the cheapest way to clear the pending channels that aborted batches leave on
the peers, and it is far quicker than `make harness`.

### The CPFP child, and the arithmetic Core already does

I-4 says the batch can never be replaced, so a batch that is underpaying can only
be accelerated by spending its own change. `internal/plan` already computed the
floor that keeps such a child viable (`ChangeFloor`, `DefaultCPFPMultiple`);
`settle.BuildChild` is where one actually gets built.

The finding here is Core's, not LND's. **`walletcreatefundedpsbt`'s `fee_rate` is
not the child's own rate when the child spends an unconfirmed input.** Core
accounts for the unconfirmed ancestors and charges the fee that lifts the whole
package to the rate it was given:

    fee = ceil(rate * (parentVsize + childVsize)) - parentFee

which is, term for term, `plan.ChildFeeSat` — the expression `ChangeFloor` is
built out of. Measured against Core 29 at 50, 200 and 600 sat/vB on a 7,007 vB
parent paying 1 sat/vB: exact agreement at all three, to the satoshi.

So `BuildChild` asks Core for the package rate and then checks the answer with
its own arithmetic, which is the relationship `internal/plan` already has with
`psbt_verify`. It is one call, not two. Three options in it are load-bearing:

- `add_inputs: false`. A second, already-confirmed input would make a cheaper
  child and would also let a miner take the child without the parent, which is
  the one thing a CPFP child must not allow.
- `subtractFeeFromOutputs: [0]`. The only input is the change and the output
  already claims all of it, so there is nowhere else the fee can come from — and
  it is what stops Core adding a change output of its own.
- `include_unsafe: true`. Core's "unsafe" is a heuristic about provenance: an
  unconfirmed output is safe only if every input of the transaction that created
  it belongs to this wallet. That holds for a batch the cold wallet funded and
  does not hold in general. The input here is named explicitly, so there is no
  coin selection for the heuristic to protect, and without it a batch whose
  parent Core distrusts could not be accelerated at all — which under I-4 leaves
  it with no remedy.

The reported package rate is a **floor**, not an estimate: the child's size is
measured with `plan.SizeOf`, which uses the verifier's upper bounds, so the
transaction pays at least that much per virtual byte. `RateTolerance` is 2%,
which covers the gap between our 73-byte signatures and Core's dummies and
nothing else — a genuine failure to bump misses by orders of magnitude.

When Core refuses, it says only *"The transaction amount is too small to pay the
fee"*, which names neither the shortfall nor the rate the change *could* reach.
Both are what the operator needs, since "a smaller lift" is the remaining option.
So the refusal is re-derived: the child's size is computed from the change
output's own `scriptPubKey` and `witnessScript`, read back from `listunspent`, via
`plan.ChildVsize` — the same estimate the verifier used when it decided this
change output was big enough. No second build is needed and none is made.

The child is returned **unsigned**, and that is the shape of it: the change
belongs to cold storage, so accelerating the batch costs another signing round.
There is no shortcut that does not amount to a hot key able to spend the batch's
change.

### The recovery screen

`CLAUDE.md` says the recovery screen is the highest-stakes copy in the product
and should be written before the happy path. The happy path arrived first; that
debt is paid in `internal/prose/recovery.go`.

Three things decide everything the operator does next, and all three come out of
the journal without asking LND anything — so it is the same screen on a node that
is down:

- **whether the run's transaction might be public.** `RecoveryList` flags it and
  `Recovery` refuses outright, in the register the situation deserves: this is
  the one combination in the design that loses money, and the screen says so and
  then says what to do instead (look for the txid; re-broadcast from the
  journal; do not abandon; do not replace).
- **which channels reached `chan_pending` and which did not** — one side is free
  to cancel, the other costs a peer slot for 2016 blocks, and the screen prices
  both before the operator presses anything.
- **what an abort will not clean up.** It is local. The peers keep their side, and
  telling somebody their node is clean when their peers are not is how a second
  incident starts.

`BluntConfirmation` is the only prompt in the product where a human authorises
something that could lose funds if the premise were wrong, so it says what the
premise is, why LND's refusal of the safe flag is expected rather than alarming,
what the fallback gives up, and what has already been checked on their behalf.

The copy is tested for what it must say and for the pane it must fit in.
`TestTheRecoveryScreensStayInThePane` caught a real overrun: a 64-character txid
beside a label does not fit 78 columns, so txids and addresses now get their own
line at a small indent throughout.

## The composition layer

Every package existed and nothing called them. `prose.RecoveryList`,
`journal.Unfinished`, `rehearsal.Gate`, `peers.ReadyToArm`, `fees.Estimate`,
`settle.Settle` and `reserve.Finding.StillApplies` all had zero non-test
callers, and the only place the whole sequence was assembled was `drive()` in
`internal/arm/arm_regtest_test.go` — a fixture, which cannot ship. That is what
`internal/config`, `internal/policy`, `internal/signers`, `internal/doctor` and
`internal/run` are: four commands' worth of wiring and no new mechanism.

`settle.BuildChild` was the last of *that* list with no caller, and
`internal/bump` is now that caller.

`internal/coldwallet`'s setup path was the last package on nobody's list.
`Install`, `Confirm` and `RunPreflight` had test callers only; `internal/setup`
is now the caller, and there is no written-and-uncalled code left. See
"`winthistle setup`" above — it was not the wiring job the rest of this list was.

### `winthistle.toml`, and the two keys nothing read

`internal/config` reads the design's block, with a reader that is deliberately
not a TOML parser. Strictness is the feature: **every unknown section and every
unknown key is an error naming its line.** The file is read once, at the start
of an evening that may end with a cold wallet on the table, and the failure
worth engineering against is not the malformed file — that one announces itself
— but the well-formed file with `abort_after_signing_seconds` misspelled,
running to completion on a default nobody chose.

Two keys were named by code and read from nowhere, and both are now read:

- `limits.abort_after_signing_seconds` defaults to
  `rehearsal.DefaultAbortAfterSigning` rather than to 300 written a second time,
  and `winthistle run` passes it to `rehearsal.Run` as the gate.
- the fee floor is `[fees] floor_sat_per_vb`, which the design's block did not
  have at all. **It has no default and it must not get one.** Zero means the
  operator set none, which is legal and is exactly the state that makes a node
  with no estimate an error rather than a guess. `doctor` warns when it is unset
  even on a node that has an estimate today.

**`allow_rbf` is refused, including when it is set to `false`.** It was in the
design's own config block, so an operator will paste it. Honouring it would be a
lie; ignoring it silently is worse, because somebody who writes `allow_rbf =
true` and watches the run proceed will reasonably conclude it was honoured. So
the reader refuses the line, says why, and the design doc no longer offers it.

The bind check is the other thing enforced here: a wildcard bind is refused
because `0.0.0.0` is not an interface anybody chose, while an explicit
non-loopback address is accepted with a warning — that is the design's "unless a
config key explicitly names the interface", read literally.

### The per-peer policy table

`settle.Policy` had no chooser; the tests hand-built one. It is now
`policy.Policy`, in a package of its own, because three packages need it and two
of them cannot import each other — `internal/settle` already imports
`internal/plan` for the CPFP arithmetic. `settle.Policy` is an alias, so nothing
in Phase 2 changed.

The batch file is where it is chosen: a `[policy]` block every `[[channel]]`
inherits and may override key by key. The plan document renders it **beside the
amount**, which is the point: a channel's capacity and what it will charge to
route are one decision, and after that document is approved nobody is asked
again. A channel with no policy is not neutral — it is 1000 msat and 1 ppm — so
the document says that in full sentences rather than leaving a blank.

`plan.Outputs` now validates every policy, which is where a bad one has to be
caught. Phase 2 is too late: `UpdateChannelPolicy` reports an invalid CLTV delta
*inside a successful response*, the transaction is public by then, and the
channel routes at 1 ppm until a human notices.

### `winthistle doctor`, and how the credential is checked

Ten checks, in order, each failure carrying the command that fixes it: the
config file, LND, the macaroon, Core, the cold wallet, the coins, the anchor
reserve, the fee rate, the peers (with `--batch`), and the journal — including
Core's coin locks that no run in the journal claims, which is what a crash
between `walletcreatefundedpsbt` and the journal write leaves behind.

The cold-wallet check now reads the answer to the round-trip address check out of
the journal and has four verdicts about it, one of them a refusal — see
"`winthistle setup`" above. Its `bitcoin-cli importdescriptors` fix line is gone;
it prints `winthistle setup` instead. `doctor` also opens the journal once, at the
top of `Run`, rather than inside `checkJournal`: two checks need it now, and
opening it twice would report the same failure in two places.

The credential check is the part with a finding in it.

**`lnrpc.CheckMacaroonPermissions` answers the question without calling
anything.** The obvious way to find out whether a macaroon authorises a method
is to invoke it, and for most of this build's list that is a bad idea: the
macaroon interceptor runs before the handler, so a *refused* method costs
nothing, but an *allowed* one runs, and `AbandonChannel`, `FundingStateStep`,
`OpenChannel` and `PublishTransaction` are not things to try with junk arguments
to see what happens. `CheckMacaroonPermissions` takes the macaroon to examine in
the request rather than using the caller's, and `macaroons.Service.CheckMacAuth`
then runs exactly the check the interceptor would — the coarse ops first, then
the `uri:<full_method>` form this tool's credential actually carries. No handler
is reached. It earns one registry entry, `macaroon:read`, whose only method is
that read-only self-check.

That also checks the half nothing else could: **the never-list, against the
file**. The registry promises the credential cannot send coins, close a channel,
sign a message or widen itself, and that is a promise about the operator's
macaroon rather than about this build's intentions — an operator pointing the
tool at `admin.macaroon` has broken every part of it while everything still
works. `Capability` now carries the LND ops each forbidden method needs, and
`doctor` asks about all ten. The harness test asserts exactly this: the harness
credential is a copy of `admin.macaroon`, so `doctor` is *expected* to fail, and
it fails naming "send coins on-chain", "close a channel" and "bake itself a
wider credential".

**The two refusals that look alike.** Both carry bakery's plain "permission
denied" text and they mean opposite things:

- `codes.InvalidArgument` from `CheckMacaroonPermissions` is the *answer*: the
  macaroon being examined does not authorise that method.
- an untyped error with the same text is LND's interceptor refusing *our* call.
  `bakery.ErrPermissionDenied` is an `errgo` error with no gRPC status attached,
  so it arrives as `codes.Unknown`. It means the configured credential is too
  narrow to run the check at all — which is itself the diagnosis: it was baked
  before this build existed.

Matching on the code alone confuses them and matching on the text alone does
too. `doctor.tooNarrow` matches both, and `TestTheTwoRefusalsThatLookAlike` pins
it.

### `winthistle run`, and the one branch

`run.Do` is `drive()` with the gates actually wired: `setup.Check` first, before
LND is asked anything, `rehearsal.Gate` and `peers.ReadyToArm` before
`arm.Open`, `reserve.Finding.StillApplies` once the streams are open, and the
journal at the publish call. `--probe` is opt-in and
then waits out `Probe.HoldsUntil` before arming, because a successful probe is
step 2 with the answer thrown away and the peer holds that reservation for about
eleven minutes.

**`--stop-before-publish` forced exactly one branch**, and it is worth being
precise because the cold probe's whole value rests on it. Everything through
`arm.Finalize` — the streams, the plan, the build, our verifier, *n*
`psbt_verify`, the signing round, the merge, `testmempoolaccept`, *n*
`psbt_finalize`, *n* `chan_pending`, the backup export — is unconditional, and
the flag is read once, in one `if`, between `arm.Finalize` and `arm.Publish`.
It is read nowhere else. The type system carries the rest: `Publish` takes an
`*arm.Armed` whose raw transaction is in an unexported field only `Finalize`
fills, so there is no second way to reach step 9 to be tempted by.

What differs *after* the branch is a fact rather than a path: nothing was
published, so the run ends through the abort path — which is the other half of
what the probe proves, and the reason the build order was inverted.

**Any failure inside the armed window ends the same way.** Nothing has been
broadcast, so the answer is always cancel the shims, abandon what reached
pending, release the locks — and `run` takes it rather than printing a
suggestion, through `journal.Recover`, which is the same call `winthistle
recover` makes and which refuses outright to abort a run that reached the
publish call. `arm.Open`'s partial failure is journalled *before* the error is
returned, because a stream that opened has a pending channel id that is the only
handle able to release it.

`winthistle recover` with no argument is `journal.Unfinished` plus
`prose.RecoveryList`; with a run id it is `prose.Recovery`, then
`journal.Recover` with `prose.BluntConfirmation` as the prompt. That copy has
now been shown to something: it is what the regtest tests authorise through, and
it is what an operator sees on every abort, because LND's safe flag rejects
every channel this app opens.

### Signers, and the transport seam

`run.Deps.Signers` is an interface with one useful method, `Round(name)`,
returning `[]rehearsal.Device` — the same `rehearsal.Signer` the dress rehearsal
measures, which is what makes the measurement a prediction about the real round
rather than about a different code path. `internal/signers` implements two
transports: a command from `winthistle.toml` reading a base64 PSBT on stdin, and
a file handshake that writes a file, prints what to do with it, and waits.

The round's *name* is in the file names, and stale answers are deleted before
the question is written. That is not tidiness: the rehearsal and the batch are
two rounds minutes apart, and a signed file left over from the first and picked
up by the second is a signature over the decoy — which `internal/combine` would
refuse at the worst possible moment, with a message about a moved txid rather
than about a leftover file.

### Two smaller things found while wiring it

- **`ExportAllChannelBackups` is node-wide, not batch-scoped.** Obvious in
  hindsight from the name, and it matters for what a test may assert: on the
  harness the snapshot covers all 33 channels alice has, not the 2 in the batch.
  Assert "at least the batch", never "exactly n".
- **`testmempoolaccept` had three copies.** It is now
  `bitcoind.Client.TestMempoolAccept`, called by `internal/rehearsal` and
  `internal/run`. A second function that takes a raw transaction and talks to
  Core is a second thing to check when reading this repo for broadcast paths,
  which is the review CLAUDE.md's "no other publish call site" rule invites.

## `winthistle bump`, and the ceiling nothing had met

`internal/bump` is the CPFP child from end to end: find the parent, build the
child, verify it, sign it from cold storage, verify what came back, broadcast it.
`settle.BuildChild` did the arithmetic already and had no caller; what was
missing was everything around it, and that turned out to be most of the work —
because the change output belongs to cold storage, so a bump is a **second
cold-wallet session** with its own transport, its own verification and its own
journal rows.

Three decisions were made before any of it was written, and the first was a
change to a rule rather than a reading of one.

### The rule about publish call sites changed, and is now enforced

CLAUDE.md's rejected list said *"There must be no other publish call site."*
**Nothing enforced it.** `TestEveryLNDCallSiteIsRegistered` groups call sites by
method into a `map[string]*usage` and asserts non-empty in both directions; it
never counts, so a second production caller of `WalletKit.PublishTransaction`
passed silently and the only thing that became false was a sentence in a comment.
The claim was made in five prose comments and checked in none.

The rule now says what it actually protects — no broadcast that could carry a
**funding** transaction outside the I-1 gate — and there are exactly two publish
call sites:

1. `arm.Publish`, the funding transaction, behind the gate.
2. `bump.Publish`, the CPFP child of a batch that is already public.

The second needs no gate and cannot be given one: by the time a child can be
built the parent is in a mempool, every channel reached `chan_pending` before
that happened, and the child spends the batch's change — an output no channel
depends on. There is no "early" for it to be published in.

Two things keep them apart, and neither is a convention. `arm.Publish` takes an
`*arm.Armed` and `bump.Publish` takes a `*bump.Signed`; each has its raw
transaction in an **unexported** field that exactly one constructor fills, after
that constructor's own checks, so neither line can be handed the other's bytes.
And `methods.Method.CallSites` pins the count at 2, with the call-site test
failing on a third and naming what has to change with it. `CallSites` is zero —
unconstrained — for the other nineteen entries, deliberately: for those, "is it
called" is the right question, and a test that failed when a function was split
in two would be a refactoring tripwire rather than a safety check.

### A CPFP child has a relay ceiling, and it is reachable on mainnet

**This is the finding.** A child published through WalletKit is refused above
**10,000 sat/vB of its own fee rate**, and nothing in this build can raise it.
The path, at `v0.19.3-beta`:

```
WalletKit.PublishTransaction        → w.cfg.Wallet.PublishTransaction
BtcWallet.PublishTransaction        → b.chain.TestMempoolAccept(txs, 0)
                                      // comment: "a max feerate of 0 means the
                                      // default ... 0.10 BTC/kvb, or 10,000 sat/vb"
                                    → w.wallet.PublishTransaction
wallet.publishTransaction           → chainClient.SendRawTransaction(tx, false)
                                      // false is allowHighFees, hard-coded
rpcclient.SendRawTransactionAsync   → maxfeerate: defaultMaxFeeRate = 0.1 BTC/kvB
```

Core's own help agrees from the other end, and adds a limit on the limit:
`maxfeerate` defaults to `"0.10"` BTC/kvB, *"Set to 0 to accept any fee rate"*,
and *"Fee rates larger than 1BTC/kvB are rejected"* — so the escape is passing
**zero**, not passing something large.

**Why this bites a child and nothing else.** A child concentrates the whole
package's lift into about 150 virtual bytes, so its own rate is roughly
`parentVsize / childVsize` times the package target — a multiplier of about
**47.7** on a three-channel batch. Measured: a 250 sat/vB package target on a
7,007 vB parent needs a 150 vB child paying 1,782,243 sat, which is
**11,882 sat/vB** and refused. That is a package target an operator could
reasonably ask for on a busy day, and it gets *lower* as the batch gets bigger.
The funding transaction never comes close — it is thousands of virtual bytes
paying its own rate — which is why nothing had met this before.

`bump` refuses that lift **twice before a device is asked for anything**: once
from the change output's own scripts before Core is called, and once in the
verifier. Discovering it after a cold-wallet signing round is exactly the failure
this project engineers against. `ceilingDetail` names both ways out — a smaller
lift, or `bitcoin-cli sendrawtransaction <hex> 0`, which is the same bytes by a
route that takes an argument and gives up only LND's rebroadcaster.

Worth knowing: `bitcoind.Client.TestMempoolAccept` passes no `maxfeerate`
either, so **our own pre-flight applies the same ceiling the broadcast will**.
That is defence in depth by accident rather than design, and it is left alone —
Core's message there is `max-fee-exceeded`, which names neither the ceiling nor
the way out, which is why the earlier check exists.

`TestCoreEnforcesTheRelayCeilingAndZeroLiftsIt` proves the far end against the
node rather than taking the source reading on trust: a 192 vB transaction paying
25,938 sat/vB, refused with `max-fee-exceeded` at the default and accepted with
`maxfeerate 0`. Nothing is broadcast — `testmempoolaccept` validates without
relaying, so the absurd fee is never paid.

### The arithmetic is about the ancestor package, not the transaction

`walletcreatefundedpsbt` charges the fee that lifts the whole unconfirmed
**ancestor package** to the rate it is given, and "the ancestor package" means
all of it. `getmempoolentry`'s `ancestorsize` and `fees.ancestor` are documented
as *"including this one"*, so for a batch funded from confirmed coins they equal
the transaction's own figures — the ordinary case, and exactly why the difference
is easy to miss. `bump.Locate` uses the ancestor figures when `ancestorcount > 1`
and the report says it is doing so. Passing the parent's own size there would ask
`plan.ChildFeeSat` a different question from the one Core answers, and the
disagreement would be reported as Core misbehaving.

Everything else about the parent is read rather than remembered, for the same
reason: a transaction being bumped is unconfirmed by definition, so Core holds
its exact size and fee and summing prevouts would be a second opinion that could
quietly disagree with the one the fee market uses.

### The child lives in its own journal tables, not in `runs`

Three new tables — `bumps`, `bump_signers`, `bump_locks` — each mirroring the
shape of its batch counterpart, keyed `(run_id, seq)`.

A second row in `runs` was the alternative and it does not work. `runs` is the
I-1 gate's state machine: `Begin` refuses a run with no channels, so a bump row
would need fictional ones; `Unfinished` would list it; `Run.AbortTarget` would
build a batch abort for it; and `Recover` would run `abort.Run` over it. Three
special cases in the recovery path, which is the one path whose value comes from
being uniform.

Extra columns on `signers` and `locks` do not work either, and that one is a fact
rather than a preference: `journal.Open` runs a single
`CREATE TABLE IF NOT EXISTS` block with **no version table and no migration
machinery**, so an `ALTER` would silently not reach a journal an earlier build
wrote. New tables are the only shape that works on an operator's existing file.

Two things about the bump's state machine are deliberate:

- **`BeginBump` claims the change outpoint before Core is holding it.** The row
  and the lock row are both written before `walletcreatefundedpsbt`, because
  that call is what takes the lock — so a crash inside it leaves a lock the
  journal admits to rather than an orphan only Core knows about. The cost is a
  release attempt for a lock that was never taken, which `ReleaseLocks` filters
  out because it asks Core what it actually holds first.
- **`AbortTarget` does not refuse a child that may be public**, where
  `Run.AbortTarget` does, and the difference is what the action costs.
  Abandoning a pending channel whose funding transaction then confirms strands
  its funds; releasing a coin lock takes nothing back and cannot strand
  anything. What the operator has to be told is that it is not
  un-broadcasting the child, and that is the report's job rather than a
  refusal. `AbandonBump` leaves a published child's *state* alone for the same
  reason — "abandoned" would be a claim about the network it cannot make.

### It needs its own verifier, and `combine` grew one accessor

`plan.Plan` refuses a plan with no channels in it, so a one-in one-out child
cannot go through `internal/plan` — and an unverified PSBT reaching a
cold-storage device is what `internal/plan` exists to prevent. Relaxing that
refusal would have been the wrong repair even if it were free: four of the batch
verifier's checks mean something different here or nothing at all. The fee
tolerance is about the parent's own rate where a child's is about the package's,
`ChangeFloor` is meaningless for a transaction that *is* the change being spent,
the assisted-mode change recognition does not apply because the app named the
address, and the exact-amount rule inverts. A verifier with four switches is
worse than two verifiers.

What is shared is shared as code rather than as a convention:
`plan.IsSegwitSpend`, `plan.MaxNonReplaceableSequence`, `plan.SizeOf`,
`plan.ChildVsize`, `plan.ChildFeeSat`, `plan.DustSat`, `plan.ScriptFor`,
`plan.Params`. So the two verifiers cannot drift on the facts they both depend
on.

The two verifiers also disagree on purpose, and that turned out to be the point:
`internal/plan` refuses a replaceable funding input and `internal/bump` requires
a replaceable child. See "The second lift" below.

`bump.Verify` runs on what goes out and `bump.Recheck` on what comes back —
`internal/combine`'s discipline, for the same reason. The second pass needed one
change to `internal/combine`: **`Finalized.View()` is now exported.** `Recheck`
was hardcoded to `*plan.Plan` and the plan-shaped view was unexported, and
rebuilding that view in `internal/bump` would have been a second copy of a fiddly
reassembly — btcd's finalizer replaces each input with
`NewPsbtInput(nil, WitnessUtxo)` plus the final witness, discarding exactly the
redeem script, witness script and non-witness UTXO a verifier needs. The
derivation stays in one place; only the accessor is new.

The second pass adds the one check the first cannot make: with every witness
present the size is a measurement rather than an upper bound, so the real
transaction may be smaller but **must not be larger**. Larger would mean the rate
the operator approved was not the floor it was presented as, and I-4 leaves no
second attempt on the parent. Observed on the harness: a 149 vB signed child
against a 150 vB estimate.

### The horizon is re-checked after the signing round and deliberately not enforced

This is the one place in the design where a signing round races something that
does not wait, and the answer inverts how the armed window treats a stale gate.

`bump` reads `funding_expiry_blocks` before the round, prices it in wall clock at
ten minutes a block, re-reads it after, and **publishes either way**. The reason
is what each staleness costs. A stale reserve finding means `psbt_verify` will
refuse, so continuing achieves nothing. A passed horizon means the channels are
lost — the responder closed its side as `FundingCanceled`, and our node, the
initiator, never times out — and the coins are still in an unconfirmed
transaction that I-4 forbids replacing, so getting it confirmed is the only way
they ever become spendable again. Withholding the publish there would spend the
coins to save channels that are already gone.

The copy has to hold both halves at once and is tested for it: saying only "the
horizon has passed" reads as "do not bother", and saying only "build the child"
hides that the operator is about to own channels their peers have forgotten.

### What the harness runs proved, and what the fixture cannot

The parent is a miner-to-cold payment at 1 sat/vB rather than a real batch — the
same choice `internal/settle`'s CPFP test makes, and for the same reasons:
publishing a real batch costs cold coins and leaves open channels nothing can
close, and what a child needs from a parent is structurally simpler than a batch.
The journal's run row is therefore written by hand. Every gate is still the real
one, but the channels in that row are fictional, so **the funding countdown
cannot be tested from this fixture** and the test asserts that it reports absent
rather than pretending otherwise.

Measured against a real unconfirmed parent (867 vB, 896 sat, 1.03 sat/vB): Core
and `plan.ChildFeeSat` agree **to the satoshi** at 20, 50 and 200 sat/vB. The
whole command then signed and broadcast a child, and Core's own ancestor
accounting confirmed the package it produced — 1016 vB, 20,340 sat, 20.02 sat/vB
against a 20 sat/vB target, arrived at from Core's numbers rather than ours.

`TestTheWholeBumpSignsAndPublishesAChild` **does broadcast**, which makes it the
second test in the repo that does. It has to: "the child reaches the network on
exactly one line, and only after being merged, finalized and verified in-app" is
not a claim a dry run can make — the same reasoning `internal/arm`'s publish test
runs on. Unlike that one it leaves nothing behind: the child pays the cold
wallet's own change back to the cold wallet, so the only cost is the fee, and the
cleanup mines it away.

## The second lift, and the one sentence in I-4 that had to become precise

`winthistle bump` shipped building a non-replaceable child, with the reasoning
left in place from before there were two verifiers: *"one verifier for both
transactions is worth more than the option to bump."* That reason stopped being
true the moment `internal/bump` grew a verifier of its own — a one-in one-out
child cannot go through `plan.Plan`, so there are two verifiers whether or not
the child is replaceable, and keeping it non-replaceable was buying nothing while
costing the second lift.

The child is now built at `plan.MaxBIP125Sequence` with `replaceable: true`, and
`internal/bump`'s verifier **requires** it. A second `winthistle bump` on the
same run replaces the standing child instead of refusing.

### I-4's heading changed, and it is a clarification rather than a relaxation

It read *"No RBF, ever"*, and the body ended *"Replaceability is disabled at
construction and is not operator-adjustable"* — with no subject. That was
unambiguous when this build made one transaction. It makes two, so:

- **the funding transaction: never.** `coldwallet.Build` passes
  `replaceable: false`, `internal/plan` refuses any funding input below
  `MaxNonReplaceableSequence`, and no code path in the repository replaces one.
  Every word of I-4 applies to it, unchanged.
- **the CPFP child: always.** Nobody holds a commitment signature against a
  child's outpoints. It spends the batch's change and pays cold storage back, and
  only cold storage can sign a replacement of it — so the operator gains an
  option and no one else gains anything.

The heading is now "No RBF on the funding transaction, ever", and CLAUDE.md says
in as many words that if a change ever makes a *funding* transaction replaceable
that is the invariant breaking and the answer is to stop rather than to edit the
section. **Two verifiers is what makes both rules statable at once.** One
verifier with a flag on it would have been a switch on the invariant, which is
the shape to refuse.

### Every part of it is Core's behaviour, and all of it was measured

Three things had to hold and none was obvious from documentation:

1. **Core will build a transaction spending an outpoint its own mempool already
   shows as spent.** It does, because the input is named explicitly and
   `add_inputs` is off, so there is no coin selection to refuse it. Probed
   directly before any of this was written.
2. **The package arithmetic is unchanged.** Core does *not* count the child being
   replaced as an ancestor of the replacement, so `plan.ChildFeeSat` still
   matches to the satoshi. Measured: a 141 vB parent at 2 sat/vB, a child at 10,
   then a replacement at 40 — Core charged 10,998 sat and `ChildFeeSat` wanted
   10,998. Had it counted the replaced child the figure would have been 14,100.
3. **The replacement evicts the first.** Confirmed, and confirmed again in the
   live end-to-end test: a 20 sat/vB child replaced by a 60 sat/vB one, the first
   gone from the mempool, Core's own ancestor accounting reporting 60.05 sat/vB
   over 1287 vB.

### Two refusals that had to be added, and one that had to be exact

**BIP-125 rule 3 is about an absolute fee, not a rate**, and on a second lift
those are easy to confuse because the operator is thinking in package sat/vB
while the network compares two totals. Core's refusal is `insufficient fee` and
names neither figure. `bump.checkReplacement` runs before a device is asked,
names both, and says what package target *would* clear the bar. Rule 4 is in
there too — the replacement pays for its own bandwidth at
`IncrementalRelaySatPerVB`, a constant rather than something read from the node,
because a refusal that depends on a setting the operator cannot see in the
message is harder to act on rather than easier.

**The sequence check is equality, and the reason is BIP-68 rather than
tidiness.** The child is a version 2 transaction, so a sequence with bit 31 clear
stops being an RBF signal and becomes a *relative timelock*: the child would not
be spendable until the parent had confirmations, and a CPFP child that cannot be
mined beside its parent cannot enter a mempool at all. The replaceable-and-
BIP-68-disabled range is `0x80000000`–`0xfffffffd`; accepting all of it would
wave through a lot of ways to be subtly wrong. `TestASequenceThatIsATimelockIsRefused`
pins it with sequence 1.

### Finding the change output after something has spent it

Core drops an output from `listunspent` the moment an unconfirmed transaction
spends it — which is exactly the state a second lift starts in. So `bump.Change`
now carries its own scripts and there are two routes to filling it:

- `listunspent`, for a first lift. Still the identification rule rather than a
  heuristic: every other output of a batch belongs to somebody else.
- the parent's own outputs plus `getaddressinfo`, for a replacement. The parent
  is unconfirmed, so `getrawtransaction` answers without `txindex`; the value and
  `scriptPubKey` come from the transaction and the **witness script from
  `getaddressinfo`'s `hex` field**, which is the script behind a P2WSH address.
  The journal supplies only *which* output to look at, and ownership is
  re-checked with `AddressInfo.Ours()` rather than taken from the row — so a
  journal pointed at the wrong wallet produces a refusal instead of a transaction
  paying a stranger.

`AddressInfo.Ours()` matches `ismine || iswatchonly`, and both are needed: a
watch-only descriptor wallet reports `ismine: true` and `iswatchonly: false`,
observed on the harness's cold-watch, which is not what the names suggest.

### No schema change, again

Which child superseded which is **derived**, not recorded. Every bump row already
carries the change outpoint it spends, so `journal.StandingChild` finds the
published child of that outpoint with a query and nothing had to be stored.
`BumpSuperseded` is a new *value* in an existing TEXT column, which is the only
kind of growth this journal supports — see the note under "`winthistle bump`"
about there being no migrations.

`Supersede` is called after the replacement's publish returns and never before:
until those bytes are out the older child is still the one in the mempool. The
superseded row keeps its txid and its raw transaction, because it is a record —
those bytes really were broadcast, and a journal that deleted them would be
claiming they never existed.

## The server, and the three decisions with guards on them

`internal/server` is the first `net/http` surface in this repository. The only
other one is bitcoind's JSON-RPC client, which is a client, so nothing here had
a precedent to follow. That is why **the security shape was the deliverable of
the first slice and the screen was only what proved the shape carries a screen.**

`winthistle serve` binds `[server] bind`, prints one URL with a token in it, and
serves nine routes:

| | |
|---|---|
| `GET /` | the overview, the batch, the control that starts a run, and the runs this process has driven |
| `GET /doctor` | every pre-flight, verbatim. The one handler with no clock, no signer and no state |
| `GET /runs/{id}` | the attach: the transcript from the beginning, whatever the run is waiting on, and "stop this run" |
| `POST /runs` | start a run. Refused while one is going |
| `POST /runs/{id}/answer` | one answer to the one question the run is asking |
| `GET /runs/{id}/abort` | what stopping costs, and then the button — or the refusal, for a run that reached the publish call |
| `POST /runs/{id}/abort` | cancel the run's context, which is what Ctrl-C does |
| `GET /recover` | the journal: the runs that stopped, and the CPFP children left half-done. Read-only, and the only screen that works on a node that is down |
| `GET /recover/{id}` | one journalled run, and what an abort would do to it. Read-only |

The three unsafe methods are the first in this repository, and the guard in front
of all seven was written before there was anything to guard: it refuses an unsafe
method that cannot say where it came from — no `Origin` and no `Sec-Fetch-Site:
same-origin` is a refusal, not a fallthrough. What each `POST` had to decide for
itself is in "The four seams" below.

### The security shape

`docs/design.html`: "a token printed to the terminal at startup and required on
every request, plus strict `Origin` and `Host` validation, and no CORS at all",
because **a localhost bind is not an authentication boundary** — any process on
the machine can reach it, and any web page the operator visits can attempt DNS
rebinding against it.

- **The token is 32 bytes from `crypto/rand`, generated at every start, and it is
  not a config key.** It must not become one: `internal/config`'s package comment
  says `winthistle.toml` holds connection details and never a secret, only the
  paths to them, and a token in a file is a long-lived secret in a file. There is
  no way to set it, so there is no way to set it badly.
- **It is carried by a cookie**, because the screens are plain navigation with no
  JavaScript and a cookie is the only thing a browser sends on a link click. The
  startup URL carries it in the query exactly once; the guard trades it for the
  cookie and 303s to the bare path, so it leaves the address bar and the history
  before the first screen is drawn. `Referrer-Policy: no-referrer` is on every
  response so it cannot leave in a `Referer` either.
- **`Host` is checked before the token, and the ordering is the point.** A
  rebinding attempt is refused for naming the wrong authority before it is told
  anything about a token, so the refusal carries no signal about how close a
  guess was. The comparison is constant-time regardless; the ordering is free and
  the disclosure is not.
- **`Sec-Fetch-Site: same-site` is refused along with `cross-site`.** Another
  server on 127.0.0.1 at a different port is same-site and is not us. Only
  `same-origin` and `none` pass, and an absent header falls through to `Origin`,
  because a plain navigation sends neither and that is most of this UI.
- **A loopback bind accepts loopback's three spellings** — `127.0.0.1`,
  `localhost`, `[::1]`, each with the bound port — because an operator who types
  `localhost` and an SSH tunnel that forwards to `::1` are both ordinary. A bind
  that names some other interface gets exactly what it named; the guard does not
  invent a second name for an interface the operator chose.
- **No CORS anywhere**, and `TestNoCORSHeaderEverAppears` asserts it on every
  response including the refusals. That test exists for the commit where someone
  adds a `fetch()` to a screen and reaches for the header that makes the console
  error go away.
- **The CSP is `default-src 'none'` plus inline styles.** A `<script>` or a
  `<link rel=stylesheet>` added later does not load, and
  `TestNoScreenFetchesAnything` fails before a browser gets the chance to fail
  quietly.

`config.checkBind` already refused a wildcard; `server.New` refuses it again,
because this package must not depend on having been handed a configuration that
went through that check.

### Decision 1 · `run.Do` stays a straight-line blocking function

Driven by a goroutine, with HTTP handlers feeding the four callback seams —
`rehearsal.Signer`, `abort.Confirmation`, `bump.Approve`, `setup.Ask` — over
channels. One code path for the CLI and the UI, and `--stop-before-publish`
stays one `if` between `arm.Finalize` and `arm.Publish`, read nowhere else. The
alternative — an explicit state machine the handlers step — is the shape a web
application wants and it would put a second route through the armed window.

**The guard: a handler must not be able to reach a publish call, and there are
two locks on it.**

1. *The count.* `Method.CallSites` pins `WalletKit.PublishTransaction` at two
   production call sites and `internal/methods`'
   `TestEveryLNDCallSiteIsRegistered` type-checks the whole module — this
   package included — and fails on a third. A web handler is exactly where a
   third appears, which is why this is the lock that has to be mechanical.
2. *The import ban.* `internal/server` may not import `internal/arm`,
   `internal/bump` or `internal/journal`.
   `TestTheServerCannotReachAPublishCallOrWriteTheJournal` parses this package's
   non-test files and fails on any of them, so no function here can be handed an
   `*arm.Armed` or a `*bump.Signed` — the two types whose unexported raw
   transaction is filled by exactly one constructor after that constructor's own
   checks — and none can name the table a setup answer is recorded in. A handler
   cannot name the arguments, so it cannot make the call by accident.

**`internal/journal` went on that list when the seams did, and it is about the
record rather than the network.** Two things rely on it. `setup.Ask`'s three-way
shape exists so a comparison nobody made is never written down as a verdict, and
this is what makes "no handler can write `NotAnswered` into the `setups` table" a
fact rather than a habit. And whether a run reached the publish call is
`journal.Run.AbortTarget`'s answer — the same one `run.RecoverOne` refuses on — so
the abort control has to ask for it, through `Launcher.AbortRefusal`, instead of
holding a second copy of the rule that could drift.

Note what is deliberately *not* banned, because the reasoning matters more than
the list. None of the three bans makes the banned code unreachable at run time and
none is meant to: `internal/server` imports `internal/doctor`, which imports
`internal/journal` transitively, and banning `walletrpc` would not help either
since an import of `internal/lnd` yields an `*lnd.Client` whose `WalletKit` field
can be selected without naming `walletrpc` at all. What a ban removes is the
ability to *name* a type or call a function, which is what stops a handler being
handed the argument. The reachability half is the count's job.

What the server may do is start `run.Do` and answer its questions. Publishing
stays inside the sequence that earned it, and recording stays with the package
that owns the record.

### Decision 2 · A closing browser tab does not abort — and this is the dangerous one

**It does not.** A tab closing is indistinguishable from a reload, a laptop lid
or a Wi-Fi blip, and aborting a partially-armed batch on any of those is worse
than what abort protects against. What ends a run is the clock — the 5:00 gate
`rehearsal.Gate` measures against, and the peers' ten minutes, which is *their*
clock and not ours because `pruneZombieReservations` skips PSBT reservations —
or the operator's Ctrl-C on the process.

**`Ctrl-C` and a closed tab do different things, and that asymmetry is the most
dangerous part of this change.** HANDOFF asked for it to be written here, so:

| | what it means | what happens |
|---|---|---|
| `Ctrl-C` on `winthistle run` | the operator, at the machine, saying stop | the context is cancelled and the run unwinds through the abort path — cancel the shims, abandon what reached pending, release Core's locks. |
| `Ctrl-C` on `winthistle serve` | the same thing, from the other front door | the socket shuts down, **and a run in flight is cancelled and waited for**. |
| the abort control on the run screen | the operator saying stop, about this run | identical to Ctrl-C: `Run.Abort` cancels the run's context. Refused for a run that reached the publish call. |
| the tab closing | nothing legible at all | nothing. The run keeps going and the transcript keeps accumulating. |

**The second row changed in this slice, and it is a correction rather than a
relaxation.** It used to read "no run is touched", which was true only because
nothing this server served could start one. Now that it can, leaving the run alone
would be worse rather than safer: the process exits when `Serve` returns, so a run
left running is killed between two RPCs with *n* shims open and Core holding coin
locks — the exact state the abort path exists to avoid. So the socket closes
first, and then `Serve` waits for the run to come apart (`UnwindGrace`, a backstop
over `run.TeardownBudget`, which is the real bound). This is decision 2's own
sentence, not an exception to it: what ends a run is the clock, or the operator's
Ctrl-C on the process. A tab closing is still nothing.

The failure mode this asymmetry buys is real and it is the lesser one: an
operator who closes the tab believing they have stopped the batch has not
stopped it, and a batch they meant to abandon proceeds to the publish. The
mitigation is copy plus a control. The overview screen says, in the pane, that
closing the tab does not stop anything and names what does; and the run screen
carries "stop this run", which is a link to a screen that says what stopping costs
before it offers the button — there is no JavaScript in this UI to put a dialog up
with, and one click is the wrong price for a partially-armed batch.

**The abort control is refused for a run that reached the publish call**, and it
asks the journal rather than deciding. `journal.Run.AbortTarget` refuses
`StatePublishing` and `StatePublished` with `ErrMayBePublished` — abandoning a
pending channel whose funding transaction later confirms strands its funds with no
force-close path — and `run.RecoverOne` refuses the same run for the same reason.
The control refuses it too, at both the screen and the POST (a form kept open from
before the publish is exactly how the second arrives), and the refusal says the
exit is forward: let it confirm, or `winthistle bump` it. It fails closed — a
journal that cannot be read is not a permission — with one exception,
`ErrNoRun`, because a run that journalled nothing opened no stream and has nothing
to take apart.

The failure mode the other choice buys is worse and it is not recoverable by
saying something: a Wi-Fi blip during the armed window tears down *n* peers'
reservations and abandons channels that had reached `chan_pending`, and the
ceremony is done again from the beginning. A transport event must not be able to
decide that.

**The consequence built for: the token is joinable per run, not per tab.** If it
were per tab, a reconnecting browser would be a new session and the live run
would be unreachable from it, which would make "a closing tab does not abort" a
promise with no way to collect on it. So: one token per server start, runs
addressed by their journal id in `server.Registry`, and the transcript
accumulated on the `Run` and rendered *from the beginning* on every attach. An
attach is a view, not a subscription — a subscription is a thing a tab owns.

`Registry`'s doc comment is where the three consequences are written down, and
the shape is the guard: nothing in the package cancels a run's context when a
response ends, there is no `OnDisconnect`, and no heartbeat whose absence means
anything. The run's context comes from `Server.base`, which is the process's, and
`Run.cancel` is held only by the abort control.

**That guard now has a mechanical half, which the last slice said it could not
have.** The worry was exactly right: a handler that plumbed `r.Context()` into
`run.Do` would pass every behavioural test in the package and abort a batch on a
laptop lid. `TestOnlyTheDoctorScreenReadsTheRequestContext` parses the package,
finds every `r.Context()` in a function that takes an `*http.Request`, and
requires the enclosing function to be `doctor` — and fails if it finds none at
all, so it cannot pass by the parameter being renamed. The `doctor` screen is the
one legitimate use and the distinction is not the transport: a pre-flight has
nothing to unwind, a run has peers holding reservations. The abort control's own
journal read is a request-shaped read that deliberately does *not* use it, because
a browser that gives up mid-check must not turn a refusal into a permission.

### Decision 3 · The twelve `Report() string` renderers are served verbatim

In a `<pre>`, at `prose.PaneWidth`, for v1. That makes the design's "guided web
UI" a terminal in a browser, and it is the honest thing to ship before the copy
has a second rendering: `CLAUDE.md` says the recovery screen's wording is the
highest-stakes copy in the product, and two renderings is how it drifts while
the overrun tests cover only one.

**The guard, for when a screen is later re-rendered as HTML: the text version is
the test oracle.** Every screen goes through one function, `screen()`, and
`TestTheTextIsTheOracle` takes the served page, unescapes the `<pre>`, and
requires the original back byte for byte. The recovery screen will get an HTML
rendering — it deserves better than a terminal — and on that day the test is not
deleted. It has to keep passing, which means the HTML has to be built from the
same string `internal/prose`'s overrun tests measure, rather than beside it.

The adversarial inputs are half the test. Copy in this repository contains `<`,
`>`, `&` and apostrophes — "channel < the minimum", "you can't", every "*n* of
*m*" — and a renderer that escapes for HTML and forgets to unescape for a diff is
how "don't" becomes "don&#39;t" in the one screen nobody wants to be surprised
by. The same chokepoint is what stops a transcript becoming markup: a run's
transcript carries peer pubkeys, file paths and error strings from LND and Core,
none of which this repository chose.

## The QR decision and the file transport

Item 1.4's first half, and the decision that had to be settled before any of it.

### The decision: in-house animated QR is out of scope, held open

Made by the owner on 2026-08-24, as triage item 7's outcome 3. Three documents —
`CLAUDE.md`, this file and `docs/design.html` — promised animated QR (BBQr,
`ur:crypto-psbt`, webcam capture on the return leg) and a fourth argued it out of
scope, so the promise came out of all three in the same commit as the download
leg. **It is out of scope, not rejected**, and the difference is recorded: the
triage's "Held open" section carries the cost of picking it up.

Two facts settled it, and **do not re-derive them**:

- **The audit surface, which is repo-specific.** The server has no JavaScript,
  and that is enforced: `guard.go` sets `default-src 'none'; style-src
  'unsafe-inline'; img-src 'none'`, and `TestNoScreenFetchesAnything` fails on
  `<script`, `<img`, `http://` or `https://` in any page body. Webcam capture is
  in the browser and has no server-side route, so building it *begins* by
  reversing the strongest property this UI has. Add a vendored JS decoder (a new
  supply chain in a repo whose dependency surface is `go.sum`) or Go→WASM with
  `wasm_exec.js`, three multipart encodings, and no QR device on this machine to
  test any of them against — which is the failure mode already recorded here
  once, as the invented `Origin: http://127.0.0.1:7420` header.
- **The loss is smaller than the design assumed.** A QR-only signer is reached
  through a desktop wallet: download the file, let that wallet do the animated-QR
  round trip with the device, export the result, upload it. The webcam is the same
  desktop webcam; what changes is only whether this code drives it.

What replaces the QR sentences in all three documents is the contract, which the
triage found VALID regardless: **any wallet that round-trips BIP174 against the
descriptor works.** Naming one application was wrong for SD-card signers and
wrong for a headless node.

One caveat is worth keeping straight, because the first draft of it overstated the
case. The rule is about **our** contract, not about any named wallet's export
behaviour, which is not testable from this machine: only *partial* signatures may
come back, and `combine.mergeInput` refuses a part whose input is already
finalized (`ErrAlreadyFinalized`, citing I-2). Separately and checkably, `Merge`
requires a distinct label per part and counts signatures per device, so one packet
carrying two devices' signatures loses the attribution every refusal in
`internal/combine` is built to name. That is why the advice is one device per
round — a fact about rounds, not a claim about a wallet.

### The download: binary, and named for the round and the device

`GET /runs/{id}/payload/{question}` in `internal/server/transport.go`. It serves
the pending question's `Payload` decoded to bytes, as an attachment. The link is
on the payload label's line — "or download it as a .psbt file" — so the two ways
of taking the packet read as one choice, and the read-only field stays.

**Binary rather than base64 text**, and the reason is the file format rather than
a device report: BIP174 defines the `.psbt` file as the raw serialisation, base64
is the encoding for a text transport, and the text transport is the textarea
beside this link. The decode is `base64.StdEncoding`, which is exactly what
produced the string — `coldwallet.Build` decodes Core's `psbt` field with it
(`build.go:184`) to fill `Built.Raw`, and `Built.PSBT`, the string that reaches
this handler, is that same field untouched. `Built.PSBT`'s own comment already
said "as Core returns it and as a browser download carries it".

**The name carries the round and the device**, `{round}-{label}.psbt`, which is
`internal/signers`' file-handshake naming exactly — and for its reason, not for
consistency: the rehearsal and the batch sign two different transactions minutes
apart, both files land in one Downloads folder, and a signed rehearsal file picked
up as the batch's is a signature over the decoy, which `internal/combine` refuses
at the worst possible moment with a message about a moved txid. `internal/server`
cannot compose that name — it does not know what a round is — so it comes from
`internal/webrun` on `Question.PayloadFilename`. There is **no second copy of the
packet** anywhere: the download decodes the same `Payload` the field shows.

**A stale link is refused, by question id**, the same check and the same reason as
`Run.Reply`: the two rounds ask the same devices about two different
transactions, so serving a superseded packet would hand a device the wrong one to
sign.

### Three defects, and two of them only a browser could find

1. **A silent filename collision.** A label written in a script the allowlist has
   no letters for — 冷1, 寒2 — sanitised to punctuation and then to nothing, so
   *every* device in *every* round downloaded under one fixed name. That removes
   precisely the property the name exists for, with nothing on the screen to say
   so. The fallback is now built from the run and question ids, which are
   counters, so two packets always get two names.
   `TestTwoUnwritableLabelsStillDownloadUnderTwoNames`.
2. **The link read backwards.** Below the payload box it sat equidistant between
   the packet and "paste what cold1 gave back", and "download it *instead*"
   parsed as *instead of pasting* — when it replaces the copy, not the reply. It
   is on the label line now, above the blob, because which way to take the packet
   is decided before it is read, and "instead" became "or".
3. **The stale-download refusal was a dead end.** Correct copy, inside the pane,
   naming the run screen — with no link to it, on a page whose nav offers
   overview, doctor and recover. It links back now, the way `noSuchRun` links to
   the journal.

The header is an allowlist rather than a blocklist because the name is built from
`config.Signer.Label`, an operator string that lands inside a quoted
`Content-Disposition` parameter and then in a filename. `net/http` turns newlines
in a header value into spaces, so header splitting was never the hole; a quote
closing the parameter early was, and a slash naming a path was. The property
tested is that a label cannot become *syntax* — one quoted value, one parameter,
still `.psbt` — not that its text disappears, because it does not need to.

### What the browser proved that the tests could not

Rendered against a real listener on 127.0.0.1:7420, in Chrome, with one signing
question pending. The file arrives as `rehearsal-cold1.psbt`, 86 bytes beginning
`70 73 62 74 ff`, byte-identical to the decoded payload with no base64 in it; the
click downloads without navigating, so the question survives it; the 409 renders
in the pane and its link returns to the run. Defects 2 and 3 above were both
invisible to a passing suite.

### The upload: multipart, and one sniffer for three transports

The return leg is a file input beside the paste field, so the operator gives the
run one packet either way and presses the same button. Four things are worth
knowing about it.

**The tolerance moved into `internal/combine`.** `signers.decode` was the
both-forms reader — binary or base64, settled on BIP174's five-byte magic rather
than by guessing — and it is now `combine.Parse`, with `signers.decode` a one-line
wrapper over it. The reason is not tidiness: three transports read a file a wallet
wrote, and two sniffers in two packages could disagree about one file. A
disagreement there surfaces as a *device* being blamed for something a
*transport* did, which is the worst place in this product to be wrong about who
is at fault. `TestParseReadsEitherEncoding` and
`TestParseSaysWhatWasActuallyThere` are in `internal/combine` now, and the second
pins the property that valid base64 of a non-PSBT is not reported as bad base64.

**`internal/server` still does not know what a PSBT is.** `Answer.Upload` is the
file's bytes verbatim and `internal/webrun` runs them through `combine.Parse`,
which is the same division the download's filename has from the other direction:
the server moves bytes, and the package that already holds every other fact about
a PSBT decides what they are.

**Only the signing form is multipart.** `enctype` is set when `Question.Reply` is
non-empty, so the three decision-only questions post exactly what they always
posted — including the blunt-abandon confirmation, and a new parsing path under
*that* prompt is not something to acquire as a side effect of adding a file picker
elsewhere. `TestADecisionFormStaysUrlencoded`.

**No uploaded packet reaches the disk, and it is arithmetic rather than policy.**
`http.MaxBytesReader` caps the body at `maxAnswer` (1 MiB) and
`ParseMultipartForm` is given the same number as its in-memory budget, so the body
cannot exceed the budget and no part can spill to a temp file. Raise one of the
two without the other and a signed PSBT starts being written to `/tmp`.

And one refusal that is a decision rather than a validation: **a form carrying a
paste *and* a file is refused**, not resolved. Two packets is the operator having
done two things, and picking one here would be a second place a verdict is
decided — the rule `Question.Choices` already carries. Nothing is recorded and the
question stays pending, so the refusal costs one more form rather than a round.

### Three more defects, all three from rendering the upload

1. **The file input flowed into the button row.** Inline by default, so
   "Choose File" sat beside "This is the signed packet" and pushed "cold1 cannot
   sign" onto its own line — the two *choices* stopped reading as the pair they
   are, on the screen where one of them ends a signing round. It is `display:
   block` now, like the textarea it is the alternative to.
2. **`*and*` rendered as asterisks.** The refusal is served inside a `<pre>`, so
   a markdown emphasis marker is just punctuation in the middle of a sentence
   someone is trying to act on. There is no markdown anywhere in this UI and the
   copy should not imply there is.
3. **Three refusals named the run screen and none could reach it** — the
   both-set refusal, the unreadable-form refusal, and `staleAnswer`, which is
   pre-existing and is the one an operator meets after a back button or a second
   tab, exactly when they have lost their place. The nav carries overview, doctor
   and recover. `Server.refuseRunScreen` is the one helper they all use now, and
   the stale *download* refusal uses it too.

Verified in Chrome with a real file picker: a binary `.psbt` chosen from disk
arrives at the seam as its own 86 bytes with its filename, `Answer.Text` empty; a
paste in the same multipart form arrives trimmed with `Upload` nil; and a form
carrying both is refused, after which the question is *still pending* and the same
file answers it. That last one is the property that makes the refusal cheap, and
it is worth re-checking by hand if this path is ever touched.

### Mixing them per device, which finished item 1

A browser-driven run used the browser for every device even when a `[[signer]]`
block named a working command, so an operator who had automated one device was
asked for it anyway. `Set.signer` already branched on `d.Command`, so the shape
existed on the CLI side; what was missing was that branch reaching a
browser-driven round. It does now, and six things about it are worth keeping.

**The rule, which is a decision rather than a fact.** A device whose `[[signer]]`
block names a command is answered by that command and is never asked on the page;
a device with no command is the page's. The **file handshake is deliberately not
in the mix**: its instructions are "put this file at /some/path and wait", and on
a browser-driven run the operator is already at a page that can hand them the
bytes and take them back. So `signers.Options.Dir` and `Options.Out` never come
into it — which is also why nothing here had to decide what "put this file at X"
would mean rendered into a `<pre>`.

**No new exported API, and no second copy of `runCommand`.** `webrun.Signers`
holds `byCommand []*signers.Set`, parallel to `cfg.Signers`, with a **one-device**
`signers.Set` at each index whose block names a command and nil elsewhere.
`signers.New([]config.Signer{d}, signers.Options{})` needs no `Dir`, because that
requirement fires only for a signer with *no* command. Parallel rather than a
compacted list consumed in step, because two slices of different lengths walked
together is exactly how a device ends up on the wrong transport.

**The invariant that was easy to break, and what holds it.**
`internal/signers`' package comment states it: the rehearsal's measurement
predicts the armed window *only if the two rounds go through the same transport*.
So the choice is resolved once, in `newSigners`, off the configuration, and
`Round` indexes it rather than deciding again — a rule that could answer
differently the second time would make the measured number a prediction about a
round that never happened, and the round it mispredicts is the one with *n* peers'
clocks running. `TestTheTransportIsPerDeviceAndTheSameInBothRounds` drives both
rounds and compares.

**One paragraph of copy exists only because the round can be mixed.** `Index` and
`Of` count the whole round, so an operator asked for "device 2 of 2" who is never
asked for device 1 would have to guess whether the page lost it.
`SignRequest.ByCommand` carries the labels and `signPrompt` writes one paragraph
naming them; on an all-page round — the ordinary one — it is not written at all,
because a paragraph explaining an absence that is not there is noise on the screen
where noise costs most. What is **not** a bug: the round's deadline is the round's,
so a command that takes four minutes leaves the browser device after it with one
minute of the gate. That paragraph says so, and
`TestTheRoundsDeadlineIsSharedByEveryDevice` is the guard.

**What the browser proved.** A one-channel cold probe against the harness with
`cold1` given a real `walletprocesspsbt` command and `cold2` left to the page. Both
rounds behaved: the rehearsal report reads `cold1 453ms` / `cold2 1m21s`, so the
mix is visible in the measurement rather than hidden by it; the batch round's
transcript reads `cold1 signed (0s elapsed)` with no question in between, then
asks for `cold2` as "device 2 of 2" and explains where device 1 went; the two
partials combined and `testmempoolaccept` allowed the result; step 9 was withheld
and the abort path took the batch apart.

**What it also turned up, which is not about transports.** The clock sentence over
a batch's pending question said "that is the 5m0s signing gate … letting it pass
costs one more signing round" over the **blunt-abandon confirmation**, asked during
a teardown when the signing round is over. Both clauses were false there, on the
one prompt in this product where a human authorises something that could lose
funds if the premise were wrong. `waitingOn` now branches on `Question.Reply` —
non-empty means a packet is being asked for, which is the same distinction
`questionForm` already draws to decide the form's `enctype` — and a batch's
decision question says what letting it pass actually costs. One more false
statement in operator copy found by looking at the page rather than by a test, and
like every one before it, it was true when it was written.

## The four seams, and the five things that were actually hard

`run.Do` stops four times to ask a person something. Those are the seams, and
this slice wires them to a browser: `internal/server`'s `ask.go` is the channel,
and `internal/webrun` is where a `Question` becomes one of the four concrete seam
types. Two of the four are reachable from `run.Do` itself — `rehearsal.Signer`
through `Deps.Signers`, and `abort.Confirmation` through `Deps.Confirm` — and both
are exercised end to end by the browser-driven cold probe. `setup.Ask` belongs to
`setup.Do` and `bump.Approve` to `bump.Do`, so their adapters are built and unit
tested here and their POSTs arrive with the setup and bump screens (item 1.6).
That is a deliberate, named exception to "no written-but-uncalled code", and it is
made for `setup.Ask` in particular because its guard is the one worth having
before its screen rather than after.

### Why `internal/webrun` exists at all

Because `internal/server` may not name the run's types. A server that took a
`run.Options` would import `internal/run`, and `run.Result.Armed` is an
`*arm.Armed` — which a handler could then hold without importing `internal/arm` at
all, since Go infers the type. That would leave decision 1's import ban intact on
paper and hollow in fact. So the boundary is a three-method interface,
`server.Launcher`, and `internal/webrun` is on the other side of it holding
`internal/run`, `internal/setup`, `internal/abort` and `internal/journal`.

`run.Connect` moved out of `cmd/winthistle` in the same change. Decision 1 is that
the CLI and the UI are one code path through `run.Do`; two sets of dialling
decisions underneath that — which timeout, which wallet scope, where the PSBT
directory is — would have drifted. `winthistle run`'s `connect()` is now four
lines over it, and what stays in `main.go` is the part that is genuinely a
terminal's: stdout, and the two prompts that read stdin.

### 1 · A browser can stop answering, and the bound is the gate

A seam waiting on a channel must not block `run.Do` forever, and a closing tab is
not detectable. So `Run.Ask` waits on a deadline — and the deadline is
`limits.abort_after_signing_seconds`, the 5:00 gate `rehearsal.Gate` measures a
signing round against, not a transport timeout invented for the occasion.

**Two clocks bound this product and only one of them is ours.** The gate is: we
set it, we measure against it, `rehearsal.Gate` refuses to arm a batch whose
rehearsal was slower, and it is therefore a number code may enforce. The peers'
ten minutes are not: `pruneZombieReservations` skips PSBT reservations, so our
node never expires one and the peer's own sweeper ends it, on the peer's clock.

*What stops the second becoming the first.* Three things, and the first was
already there:

- **`config.Validate` refuses a configuration whose `AbortAfterSigning` is
  greater than or equal to `rehearsal.PeerWindow`.** So a deadline built from the
  gate is inside the peers' window by construction. That check predates this
  slice and is what makes the whole argument sound rather than merely tidy.
- **`internal/server` cannot compute a deadline.** `Question.Deadline` is a
  `time.Time`, and `Ask` refuses a question that arrives without one
  (`ErrNoClock`) rather than waiting. The number always comes from whoever read
  the configuration.
- **`internal/webrun` never reads `PeerWindow`.**
  `TestTheSeamsAreBoundedByTheGateAndNotThePeers` parses the package — parses,
  not greps, so the prose is free to explain the thing the code may not read —
  and also fails if nothing reads `AbortAfterSigning`.

A bound from the gate can expire early relative to the peers, never late, and
early is the safe direction: it costs one more ceremony, and nothing is published
while a question is open.

**The deadline belongs to the round, not to the device.** *m* devices each given
the gate's worth would let a round run to *m* times the gate and out past the
peers' window. `Signers.Round` is called once per round, so it stamps one deadline
and every device in that round shares it; a second device asked after the budget
is gone is told the round is over rather than handed another five minutes.
`TestTheRoundsDeadlineIsSharedByEveryDevice`.

One interaction worth knowing: the blunt-abandon confirmation is asked *during* a
teardown, which has a budget of its own, so `webrun.deadlineFor` clamps a seam's
deadline inside the context's. Unclamped, the context would end first and `Ask`
would return `ctx.Err()` rather than `ErrUnanswered` — the difference between
"finish this by hand" and "something went wrong".

**Every seam's expiry is the answer a nil seam would have given.** That is the
property that makes an abandoned browser indistinguishable from no browser:
`setup.Ask` → `NotAnswered`; `abort.Confirmation` → `false`, which is what nil
means (never escalate); `bump.Approve` → `false`, which releases the child's coin
lock; `rehearsal.Signer` → an error, because `combine.Complete` wants exactly *m*
partials and a device that declined quietly would produce a packet that does not
finalize and a failure blamed on the wrong thing.

### 2 · `NotAnswered` must never be journalled

The highest-stakes of the four. `setup.Answer` has three values because "no" and
"not yet" are different facts and only one of them is a wallet that must not fund
a batch — and because the only verdict that exists about a cold-storage descriptor
is a comparison a human made.

Four layers, and they are layers rather than one check restated:

1. **The form is three-way**, with a third button that says "I have not compared
   them yet". Not a "cancel": the wording is the answer.
2. **The adapter's mapping is total and it fails to `NotAnswered`.** An expired
   question, a cancelled run, an empty form, a choice this question did not offer,
   a stale question id — every one is "nobody compared anything".
   `TestNotAnsweredIsWhatEverythingElseRecords` enumerates the garbage, including
   `"true"`, `"match"` and a `yes` borrowed from a different seam.
3. **`setup.Do` returns before `RecordSetup` on `NotAnswered`**, and is the only
   writer of that table.
4. **`internal/server` cannot import `internal/journal`**, so no handler can name
   `journal.Setup` or call `RecordSetup` at all.

There is a general rule underneath layer 2, and it is written on
`server.Question.Choices`: **`Choices` are the buttons, not a filter.** A
submission naming something else reaches the seam verbatim, because a server that
silently rewrote an answer would be a second place a verdict is decided. The
obligation that follows is on every adapter — match the affirmative explicitly,
never the negative. `== yes` is safe for any string a form could carry; `!= no`
reads a typo as consent. All four are written the first way and
`TestApprovalMatchesTheAffirmativeOnly` is the check.

### 3 · `abort.Confirmation` is a func per channel, and it stays one

"A func rather than a bool so it cannot be set once and forgotten: the caller is
asked per channel, at the moment of the rejection." A handler that turned it into
a set-once checkbox would authorise the second channel with the answer given about
the first.

So: every call is its own `Question` with its own id, carrying that channel's
outpoint and LND's verbatim rejection, and there is no stored permission anywhere
in `internal/server` or `internal/webrun`. `Run.Reply` checks the posted question
id against the pending one and refuses a mismatch with `ErrStaleQuestion` — the
back button, a second tab and a double submit are all the same shape as reusing an
answer. `TestEachChannelIsItsOwnConfirmation` proves a "yes" about one channel
buys nothing for the next, and `TestAStaleAnswerIsRefusedRatherThanApplied` proves
it through the handler.

What that does *not* give up is worth restating: `abort.AbandonPending` asks
`PendingChannels` itself and refuses a channel that is not pending, offering no
confirmation at all, because there is nothing a human could usefully authorise
about removing a live channel.

### 4 · The first unsafe method, and what refuses a second run

`POST /runs` is the first request in this repository that changes something. The
guard already refused an unsafe method that cannot say where it came from — no
`Origin` and no `Sec-Fetch-Site: same-site` is a refusal, not a fallthrough — so
that half was done before there was anything to check.

What is new is **`Registry.Start` refuses a second concurrent run**, and the
refusal is in the registry rather than the handler because it is a property of the
runs. One journal, one cold wallet, one armed window. The journal is not the
expensive collision — SQLite serialises the writes — **the coins are**: the second
run's dress rehearsal builds a decoy over the same inputs the first run is about
to spend, so it would either lose coin selection or take the inputs out from under
a batch that is already armed, with the cold wallet out and *n* peers waiting. The
overview screen also stops offering the control while a run is live, because a
control that is offered and then refused teaches an operator to press it twice.

Two winthistles against one journal is a different problem and is **not** solved
here. SQLite keeps the journal honest and does nothing at all about the coins.
`winthistle doctor` is what reports that state.

### 5 · The run's context is not the request's

`startRun` derives it from `Server.base`, which `Serve` sets to the process's
context, and the only thing that cancels it is `Run.Abort`. `Server.baseCtx` falls
back to `context.Background()` when `Serve` was never called, which is the case in
a test driving `Handler()` directly.

`TestTheRunsContextIsNotTheRequests` cancels the request's context — which is what
a closed tab, a reload, a lid and a blip all look like from inside a handler — and
requires the run's to be untouched.
`TestOnlyTheDoctorScreenReadsTheRequestContext` is the mechanical half. See
decision 2.

### One pre-existing bug this slice had to fix

**The abort path did not survive the cancellation that triggered it.**
`run.recoverRun` was passed the run's own `ctx`, so on Ctrl-C every call in the
teardown failed immediately: the journal read is `database/sql`, the shim cancels
and the abandons are gRPC, and Core's lock release is JSON-RPC. An abort triggered
by Ctrl-C would have reported `context canceled` and taken nothing apart. No test
caught it because no test cancelled a run mid-flight — the abort path was only ever
exercised via a *failure*, which leaves a live context.

It now runs on `context.WithoutCancel` with a deadline of its own,
`run.TeardownBudget`, the way `releaseFence` already did. The budget is one
signing gate's worth (5:00) and the unit is deliberate: the slow part of a
teardown is not the RPCs, it is the blunt-abandon confirmation, which asks a human
once per channel. The web abort control depends on this fix entirely — it is the
same cancellation from the other front door.

### The bug the first render found, and why every test missed it

**Chrome sends `Origin: null` on a same-origin top-level form POST.** The guard's
Origin check read that literal as "a different origin" and refused with `403`, so
nothing in this UI could be started, answered or stopped from a browser. The very
first click on "Open this batch" found it.

Measured rather than inferred, because it is the kind of claim that is worth
being sure of: Chrome 152, with the referrer policy set to `no-referrer` and then
to `same-origin` (it is not the referrer policy), from a genuine click and then
from a scripted submit. Every time: `Origin: null`, `Sec-Fetch-Site: same-origin`,
`Sec-Fetch-Mode: navigate`.

*The fix.* `null` is the serialisation of an *opaque* origin, and an opaque origin
says nothing in either direction — so it is treated as an absent `Origin`, and the
decision falls through to the unsafe-method check, which requires
`Sec-Fetch-Site: same-origin`. That check was already there as the belt to the
Origin braces; it is now the load-bearing one for every form in the UI, and it is
the better signal anyway: the browser computes it, it is a forbidden header name
so page JavaScript cannot set it, and `same-origin` means the initiator was us.
We cannot be framed into producing one either — `frame-ancestors 'none'` plus
`X-Frame-Options: DENY`, and a sandboxed frame of our own page has an opaque
origin and arrives as `cross-site`, which is refused before this point.
`TestABrowsersFormPostIsAdmitted` is the table, including the halves that must
still be refused: an opaque origin with no fetch metadata, an opaque origin from
another site, and a real foreign origin claiming `same-origin`.

**Why the whole suite passed.** Every test chose `Origin:
http://127.0.0.1:7420`, and that is a header no browser sends here. A test that
invents a header the browser controls is a test that asserts its own assumption.
So the helpers now send what Chrome was measured sending, and they say so — if a
future change makes a test fail because of it, that is the guard telling you it
has stopped accepting browsers, not an invitation to put a real `Origin` back.

Two render defects came out of the same session, and one of them was a safety
affordance rather than a cosmetic:

- **The blunt-abandon button carried the full 66-character outpoint**, so it
  wrapped to three centred lines and became the largest thing on the screen —
  making the dangerous choice visually dominant over "No — leave this channel
  alone", on the one prompt in the product where a human authorises something
  that could lose funds if the premise were wrong. The outpoint is now
  abbreviated on the button; the prompt directly above still states it in full
  twice, and what stops a stale form answering about the wrong channel is the
  question id rather than the reader.
- **The batch summary always soft-wrapped.** Two spaces plus a 14-wide amount
  plus a 66-character pubkey is 84 characters against a 78-column pane, so the
  first screen an operator sees made a correct batch look mangled. The peer is now
  on its own indented line — not abbreviated, because this is the screen where it
  is checked — and every line is inside the pane. `winthistle run`'s pre-arm
  confirmation shares the function and gets the same fix.

### What rendering `doctor` and the recovery screens found

Both were rendered against the live harness. `doctor` has a route; the recovery
screens do not — they reach the browser as part of a run's transcript, which is
worth being precise about:

| screen | how it reaches a browser |
|---|---|
| `doctor`'s report | `GET /doctor` |
| `prose.Recovery` — what an abort would do | written to the run's transcript by `recoverRun` |
| `prose.RecoveryOutcome` — what it did | the same |
| `prose.BluntConfirmation` | the pending question's prompt, and now the transcript record too |
| `prose.RecoveryList` — the runs that stopped | **no route at all.** `winthistle recover` with no argument is the only way to it, and it is the screen an operator reads on a node that is down. Item 1.6 |

**The pasteable command survives a soft wrap.** `doctor`'s report allows exactly
one thing past the pane — a shell command, because a command cannot be wrapped
without changing it — and in a browser that line came out 189 characters, wrapped
across three visual rows. It is still *one line in the DOM*, so selecting it
copies the intact command. That is the property the pane exemption depends on and
it now has a measurement behind it rather than an assumption. Exactly two lines
exceeded the pane on a real report, both absolute paths, which is the documented
case.

**Both shapes of recovery screen read correctly.** The partially-armed one —
forced by asking a peer for less than LND's minimum, so `arm.Open` returns partway
— says "everything about this run is cancellable for free" and cancels one shim
with no confirmation asked, because nothing reached `chan_pending`. The armed one
abandons two channels, asks per channel, and reports "of those, blunt flag 2".

Four defects came out of it, and none of them was cosmetic:

- **The transcript kept no record of what the operator was asked.** A question
  vanished when it was answered. The blunt flag was authorised for two channels,
  and a reload showed no trace of either — the journal had the count, and the
  transcript, which decision 2 calls the thing rendered from the beginning on
  every attach, had nothing. `Run.Ask` now writes a summary of every resolved
  question: the subject, and the label of the choice taken, or the reason nobody
  answered. `TestTheTranscriptKeepsWhatWasAskedAndAnswered`.
- **The dress rehearsal's report claimed `testmempoolaccept` had refused when
  Core was never asked.** `Measurement.Accepted` is a bool, and false is both
  "Core refused" and "we stopped before asking" — so a rehearsal that failed in
  the finalizer printed *"testmempoolaccept refused the result: """*, with the
  real error reported separately. That is a false statement in operator copy
  about the one call that decides whether a batch is worth arming, and it cost
  twenty minutes here looking for a fee problem that did not exist.
  `Measurement.Asked` now separates them.
- **`internal/plan` had no pane test**, alone among the report packages, and it
  renders two of the twelve screens — including the plan document an operator
  approves before the cold wallet comes out. A 79-character line had shipped in
  the verification report, and its width depended on the vsize's digit count, so
  a big batch would have been worse. `TestTheReportsFitThePane` and
  `TestTheEstimatedSizeNoteFitsWhateverTheSizeIs` close it.
- **Two lines went to the transcript unwrapped** — the armed-window failure
  (which carries LND's verbatim errors, 251 characters on one of them) and the
  `psbt_verify` confirmation. They were the only paragraphs in the transcript not
  written to the pane, at the two moments an operator is reading hardest.

And one test was measuring the wrong thing: `internal/doctor`'s pane check counted
*bytes*, so every em dash in that report read as three columns. It counts runes
now, like every other pane test in the repository. A byte count is worse than no
check, because it is the kind of wrongness that gets fixed by widening the pane.

## The journal's own screens, and the four decisions behind them

`prose.RecoveryList` was the last of the four recovery screens with no route at
all. It has one now — `GET /recover` — with `prose.Recovery` under `GET
/recover/{id}` beside it.

It was the one to do first for a reason that is also the constraint on it: **it
needs nothing but the journal**, so it is the screen an operator reads on a node
that is down. Nothing in `internal/server/recover.go` or in the two launcher
methods behind it dials LND or Core, and
`TestTheRecoveryScreensDoNotNeedANode` builds a launcher whose configuration
names neither and renders both screens through it.

### Decision 1 · A journalled run is not a live run, and the screen says which

`Registry` holds the runs this process is driving; the journal holds runs that
stopped, possibly from a process that is gone. **They overlap**, and not
marginally: a run started from this UI writes its journal row when the funding
streams open and stays in `Unfinished` until it is published or aborted, so for
most of its life it is in both. An operator who reads a journal row as "this is
running" waits for something that is not happening.

The screen lives at its own path rather than folded into `GET /`, because two
lists of run ids on one page is the blur rather than a fix for it. What separates
them is three things:

- **Two paths and two vocabularies.** The registry's runs are on the overview
  under "Runs" and at `/runs/{id}`. The journal's are under `/recover`, and the
  screen's own first line is "the run journal".
- **A paragraph above the rows, written by the process rather than by `prose`.**
  `journalNote` names the live run — there is at most one — and says whether its
  row is below, and that the row is a snapshot rather than a view. It cannot live
  in `prose`, because `prose.RecoveryList` is shared with `winthistle recover`,
  which has no registry.
- **The empty list, which is the case worth building the rest for.**
  `journal.Begin` has not written anything in the first seconds of a run, so the
  journal can be empty while a batch is being armed — and `prose.RecoveryList`
  renders "No unfinished runs. Nothing to recover." over it. That sentence is
  true about the journal and false about the node. It is the same failure mode as
  listing runs without listing the CPFP children, and it now says so:
  `TestAnEmptyJournalDoesNotClaimACleanNodeWhileARunIsGoing`.

Both branches were rendered against a real run in a browser, not only asserted.

### Decision 2 · `GET /runs/{id}` does not fall back to the journal

It still 404s. A fallback is the obvious convenience and it makes one URL mean
two things: a live run, with a transcript, a pending question and a control that
stops it — or a journal row with none of those. Which one an operator gets would
depend on a fact they cannot see from the URL, and it is exactly the fact that
decides whether waiting is reasonable.

**What it costs is real and is worth naming.** An operator who bookmarks a run
screen and comes back after a restart gets a 404 rather than the record. What
they get instead is a 404 that says where the record is and links to it, and the
copy on that page now describes what a journal row is and is not.
`TestRunsIDDoesNotFallBackToTheJournal` puts the run in the journal and requires
the 404 anyway.

### Decision 3 · The UI lists, and refuses to abort

`run.RecoverOne` would be the fourth unsafe method in this repository and the
first that abandons channels on a run this process never started. It is not
here, and the reason is mechanical rather than cautious: it asks
`abort.Confirmation` **once per channel** — a func per channel, so that a yes
about one channel cannot authorise the next — and a browser answers those through
`server.Run.Ask`, which needs a `Run`. A journalled run has none.

Inventing one is the move to refuse. A synthetic registry entry for a run this
process is not driving would put a journal row on `/runs/{id}` with a question
and an abort control on it, which is decision 1's blur, at the moment an operator
can least afford it.

So the screen ships the listing and points at `winthistle recover ID`, which
already exists, already asks per channel, and is already tested on both shapes of
a bad night. **There is no `POST` route under `/recover`**, so a control someone
adds later is a 405 from the mux rather than a handler nobody reviewed —
`TestTheJournalScreenOffersNoAbort` asserts both the absent form and the 405.

What the screen *does* still ask is whether a run may be aborted **at all**,
because that decides whether to name that command or warn against it. The answer
comes from `journal.Run.AbortTarget` through `Launcher.AbortRefusal` — the same
function `run.RecoverOne` refuses on and the same one the live abort control
asks. `internal/server` cannot hold a second copy of that rule; it may not import
`internal/journal`.

### Decision 4 · The launcher returns rendered text, plus the ids

Two methods: `Unfinished(ctx) (text string, ids []string, err error)` and
`Journalled(ctx, runID) (string, error)`.

Text rather than a data type the server could render, for decision 3's reason.
`prose.RecoveryList` and `prose.Recovery` are the highest-stakes copy in the
product and the overrun tests are over them; a `[]server.JournalRun` the server
rendered would be a second rendering of that copy, measured by nothing, drifting
from the one `winthistle recover` prints. The ids are the exception because they
are not copy: a link needs them, `prose` does not render links, and they are the
same strings `PathValue` already hands the package.

`journal.ErrNoRun` becomes `server.ErrNoJournalledRun` on the way across, because
the ban means the server cannot name the real sentinel. It is a translation and
not a second decision — the difference between "no such run" and "the journal
could not be read" is the difference between a 404 and a page that must not
pretend it looked, and both are kept.

### The pairing has one function now instead of two conventions

`winthistle recover` with no argument listed the runs and then the CPFP children,
and the reason is not symmetry: Core leaves locked outputs out of `listunspent`,
so an unfinished child looks exactly like change that was already spent. A screen
that listed only the runs would tell somebody their node is clean while a coin of
theirs is locked.

That pairing was a comment in `recoverCmd`. It is now **`run.Unfinished`**, which
writes both halves, and `run.List` is unexported as `listRuns`. An exported
function that lists the runs and not the children is a trap: it reads like the
whole answer, it is the obvious thing for a third front door to call, and what it
leaves out is invisible from the screen it produces. Both front doors call the
one function; `TestTheRecoveryListPairsRunsWithTheirChildren` seeds a journal
whose only unfinished thing is a child and requires it on the web screen.

### The trap, and how it was avoided

`TestOnlyTheDoctorScreenReadsTheRequestContext` requires every `r.Context()` in
`internal/server` to be inside the `doctor` handler. Both new handlers take their
context from `Server.base` with a timeout instead, which is what the abort
control's journal read does. The allowlist was **not** widened.

The reason it is right rather than merely required: a browser that gives up
mid-read must not change what a screen says. On the abort control that turns a
refusal into a permission; here it would turn "the journal has these three runs"
into "the journal could not be read", which is the same class of mistake with a
smaller blast radius and no reason to prefer it. `journalReadTimeout` is 30
seconds and is deliberately not `abortCheckTimeout`: that one bounds a refusal,
where an unanswered question is a no, and this one bounds a screen, where an
unanswered question is an error page.

`doctorMu` is not taken. It serialises the pre-flight so two of them cannot open
the journal twice; these handlers open it, read, and close, which
`Launcher.AbortRefusal` has done since the last slice.

### What rendering it found — seven defects, none of them cosmetic

Six were in `internal/prose` and had been shipping in `winthistle recover` all
along; the browser is only where they were finally looked at.

1. **`prose.RecoveryList` had no pane test at all**, alone among the recovery
   screens — `TestTheRecoveryScreensStayInThePane` covered `Recovery` and
   `BluntConfirmation` and not the list. It was **83 columns at one channel per
   state and 88 on a batch of a few dozen**, because a half-aborted run carries
   channels in all five states and the counts went out on one unwrapped line.
   That is the same defect `internal/plan` shipped, for the same reason.
2. **The columns did not line up.** The id field was `%-14s` and `NewRunID`
   produces 22 characters, so two ids of different lengths put the state column
   in two different places — and every row's detail line sat at column 17, which
   is *inside* the id above it rather than under anything. `bump.List` had the
   same field and the same two faults. Both now measure the widest id, clamp it
   so one hand-picked `--id` cannot pad every other row off the pane, and put the
   details at a four-space indent beside the txid.
3. **Two screens pointed at sections they did not have.** The list said an
   unabortable run was explained "see below" and that paragraph was the last
   thing on the screen; the abort plan said the blunt-flag prompt was described
   "see below" and it is described in `BluntConfirmation`, which appears only
   when the abort runs. Both survivable in a terminal, where the next command's
   output follows. Both read as broken the moment a page ends.
4. **The list asserted that every run in it had stopped.** "They stopped
   somewhere they should not have" — read next to a run that was arming at that
   moment. The journal knows a run is neither published nor aborted; it does not
   know whether something is driving one. The sentence is conditional now.
5. **And then said "…right now, They stopped…"** — `theyThey` returned a capital
   because that clause used to open the paragraph. A test asserting the new
   clause passed while the screen read wrong.
6. **`unwinding` and `stillUnwinding` went to the terminal at 185 and 377
   columns.** The two lines Ctrl-C prints while a batch with *n* shims open comes
   apart underneath, and the only operator copy in `internal/server` that does
   not go through `screen()` — so no page test was ever going to look at it.
   `internal/server` had no pane test of its own copy, which is the same gap
   `internal/plan` had.
7. **An empty `journal.State` rendered as a sentence with its subject missing** —
   "The journal has this run as ." — and as eleven blank columns in the list.
   Unreachable through this journal, `state` being `NOT NULL`, which is exactly
   why it rendered whatever the last person assumed.

### Every defect these four slices found, and the guard on each

Kept as a table because the prose above is spread over four sections and a defect
described in prose is a defect that comes back. **Sixteen found, sixteen with a
mechanical guard.** Five of them had only prose for a while, which is how the
list came to be written.

| what was wrong | how it was found | what stops it returning |
|---|---|---|
| `recoverRun` ran on the context whose cancellation triggered it, so Ctrl-C during a run took *nothing* apart | reading the code while wiring the abort control | `TestCancellingMidRunStillTakesTheBatchApart` — the only test here that cancels a run mid-flight. Verified to fail on the pre-fix code |
| `Origin: null` on a browser's form POST was read as a foreign origin, so every form in the UI answered `403` | the first click, in the first browser render | `TestABrowsersFormPostIsAdmitted`, plus test helpers that send the measured headers |
| The transcript kept no record of what the operator was asked; an answered question vanished | rendering the recovery screen and reloading it | `TestTheTranscriptKeepsWhatWasAskedAndAnswered`, and the assertions in the browser-driven cold probe |
| The rehearsal's report claimed `testmempoolaccept` had refused when Core was never asked | rendering a rehearsal that failed in the finalizer | `TestTheReportDoesNotInventAVerdictFromCore` |
| The blunt-abandon button carried the full 66-character outpoint, wrapped to three lines, and outweighed the safe choice beside it | looking at the rendered screen | `TestEveryPromptAndButtonFitsThePane` |
| The batch summary always soft-wrapped: 84 characters against a 78-column pane, on the first screen an operator sees | looking at the rendered screen | the same test |
| A 79-character line in the plan verification, whose width grew with the vsize's digit count | rendering, because `internal/plan` had no pane test | `TestTheReportsFitThePane` and `TestTheEstimatedSizeNoteFitsWhateverTheSizeIs` |
| Two lines written to the transcript unwrapped — the armed-window failure at 251 characters, and `psbt_verify` | measuring a rendered transcript | `assertFitsThePane` over a whole real transcript in the browser-driven cold probe |
| `internal/doctor`'s pane test counted bytes, so every em dash read as three columns | writing the neighbouring test | the test itself, now counting runes |
| `prose.RecoveryList` had no pane test and ran to 88 columns: a half-aborted run carries channels in all five states and the counts went out unwrapped | giving it a route, then measuring it | `TestTheRecoveryScreensStayInThePane` over both list shapes, and `TestTheBreakdownNeverBreaksACountAwayFromItsState` |
| The list's id field was `%-14s` and a run id is 22, so unequal ids put the state column in two places and every detail line sat *inside* the id above it. `bump.List` had both faults too | rendering it in a browser | `TestTheListColumnsLineUp`, with ids of different lengths and a 90-character one |
| Two screens said "see below" where nothing below said it — the list's publish warning was the last paragraph, and the blunt-flag note points at `BluntConfirmation`, a different screen | rendering them as pages that end | `TestNoScreenPointsBelowItself` |
| The list asserted "They stopped somewhere they should not have" about a run that was arming at that moment | reading it beside a live run in a browser | the conditional clause, asserted in `TestRecoveryListSaysWhenSomethingMustNotBeTouched` |
| …and then read "…right now, They stopped…", because `theyThey` capitalised for a position the clause no longer had | the next render, after a test on the new clause passed | the same test, which now refuses a capital after a comma |
| `unwinding` and `stillUnwinding` printed 185 and 377 columns to the terminal, while a batch with *n* shims open came apart underneath | writing `internal/server`'s first pane test | `TestTheServersOwnCopyFitsThePane`, over every copy function in the package |
| An empty `journal.State` rendered as "The journal has this run as ." and as a blank column | auditing zero values before rendering | `TestABlankStateIsNotRenderedAsAVerdict` |

Three patterns are worth more than the list:

- **A header a browser controls is not a header a test may invent.** The `403`
  shipped green because every test chose an `Origin` no browser sends. The same
  shape of error is available anywhere a test supplies input the real client
  produces.
- **When a report renders a verdict, check that the zero value is not a verdict.**
  `Accepted bool` meant both "Core refused" and "we never asked", and the report
  read the second as the first.
- **A defect fixed with prose is a defect with no guard.** Five of the first nine
  sat in HANDOFF and in a commit message with nothing enforcing them. Prose says
  what happened; a test says it will not happen again.
- **The screen with no pane test is the one that is over the pane.** It was
  `internal/plan`, then `prose.RecoveryList`, then `internal/server`'s own copy —
  three for three. Every package that renders operator text has one now, and the
  two that print to a terminal rather than to a page are the ones to check first,
  because no page test will ever look at them.
- **An assertion that passes is not a screen that reads.** The capital in
  "…right now, They stopped…" shipped past a test written for that exact
  sentence, in the same session, minutes after the sentence was written. Render
  it.

### What this slice does not do

No transports beyond the minimum — the browser signer is one read-only field out
and one field back, and the file up/down goes *around* that same `Question` rather
than beside it. (Written before the transports slice: the download exists now, and
the animated QR this paragraph also named is out of scope as of 2026-08-24.) No countdown. No setup or
bump screen, so two of the four adapters have no POST yet. A browser-driven run
uses the browser for every device: the `[[signer]]` blocks supply the labels and
the count, not a command, and a signer with a working `hwi` command cannot yet be
mixed in — that is transport selection, and it belongs with item 1.4. (Item 1.4 is
done: a commanded device is answered by its command now. See "Mixing them per
device" above.)

Exercised against the live harness: the browser-driven cold probe above; `421` for
a rebinding `Host`, `403` for a cross-origin fetch and for a missing token; and
`GET /doctor` returning the ten-check report in about 250 ms.

**And exercised in a real Chromium**, which is what found the `Origin: null` bug.
The whole ceremony, in a browser, against the harness: the token-in-query traded
for a cookie and `303`d to a bare `/`, the cold-probe checkbox, the run started by
a click, four real partial signatures read out of a read-only `<textarea>` and
pasted back into an editable one, two per-channel blunt confirmations each naming
its own outpoint under its own question id, step 9 withheld, the batch taken
apart, and the abort screen then declining to offer a control for a run that had
stopped. **Zero console messages**, so the `default-src 'none'` CSP blocks nothing
the pages need. The driver is not committed — it is a scratch script — but it is
reproducible from this description in a few minutes, and the durable half of it is
`TestABrowsersFormPostIsAdmitted`.

## The three report screens, and the two that are not screens

The five `Report()` renderers item 1.7 was still missing turned out not to be
five of a kind, and finding that out is most of what this slice produced.

### They already reached a browser, which is what made the decision

`run.Do` prints every one of the five to `d.Out`, and for a browser-driven run
`internal/webrun` sets `d.Out = r` — the `*server.Run`, which is the transcript.
So a run's copy of all five reports is already served, verbatim, in the `<pre>`
at `/runs/{id}`. Nothing stores a `peers.Facts`, a `fees.Rate` or a
`reserve.Finding` anywhere, and there is no settlement table in the journal.

That settles "does a screen re-run the check, or view what a run produced": the
second option does not exist. Viewing would mean either a second rendering of the
transcript or a new store for values nothing else keeps, and neither is worth a
route. So the three that can be reached standalone re-run, and the reasoning is
in `internal/server/reports.go` rather than in a commit message.

### Why re-running is cheap here and is not a second pre-flight

Three things, and the middle one is the load-bearing one:

- **They are read-only by construction.** `peers.Check`'s own comment says it
  opens no funding stream, so it starts no clock and costs nothing to run again;
  `fees.Estimate` is one `estimatesmartfee`; `reserve.Check` is a balance, a
  lease total and three `RequiredReserve` calls.
- **None of them opens the run journal.** That is what `doctorMu` actually
  serialises — two concurrent pre-flights opening the journal twice and reporting
  one failure in two places — so these take no share of it and a doctor screen
  loaded beside a report screen collides over nothing. This is also why they do
  not go through `run.Connect`, which opens the journal: a handle held behind a
  screen an operator reloads is a write lock held against `winthistle recover` in
  another terminal.
- **They are not a second rendering.** `doctor` calls the same three checks and
  prints `Summary()` plus its own verdict-and-fix lines; these print `Report()`,
  which is the text `winthistle run` prints in Phase 0. Both renderings already
  existed and both were already measured against the pane in their own packages.

What separates them from `doctor` is the question. `doctor` answers "is anything
wrong", one line per check with the command that fixes it. These answer "what are
the figures I am about to commit to", asked at a different moment.

### The one leg that would change the node, removed rather than guarded

`peers.Check` dials a peer whose `Want` carries a host. `winthistle doctor`
strips the hosts unless given `--connect`, and there is no `--connect` on a page,
so `webrun.noHosts` strips them unconditionally before the call. Its own function
so the rule can be tested without a node.

**This is the screen's rule and not the whole UI's**, and the copy says so
because a browser showed the version that did not: `winthistle serve --connect`
sets `doctor.Options.Connect`, so the doctor screen in the same tab strip *will*
dial. Copy claiming the connecting version lives only in a terminal would have
been false about the screen next to it.

### Three routes rather than one, on purpose

Each report dials only what its own check needs — `/fees` is Core alone, `/peers`
and `/reserve` are LND alone — so a report answers on a node that is half down.
That is the same property `/recover` has for the journal, and it is worth more
than one screen that needs everything.

`/peers` refuses without a batch (`server.ErrNoBatch`, a sentinel because "there
is no batch" is not a failure to look). `/fees` and `/reserve` answer anyway, and
`/reserve` falls back to one announced channel the way `doctor` does, saying on
the page which of the two questions it answered.

### Why there is no plan screen and no settlement screen

- **The plan document.** A `plan.Verification` comes from verifying a PSBT that
  has already been built, and building one is coin selection against the cold
  wallet — step 7, inside a run. A screen that built one to display would be the
  thing the overview already refuses a second run for: a decoy over the coins a
  batch is about to spend. One route, and the run is it.
- **The settlement report.** `settle.Settle` is called with `members(armed, …)`,
  so holding a `settle.Result` means holding an `*arm.Armed` — the type decision
  1's import ban stops `internal/server` naming. It cannot come off the journal
  either: there is no settlement table, and the journal has no migrations, so a
  table added to serve a screen is schema this build would owe forever.

Both already reach a browser through the transcript of the run that produced
them. That is the finding rather than the gap.

### What the browser found, again

Six render passes had produced ~30 defects; this one produced four more, three of
which no test could see:

- **The same paragraph three times.** Every peer's `Report()` closes with the
  identical four-line "nothing here is authoritative" caveat, and a page showing
  three peers at once put those four lines in the reader's way three times in
  forty. A rule between the reports fixes it without touching any report —
  `webrun.peerRule`, at `prose.PaneWidth`.
- **Two paragraphs both explaining I-4** on `/fees`, four lines apart, plus the
  report's own third statement of it at the bottom. The standing note dropped it;
  the live-run note kept it, because that one is aimed at an operator looking at
  a higher number with a batch already going.
- **Two paragraphs both opening on "this node's own on-chain wallet"** on
  `/reserve` with no batch. Same cure: the second one now says only which
  question was asked.
- **"There is no RBF on this transaction"** with no transaction. `fees.Rate.Report()`
  is written for Phase 0, where one is about to exist; on a standalone screen
  "this transaction" named nothing, so the screen's standing note says which
  transaction it would be. The report was not edited — that would have been wrong
  for the run.

The Kind trap was checked in a browser rather than only in a test: a live setup
adds no paragraph to any of the three, because a setup opens no channel, connects
to no peer and pays no fee.

## Next actions, in order

1. ~~The rest of the UI~~ — **done.** Every sub-item below is struck through and
   1.4 was the last of them. The security shape, the three decisions with their
   guards, the four callback seams and the abort control all exist — see "The
   server, and the three decisions with guards on them" and "The four seams"
   above. What remains for this UI is not on this list: it is whatever the next
   browser session finds, plus `run.RecoverOne`'s route, which is decision 3
   rather than an omission. Kept in full because each entry carries the decision
   it made:

   1. ~~The four callback seams~~ — done, with the `POST` that starts a run.
   2. ~~An explicit abort control on the run screen~~ — done, as a link to a
      screen that says what stopping costs, refused for a run that reached the
      publish call.
   3. ~~Render it in a browser~~ — done, and it keeps paying: three rendering
      sessions have now produced twelve defects between them, including a guard
      bug that made every form in the UI unusable and four false statements in
      operator copy. A browser is installed here; `npx @playwright/mcp
      install-browser chrome-for-testing` fetches the headless shell the MCP
      wants. What has *not* been driven in a browser yet is `doctor` under a slow
      node, and the screens that do not exist.
   4. ~~The transports~~ — **done, and item 1 with it.** The file transport
      shipped both legs (`GET /runs/{id}/payload/{question}` out, a multipart
      file input back) and **mixing per device** finished it: a device whose
      `[[signer]]` block names a command is answered by that command and never
      asked on the page. Animated QR was the third item here and is out of scope
      as of 2026-08-24, held open in the triage rather than rejected. They went
      *around* the existing `server.Question` rather than beside it — `Payload`
      and `Reply` were already the seam — and the mixing needed no new exported
      API at all. See "The QR decision and the file transport", whose last two
      sections are the mixing and what the browser proved about it.
   5. ~~The countdown~~ — done, and this entry was stale for a slice because it
      was never struck through: `prose.Progress` renders it on the attach screen,
      above the transcript, reaching the server through `Launcher.Progress`. See
      "Then the countdown and the live state" near the top of this file. What
      remains true and must not be re-derived: it is the *peers'* clock, not
      ours — `pruneZombieReservations` skips PSBT reservations, so our node never
      expires one — and `server.Question.Deadline` is the *gate*, a different
      clock that must not be relabelled as this one.
   6. ~~The recovery screens~~ — done. `GET /recover` and `GET /recover/{id}`,
      read-only, needing nothing but the journal. Four decisions with guards, and
      seven defects out of rendering it; see "The journal's own screens".
   7. ~~The remaining screens~~ — **done, and two of the five reports are
      decisions rather than screens.** The setup and bump screens shipped first
      and gave the last two callback seams their callers; **there is no
      written-but-uncalled code left in this UI.** The reports finished the item:
      `GET /peers`, `GET /fees` and `GET /reserve`, each re-running its own check
      and serving the same `Report()` the command line prints, plus a written
      finding that the plan document and the settlement report have exactly one
      route each and already take it. See "The three report screens" below.

      **What the bump screen decided.** It hangs off `/recover/{id}` rather than
      living under it: /recover stays read-only with no POST route, and the link
      is chrome. The link is offered **only for a run the journal refuses to
      abort**, and the two conditions are the same condition — `AbortTarget`
      refuses a run in publishing or published, which is exactly when a parent
      exists in a mempool to accelerate. A bump does **not** reuse the batch's
      run id as its registry key, because that would put it on the URL that
      means "the batch is going in this process"; the batch's id goes in
      `Run.About`. And `internal/bump` now exports `RoundPrefix`/`RoundName`,
      because webrun renders different copy for a bump round than for a batch's
      and matching on a literal spelled in two packages is how that copy lands
      on the wrong round.

      **What the setup screen decided, since the next screen inherits it.**
      Three things. First, it is `winthistle setup`'s **resume path only** and
      nothing on the form could change that: a descriptor file needs a path, and
      a path posted from a browser is a browser choosing which file this process
      opens, imports and rescans against — so the install stays in the terminal
      and the question, which is the half built to be asked twice, is what a
      browser gets. Second, `server.Run` now carries a **`Kind`**, because a
      setup goes in the same registry as a batch and almost every sentence a
      screen says about a batch is false about a setup; four screens were saying
      one of those sentences and a browser found all four. Third, it has **no
      abort control**, and `/runs/{id}/abort` refuses for a run that is not a
      batch: that control cancels an armed window with n shims, n pending
      channels and Core's coin locks behind it, and a setup has two read-only
      RPCs and a question. Its clock is not the gate either —
      `webrun.AddressCheckWindow`, fifteen minutes, and it is not a safety bound
      but how long the one-at-a-time slot is held while the operator is at a
      safe.

      **Two things the recovery slice leaves for whoever does these.** First,
      `run.RecoverOne` still has no route and that is decision 3 rather than an
      omission — an abort of a journalled run asks per channel and a browser
      needs a `Run` to ask through. If a later slice wants it, the thing to
      change is that, not the screen. Second, every one of these screens needs a
      pane test in the package that renders it before it gets a route: three
      screens in a row have shipped over the pane because nobody measured them.

   Still true before the first browser-driven armed window on anything that
   matters: the startup-token cookie is not port-scoped (see "Watch out for").
2. ~~**The adversarial harness**~~ — **done 2026-08-25.** All seven of finding
   3's scenarios have a test, and `docs/review-2026-08-triage.md` item 3 is
   `DONE` with the table re-audited row by row. Three things from it are worth
   carrying forward rather than re-deriving:

   **Two of the seven rows were already covered when the table said "absent".**
   `internal/journal/receipts_test.go` and `internal/arm/publish_refused_test.go`
   both landed one commit *after* the triage was written. Auditing the worklist
   was a third of the slice and it was the third that paid: without it the slice
   would have spent its budget rebuilding tests that existed.

   **Five of the seven are stub tests, and that is the stronger test here.** What
   these scenarios exercise is our handling of a counterparty failure — a peer
   that goes silent, a node that restarts, a backend that is gone. A stub returns
   that failure at exactly the chosen channel, every time; a container broken at
   the right moment does not. The two that stayed on the cluster are the two
   whose subject is LND's own behaviour. `internal/arm`'s publish test is still
   the only test in this repository that publishes and must stay so.

   **The one thing driving them found is a code gap, and it is item 3 below.**

3. **Nothing bounds the wait for a `chan_pending`, and the fallback shares the
   context that ends it.** `arm.finalizeOne` blocks in `Recv` with no deadline of
   its own, and `winthistle run` builds its context from `signal.NotifyContext`
   and nothing else — so a peer that accepts `psbt_finalize` and never sends
   `funding_signed` parks the armed window until `Ctrl-C`, with the rest of the
   batch already armed behind it. Then, because the context that ended the wait
   is the one `isPending` is asked on, the lookup that decides abandon-versus-
   cancel fails too, and the operator is told "this channel's state is unknown"
   about a channel LND could still have answered for.

   Neither is a safety failure: the batch is unarmed, unpublishable and abortable
   throughout, and `TestAPeerThatNeverAnswersFinalizeLeavesTheBatchUnarmed` pins
   both as current behaviour rather than as correct behaviour. It was left alone
   on purpose — a deadline on the armed window is a decision about the countdown
   and the 5:00 gate, not a test fixture, and the seams' rule already says which
   clock may bound what ("Watch out for"). The `isPending` half is much smaller
   than the deadline half and could be taken on its own: a `context.WithoutCancel`
   plus a short timeout would let the one question that matters still be asked.
4. **The mainnet cold probe.** `winthistle run --stop-before-publish`.
   Everything it needs exists: steps 1 to 8 are the production code path, step 9
   is one call inside one `if` that it does not make, and the abort path it
   terminates through runs on every failure and is tested on both.
5. **Nothing new at this level.** What remains is items 3 and 4, in that order,
   and only the last of them needs something this machine does not have — mainnet
   coins and real peers. Four claims on this list have now been wrong the same
   way: that no browser was installed, which cost a guard bug a single click would
   have found; that signet needed absent hardware; that finding 3 had five missing
   scenarios, when two of them had shipped a commit later; and the "what is
   missing" sentence at the top of this file, which named four built things. All
   four survived several handoffs because nobody checked. Check before writing "we
   cannot" — and check a worklist before working it.

Done since the last handoff, all from the previous list:

- **Mixing transports per device** — item 1.4's last half, and the last of item
  1. `webrun.Signers` holds a one-device `signers.Set` per commanded device, the
  rule is written where the code is, and the choice is resolved once so the
  rehearsal and the batch cannot disagree. One paragraph of copy exists only
  because a round can be mixed. See "The QR decision and the file transport".
  Rendered as a mixed cold probe against the harness, which also turned up the
  blunt-abandon confirmation's false clock — fixed in the same slice, in
  `waitingOn`.

- **The three report screens, and the two reports that are not screens** — the
  rest of item 1.7. `GET /peers`, `GET /fees` and `GET /reserve`, each re-running
  its own read-only check and serving the same `Report()` Phase 0 prints; a
  `noHosts` strip so a screen never dials a peer; and the written finding that a
  `plan.Verification` needs coin selection and a `settle.Result` needs an
  `*arm.Armed`, so both have one route and the transcript already is it. See "The
  three report screens" above.

- **The journal's two screens** — item 1.6's first half. `GET /recover` and `GET
  /recover/{id}`, the four decisions behind them, `run.Unfinished` so the runs
  and the CPFP children can no longer be listed apart, and seven defects with a
  guard each. Everything on them comes off the journal, so they are the screens
  that work on a node that is down.

- **The four callback seams and the `POST` that starts a run** — items 1.1 and
  1.2. `internal/server`'s `ask.go` and `control.go`, `internal/webrun`, the
  abort control, and a browser-driven cold probe against the harness. Five things
  turned out to be the difficulty and each carries a guard; see "The four seams".
- **`run.Connect`**, so the CLI and the UI dial the same way, and the
  `internal/journal` ban on `internal/server`.
- **A pre-existing bug in the abort path**: `recoverRun` ran on the context whose
  cancellation had just triggered it, so Ctrl-C during a run would have reported
  `context canceled` and taken nothing apart. It now runs on
  `context.WithoutCancel` with `run.TeardownBudget`. The web abort control
  depended on this entirely.
- **The server's first slice**, before that — `winthistle serve`, the startup
  token, the `Origin`/`Host` guard, the run-attach registry, and `doctor`'s
  screen end to end.

## Watch out for

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

- **`Ctrl-C` on `winthistle serve` now cancels a run in flight, and it used to
  say it did not.** That row of decision 2's table changed in the seams slice and
  the reasoning is with it: a run left alive while the process exits is killed
  between two RPCs with *n* shims open. A tab closing is still nothing at all.
  Whatever else changes, do not let a *transport* event end a run.

- **A browser's same-origin form POST carries `Origin: null`, and a test may not
  invent that header.** Measured on Chrome 152, both referrer policies, real click
  and scripted submit. It is the opaque-origin serialisation and it means nothing
  in either direction, so the guard treats it as absent and leans on
  `Sec-Fetch-Site: same-origin` — which page JavaScript cannot set. Every test in
  `internal/server` had chosen a real `Origin`, which is why a `403` on every form
  in the UI shipped green. The helpers now send the measured headers and say why.

- **A device's transport must not be recomputed between the rounds.** The
  rehearsal's number predicts the armed window only because the two rounds go
  through the same transport, so `webrun.newSigners` resolves the choice once, off
  the configuration, and `Signers.Round` indexes it. If a later change makes that
  choice depend on anything that can move — a flag on the form, a probe of whether
  the command still exists, a fallback when it fails — that is the invariant
  breaking rather than a nicety, and the measurement quietly stops meaning
  anything. `TestTheTransportIsPerDeviceAndTheSameInBothRounds` is the guard.

- **A browser is installed on this machine.** Two handoffs said there was not,
  and that claim was never checked; `which chromium google-chrome firefox` finds
  three. Render before believing a page works. Three rendering sessions have now
  produced twelve defects between them, most of which no test would have caught,
  and four of which were false statements in operator copy.

- **`estimatesmartfee` can start answering on regtest, and one fees test has no
  guard for it.** `TestOnRegtestWithNoFloorThereIsNoRate` asserts there is no rate
  when nothing is configured, which holds only while Core has no estimate. Mine
  enough blocks carrying fee-paying transactions — a browser render followed by
  `make -C regtest mine N=2016` did it — and Core produces one for a while (1.11
  sat/vB, observed once) before its estimator decays back to "Insufficient data".
  The test then fails with "a rate of 1.11 sat/vB was produced with nothing behind
  it", which reads exactly like the refusal having been lost. It is harness state:
  re-run it. Note that its sibling above it,
  `TestRegtestHasNoFeeEstimateAndTheFloorCarriesIt`, has the `t.Skipf` guard for
  precisely this state and says so in its message; this one does not, and giving it
  the same guard is the fix if it recurs.

- **`go test ./...` has a second way to fail now, and it is not regtest.**
  `internal/webrun`'s `TestTheRoundsDeadlineIsSharedByEveryDevice` races two
  goroutines against a two-second poll, and under twenty package binaries
  competing for the CPU it lost once and reported "the seam never asked
  anything". It passes standalone and under `make check`. Same rule, second
  reason: `-p 1`, always.

- **`git checkout <file>` discards unstaged work, and this repository is worked
  on with everything unstaged.** One `git checkout internal/prose/recovery.go`,
  used to undo a two-line experiment, threw away six reapplied edits and cost
  fifteen minutes. Copy the file to the scratchpad and copy it back.

- **A `bool` that means "it failed" cannot also mean "we never tried".**
  `rehearsal.Measurement.Accepted` was both, and the report read the second as the
  first: a rehearsal that died in the finalizer announced that
  `testmempoolaccept` had refused it. `Asked` separates them now. When a report
  renders a verdict, check that the zero value is not a verdict.

- **A Core container restart unloads the harness wallets and the tests reload
  nothing.** Symptom: `Requested wallet does not exist or is not loaded`, from
  `getwalletinfo`. `./bin/bcli listwallets` will show only the
  `winthistle-*-test` wallets that earlier test runs created — `miner`,
  `cold-watch`, `cold1` and `cold2` are gone. `./bin/bcli loadwallet NAME` for
  each of the four is the quick fix; `make -C regtest bootstrap` is the blunt
  one. Distinct from the generation mismatch below, which bootstrap cannot fix.

- **The seams' bound is the 5:00 gate and never the peers' ten minutes.** The
  gate is ours and enforceable; the peers' clock is theirs, because
  `pruneZombieReservations` skips PSBT reservations. `config.Validate` already
  refuses a gate greater than or equal to `rehearsal.PeerWindow`, which is what
  makes a gate-derived deadline inside the peers' window by construction. If a
  seam ever needs a longer wait, the answer is not to reach for `PeerWindow` —
  `internal/webrun` is tested for not reading it.

- **The startup-token cookie is not port-scoped, and cookies never are.** Any
  other server on 127.0.0.1 that the operator's browser visits is sent this
  cookie, so a second local service can read the token out of its own request
  logs. That is not a regression on the threat model — `docs/design.html` is
  explicit that a localhost bind is not an authentication boundary and any
  process on the machine can reach the socket anyway — but do not describe the
  token as protecting against local software. It protects against a *web page*:
  DNS rebinding, a cross-origin form post, a stray `fetch`. `internal/server`'s
  package comment says so in those words; keep it saying so.

- **Not every question a batch asks is a signing round, and the clock sentence
  used to assume it was.** `waitingOn` called the blunt-abandon confirmation's
  deadline "the 5m0s signing gate" and said letting it pass cost "one more signing
  round" — during a teardown, where the round is over and what it actually costs is
  an abort finished by hand. It branches on `Question.Reply` now: non-empty means a
  packet is being asked for. If a fourth kind of question is ever added to a batch,
  that discriminator is what has to be revisited, not the copy.

- **A report written for a run says things that are false on a screen of its
  own.** `fees.Rate.Report()` closes on "there is no RBF on this transaction
  (I-4)", which is exact in Phase 0 and names nothing at `/fees` with nothing
  going. The cure is a note from the screen saying which transaction it would be,
  never an edit to the report: the report is right where it is called from, and
  editing it would break the caller that matters. Same shape as the pane rule
  below — what the page owes a report is framing, not rewriting.

- **`winthistle serve --connect` makes the doctor screen dial peers, and the
  report screens still do not.** `doctor.Options.Connect` is the server's, so a
  server started with that flag has one screen that connects and one that never
  will. Copy on `/peers` says exactly that, and the earlier draft — which put the
  connecting version in a terminal — was false about the tab next to it.

- **A served report is regularly wider than the pane, and the page must not fix
  it.** `internal/doctor`'s own pane test uses a synthetic report, and a real one
  against a real node contains tokens `prose.Wrap` cannot break: an absolute path
  to `winthistle.toml`, a 66-character pubkey, a descriptor with its checksum.
  A width assertion at the HTTP layer therefore measures `$TMPDIR`'s length —
  `server_regtest_test.go` had one for exactly one commit and it passed only
  because `t.TempDir()` is short. What the page owes the report is not to reflow
  it (`TestTheTextIsTheOracle`) and to soft-wrap what it cannot shorten
  (`TestALongLineSoftWraps`: `white-space: pre-wrap` and a `max-width` in the
  same column the text was written to). Fixing an overrun by rewrapping in HTML
  would break decision 3's oracle, which is the point of having one.

- **`getaddressinfo`'s `parent_desc` can name the wrong descriptor, and it is not
  a gate.** A Core wallet can hold two descriptors that derive the same address —
  the state a corrected setup leaves behind — and Core credits the address to the
  key manager created first, active or not. Measured: three of five addresses of a
  *correct* pair attributed to a rejected one. `AddressCheck.Recognised()` is what
  the setup question is gated on; `Attributed()` is reported. If you find yourself
  gating on `Consistent()` again, that is the bug — see "`winthistle setup`".

- **Core deactivates the descriptor it replaces and keeps its coins.** One active
  external and one active internal per wallet, no RPC to remove either, and
  `listunspent` still lists a deactivated descriptor's coins. So a corrected setup
  is a new wallet name, not a second import.

- **The CPFP child is replaceable and the funding transaction never is.** Two
  verifiers enforce opposite rules on purpose: `internal/plan` refuses a funding
  input below `MaxNonReplaceableSequence`, `internal/bump` requires exactly
  `MaxBIP125Sequence`. If you find yourself wanting one verifier with a flag,
  that is the invariant asking to be switched off — see "The second lift".

- **The child's sequence check is equality, not "anything replaceable".** The
  child is version 2, so a sequence with bit 31 clear is a BIP-68 relative
  timelock rather than an RBF signal, and a timelocked CPFP child cannot enter a
  mempool at all.

- **A second lift has to beat an absolute fee.** BIP-125 rule 3 compares totals,
  not rates, and Core says only `insufficient fee`. `bump.checkReplacement`
  refuses before a device is asked and names the target that would work.

- **`listunspent` cannot see a change output a mempool transaction has spent**,
  which is the state every second lift starts in. The change is reconstructed
  from the parent's outputs plus `getaddressinfo` — whose `hex` field is the
  witness script behind a P2WSH address, and whose `ismine` is true on a
  watch-only descriptor wallet while `iswatchonly` is false.

- **A CPFP child has a fee-rate ceiling of 10,000 sat/vB and this build cannot
  raise it.** btcwallet calls `SendRawTransaction(tx, false)` and rpcclient turns
  that into `maxfeerate: 0.1` BTC/kvB; no WalletKit parameter reaches it. Because
  a child concentrates the package's lift into ~150 vB, its own rate is roughly
  fifty times the package target on a three-channel batch — so a ~210 sat/vB
  package target is the practical limit, and lower on a bigger batch.
  `bump.Verify` refuses above it before any device is asked, and names
  `sendrawtransaction <hex> 0` as the way out. Full detail under
  "`winthistle bump`".

- **`bitcoind.Client.TestMempoolAccept` passes no `maxfeerate`**, so our own
  pre-flight enforces that same ceiling. Do not "fix" that by passing zero: the
  pre-flight should apply the ceiling the broadcast will, and the earlier,
  better-worded refusal is what an operator should hit first.

- **Core hides locked outputs from `listunspent`**, so a change output an
  unfinished bump is holding looks exactly like one that has already been spent —
  and the two call for opposite actions. `bump.Locate` consults the journal to
  tell them apart and names the holding bump. Nothing else can.

- **`getmempoolentry`'s ancestor figures include the transaction itself.** For a
  batch funded from confirmed coins they equal its own, which is the ordinary
  case and why the difference is easy to miss. `walletcreatefundedpsbt` charges
  for the whole ancestor package, so the arithmetic has to use the ancestor
  figures when `ancestorcount > 1`.

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
- **A shim probe that succeeds is not free.** The peer holds a reservation for
  about eleven minutes and `shim_cancel` does not tell it otherwise. Probing all
  *n* peers and then arming collides with itself against any peer running LND's
  default `--maxpendingchannels=1`. `peers.ReadyToArm` is the gate; the long
  version is in "Phase 0" above. A probe that is *refused* costs nothing at all,
  because every limit check runs before the peer creates a reservation.

- **`UpdateChannelPolicy` reports failure inside a success.** Nil error,
  `failed_updates` populated. Reading only `err` records a policy that was never
  applied. `settle.ApplyPolicy` is the only place this build calls it.

- **`minimum_depth` is not readable.** It is in `accept_channel` and in
  `OpenChannel.NumConfsRequired`, and in no RPC. `settle.ExpectedDepth` predicts
  what a stock LND peer will do and `State.ObservedDepth` records what it
  actually did, which is only knowable after the fact. Do not add a UI that
  promises "usable after k confirmations" up front — the design asks for it and
  it cannot be delivered.

- **`estimatesmartfee` succeeds when it has nothing to say.** No `feerate` field,
  an `errors` array, and a 200. Every regtest node is in that state permanently.
  `-fallbackfee` does not help: it is a wallet setting and `estimatesmartfee`
  never consults it.

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

- **Core does the CPFP arithmetic for you, and it is the same arithmetic.**
  `walletcreatefundedpsbt`'s `fee_rate` applies to the whole unconfirmed ancestor
  package. Passing "the rate I want the child to pay" would badly overpay; the
  right value is the package target. Verified to the satoshi against
  `plan.ChildFeeSat` at three rates.

- **`chan_pending` txids are chainhash bytes**, i.e. reversed relative to every
  txid a human or Core sees. `lnd.ChannelPointFromPending` handles it; hex-encoding
  those bytes directly yields a plausible txid that matches nothing.
- **`lnd@latest` resolves to `v0.0.2`**, a retracted tag — lnd's real versions are
  pre-releases. Pin `v0.19.3-beta`, matching `regtest/.env`.
- **lnd needs a forked protobuf.** `lnrpc` uses `protojson`'s `UseHexForBytes`,
  which only exists in Lightning Labs' fork. A dependency's `replace` does not
  apply transitively, so `go.mod` restates it. Do not remove it.
- **grpc is held at lnd's own pin (v1.59)**, which has no `grpc.NewClient`;
  `DialContext` is deliberate, not legacy.
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
  "Unsupported script type". Worth knowing before designing the signing UI: it
  must collect exactly *m*.

- **A device that returns a *finalized* PSBT is refused, on purpose.** That is
  I-2: a finalized input is a complete witness, so that device held a
  broadcastable transaction. It is the right refusal and it is also the one most
  likely to surprise an operator in assisted mode, where a wallet's default "sign"
  button may finalize. The design's answer is to let the external wallet apply
  *m*−1 signatures and collect the last partial here.

- **`internal/arm`'s publish test leaves open channels and spends cold coins.**
  It is the only test that publishes, and it has to, because "the transaction
  reaches the network on exactly one line" is not a claim a dry run can make.
  Nothing can close what it opened — `CloseChannel` is on the never-list — so
  `make harness` is the reset.

- **A stream must be read promptly, or the funding manager waits.**
  `funderProcessFundingSigned` sends `chan_pending` on `resCtx.updates`, a channel
  with a buffer of 2 (`server.go`), and blocks on `f.quit` if it is full. One
  buffered slot is spent on `psbt_fund`, so a batch that finalized every channel
  before reading any receipt would be relying on that buffer. `arm.Finalize`
  finalizes one at a time and reads each receipt, so it never gets close — but
  this is why, and not merely tidiness.

- **A macaroon refusal has two shapes and they mean opposite things.**
  `codes.InvalidArgument` from `CheckMacaroonPermissions` is the answer about
  the macaroon in the request; an *untyped* error carrying the same "permission
  denied" text is the interceptor refusing the caller. Match both, as
  `doctor.tooNarrow` does — the code alone confuses them and the text alone does
  too.

- **`ExportAllChannelBackups` is node-wide.** The snapshot covers every channel
  the node has, not the batch's. A test may assert "at least the batch"; on the
  harness it comes back with 33.

- **"No device added a signature" is a harness fault, not a signing bug, and
  `bootstrap` does not fix it.** A container restart leaves Core's non-default
  wallets unloaded, which every wallet call reports as "Requested wallet does not
  exist" and `make -C regtest bootstrap` cures. A *different* failure looks like a
  code bug and is not: `internal/run` and `internal/settle` failing with
  "combining the cold wallet's partials: no device added a signature — every
  device returned the packet it was given" means `cold1`/`cold2` hold keys from a
  different generation than the descriptors in `cold-watch`, so
  `walletprocesspsbt` returns the packet untouched. Only `make -C regtest reset`
  (~1 min) fixes that, and `winthistle doctor` will report the coins as fine
  throughout, because they are. Reach for `reset` on that message rather than
  reading the combine path.

- **`internal/run`'s regtest tests open real channels and abort them.** Two
  channels to two peers for the cold-probe test, one stream for the failure
  test, all cancelled or abandoned — so they add to the pending-channel pressure
  every abort test creates. Same cure: `make -C regtest mine N=2016`.

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
  and the failure is loud rather than silent. That is the intent; it is also
  worth remembering before renaming one.

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
  yet synced" — exactly two hours, at v0.19.3-beta. `OpenChannel` then refuses
  with `channels cannot be created before the wallet is fully synced`, which
  reads like a node problem and is a *clock* problem: nothing has been mined.
  It bit the funding-timeout test on its first run, where the stream opened at
  t+0 and the one at t+6m did not. `env.Mine(t, 1)` at the start buys two hours
  of headroom and confirms nothing but itself. Any test that spans minutes on an
  idle cluster wants it.

## Open questions

Listed at the end of `docs/design.html`. All of them are now closed, four by the
earlier work — `shim_cancel`-after-verify, `chan_pending`-without-broadcast at
*n* = 3, `RequiredReserve` for private channels, and Core's `finalizepsbt`,
answered by not needing Core — and the last two by Phase 2.

**`UpdateChannelPolicy` on a pending channel:** it does not refuse, it does not
accept, and it does not error. It returns success with the refusal inside it, as
`UPDATE_FAILURE_PENDING` / *"not yet confirmed"* in `failed_updates`. Polling was
the right answer for a better reason than the question knew. Observed live —
"Phase 2" above.

**The settlement pass on a real confirmation:** proved on regtest, one block at a
time. The channel opened at 3 confirmations, `ExpectedDepth` predicted 3, and the
policy landed and read back out of the announced graph. That is not the same as
proving it on mainnet, and the design's answer to *that* still stands: make the
first live batch a deliberately small one and treat it as commissioning.

Those three corrections were made in `docs/design.html` itself — `minimum_depth`
cannot be read, the shim probe is free only when it is refused, and the *peer*
is the one that forgets after 2016 blocks. The doc is the spec, so they belong
there rather than only here, and this file used to say they were still
outstanding. They are not.

**Four more changes went into the doc with the composition layer**, and it has
been republished:

- the configuration block lost `allow_rbf` and gained `[fees]` and `[[signer]]`,
  with a paragraph on why a key that changes nothing is worse than no key;
- Phase 0 gained the step where the forwarding policy is chosen, and why it
  belongs in the plan document beside the amounts;
- the setup section gained how the credential is checked — by asking
  `CheckMacaroonPermissions` rather than by calling methods — and the two
  refusals that look alike;
- the commissioning section names `winthistle run --stop-before-publish` and
  says what composing it actually cost: one `if`, between `arm.Finalize` and
  `arm.Publish`.

**One more change went into the doc with `winthistle setup`, and it has been
republished.** The "Two setup traps" box is now four, and one of the original two
was wrong: the doc said a `multi()`-where-you-wanted-`sortedmulti()` wallet "shows
a zero balance", and the harness says it shows a plausible *partial* one — which
is a better disguise and the reason the sample is five addresses rather than one.
That correction was measured in the earlier coldwallet work and had never reached
the spec. The two new traps are the multipath descriptor Core silently halves and
the descriptor Core deactivates but cannot forget. The section also now says where
the operator's answer goes, that both `doctor` and `run` read it back, and that
nothing overrides the refusal.

The ten-minute clock is closed too, and the answer is not one of the two options
the question offered. **We have no clock at all**: `pruneZombieReservations` skips
PSBT reservations outright — *"these reservations are always initiated by us and
the remote peer is likely going to cancel them after some idle time anyway"* — so
the only clock is the peer's. On the peer it is `resCtx.lastUpdated`, set by
`defer resCtx.updateTimestamp()` at the end of `handleFundingOpen`, which is the
same handler that sends `accept_channel`. The two candidates are therefore one
network round trip apart against a ten-minute budget: not distinguishable on
regtest, and not worth distinguishing on mainnet either.

What *is* answerable, and answered: the timeout is
`chanfunding.DefaultReservationTimeout` = 10 minutes, checked by a sweeper on
`lncfg.DefaultZombieSweeperInterval` = 1 minute, and neither is adjustable in a
release build — `lncfg/dev.go` returns the constant and only a `dev`-tagged build
reads the flags. So the countdown should start at our own `OpenChannel` call and
treat 10:00 as an upper bound with up to a minute of slack past it, and none of it
binds a peer that is not LND. `TestWhoOwnsTheTenMinuteClock` measures it against
bob; it needs `WINTHISTLE_SLOW=1` because it takes eleven minutes. Measured
**10m41s** on 2026-08-23, which is the predicted 10:00 plus sweeper granularity.
What the operator sees is *"remote canceled funding, possibly timed out"* —
`chanfunding.ErrRemoteCanceled` wrapped around a peer error whose own text is only
"funding failed due to internal error". The "possibly" is ours; the peer does not
say it timed out.

**The CPFP child gained a hazard the doc did not have**, and it went in with the
`winthistle bump` work: the relay ceiling on a child's own fee rate, in I·4, with
the source chain and the measurement. The hazard table's single-publish row now
says two, counted. The out-of-scope list lost "automated fee-bumping beyond the
manual CPFP offer" — the child is built, verified, signed and broadcast now — and
gained "a second lift on the same batch", which is genuinely not built.

What is left is not an open question but an untested claim: everything past the
broadcast line has been proved against regtest and against nothing else. The
first small live batch is what turns that into evidence.
