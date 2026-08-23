# Winthistle — handoff

Read this first. `CLAUDE.md` is loaded automatically and carries the four
invariants and the do-not-reintroduce list; treat those as settled.

**State: the happy path works end to end on regtest.** Three channels, one
transaction funded from the simulated 2-of-2 cold wallet, both halves signing
partially, combined and finalized in-app, verified against the plan and against
all three streams, finalized to three `chan_pending` receipts with the mempool
checked after every one of them, backups exported, published exactly once,
confirmed, and all three channels open. Everything before it still holds: the
abort paths, the run journal, the generated macaroon, the reserved-value
pre-flight, the watch-only Core wallet and the batch-plan verifier.

Two of `docs/design.html`'s open questions are now answered rather than open —
Core's `finalizepsbt` and the ten-minute clock. The two that remain both sit past
the broadcast line.

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
| `internal/arm` | the armed window — steps 2 to 9, and the only place in the repo that can broadcast |
| `internal/regtestenv` | test support: drives funding streams, signs with the cold wallet's two halves, and aborts what it opened |

`internal/prose` is a lift-and-shift out of `internal/reserve/report.go`, which
had the only copy of the wrapper and the satoshi formatter. Nothing about the
reserve reports changed; there are just three more screens now that have to line
up in the same pane.

`make check` lints, vets and runs everything. `make test-unit` (`-short`) is the
subset that needs no harness; the harness-backed tests skip with a reason when
regtest is down, so a bare `go test ./...` is honest either way. A fresh clone
needs `make harness` first — `regtest/creds/` is gitignored, so until it exists
every harness-backed test skips.

`make test` passes `-p 1`, and that is load-bearing rather than tidy. Seven
packages now drive the harness — `abort`, `arm`, `coldwallet`, `combine`,
`journal`, `plan` and `reserve` — and `go test ./...` runs package binaries
concurrently by default, which puts several test processes through the same
alice. `internal/reserve` makes this sharper than it was: its tests deliberately
lease *every* coin alice has, so a concurrent package would see a node with no
balance and fail over the reserve. Use `make test`, not a bare `go test ./...`,
when the harness is up.

`internal/arm`'s publish test is the first one that spends real regtest coins and
leaves three *open* channels behind. Nothing can close them — `CloseChannel` is
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
   therefore consumes one of each peer's pending-channel slots permanently, which
   is why `--maxpendingchannels` is now 200 rather than Polar's 10; `make reset`
   is still the actual cure. Consequence past the harness: after an abort, a
   re-arm against the same peer costs another of *its* slots, and a peer running
   the LND default of 1 will refuse. "Re-arming is free" holds for cancelled
   shims, not for channels that already reached `chan_pending`.

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

`internal/arm` is steps 2 to 9 and the only code in this repo that can broadcast.
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
taken back and the narrowest way to say so is a type.

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

## Next actions, in order

1. **Phase 0 and Phase 2, the two ends the sequence does not include.**
   `internal/arm` covers steps 2 to 9; what is around it is still the caller's
   business. Phase 0 needs peer pre-flight (`ConnectPeer`, `GetNodeInfo`, the
   shim probe that reads `accept_channel`), a fee rate from Core's
   `estimatesmartfee`, and the dress rehearsal that measures a signing round —
   the number the five-minute gate is supposed to compare against. Phase 2 needs
   the confirmation watch and `UpdateChannelPolicy`, polled unconditionally so
   the pending-but-inactive question stops mattering.
2. **The server and the UI.** One binary, loopback bind, a startup token, strict
   Origin and Host checks, no CORS. The transports the design asks for — base64,
   file up/down, animated QR — and the countdown. The recovery screen's wording is
   the highest-stakes copy in the product and `CLAUDE.md` says to write it before
   the happy path; the happy path arrived first, so that debt is now due.
3. **`winthistle doctor` and `winthistle.toml`.** The registry, the reserve
   pre-flight, the coldwallet pre-flight and the coin filter are all already the
   checks it has to run; what is missing is the config plumbing and the one place
   that runs them in order.
4. **Signet, for the two things regtest cannot reach.** The descriptor-import
   rescan and the prune-horizon pre-flight both need a chain with history. Both
   are built and both are untested; see the note in
   `internal/coldwallet/coldwallet_regtest_test.go`.
5. **The mainnet cold probe.** Everything it needs now exists: steps 1 to 8 are
   the production code path, step 9 is one call it simply does not make, and the
   abort paths it terminates through are tested.

Done since the last handoff, both from the previous list:

- **Combining and finalizing in-app** — `internal/combine`, described above.
- **Directed mode's step 4** — `coldwallet.Build`, and the fixtures that used to
  have their own copy of it now go through it.

## Watch out for

- **The peers do not forget an aborted batch.** See finding 6: `AbandonChannel`
  touches only our own database. If the harness starts refusing opens with
  *"Number of pending channels exceed maximum"*, that is what it is, and
  `make harness` clears it. `internal/plan`'s regtest tests add to the pressure
  from the other side: they open eight shim streams per run and cancel every one
  without finalizing. A cancelled shim costs *us* nothing, but the peer has
  already sent `accept_channel` and holds its reservation until its own timeout,
  so a tight run of `make test` can still crowd a peer for ten minutes.
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

## Open questions

Listed at the end of `docs/design.html`. Four are now closed:
`shim_cancel`-after-verify, `chan_pending`-without-broadcast at *n* = 3,
`RequiredReserve` for private channels, and Core's `finalizepsbt` — answered by
not needing Core.

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

The two that remain both sit past the broadcast line, and one is defused by having
Phase 2 poll unconditionally.
