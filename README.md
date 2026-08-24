# Winthistle

Open a batch of Lightning channels in one on-chain transaction, funded directly
from cold storage — including multisig cold storage — through a local, guided
web interface.

> ### Status: works on regtest, never run on mainnet. Do not use.
>
> The command line works: `setup`, `doctor`, `run`, `bump`, `recover`, `serve`.
>
> The web interface exists and can open a batch — it starts a run, answers the
> four questions a run asks, and stops one — but it is not finished. Missing from
> it: the file and animated-QR transports (the browser transport is one field out
> and one field back), the countdown, and the setup and bump screens. `recover` is
> still the only way to read what an earlier run left behind.
>
> The central safety property below was verified by reading LND's source and
> **has since been observed on regtest at *n* = 3**: three channels armed from one
> transaction, with an empty mempool checked after every finalize, including the
> last.
>
> It has never been run against mainnet, and the mainnet cold probe that would
> commission it has not been done. Nothing here should be pointed at a node
> holding funds you care about.

## What it replaces

The manual `lncli openchannel --psbt` process, where a human holds the whole
state machine in their head: which terminal is which peer, which prompt wants
base64 and which wants raw hex, which one has `--no_publish`, how much anchor
reserve LND will demand once the new channels exist, and how many of the ten
minutes are left. Every one of those is mechanical, and every one is a place to
lose a channel.

Background: [Lightning channels from an external wallet via PSBT](https://blog.dpinkerton.com/posts/lightning-channels-from-external-wallet-psbt/)

## Why not just sign it in Sparrow

Because a signing wallet cannot tell you what it is signing.

A channel funding output is a P2WSH 2-of-2 between your node and the peer. No
signing wallet can attribute one: Sparrow, Coldcard, Jade and Passport all show
you payments to unrecognised addresses, plus change. Nothing on the device's
screen says which peer an output funds, or that the amounts landed against the
peers you chose, or that two of them were not swapped. Air-gapped verification
that cannot verify is theatre.

Sparrow signs a transaction. Winthistle knows what the transaction *means*, and
so it can check both directions:

- **Outbound**, before the PSBT is handed over: it prints the batch plan — every
  output with the peer it funds, the exact amount, the address on its own line,
  the forwarding policy that channel will carry, the change address and its
  floor, the fee target and the tolerance either side of it, and the coins it
  will spend *and the ones it excluded*, so your wallet's balance and the plan
  disagreeing is explained rather than alarming. You read that against the
  device's output list.
- **Inbound**, before anything is finalized: it refuses a returned PSBT that
  moved the transaction (I-3), that a device finalized itself (I-2 — that device
  held something it could have broadcast), that carries two different signatures
  for one key, that added, removed or swapped an input, whose fee left the
  tolerance, whose change fell below the CPFP floor, that has a legacy input, or
  that became replaceable (I-4). Every refusal names the device.

Then it runs the finalized transaction through `testmempoolaccept`, which
validates without relaying, and only then does the gate below open.

Winthistle is not a wallet and does not want to be one. Signing stays wherever
it is now.

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

## What you need

Written in Go, no runtime dependencies, one binary:

```sh
git clone https://github.com/AusDavo/winthistle && cd winthistle
make build            # or: go build ./cmd/winthistle
make check            # lint, vet, tests. Harness-backed tests skip if regtest is down
```

Go 1.24 or newer. `make harness` rebuilds a regtest cluster if you want to
exercise it; see `regtest/`.

**Where it runs.** On the same machine as LND, or anywhere that can reach LND's
gRPC port and Core's RPC port. It binds its web interface to loopback only
(`127.0.0.1:7420` by default), spelled `127.0.0.1` rather than `localhost` so it
cannot resolve to something else, and prints one URL carrying a token generated
at startup. The first request exchanges that token for a cookie and redirects, so
the token is in a query string exactly once. It opens no listening port other
than that one, and it makes no outbound connection except to your LND and your
Core.

**From LND** it needs the gRPC address, `tls.cert`, and a **baked macaroon** —
not `admin.macaroon`. `winthistle print-macaroon-command` prints the
`lncli bakemacaroon` line for the build in front of you, generated from the
method registry in `internal/methods` rather than written down anywhere, so it
cannot drift from what the code calls. It grants 17 methods. Whatever else
happens, that credential cannot bake itself a wider credential, close a channel,
send a payment, send coins on-chain, sign a message, sign a transaction or spend
the node's own coins — those are refused at the registry, not merely left out of
it. `winthistle doctor` asks LND to confirm all of that, including the
never-list, by way of `CheckMacaroonPermissions`.

**From Bitcoin Core** it needs the RPC address, a cookie file or user/password,
and a wallet name. `winthistle setup` creates that wallet: watch-only,
descriptor-based, built from your cold wallet's two branches. Core v29 is what
this builds and tests against. Core is also the only fee source —
`estimatesmartfee`, never a fee API or a block explorer, because those log
exactly the amounts, peers and timing this tool exists not to leak. Peer facts
come from LND's own local gossip graph for the same reason.

**Signers.** Any wallet that round-trips BIP174 with your descriptor works —
that is the whole contract. Configure one `[[signer]]` block per cold-storage
device, with a command that reads a base64 PSBT on stdin and writes a signed one
back; the web UI can instead hand you the base64 directly. Signers return
**partial** signatures and this app does the combining and finalizing, because no
external party may ever hold a broadcastable copy of the funding transaction
(I-2). Consequences worth knowing before you choose:

- Exactly *m* signers for an *m*-of-*n* descriptor. A 2-of-3 handing over three
  partials does not finalize — btcd's finalizer wants exactly *m*.
- The witness script must be a bare *m*-of-*n* multisig. `wsh(sortedmulti(…))`
  and `wsh(multi(…))` both qualify; a miniscript policy with a timelock in it
  does not.
- **Single-sig cold storage has no partial signature to give.** A single-sig
  wallet that signs at all returns a complete, broadcastable transaction, so
  there is no partial-signature path to hold it up in — and no software check can
  establish that a device is genuinely air-gapped, because that is a fact about a
  room. So this one is deliberately yours: single-sig runs work, nothing refuses
  one, and on single-sig I-2 rests on your setup rather than on a gate. Multisig
  is where the property comes free, which is the reason to prefer it.
- CI uses two key-enabled Core wallets as a simulated multisig cold wallet, which
  exercises exactly the partial-signature path. No hardware needed to try it.

## Trying it

```sh
winthistle example-config > winthistle.toml   # then edit it
winthistle example-descriptors > cold.toml    # the cold wallet's two branches
                                              #   and its birthday
winthistle setup --descriptors cold.toml      # build the watch-only wallet
winthistle print-macaroon-command | sh        # bake the narrow credential
winthistle example-batch > batch.toml         # then edit it: peers, amounts, policy
winthistle doctor --batch batch.toml          # every prerequisite, with fixes
winthistle serve --batch batch.toml           # the web UI, or drive it from the CLI:
winthistle run --batch batch.toml --stop-before-publish
```

`setup` does not end in a success message, and that is deliberate: nothing a
program can check separates a correct cold-storage descriptor from a plausible
wrong one. Core parses both, the import succeeds for both, and a wrong one shows
a *partial* balance rather than an empty one — measured, on the harness's own
2-of-2. So it ends by putting five addresses of each branch on screen and asking
whether they are the ones your own wallet software shows. Run it again with no
arguments to answer later. `winthistle doctor` reads the answer back and so does
`winthistle run`, before it asks LND anything: a wallet whose exact descriptors
somebody compared and rejected does not open a batch, and no flag overrides
that.

`doctor` makes ten checks — the config file, LND, the macaroon, Core, the cold
wallet, the coins, the anchor reserve, the fee rate, the peers, the run journal —
never stops at the first failure, and prints the command that fixes each one
rather than a paragraph about it. Give it `--batch` and the peer and anchor-reserve
checks are about the batch you actually mean to open. It changes nothing on your
node: it will not even connect to a peer unless you pass `--connect`.

The last line of the block is the cold probe: the whole production sequence, with
the one call that broadcasts withheld and the batch taken apart afterwards. It is
not a test mode and not a separate code path — see the design's commissioning
section.

## Budget eleven minutes for the signing round

The ten-minute clock is **the peer's**, and it starts when the peer handles your
`open_channel` — which is before there is a transaction to sign, because LND
issues the funding addresses at that point and the PSBT is built from them. So
the shims are open while your signers are still assembling.

Read out of the vendored LND rather than from any doc, including this one:
`chanfunding.DefaultReservationTimeout` is 10 minutes and
`lncfg.DefaultZombieSweeperInterval` is 1 minute, neither adjustable in a release
build, so a peer holds a reservation for at most 11 minutes. Measured against the
harness: 10m41s. Our own node never expires a reservation; only the peer's does.

Two things follow. The signing round has a budget —
`limits.abort_after_signing_seconds`, 5:00 by default — and Phase 0 **times a
full dress rehearsal** against a mirror of the batch's amounts before any shim is
opened. If that round is slower than the gate, the batch is not armed at all, and
the clock the gate protects has not started yet. That is the point of doing it
first.

If a window does lapse mid-batch, the cost is one more signing round: nothing was
broadcast, `shim_cancel` still works after a successful `psbt_verify`, and
re-arming is free.

## No RBF on the funding transaction — and what actually enforces that

Replacing the funding transaction changes every outpoint in it and destroys every
channel in the batch. So every input is built at nSequence `0xfffffffe`, the
verifier refuses the transaction if any input is below that, and **no code path
in this repository builds a replacement.** The CPFP child is the deliberate
exception: it is built at `0xfffffffd` and *is* replaceable, because nobody has
committed to anything about a child.

**The network does not enforce this, and you should not believe that it does.**
Verified against Core v29 — the version this develops against — full-RBF is
unconditional: `mempoolfullrbf` does not exist even as a hidden debug option, and
`getmempoolinfo` reports `"fullrbf": true` with no way to turn it off. A
higher-fee conflict relays regardless of what our sequence numbers signal.
`replaceable: false` is a statement of intent, not a defence.

What actually holds is narrower and worth stating plainly: **only you can sign
those inputs, and nothing here builds a replacement.** Your *other* wallet, with
the same keys, can. The discipline lives in the human. If you ever do double-spend
a batch's input out of band, `docs/design.html` documents the ordering that keeps
it from losing funds — keep every channel's state until the replacement is deeply
confirmed, and abandon only then.

## Every batch has a change output, on purpose

The verifier refuses a batch with no change output, or with change too small to
fund a child that lifts the package to three times the fee target. Coin selection
will not consume your inputs exactly.

The reason is not custody. While the batch is unconfirmed nothing is at risk: the
coins are yours, unspent, in a transaction only you could have signed. But it
cannot be abandoned either — its inputs stay unspent, so it stays valid
indefinitely, and eviction from mempools does not invalidate it. `recover`
therefore refuses any run that reached the publish call, because abandoning a
pending channel whose funding transaction *later* confirms strands its funds with
no force-close path.

So a batch that never confirms is **frozen**: not abortable, not safely
spendable, waiting on the mempool. CPFP is the exit from that state rather than a
speedup — confirm the batch, then close the channels normally if you no longer
want them. The change output is what keeps the batch from having no way out at
all, and a few tens of thousands of satoshis is a cheap price for that, which is
why it is enforced rather than offered.

One case it cannot cover: a child of a parent that has been evicted everywhere is
an orphan, and `bump` says so (`ErrParentMissing`) rather than pretending.

## If a batch goes out too cheap

`winthistle bump <run-id>` builds a CPFP child from the batch's change output,
verifies it against the parent, takes it to the signers and broadcasts it. The
funding transaction is never replaced. The *child* is replaceable, so running the
command again lifts the batch further by replacing the child rather than chaining
another transaction onto it. Either way it spends cold-storage change, so a bump
costs a second cold-wallet session — there is no version of this that does not.

## If something stops halfway

Every run is journalled to SQLite (`~/.winthistle/runs.db`, via a pure-Go driver
— there is no cgo in this build) *before* the calls it describes, so a crash mid-publish leaves artifacts rather than mystery. A run is
in one of seven states — arming, signing, armed, publishing, published, aborting,
aborted — and `publishing` is written *before* `PublishTransaction` is called,
deliberately, because "we may have broadcast" is the state that needs recording.

`winthistle recover` lists the runs that stopped and takes one apart: cancel the
shims, abandon what reached pending, release Core's coin locks. It is safe to run
as many times as it takes and everything under it is idempotent. It refuses a run
that reached the publish call, and nothing in this tool will abort one — read that
run on its own, which begins with looking for the txid rather than touching
anything.

The abort and recovery paths were built *before* the happy path, because the
commissioning cold probe runs the real production flow and terminates through
them.

## Documentation

- [`docs/design.html`](docs/design.html) — the full design: invariants, RPC
  sequence, phase model, hazard register, prerequisites, hosting.
- [`HANDOFF.md`](HANDOFF.md) — current state and build order.
- `winthistle print-macaroon-command` — the `lncli bakemacaroon` line for the
  build in front of you, generated from the method registry rather than written
  down anywhere. `make macaroon` is the same command, for use from a checkout
  without a built binary; there is only one door.

## License

MIT
