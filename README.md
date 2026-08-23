# Winthistle

Open a batch of Lightning channels in one on-chain transaction, funded directly
from cold storage — including multisig cold storage — through a local, guided
web interface.

> ### Status: works on regtest, never run on mainnet. Do not use.
>
> There is a working command line — `winthistle doctor`, `winthistle run`,
> `winthistle bump`, `winthistle recover` — and the web interface described in
> the design does not exist yet. The central safety property below was verified by reading LND's
> source and **has since been observed on regtest at *n* = 3**: three channels
> armed from one transaction, with an empty mempool checked after every
> finalize, including the last. It has never been run against mainnet. Nothing
> here should be pointed at a node holding funds you care about.

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

## Trying it

```sh
winthistle example-config > winthistle.toml   # then edit it
winthistle print-macaroon-command | sh        # bake the narrow credential
winthistle doctor                             # every prerequisite, with fixes
winthistle example-batch > batch.toml         # then edit it: peers, amounts, policy
winthistle run --batch batch.toml --stop-before-publish
```

The last line is the cold probe: the whole production sequence, with the one
call that broadcasts withheld and the batch taken apart afterwards. It is not a
test mode and not a separate code path — see the design's commissioning section.

If a batch goes out and then sits in the mempool, `winthistle bump <run-id>`
builds a CPFP child from its change output, verifies it against the parent,
takes it to the signers and broadcasts it. The funding transaction is never
replaced — replacing it moves every outpoint in it and destroys every channel in
the batch, and no code path in the binary can. The *child* is replaceable, so
running the command again lifts the batch further by replacing the child rather
than chaining another transaction onto it. Either way it spends cold-storage
change, so a bump costs a second cold-wallet session — there is no version of
this that does not.

## Documentation

- [`docs/design.html`](docs/design.html) — the full design: invariants, RPC
  sequence, phase model, hazard register, prerequisites, hosting.
- [`HANDOFF.md`](HANDOFF.md) — current state and build order.
- `make macaroon` — the `lncli bakemacaroon` line for the build in front of you,
  generated from the method registry rather than written down anywhere.

## License

MIT
