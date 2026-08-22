# Winthistle

Open a batch of Lightning channels in one on-chain transaction, funded directly
from cold storage — including multisig cold storage — through a local, guided
web interface.

> ### Status: design only. Do not use.
>
> There is no implementation yet. The central safety property below has been
> verified by reading LND's source but **has not been tested against a running
> node**. Nothing here should be pointed at a node holding funds you care about.

## What it replaces

The manual `lncli openchannel --psbt` process, where a human holds the whole
state machine in their head: which terminal is which peer, which prompt wants
base64 and which wants raw hex, which one has `--no_publish`, how much anchor
reserve LND will demand once the new channels exist, and how many of the ten
minutes are left. Every one of those is mechanical, and every one is a place to
lose a channel.

Background: [Lightning channels from an external wallet via PSBT](https://blog.dpinkerton.com/posts/lightning-channels-from-external-wallet-psbt/)

## The load-bearing property

`no_publish` is set on **every** channel in the batch — not "all but the last",
as LND's docs suggest. Finalize all *n*, wait for all *n* `chan_pending`, then
publish once via `WalletKit.PublishTransaction`.

This works because in `funding/manager.go`, `chan_pending` is emitted strictly
after `CompleteReservation(nil, commitSig)` has stored the peer's commitment
signature. So each `chan_pending` is a receipt that the channel is already
recoverable by force-close. Gating a single publish on *n* of *n* receipts means
the transaction cannot reach the network while any channel is unrecoverable —
we hold the only copy of it until the gate opens.

LND's "DO NOT PUBLISH … OR THE FUNDS CAN BE LOST" warning is about *ordering*,
not authorship. The "all but the last" idiom exists because `lncli` has no way
to broadcast afterwards; an orchestrator is not bound by that.

## Documentation

- [`docs/design.html`](docs/design.html) — the full design: invariants, RPC
  sequence, phase model, hazard register, prerequisites, hosting.
- [`HANDOFF.md`](HANDOFF.md) — current state and build order.

## License

MIT
