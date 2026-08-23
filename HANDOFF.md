# Winthistle — handoff

Read this first. `CLAUDE.md` is loaded automatically and carries the four
invariants and the do-not-reintroduce list; treat those as settled.

**State: the three phases exist as packages, something composes them, and every
function in *them* now has a caller — the setup path is the one part of the build
that is still written and uncalled.** `winthistle run` drives Phase 0 —
peer pre-flight, fee rate, anchor reserve, dress rehearsal — then Phase 1's
armed window and its single publish, then Phase 2's confirmation watch and
policy pass; `winthistle bump` builds, verifies, signs and broadcasts the CPFP
child of a batch that went out too cheap; `winthistle doctor` runs every
pre-flight in order and prints the command that fixes each failure;
`winthistle recover` is the recovery screen and the abort behind it. All of it
is configured by `winthistle.toml` and a batch file, and all of it is exercised
against the live regtest node. What is missing is the server, the UI, signet,
and the mainnet cold probe.

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
| `internal/config` | `winthistle.toml` and the batch file, read by a strict reader that refuses every key it does not know — and refuses `allow_rbf` even when spelled correctly |
| `internal/signers` | how a base64 PSBT reaches a device and a partial comes back: a command, or a file handshake |
| `internal/doctor` | every pre-flight in order, each failure with the command that fixes it. Also the credential check, which asks LND rather than calling things |
| `internal/run` | the composition: Phase 0, the armed window, Phase 2, and the abort that any failure ends in |
| `internal/bump` | `winthistle bump`: find the parent in Core's mempool, build the child, verify it twice, sign it from cold storage, and broadcast it. The second and last publish call site |
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

### The rescan and the prune horizon are untested

Regtest has no history. A wallet imported with the right birthday and one
imported with a wrong one find precisely the same nothing, and the node cannot be
made meaningfully pruned. `RunPreflight` dates Core's `pruneheight` by reading
that block's header and compares it against the birthday, and
`Config.Validate` refuses a birthday it was not given — both are written, neither
is proved. That needs signet, per `CLAUDE.md`. The regtest tests say so in a
named constant rather than by omission.

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

**One package is still written and uncalled, and it is not this list:**
`internal/coldwallet`'s setup path. `Install`, `Confirm` and `RunPreflight` have
test callers only — `winthistle setup` is what would call them, and it does not
exist. See next actions.

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

`run.Do` is `drive()` with the gates actually wired: `rehearsal.Gate` and
`peers.ReadyToArm` before `arm.Open`, `reserve.Finding.StillApplies` once the
streams are open, and the journal at the publish call. `--probe` is opt-in and
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

## Next actions, in order

1. **The server and the UI.** One binary, loopback bind, a startup token, strict
   Origin and Host checks, no CORS. The transports the design asks for — base64,
   file up/down, animated QR — and the countdown. Every screen it has to render
   exists as text and every one of them now has a caller: the peer reports, the
   fee report, the reserve report, the plan document, the rehearsal measurement,
   the settlement report and the recovery screens. `internal/run` is the state
   machine in the order the UI needs it; what is missing is the shell, the
   transports and the fact that a browser cannot block on a signing round the
   way a terminal can.
2. **Signet, for the two things regtest cannot reach.** The descriptor-import
   rescan and the prune-horizon pre-flight both need a chain with history. Both
   are built and both are untested; see the note in
   `internal/coldwallet/coldwallet_regtest_test.go`. `winthistle doctor` reports
   the prune horizon against the birthday and has never had one to report.
3. **The mainnet cold probe.** `winthistle run --stop-before-publish`.
   Everything it needs exists: steps 1 to 8 are the production code path, step 9
   is one call inside one `if` that it does not make, and the abort path it
   terminates through runs on every failure and is tested on both.
4. **`winthistle setup`, or the honest absence of it.** `coldwallet.Install` —
   create the watch-only wallet, checksum and import the descriptors, read them
   back, derive the round-trip address check — still has no caller outside its
   tests. `doctor` diagnoses a wallet that has not been set up and prints the
   `bitcoin-cli importdescriptors` line, which is a worse experience than the
   guided screen `Install` was written for. It needs the descriptors and the
   birthday, which is the one part of setup no program can supply.
5. **Nothing new at this level.** `winthistle bump` and the second lift are both
   built — see "The second lift" below. What is left is items 1 to 4, and item 4
   is the only remaining written-but-uncalled code in the repository.

Done since the last handoff, all from the previous list:

- **`winthistle.toml`** — `internal/config`, and the two keys that were named by
  code and read from nowhere.
- **The per-peer policy table** — `internal/policy`, chosen in the batch file
  and rendered in the plan document beside the amount.
- **`winthistle doctor`** — every pre-flight in order, with the command that
  fixes each, and a credential check that asks LND rather than calling things.
- **`winthistle run` and `winthistle recover`** — the composition, the gates
  wired, and the abort that any failure ends in.
- **`winthistle bump`** — the CPFP child, end to end, and the enforced
  publish-call-site count that came with it.
- **The second lift** — the child is now built BIP-125 replaceable, so a further
  acceleration replaces it rather than chaining onto it. This was on the previous
  next-actions list as the thing deliberately *not* done; it is done.

## Watch out for

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
