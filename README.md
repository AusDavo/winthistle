# Winthistle

Open a batch of Lightning channels in one on-chain transaction, with Sparrow and
your own LND. A command-line wizard that does the two things those two cannot do
between them: **tell you what the transaction means**, and **hold the funding
transaction back until every channel is already recoverable**.

It builds nothing, holds no keys, selects no coins, and derives no addresses. You
already have a wallet; keep using it.

## Status

One batch has been opened on mainnet with Winthistle: five channels, published
once, confirmed.

| What was proved | Where | When | *n* | LND (client / node) |
|---|---|---|---|---|
| `chan_pending` over a transaction nothing had signed, mempool empty | regtest | 2026-08-25 | 2 | v0.19.3-beta / v0.19.3-beta |
| The whole sequence against real peers, stopping before step 8 — the cold probe | mainnet | 2026-08-26 | 2 | v0.19.3-beta / **v0.21.2-beta** |
| The whole sequence including step 8: 9,000,000 sat, txid `a1b2c3d4…e8f90` | mainnet | 2026-08-26 | 5 | v0.21.2-beta / v0.21.2-beta |

**The mismatched row is not a typo.** The cold probe was run by a client pinned
two minor releases behind the node it was arming — the pin had been
`v0.19.3-beta` since the scaffold commit, with no rationale recorded anywhere,
and catching it up was the next thing that happened that morning. Nothing in the
safety model turned out to be wrong at v0.21.2-beta; about thirty line numbers
were. The node's version is not journalled, so those two cells are established
from the bump commit that names it and from asking the node today, rather than
from a record written at the time.

**Read the limits as carefully as the results.** That is one node, one operator,
three runs. Nothing here has been run by anybody else, on anybody else's node,
against anybody else's peers. There is no CI. The safety property below is read
out of LND's source and observed on a running node, and both of those are things
that were true on a date rather than things anybody guarantees.

The direction changed on 2026-08-25 and the code has caught up. What this README
describes is what the tool now is — see
[`docs/replan-2026-08.md`](docs/replan-2026-08.md), which is the plan that got it
there. The commands are `run`, `doctor` and `recover`, plus
`print-macaroon-command` and two `example-*` printers.

## The sequence

```
1  pre-flight            peers reachable? anchor reserve sufficient?
2  open n streams        psbt_shim + no_publish              ← clock A starts
3  print the plan        peer · alias · address · amount · policy
4  build in Sparrow      load the recipients CSV, pick coins and fee, Save PSBT
5  verify + psbt_verify  outputs match the plan; skip_finalize; txid pinned
6  n × chan_pending      ← GATE OPEN. clock A stops, clock B starts
7  sign in Sparrow       no ten-minute pressure
8  txid unchanged? → publish once, via LND
9  watch to confirmation, apply policies
```

Steps 2 to 6 are the only part under the peers' ten-minute clock, and they
contain no signing. That is the whole point of the inversion, and everything
below is an argument for it.

## Quickstart

```sh
git clone https://github.com/AusDavo/winthistle && cd winthistle
make build                                     # one binary, no runtime deps
./winthistle example-config > winthistle.toml  # then fill in your LND details
make macaroon                                  # the bakemacaroon line for this build
./winthistle doctor                            # ask LND to confirm all of it
```

Then `./winthistle example-batch > batch.toml`, name your peers and amounts in
it, and `./winthistle run --batch batch.toml --psbt ~/batch.psbt`.

`make harness` builds a regtest cluster to exercise it against (see `regtest/`);
`make check` runs lint, vet and the tests, and the harness-backed ones skip if
regtest is down. Go 1.24 or newer. There is no CI — run the tests yourself.

## The batch file

Which peers, how much, and what each channel will route at. `winthistle
example-batch` prints one to start from:

```toml
# Every channel starts from this policy and may override any key of it.
[policy]
base_fee_msat   = 1000
fee_rate_ppm    = 1
time_lock_delta = 80

[[channel]]
peer       = "02aaaa...33 bytes of compressed pubkey, hex..."
host       = "203.0.113.10:9735"    # only used if we are not connected already
amount_sat = 5_000_000
fee_rate_ppm = 250                  # this channel only

[[channel]]
peer       = "03bbbb..."
amount_sat = 2_000_000
private    = true
```

That is where peer selection lives, and Winthistle has no opinion about it: it
opens what you name, to the pubkeys you name. Aliases are looked up from LND's
own gossip graph so the plan is readable, and that lookup is the only thing it
ever asks about a peer that you did not tell it.

Two peers named the same is refused before LND is dialled — it is the shape a
copy-paste error takes, and the answer is two batches or one fewer channel. A
default-configured peer would refuse the second one anyway; see below.

## How many channels

**At *n* = 1 everything still applies.** The gate is a gate over one receipt, and
attribution still matters, because one P2WSH output is exactly as unreadable on a
signing device as five.

**The bound is the peers, not the clock.** A peer's `--maxpendingchannels`
counts its live reservations for you plus its pending channels with you that have
no thaw height, and refuses with `ErrMaxPendingChannels` once that count is at
the limit. **LND's default for that flag is 1.** So against a default-configured
peer, one channel already pending with them is the whole of their budget for you
— and the batch's step 2 is then refused on the clock, with your wallet already
out, for a reason that was sitting in local state the whole time. Winthistle
warns when it can see a competing pending open and never refuses over it, because
the peer's setting is published nowhere and a peer that allows several will take
the batch quite happily. This is per peer, so what it bounds is channels to the
*same* peer, not the size of the batch.

**A peer that never answers costs the whole batch, not just itself.** The gate is
*n* of *n*: one missing receipt means no publish, however healthy the others are.
Their channels are genuinely armed — a peer disappearing does not take the
others' commitment signatures with it — but the batch cannot proceed, and taking
it apart means abandoning the ones that did arm.

**And that wait has no deadline.** Nothing bounds the collection of receipts, so
a silent peer parks the run until you interrupt it. Ctrl-C is the exit, and
`winthistle recover` is what takes the run apart afterwards. Note what a deadline
here would have to do to be useful — abandon channels that might have been one
second away — which is why not having one is easier to defend than picking a
number would be.

Five is the largest batch this has been run at on mainnet, and three the largest
on regtest, which is how many peers the harness has.

## What it replaces

The manual `lncli openchannel --psbt` process, where a human holds the whole
state machine in their head: which terminal is which peer, which prompt wants
base64 and which wants raw hex, which one has `--no_publish`, how much anchor
reserve LND will demand once the new channels exist, and how many of the ten
minutes are left. Every one of those is mechanical, and every one is a place to
lose a channel.

Background: [Lightning channels from an external wallet via PSBT](https://blog.dpinkerton.com/posts/lightning-channels-from-external-wallet-psbt/)

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

**This rests on behaviour, not on an interface.** That `chan_pending` follows
`CompleteReservation` is read out of `funding/manager.go`; LND documents no such
ordering and its own `docs/psbt.md` recommends the opposite idiom. The citations
here were read against v0.19.3-beta and re-read line by line against
v0.21.2-beta, where the ordering was unchanged and about thirty line numbers were
not. Nothing promises the next release keeps it.

**What that would look like if it changed, and what would catch it.** Two
failures, and they are not alike. If `skip_finalize` or `no_publish` stopped
behaving — LND demanding a signature, or broadcasting anyway — it is loud and
immediate: the receipts never arrive, or the transaction is in a mempool when it
should not be, and `TestSkipFinalizeReachesChanPendingWithNothingSigned` in
`internal/arm` fails against a harness running the new version. If instead the
*ordering* moved, so that `chan_pending` were emitted before the peer's
commitment signature is stored, **nothing here would notice.** The receipt still
arrives and looks identical; what changed is what it means. No test in this
repository force-closes a pending channel to prove the receipt was worth
something, so recoverability is inferred from the source and never exercised end
to end. `winthistle doctor` prints the node's LND version and grades it against
nothing. **So the check on a new LND is a human re-reading that function**, and
`CLAUDE.md` says so at the place somebody would otherwise assume.

## The two clocks

They have different owners and different consequences, and conflating them is the
mistake worth not making.

**Clock A — ten minutes, the peer's, covering steps 2 to 6.** Our node never
expires a PSBT reservation; the peer's does.
`chanfunding.DefaultReservationTimeout` is 10 minutes and
`lncfg.DefaultZombieSweeperInterval` is 1 minute — read at v0.21.2-beta, neither
adjustable in a release build — so a peer holds one for at most 11 minutes.
Measured against the harness twice: 10m41s on 2026-08-23 at lnd v0.19.3-beta, and
10m14s on 2026-08-26 at v0.21.2-beta. The spread is the sweeper's one-minute
granularity, not a moved timeout. Ten minutes is LND's default — a CLN or Eclair
peer has its own, and any peer can reconfigure, so design against it as a
convention rather than a guarantee.

Blowing clock A costs a restart and nothing else. Nothing was broadcast,
`shim_cancel` still works after a successful `psbt_verify`, and re-arming is
free. Winthistle's own work across steps 2 to 6 takes about a second; the ten
minutes are spent at step 4, by you, in Sparrow. That is the whole budget and it
is the only thing in it.

Step 4 got smaller. Winthistle writes the recipients as a CSV that Sparrow's
**Send to Many → Load CSV** reads in one action, so what is left inside the clock
is choosing coins and a fee. That does not make the budget generous; it makes the
part of it that grew with *n* stop growing.

**Clock B — about 2016 blocks, roughly two weeks, covering step 6 to
confirmation.** After it, the **peer** marks the channel cancelled and forgets
it; your node never does — `waitForTimeout` arms only for the responder.

**Blowing clock B is worse.** The peer has deleted their state, and a funding
transaction that confirms afterwards leaves your coins in a 2-of-2 with a
counterparty who no longer knows about the channel. So the recovery screen names
clock B in **blocks** — "peers give up at block 887,412, about 13 days" — rather
than telling you there is time.

## Step 4 in detail

```
Step 4 — build the transaction in your wallet

  channel 1  bitrefill         250,000 sat
      bcrt1q0y0m2xwq6xh3d3h6d6dqzmgyzc0gdz7zxr6c6c4h3fjqz9f4amqk8s2ea

  channel 2  acinq             250,000 sat
      bcrt1qhtl7hc8ckygznefeyjd37je92vspksmqg9dg7rrnsa0ls003c17bqhea9y

Pay exactly these recipients, choose your coins and the fee,
and save the PSBT. Do not sign it yet.

  - You do not have to type any of that. Sparrow's Send to Many →
    Load CSV reads this file, which has just been written with exactly
    the recipients above — this saves you typing; step 5 is still
    what checks it:
      /home/you/batch-recipients.csv
  - The amounts in it are BTC, not sats, and the file cannot say so
    for itself — set Sparrow's unit to BTC before you load it. In
    sats mode it finds no recipients at all and tells you why, which
    is the failure worth having: sats read in BTC mode would load a
    hundred million times too large without a word.
  - Save it here, unsigned. Binary or base64, either is read:
      /home/you/batch.psbt
```

**The table stays, and it is not the CSV's preview.** The file is how the
recipients get into Sparrow; the table is the *attribution* — which peer gets
which output, at what amount — and it is the only screen on which that is legible
to you. Addresses get a line of their own there. A P2WSH funding address is 62
characters, and a wrapped one is a string you would have to reassemble by hand.

**The CSV is a convenience and is scoped like one.** It removes the typing; it
removes nothing from step 5, which is still the whole check on what comes back.
Every way a CSV can go wrong is a way a paste could go wrong and is caught in the
same place: an amount 10⁸ out is `WrongAmount`, a row Sparrow dropped is
`MissingOutput`. Nothing about the trust boundary moved.

**Why BTC and not sats.** The amount column carries no unit, and Sparrow reads it
in whichever unit your preference is set to — a setting Winthistle cannot see. One
of the two readings is always going to be wrong; the question is which wrong you
would rather have. Sat integers loaded in BTC mode become a hundred million times
too large, silently, and only surface much later as insufficient funds at coin
selection. BTC decimals loaded in sats mode fail to parse, every row drops, and
Sparrow says *"No recipients found. Use a CSV file with three columns, and ensure
amounts are in sats."* The second names its own cause. So the file is BTC, to
eight places, with no grouping separator anywhere and the label quoted — measured
against Sparrow 2.5.3, not inferred.

**"Do not sign it yet" is enforced, not advised.** Step 4 is the one moment left
where the gate can be defeated from outside: you would be holding a broadcastable
funding transaction with nothing yet recoverable, and your wallet's Broadcast
button is two clicks from its Sign button. A packet with any signature on it is
refused here, and the cost is building it again inside clock A.

**The other names are derived, not asked for.** `--psbt FILE` is where you save
the unsigned transaction; the signed one goes to `FILE-signed.psbt` or
`FILE-signed.txn` beside it, and the recipients go to `FILE-recipients.csv`.
Because those names come from the first rather than from you, none of them can be
the same file, and the signed transaction can never overwrite the one it is about
to be compared against.

**Step 7 takes a signed PSBT or a raw transaction**, hex or binary — whichever
your wallet hands you, including the hex from Sparrow's *View Final
Transaction*. Step 4 takes a PSBT and only a PSBT, and that is the refusal above
rather than an inconsistency: what proves a step-4 packet is unsigned is reading
its partial signatures, and a raw transaction has none to read.

## Step 6 in detail

This is the moment the whole design exists to produce, and on screen it is four
sentences. From the first live batch, *n* = 5:

```
psbt_verify: all 5 channels, with skip_finalize. LND has committed to
the funding outpoints of
a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90, and
from here only signatures may be added.

5 of 5 channels reached chan_pending, with nothing signed. Every one
of them is recoverable by force-close, the peers' ten minutes are no
longer running, and nothing has been broadcast.
```

Read the second paragraph as a receipt rather than a progress report. **5 of 5**
is the gate: every one of those channels holds a commitment signature from its
peer, so each is recoverable by force-close, and only now may the transaction be
allowed near the network. **With nothing signed** is the inversion — no wallet
has been asked for anything yet, and the transaction those outpoints belong to
exists only as a file on your disk. **Nothing has been broadcast** is the
strongest of the three, because from here it is the only claim that can still be
broken, and step 8 breaks it exactly once.

Note that step 6 prints under step 5's heading rather than getting one of its
own. The receipts arrive on the same *n* streams `psbt_verify` was sent down, so
there is no second thing to announce — and the txid in the first paragraph is the
one that was published at step 8, because the rule from here on is that it may
not move.

## Verification

A channel funding output is a P2WSH 2-of-2 between your node and the peer. No
signing wallet can attribute one: Sparrow, Coldcard, Jade and Passport all show
you payments to unrecognised addresses, plus change. Nothing on the device's
screen says which peer an output funds, or that the amounts landed against the
peers you chose, or that two of them were not swapped. Air-gapped verification
that cannot verify is theatre.

Winthistle knows what the transaction *means*, so it can check both directions:

- **Outbound**, before you build anything: it prints the batch plan — every
  funding address with the peer it funds, the exact amount, and the forwarding
  policy that channel will carry. Those addresses reach Sparrow as a generated
  CSV, or by copy-paste from the terminal. You never type one.
- **Inbound**, before LND is asked to commit: it refuses a transaction that does
  not match the plan you approved — an output the plan does not name, a missing
  or duplicated output, a wrong amount, a legacy input, an input spent twice, an
  input whose UTXO does not belong to it. An output that is not the one the plan
  named is caught here, before anything is pinned, so it costs a redo rather than
  a channel — however it got there.

**The policy in that plan** is the forwarding policy the channel will route at:
base fee, fee rate and CLTV delta. It comes from the `[policy]` block of your
batch file, which any channel may override key by key — `winthistle example-batch`
prints one — and it is applied at step 9, once the funding transaction confirms.
It is in the plan at step 3 so that what each channel will route at is on screen
next to its amount, rather than being discovered afterwards. LND's own defaults
are 1000 msat and 1 ppm, which is close to free on a large channel, and they are
what the channel routes at if nothing is applied.

**How Winthistle knows which output is your change.** It does not build the
transaction, so it cannot be told: it reads the master key fingerprints off the
transaction's own inputs and accepts an output carrying those same fingerprints
on the change branch. That is the evidence a hardware wallet uses to decide an
output is its own change rather than a payment, and the report marks such an
output "recognised by key origin rather than by address" because the claim is
weaker than a script. `--change ADDRESS` names the script instead, which is
stronger, and is what a wallet that writes no key origins needs. What is never
done is trusting an output because nobody claimed it: an output nobody can
account for is refused, and that refusal is the one this program exists to make.

## The fee is yours, and Winthistle has no opinion about it

You pick the rate in Sparrow, against whatever mempool you trust. There is no fee
setting in `winthistle.toml`, no `--fee-rate` flag and nothing to declare: step 5
computes what your transaction came out at and prints it, and nothing grades it.
A config file still carrying a `[fees]` block is refused, with a sentence saying
why rather than a shrug.

Winthistle asks nobody else either — a fee API is handed the size of what you are
building and the moment you are building it, which together are most of what this
program exists not to leak. It once asked *you*, in a config key, and that turned
out to be theatre; [`docs/superseded.md`](docs/superseded.md) has the argument.

The rate you pick is the rate you get, because the funding transaction is never
replaced. Pick it against the mempool you can see when you build.

## No RBF on the funding transaction — and what actually enforces that

Replacing the funding transaction changes every outpoint in it and destroys every
channel in the batch. **No code path in this repository builds a replacement.**
That is the whole enforcement, and it is worth being precise about, because the
network does not help.

Full-RBF is unconditional in Bitcoin Core 29: `mempoolfullrbf` does not exist
even as a hidden debug option, and `getmempoolinfo` reports `"fullrbf": true`
with no way to turn it off. Checked live against bitcoind 29.0, the version the
harness pins, on 2026-08-26. A higher-fee conflict relays regardless of what the sequence numbers
signal, so `replaceable: false` is a statement of intent, not a defence, and a
verifier refusing over that signal would be a lint wearing an invariant's
clothes. This one does not read them.

What holds is narrower and worth stating plainly: **only you can sign those
inputs, and nothing here builds a replacement.** Your *other* wallet, with the
same keys, can. The discipline lives in the human. If you ever do double-spend a
batch's input out of band, `docs/design.html` documents the ordering that keeps
it from losing funds — keep every channel's state until the replacement is deeply
confirmed, and abandon only then.

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

**None of which is refused over.** A transaction with no change output is
reported under "Reported, not refused" and the batch still arms. Your change
arrangements are yours, and all the report does is say what yours came out as.
Which output *is* the change is a separate question, and a verification one — see
[Verification](#verification) above.

## If something stops halfway

Every run is journalled to SQLite (`~/.winthistle/runs.db`, via a pure-Go driver
— there is no cgo in this build) *before* the calls it describes, so a crash
mid-publish leaves artifacts rather than mystery. `publishing` is written *before*
`PublishTransaction` is called, deliberately, because "this may have been
broadcast" is the state that needs recording.

`winthistle recover` lists the runs that stopped and takes one apart: cancel the
shims, abandon what reached pending. It is safe to run as many times as it takes
and everything under it is idempotent. It refuses a run that reached the publish
call, and nothing in this program will abort one — read that run on its own, which
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

## What this is not

**Not a wallet, and not trying to become one.** Signing stays where it is.

**And not something that should have been built into Sparrow instead.** The
objection is fair — a Sparrow that held the `psbt_shim` conversation itself would
have the peer/address/amount mapping by construction, and would not need a file
handed between two programs at all. Four reasons it is a separate program
anyway:

- **The safety property is read out of LND's internals, and LND moves.** That
  `chan_pending` follows `CompleteReservation` is behaviour observed in
  `funding/manager.go`, not a documented interface. Re-reading it on every LND
  release is a cost Winthistle can carry because it is small and its author runs
  it. A wallet shipping to thousands of people on its own cadence would be
  carrying somebody else's undocumented ordering into every release.
- **A baked macaroon is a bearer credential.** Integration means a general-purpose
  wallet stores one, on a machine chosen for signing rather than for holding node
  credentials.
- **It has to be reviewable in an afternoon.** The claim Winthistle makes is that
  the transaction cannot reach the network before *n* of *n* receipts. That claim
  is worth roughly what the effort of checking it costs. Inside a wallet it would
  be a feature among hundreds.
- **Two programs that must agree is a stronger check than one program agreeing
  with itself.** Winthistle prints addresses it derived from LND and writes them
  to a file; you load them into a wallet that derived nothing; the wallet's
  output is checked back against the plan. The hand-off is the seam, and the seam
  is where a mistake becomes visible. Generating the file narrows how a mistake
  gets in; it does not move where one is caught.

**What integration would not fix**, and this is worth conceding plainly: it would
not improve verification on a hardware device. Coldcard and Jade would still show
unattributed P2WSH outputs either way, because the attribution lives in LND's
funding state and not in anything a PSBT can carry to a screen.

**What happened to the invariant about partial signatures.** An earlier design
required signers to return partial signatures only, so no external wallet ever
held a broadcastable transaction. It existed to stop anything broadcasting
*before the gate opened*. After the inversion there is no "before the gate opens"
— every channel reaches `chan_pending` at step 6 and you do not sign until step 7
— so a wallet holding a fully signed transaction front-runs nothing. The
invariant was not relaxed; it had nothing left to protect against, and the
single-sig question stopped mattering with it. Full account:
[`docs/superseded.md`](docs/superseded.md).

## Threat model

**Assume one box.** Winthistle, Sparrow and LND may all run on the same machine,
and in the setup this was built and probed against, two of the three do. Nothing
here is an air gap and nothing here should be read as one.

**What Winthistle defends against is error, not an adversary.** A swapped output,
a wrong address however it arrived, a wrong amount, a batch that reaches the
network with a
channel still unrecoverable, a signer that moved the txid: those are the failures
it refuses, and they are the ones that actually take channels. An attacker who
controls the box you are working on has your signing wallet and your node. They
do not need to defeat any of this, and no arrangement of these three programs
would stop them.

**It trusts LND, and that is the load-bearing assumption.** The funding addresses
in the plan come from LND — Winthistle derives none of them — and so does the
claim that a given address is a 2-of-2 with a given peer. What gets checked is
that the transaction you built pays exactly the addresses LND issued, in the
amounts you asked for, and that every output is accounted for. A compromised LND
handing over an address that is not what it says it is would not be caught here,
by this program or by any other on that box, because there is no second source
for that fact.

**What is bounded is this program's own reach.** The credential is baked, not
`admin.macaroon`, and it cannot bake a wider one, close a channel, send a
payment, send coins on-chain, sign a message, sign a transaction or spend the
node's own coins — refused at the registry rather than left out of it, and
`winthistle doctor` makes LND confirm the refusals. So a Winthistle that is
hostile or merely wrong is bounded by what that credential can do, which does not
include moving your money. It is a blast radius on the program, not on the box.

**What it leaks: nothing of its own.** No listening port, no outbound connection
except to your LND, no fee API, no block explorer, no Lightning explorer. Peer
facts come from LND's local gossip graph. This is a privacy property rather than
a security one, and it is the reason there is no fee estimate.

**Two smaller things that are checks rather than claims.** `--psbt` refuses a
path that already exists, because the funding addresses did not exist before the
run — LND issues them at step 2 — so a file predating the run cannot be this
batch's transaction. The generated `FILE-recipients.csv` is refused on the same
rule turned around: one already on disk was written for some other batch's
addresses, and loading it would build a transaction paying them. And the signed
file's name is derived from the unsigned one's, so a wallet cannot be talked into
overwriting the packet it is about to be compared against.

## What you need

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
hand it and exports a PSBT. Hot, cold, single-sig, multisig — which of those it
is does not matter to this program. The recipients CSV is written for Sparrow's
Send to Many dialog specifically; any other wallet takes the recipients off the
printed table, which is what the table is for.

## Documentation

- [`docs/replan-2026-08.md`](docs/replan-2026-08.md) — **the current direction**,
  and the honest account of what is built and what is not. Read this first.
- [`docs/design.html`](docs/design.html) — the full design: invariants, the
  sequence, the two clocks, the hazard register, recovery.
- [`docs/superseded.md`](docs/superseded.md) — designs Winthistle used to have,
  and why each went. Written so that a gap is not mistaken for an oversight.
- `CLAUDE.md` — the invariants and the do-not-reintroduce list, with source
  citations.
- `winthistle print-macaroon-command` — the `lncli bakemacaroon` line for the
  build in front of you, generated from the method registry rather than written
  down anywhere. `make macaroon` is the same command, for use from a checkout
  without a built binary; there is only one door.

## License

MIT
