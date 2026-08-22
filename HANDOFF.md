# Winthistle — handoff

Read this first. `CLAUDE.md` is loaded automatically and carries the four
invariants and the do-not-reintroduce list; treat those as settled.

**State:** design complete. Regtest harness working and genuinely self-tested.
**The abort paths are built and tested against it.** No funding flow yet.

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
| `internal/regtestenv` | test support: drives a funding stream far enough to abort it |

`make check` vets and runs everything. `make test-unit` (`-short`) is the subset
that needs no harness; the harness-backed tests skip with a reason when regtest
is down, so a bare `go test ./...` is honest either way.

## Docker on this machine

The login session predates the docker group, so `docker` in a fresh shell gets
`permission denied ... /var/run/docker.sock`. Group membership is fixed at
process start, so wrap it: `sg docker -c "make -C regtest reset"`. A re-login
fixes it permanently. On a new machine: `sudo addgroup --system docker; sudo
adduser $USER docker; sudo snap disable docker && sudo snap enable docker`.

## What the harness runs proved

Four things, all verified against a running node rather than inferred. Three
were bugs; the fourth answers an open question.

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

## The finding that changes operations

**`pending_funding_shim_only` declines every channel this app opens.**
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

## I-1, observed

`armOneChannel` in `internal/abort/abandon_regtest_test.go` drives one channel to
`chan_pending` with `no_publish` set and asserts the funding transaction is *not*
in the mempool. The source reading in `CLAUDE.md` also re-checked clean against
`v0.19.3-beta`: `handleFundingSigned` calls `CompleteReservation(nil, commitSig)`,
then the broadcast block gated on `completeChan.ChanType.HasFundingTx()`, then
`WatchNewChannel`, then the `chan_pending` emission.

Still to prove: the same with *n* channels at once, which is the assertion the
whole design rests on. The fixture builds one PSBT per batch already, so this is
a loop, not new machinery.

## Next actions, in order

1. **The run journal** — `pending_chan_id` ↔ funding outpoint ↔ signer state ↔
   finalized raw tx, written *before* the publish call. `abort.Target` is
   deliberately shaped like a journal row; that is the seam.
2. **Prove the n-of-n gate** — extend `armOneChannel` to a batch and assert *n*
   `chan_pending` and an empty mempool.
3. **`print-macaroon-command`** from a method registry, per `CLAUDE.md`. The
   abort paths call `FundingStateStep`, `AbandonChannel` and `PendingChannels`;
   the registry should be the single source for that list before it can drift.
4. **Core descriptor plumbing** and the batch-plan verifier.
5. **The happy path**, on top.

## Watch out for

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
- **`internal/regtestenv.SignWithMiner` does not model I-2** and says so in its
  doc comment. It signs with one key so the abort fixtures have a complete
  transaction. Do not reach for it when the real signing path arrives — the
  2-of-2 path is what `make -C regtest verify` covers, and btcd's psbt package
  has no `Combine`, so combining partials in-app is code that still needs writing.

## Open questions

Listed at the end of `docs/design.html`, less the `shim_cancel`-after-verify one,
which is now answered above. The two the cold probe cannot answer both sit past
the broadcast line; one is defused by having Phase 2 poll unconditionally.
