# Winthistle — handoff

Read this first. `CLAUDE.md` is loaded automatically and carries the four
invariants and the do-not-reintroduce list; treat those as settled.

**State:** design complete, regtest harness **working and self-tested**, **no application code yet.**
Private repo at `AusDavo/winthistle`, goes public once the cold probe passes on
mainnet and the abort paths work.

## What exists

| | |
|---|---|
| `docs/design.html` | the spec — invariants, RPC sequence, phases, hazards, hosting |
| `CLAUDE.md` | invariants as rules, with LND source citations |
| `regtest/` | working cluster: bitcoind + alice + 3 peers + simulated 2-of-2 cold wallet. `make reset` rebuilds and self-tests in ~1 min |

Docker group membership is already sorted on this machine. On a fresh one, snap's
docker needs `sudo addgroup --system docker; sudo adduser $USER docker; newgrp
docker; sudo snap disable docker && sudo snap enable docker`. Note that group
membership is fixed at process start, so an already-running shell needs `sg
docker -c "..."` or a restart.

## Next actions, in order

1. **`make -C regtest reset`** as a sanity check (~1 min, self-tests at the end).
   It has been run end to end and passes: chain matures, four nodes healthy,
   alice peered with all three, cold-watch funded with 4 UTXOs, and the 2-of-2
   fixture verified.
2. **Scaffold the Go module.** `btcd`/`btcutil` for PSBT and script execution,
   `lnd/lnrpc` for the gRPC clients, `modernc.org/sqlite` or `mattn/go-sqlite3`
   for the journal.
3. **The abort paths, first** — `shim_cancel`,
   `AbandonChannel(pending_funding_shim_only)`, Core UTXO-lock release. Not
   because they are easy but because the cold probe's normal termination *is* the
   abort path, so nothing else can be exercised safely until they work.
4. **The run journal** — `pending_chan_id` ↔ funding outpoint ↔ signer state ↔
   finalized raw tx, written *before* the publish call.

## The assertion everything rests on

That finalizing all *n* channels with `no_publish` set yields *n* `chan_pending`
events and no broadcast. Prove it on regtest early. If it does not behave as the
source reading predicts, stop and re-derive rather than patching around it —
every other safety claim in the design hangs off it.

## Open questions

Listed at the end of `docs/design.html`. The two that the cold probe cannot
answer both sit past the broadcast line; one is defused by having Phase 2 poll
unconditionally rather than depending on the answer.
