# Winthistle — handoff

Read this first. `CLAUDE.md` is loaded automatically and carries the four
invariants and the do-not-reintroduce list; treat those as settled.

**State:** design complete. Regtest harness working and genuinely self-tested.
**The abort paths are built and tested against it, the n-of-n gate is proved at
n=3, the run journal exists, the macaroon is generated from a method registry,
and the reserved-value pre-flight predicts step 5's verdict against a live
node.** No funding flow yet — but the sequence it will drive has now been walked
end to end by tests.

Private repo at `AusDavo/winthistle`, goes public once the cold probe passes on
mainnet and the abort paths work.

## What exists

| | |
|---|---|
| `docs/design.html` | the spec — invariants, RPC sequence, phases, hazards, hosting |
| `CLAUDE.md` | invariants as rules, with LND source citations |
| `regtest/` | working cluster: bitcoind + alice + 3 peers + simulated 2-of-2 cold wallet. `make reset` rebuilds and self-tests in ~1 min |
| `internal/lnd` | gRPC client, `PendingChanID`, `ChannelPoint` |
| `internal/bitcoind` | Core JSON-RPC client, coin-lock lock/release |
| `internal/abort` | `CancelShim`, `AbandonPending`, `Run` — the three abort paths |
| `internal/journal` | the run journal: SQLite, pure Go, `Recover` turns a crashed row into an abort |
| `internal/methods` | the registry of LND RPCs this build calls; `make macaroon` prints the bake command from it, and a gRPC interceptor refuses anything it does not list |
| `internal/reserve` | the anchor-reserve pre-flight: predicts `psbt_verify`'s verdict before a signature is asked for, and says what to do about it |
| `internal/regtestenv` | test support: drives funding streams far enough to abort them |

`make check` lints, vets and runs everything. `make test-unit` (`-short`) is the
subset that needs no harness; the harness-backed tests skip with a reason when
regtest is down, so a bare `go test ./...` is honest either way. A fresh clone
needs `make harness` first — `regtest/creds/` is gitignored, so until it exists
every harness-backed test skips.

`make test` passes `-p 1`, and that is load-bearing rather than tidy. Three
packages now drive the harness — `abort`, `journal` and `reserve` — and
`go test ./...` runs package binaries concurrently by default, which puts several
test processes through the same alice. `internal/reserve` makes this sharper than
it was: its tests deliberately lease *every* coin alice has, so a concurrent
package would see a node with no balance and fail over the reserve. Use
`make test`, not a bare `go test ./...`, when the harness is up.

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
of them from documentation.

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
three. `TestBatchArmsEveryChannelBeforeAnythingIsPublished` asserts three
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

## Next actions, in order

1. **Core descriptor plumbing** (directed mode) and the batch-plan verifier
   (assisted mode).
2. **The happy path**, on top. `internal/journal` already has the write points it
   needs, and `internal/reserve` is the Phase 0 gate in front of it; combining
   partial signatures in-app is the part with no code yet (btcd's psbt package
   has no `Combine`).

Done since the last handoff, both from the previous list:

- **`print-macaroon-command` from a method registry.** `internal/methods` is the
  single source. `make macaroon` prints the `lncli bakemacaroon` line on stdout
  and the reasoning on stderr, so `make macaroon | sh` bakes it. Three things
  keep it from drifting, and only the last one catches a new call site before it
  ships:
  - the printed command is rendered from the registry, so the credential is a
    function of the code;
  - `lnd.Dial` installs gRPC unary and stream interceptors that refuse any call
    to a method the registry does not list;
  - `TestEveryLNDCallSiteIsRegistered` type-checks the whole module, finds every
    call on an lnrpc/walletrpc client interface, and requires the registry to
    list *exactly* those — a missing entry fails, and so does a spare one, since
    a spare entry means the operator's macaroon is wider than the code needs. It
    needs no harness, so it runs in `make check` and in `make test-unit`.

  The detector follows types rather than text, which matters because this repo's
  idiom is to take a narrow interface rather than the whole client:
  `internal/reserve` accepts a three-method interface so a pre-flight cannot move
  a coin, and the check resolves that by asking which generated LND client
  satisfies it. It discovers the client interfaces from the dependency graph, so a
  first call into `routerrpc` or `signrpc` is caught too.

  A registry entry is tagged `InApp` or `InHarness`, and the tag is checked rather
  than believed: `InApp` must have a call site outside the tests, `InHarness` must
  have none. Note what that means today — `OpenChannel` is `InHarness`, because
  this build has no funding flow and the credential it prints must not be able to
  open a channel. It moves to `InApp` with the happy path.

- **The reserved-value pre-flight.** `internal/reserve`, described in the
  operations finding above.

## Watch out for

- **The peers do not forget an aborted batch.** See finding 6: `AbandonChannel`
  touches only our own database. If the harness starts refusing opens with
  *"Number of pending channels exceed maximum"*, that is what it is, and
  `make harness` clears it.
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
- **`regtest/sys` is 2.9 MB of stray PostScript**, untracked, almost certainly a
  mistyped shell redirect. Not gitignored, so it will show up in every
  `git status` until it is deleted. Nothing reads it.
- **`internal/regtestenv.SignWithMiner` does not model I-2** and says so in its
  doc comment. It signs with one key so the abort fixtures have a complete
  transaction. Do not reach for it when the real signing path arrives — the
  2-of-2 path is what `make -C regtest verify` covers, and btcd's psbt package
  has no `Combine`, so combining partials in-app is code that still needs writing.

## Open questions

Listed at the end of `docs/design.html`, less the `shim_cancel`-after-verify one,
which is now answered above. The two the cold probe cannot answer both sit past
the broadcast line; one is defused by having Phase 2 poll unconditionally.
