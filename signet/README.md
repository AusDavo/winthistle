# signet harness

Two `bitcoind`s and nothing else: one unpruned, one pruned.

This is not a signet deployment. It exists for the two things
[`regtest/`](../regtest/) cannot reach, and both of them are Core's rather than
LND's — `setup.Deps` has no LND field, so there is no node here, no peers, no
channels and no batch.

| what | why regtest cannot | which node |
|---|---|---|
| `coldwallet.Import`'s descriptor rescan | regtest has no history, so a right birthday and a wrong one find the same nothing | unpruned |
| `coldwallet.PrunedPastBirthday`, and `doctor`'s prune-horizon warning | regtest cannot be made meaningfully pruned | pruned |

## Requires

Docker and Docker Compose v2+, and a real block download — see *The cost* below.

## Quickstart

```sh
make up                      # start both nodes; they begin downloading
make sync                    # watch it; wait for ibd=False on both
make cold                    # build the 2-of-2 watch-only wallet
make address                 # print a receive address
#   ... send signet coins to it from a faucet, then wait for a block ...
make height                  # the coin's block height: the tests' reference

WINTHISTLE_SIGNET=1 go test -p 1 -count=1 -run Signet ./internal/coldwallet/
```

`make help` lists the rest.

## The cost

One initial block download of signet. It is the only unavoidable cost of this
harness, and it is why the tests are off by default: `make test` at the repo root
never touches this, because every helper in `internal/signetenv` skips unless
`WINTHISTLE_SIGNET=1` is set.

**Signet is not small, and this repository used to say it was.** "A few GB" and
"the chain is small" were in `CLAUDE.md` and `regtest/README.md` and nobody had
measured. Signet's early years are near-empty ten-minute blocks, but its recent
ones are heavily used, and the tail is most of the chain: at 81% of the height,
only 36% of the transaction weight had been verified. Start it and do something
else.

### Measured cost

Measured on one machine (12 cores, SSD, both nodes syncing at once), 2026-08-25,
to a tip of block 319,253. Your disk and your peers will move these.

| | unpruned | pruned (`-prune=550`) |
|---|---|---|
| block data | 22.3 GB | 0.43 GB |
| whole docker volume | 24.6 GB | 4.6 GB |
| initial block download | **63 minutes, both together** | |

Three things are worth knowing before you watch the progress bar.

**Height lies here.** At 81% of the height only 36% of the transaction weight had
been verified — signet's early years are near-empty ten-minute blocks and its
recent ones are heavily used. Read `verificationprogress`, which `make sync`
prints.

**So does extrapolating `verificationprogress`.** At 48 minutes in, the measured
rate said 65 minutes remaining. It finished 15 minutes later. Both estimates were
honest and both were wrong, so treat any figure here as an order of magnitude.

**The two nodes are not independent.** The pruned one ran roughly 4× faster while
they overlapped — same validation, a twentieth of the disk writes — and the
unpruned one sped up sharply once the pruned one finished and stopped competing
for the disk. The bottleneck for the unpruned node is writing 22 GB, not
validating it.

The pruned node's 4.6 GB is almost all chainstate: `-prune=550` caps the *blocks*
and nothing else. Its horizon landed 826 blocks behind the tip, dated six days
back, which is a real mid-chain horizon and exactly what
`coldwallet.PrunedPastBirthday` needs.

**The rescan itself is the cheap part.** `coldwallet.Import` over a thirty-day
span — about 4,300 signet blocks, against a wallet watching 2,000 derived scripts
— took **17 seconds** on a synced node. The hour is the download, once; the
rescan an operator waits for at `winthistle setup` is not the same order of cost
at all. Do not quote the IBD figure at somebody asking how long setup takes.

## The faucet step is a human one

The rescan tests need the cold wallet to hold a coin at a **known height**, and
default signet cannot be self-mined: its blocks need a signature from the signet
challenge key, which is the whole point of signet. So the coin comes from a
faucet and somebody has to go and get it.

A custom signet with our own challenge key was considered and declined. A chain
we mined ourselves would be blocks we made, and a rescan over those tests nothing
an operator will meet. Signet's blocks are other people's.

What the rescan test actually spans is worth being exact about: a birthday thirty
days before the faucet coin, not genesis. That is a mid-chain birthday, which is
what proves Core starts where it is told rather than merely scanning everything —
and it is the shape of what an operator supplies, since they usually know roughly
when their cold wallet was made. The test logs how long it took; there is no
threshold on it, because a threshold would only ever fail on somebody's slower
disk.

Faucets are rate-limited, frequently down, and they rot. Measured 2026-08-25,
funding this harness:

- <https://alt.signetfaucet.com/> — **worked.** 0.00712599 BTC, one confirmation.
- <https://signetfaucet.com/> — accepted the address and said the payout was
  queued. Nothing ever arrived. Its own wording is that abuse-check failures are
  *silently* discarded, so a queued payment that never comes is indistinguishable
  from a dropped one.
- ~~signet.bc-2.jp~~ — gone; the domain now serves a condiment supplier in
  Singapore. Do not follow a faucet link out of a document without looking at
  where it lands.

A retry on a different one is normal and is not a failure. Check the address
against `make -C signet address` before pasting it anywhere.

**One of the two birthday tests needs the coin to be more than three hours old.**
Core does not start a rescan at the first block later than the import timestamp —
it winds back `TIMESTAMP_WINDOW`, two hours, so that a wallet whose clock
disagreed with the chain's still finds its coins. A birthday of *today* placed
after a coin that confirmed twenty minutes ago therefore still reaches back over
it, and the too-late-birthday test would prove the opposite of what it says.
`signetenv.RequireClearOfTheTimestampWindow` skips rather than letting that
happen, and it checks every coin in the wallet, not just the oldest — a second
faucet payment made an hour ago would be found while the first sat safely in the
past.

The rescan test has no such requirement and runs the moment the coin confirms: a
birthday thirty days before a ten-minute-old coin is still thirty days before it.
So expect the first run after funding to pass one test and skip the other, and
both to pass a few hours later. It happens once per coin.

## What you get

| | |
|---|---|
| `bitcoind` (`wt-signet`) | unpruned, RPC on `127.0.0.1:38332` |
| `pruned` (`wt-signet-pruned`) | `-prune=550`, RPC on `127.0.0.1:38352`, no wallet |
| `cold1`, `cold2` | key-holding Core wallets, one key each |
| `cold-watch` | the watch-only 2-of-2 — what Winthistle imports |

The two nodes peer with each other as well as with the network, so the pruned
node's download does not depend on which public signet peers answer.

Signet keys are `tpub`s, so a descriptor printed by `make cold` may be pasted
into a test fixture. **A mainnet `xpub` never may** — one deanonymises the whole
cold wallet's history, permanently, and git history cannot be un-published.

## Why this is a directory beside `regtest/` and not a service inside it

A second chain in `regtest/docker-compose.yml` would put two networks behind the
one thing `regtestenv.Start` connects to, and `make test`'s `-p 1` discipline is
written around there being exactly one. `-p 1` is not a speed knob: two
harness-backed package binaries through the same node fail in ways that say
nothing about the real cause.

The same discipline applies twice over here. A signet-backed test must not run
beside a regtest-backed one, which `-p 1` already guarantees.

## Throwaway wallets

The rescan tests create a **fresh** wallet per run, named for the clock. That is
the point rather than tidiness: a rescan test against a wallet an earlier run
already filled would pass with the rescan removed entirely.

Core has no RPC that deletes a wallet, so they accumulate. `make sweep` removes
them; `make clean` removes everything including the downloaded chain.

## What this harness still cannot test

The **mainnet cold probe**. Proving this node, these peers, these devices is
commissioning work with real coins and real hardware, and neither regtest nor
signet substitutes for it. See `docs/design.html`.
