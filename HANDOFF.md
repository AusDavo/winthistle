# Winthistle — handoff

**State:** design complete, nothing implemented. `docs/design.html` is the spec
(also published at https://claude.ai/code/artifact/26e5a033-564b-49eb-bf78-ad52dbe2ab52).
Not yet a git repo.

## What this is

A local, guided web UI for batch-opening Lightning channels in one transaction,
funded from single-sig or multisig cold storage. Replaces the manual tmux/lncli
process from https://blog.dpinkerton.com/posts/lightning-channels-from-external-wallet-psbt/

## The load-bearing decision

`no_publish` is set on **every** channel, not "all but the last". Finalize all *n*,
wait for all *n* `chan_pending`, then publish via `WalletKit.PublishTransaction`.

Why it's safe (verified in LND source, `funding/manager.go` `handleFundingSigned`):
`chan_pending` is emitted strictly after `CompleteReservation(nil, commitSig)` stores
the peer's commitment signature. So `chan_pending` is a receipt that the channel is
already recoverable by force-close. Gating the single publish on *n* of *n* receipts
makes "published while a channel was unrecoverable" unreachable — we hold the only
copy of the signed transaction until then.

`no_publish` → `NoFundingTxBit` (see `lnwallet/reservation.go`), which skips only the
broadcast block. It also gates `rebroadcastFundingTx`, so we own rebroadcast — hence
publishing through WalletKit rather than bitcoind.

## Build order (deliberately inverted)

1. Abort/recovery paths first: `shim_cancel`, `AbandonChannel` with
   `pending_funding_shim_only`, Core UTXO-lock release. The cold probe runs the real
   production flow and *terminates via the abort path*, so these can't come later.
2. SQLite run journal (pending_chan_id ↔ funding outpoint ↔ signer state ↔ raw tx).
3. Core descriptor plumbing / assisted-mode batch-plan verifier.
4. The happy path on top.

## First thing to prove

Run the cold probe on mainnet (zero on-chain cost): open streams, verify, sign,
finalize all *n*, confirm every `chan_pending` arrives, export backups, then abandon
and cancel. The step-8 gate closing is the single assertion the whole safety argument
rests on. No signet — see the Commissioning section of the doc for why.

## Open questions

Listed at the end of the doc. The two that can't be answered by the probe both sit
past the broadcast line; one is defused by having Phase 2 poll unconditionally.
