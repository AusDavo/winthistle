# Winthistle — handoff

Read this first. `CLAUDE.md` is loaded automatically and carries the four
invariants and the do-not-reintroduce list; treat those as settled.

**State:** design complete, regtest harness written, **no application code yet.**
Private repo at `AusDavo/winthistle`, goes public once the cold probe passes on
mainnet and the abort paths work.

## What exists

| | |
|---|---|
| `docs/design.html` | the spec — invariants, RPC sequence, phases, hazards, hosting |
| `CLAUDE.md` | invariants as rules, with LND source citations |
| `regtest/` | disposable cluster: bitcoind + alice + 3 peers + simulated 2-of-2 cold wallet |

## Before anything else: Docker permissions

The daemon is running (snap, active) but the socket is `root:root` and there is
no `docker` group yet, so `docker ps` fails. Snap's docker needs:

```sh
sudo addgroup --system docker
sudo adduser $USER docker
newgrp docker
sudo snap disable docker && sudo snap enable docker
```

## Next actions, in order

1. **`make -C regtest reset`** and fix what breaks. The harness has never been
   run — compose schema and argv splitting are verified, the shell and Python are
   only syntax-checked. Most likely wrinkles: the container paths `make creds`
   copies from, and the shape of Core's `listdescriptors` output that
   `cold-wallet.py` parses.
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
