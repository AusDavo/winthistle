# Winthistle

Open a batch of Lightning channels in one on-chain transaction, with Sparrow and
your own LND. A command-line wizard that does the two things those two cannot do
between them: **tell you what the transaction means**, and **hold the funding
transaction back until every channel is already recoverable**.

It builds nothing, holds no keys, selects no coins, and derives no addresses. You
already have a wallet; keep using it.

> ### Status: works on regtest, never run on mainnet. Do not use.
>
> The direction changed on 2026-08-25 and the code has not caught up yet. What
> this README describes is what the tool is *becoming* — see
> [`docs/replan-2026-08.md`](docs/replan-2026-08.md), which is the current plan
> and says plainly which parts are not built. The commands that exist today are
> `setup`, `doctor`, `run`, `bump`, `recover` and `serve`; the first, third and
> last of those are being cut or reshaped.
>
> The central safety property below is verified in LND's source and observed on
> regtest, most recently on 2026-08-25 with *n* = 2 channels reaching
> `chan_pending` over a transaction **nothing had signed**.
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

## Why not just build it in Sparrow

Because a signing wallet cannot tell you what it is signing.

A channel funding output is a P2WSH 2-of-2 between your node and the peer. No
signing wallet can attribute one: Sparrow, Coldcard, Jade and Passport all show
you payments to unrecognised addresses, plus change. Nothing on the device's
screen says which peer an output funds, or that the amounts landed against the
peers you chose, or that two of them were not swapped. Air-gapped verification
that cannot verify is theatre.

Sparrow moves coins. Winthistle knows what the transaction *means*, so it can
check both directions:

- **Outbound**, before you build anything: it prints the batch plan — every
  funding address with the peer it funds, the exact amount, and the forwarding
  policy that channel will carry. You paste those addresses into Sparrow as
  recipients. You never type one.
- **Inbound**, before LND is asked to commit: it refuses a transaction that does
  not match the plan you approved — an output the plan does not name, a missing
  or duplicated output, a wrong amount, a legacy input, an input spent twice, an
  input whose UTXO does not belong to it. A mis-paste is caught here, before
  anything is pinned, so it costs a redo rather than a channel.

Winthistle is not a wallet and does not want to be one. Signing stays where it is.

## The sequence

```
1  pre-flight            peers reachable? anchor reserve sufficient?
2  open n streams        psbt_shim + no_publish              ← clock A starts
3  print the plan        peer · alias · address · amount · policy
4  build in Sparrow      enter the recipients, pick coins and fee, Save PSBT
5  verify + psbt_verify  outputs match the plan; skip_finalize; txid pinned
6  n × chan_pending      ← GATE OPEN. clock A stops, clock B starts
7  sign in Sparrow       no ten-minute pressure
8  txid unchanged? → publish once, via LND
9  watch to confirmation, apply policies
```

Step 4 looks like this:

```
Step 4 — build the transaction in your wallet

  channel 1  bitrefill         250,000 sat
      bcrt1q0y0m2xwq6xh3d3h6d6dqzmgyzc0gdz7zxr6c6c4h3fjqz9f4amqk8s2ea

  channel 2  acinq             250,000 sat
      bcrt1qhtl7hc8ckygznefeyjd37je92vspksmqg9dg7rrnsa0ls003c17bqhea9y

Enter these as recipients, choose your coins and the fee,
and save the PSBT. Do not sign it yet.

  - Save it here, unsigned. Binary or base64, either is read:
      /home/you/batch.psbt
```

Addresses get a line of their own. A P2WSH funding address is 62 characters, and
a wrapped one is a string you would have to reassemble by hand at the step where
a wrong character costs a channel.

**"Do not sign it yet" is enforced, not advised.** Step 4 is the one moment left
where the gate can be defeated from outside: you would be holding a broadcastable
funding transaction with nothing yet recoverable, and your wallet's Broadcast
button is two clicks from its Sign button. A packet with any signature on it is
refused here, and the cost is building it again inside clock A.

The two paths are `--psbt FILE`, which is where you save that transaction, and
`FILE-signed.psbt` beside it, which is where the signed one goes at step 7. The
second name is derived rather than asked for, so the two cannot be the same file.

## The load-bearing property

`no_publish` and `skip_finalize` are set on **every** channel in the batch — not
"all but the last", as LND's docs suggest. Verify all *n*, wait for all *n*
`chan_pending`, then publish once via `WalletKit.PublishTransaction`.

This works because in `funding/manager.go`, `funderProcessFundingSigned` emits
`chan_pending` strictly after `CompleteReservation(nil, commitSig)` has stored
the peer's commitment signature. So each `chan_pending` is a receipt that the
channel is already recoverable by force-close, and gating a single publish on
*n* of *n* receipts means the transaction cannot reach the network while any
channel is unrecoverable.

LND's "DO NOT PUBLISH … OR THE FUNDS CAN BE LOST" warning is about *ordering*,
not authorship. The "all but the last" idiom exists because `lncli` has no way to
broadcast afterwards; an orchestrator is not bound by that.

**And `skip_finalize` is what makes the rest of it comfortable.** With
`no_publish` set, LND never needs a signature: `psbt_verify` alone drives each
channel to `chan_pending`, over an unsigned transaction. So every channel in the
batch becomes recoverable *before any wallet has been asked to sign*. Proved on
regtest on 2026-08-25: two channels reached `chan_pending` at the outpoints of an
unsigned transaction, with an empty mempool, in under a second.

That inverts the thing that made this frightening. The signing round is no longer
inside anybody's ten-minute window.

## The two clocks

They have different owners and different consequences, and conflating them is the
mistake worth not making.

**Clock A — ten minutes, the peer's, covering steps 2 to 6.** Our node never
expires a PSBT reservation; the peer's does.
`chanfunding.DefaultReservationTimeout` is 10 minutes and
`lncfg.DefaultZombieSweeperInterval` is 1 minute, neither adjustable in a release
build, so a peer holds one for at most 11 minutes. Measured against the harness:
10m41s. Ten minutes is LND's default — a CLN or Eclair peer has its own, and any
peer can reconfigure, so design against it as a convention rather than a
guarantee.

Blowing clock A costs a restart and nothing else. Nothing was broadcast,
`shim_cancel` still works after a successful `psbt_verify`, and re-arming is
free. With no signing inside it, steps 2 to 6 take about a second.

**Clock B — about 2016 blocks, roughly two weeks, covering step 6 to
confirmation.** After it, the **peer** marks the channel cancelled and forgets
it; we never do — `waitForTimeout` arms only for the responder.

**Blowing clock B is worse.** The peer has deleted their state, and a funding
transaction that confirms afterwards leaves your coins in a 2-of-2 with a
counterparty who no longer knows about the channel. So the recovery screen names
clock B in **blocks** — "peers give up at block 887,412, about 13 days" — rather
than telling you there is time.

## What you need

Written in Go, no runtime dependencies, one binary:

```sh
git clone https://github.com/AusDavo/winthistle && cd winthistle
make build            # or: go build ./cmd/winthistle
make check            # lint, vet, tests. Harness-backed tests skip if regtest is down
```

Go 1.24 or newer. `make harness` builds a regtest cluster if you want to exercise
it; see `regtest/`. There is no CI — run the tests yourself.

**Where it runs.** On the same machine as LND, or anywhere that can reach LND's
gRPC port. It opens no socket of its own and makes no outbound connection except
to your LND.

**From LND** it needs the gRPC address, `tls.cert`, and a **baked macaroon** —
not `admin.macaroon`. `winthistle print-macaroon-command` prints the
`lncli bakemacaroon` line for the build in front of you, generated from the
method registry in `internal/methods` rather than written down anywhere, so it
cannot drift from what the code calls. Whatever else happens, that credential
cannot bake itself a wider credential, close a channel, send a payment, send
coins on-chain, sign a message, sign a transaction or spend the node's own coins
— those are refused at the registry, not merely left out of it.
`winthistle doctor` asks LND to confirm all of that, including the never-list, by
way of `CheckMacaroonPermissions`.

**A wallet.** Sparrow, or anything that builds a transaction to addresses you
paste in and exports a PSBT. Hot, cold, single-sig, multisig — which of those it
is does not matter to this program, and it should stop pretending otherwise.
Earlier versions of this README argued at length about partial signatures and
air-gapped devices; that argument belonged to an invariant that no longer exists
(see below).

## No RBF on the funding transaction — and what actually enforces that

Replacing the funding transaction changes every outpoint in it and destroys every
channel in the batch. **No code path in this repository builds a replacement.**
That is the whole enforcement, and it is worth being precise about, because the
network does not help.

Verified against Core v29: full-RBF is unconditional. `mempoolfullrbf` does not
exist even as a hidden debug option, and `getmempoolinfo` reports
`"fullrbf": true` with no way to turn it off. A higher-fee conflict relays
regardless of what our sequence numbers signal, so `replaceable: false` is a
statement of intent, not a defence.

What holds is narrower and worth stating plainly: **only you can sign those
inputs, and nothing here builds a replacement.** Your *other* wallet, with the
same keys, can. The discipline lives in the human. If you ever do double-spend a
batch's input out of band, `docs/design.html` documents the ordering that keeps
it from losing funds — keep every channel's state until the replacement is deeply
confirmed, and abandon only then.

## What happened to the invariant about partial signatures

An earlier design required that signers return **partial** signatures only, so
that no external wallet ever held a transaction it could broadcast. It existed
for one reason: to stop anything broadcasting *before the gate opened* and
defeating it from outside.

There is no longer a "before the gate opens". Every channel reaches
`chan_pending` at step 6, and you do not sign until step 7. A wallet holding a
fully signed transaction front-runs nothing — every channel is already
recoverable. The invariant is not relaxed; it has nothing left to protect
against.

This is why the single-sig question stopped mattering. It used to be the one
place where a design property rested on your setup rather than on structure. It
now rests on nothing, because nothing needs it.

## Every batch should have a change output

Coin selection will rarely consume your inputs exactly, so you will usually have
one anyway. It matters more than it looks, and the reason is not custody.

While the batch is unconfirmed nothing is at risk: the coins are yours, unspent,
in a transaction only you could have signed. But it cannot be abandoned either —
its inputs stay unspent, so it stays valid indefinitely, and eviction from
mempools does not invalidate it. `recover` therefore refuses any run that reached
the publish call, because abandoning a pending channel whose funding transaction
*later* confirms strands its funds with no force-close path.

So a batch that never confirms is **frozen**: not abortable, not safely
spendable, waiting on the mempool. A CPFP child spending the change is the exit
from that state rather than a speedup — confirm the batch, then close the
channels normally if you no longer want them. Without a change output, the only
way out is an out-of-band double-spend of one of the inputs, done with your own
tools.

One case nothing covers: a child of a parent that has been evicted everywhere is
an orphan.

**How it knows which output is your change.** It does not build the transaction,
so it cannot be told: it reads the master key fingerprints off the transaction's
own inputs and accepts an output carrying those same fingerprints on the change
branch. That is the evidence a hardware wallet uses to decide an output is its own
change rather than a payment, and the report marks such an output "recognised by
key origin rather than by address" because the claim is weaker than a script.
`--change ADDRESS` names the script instead, which is stronger, and is what a
wallet that writes no key origins needs.

*Today the verifier refuses a batch with no change output, or with change too
small. That is being changed to a report — your fee and change arrangements are
yours, and the app no longer builds the transaction, so it cannot size a change
output for you. It can only tell you yours is too small.*

## If something stops halfway

Every run is journalled to SQLite (`~/.winthistle/runs.db`, via a pure-Go driver
— there is no cgo in this build) *before* the calls it describes, so a crash
mid-publish leaves artifacts rather than mystery. `publishing` is written *before*
`PublishTransaction` is called, deliberately, because "we may have broadcast" is
the state that needs recording.

`winthistle recover` lists the runs that stopped and takes one apart: cancel the
shims, abandon what reached pending. It is safe to run as many times as it takes
and everything under it is idempotent. It refuses a run that reached the publish
call, and nothing in this tool will abort one — read that run on its own, which
begins with looking for the txid rather than touching anything.

**Abort is cheap right through step 7.** Changed your mind, blew clock A, Sparrow
crashed: abandon *n* pending channels. Nothing published, nothing on chain,
nothing lost but the ceremony. LND's safe abandon flag refuses a channel opened
this way — it infers "shim funded" from a thaw height that a plain PSBT open
never sets — so each one costs an explicit confirmation. That is the normal path
here, not a warning sign.

The abort and recovery paths were built *before* the happy path, because the
commissioning cold probe runs the real production flow and terminates through
them.

## Documentation

- [`docs/replan-2026-08.md`](docs/replan-2026-08.md) — **the current direction**,
  and the honest account of what is built and what is not. Read this first.
- [`docs/design.html`](docs/design.html) — the full design: invariants, the
  sequence, the two clocks, the hazard register, recovery.
- `CLAUDE.md` — the invariants and the do-not-reintroduce list, with source
  citations.
- `winthistle print-macaroon-command` — the `lncli bakemacaroon` line for the
  build in front of you, generated from the method registry rather than written
  down anywhere. `make macaroon` is the same command, for use from a checkout
  without a built binary; there is only one door.

## License

MIT
