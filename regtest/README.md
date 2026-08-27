# regtest harness

A disposable Bitcoin + Lightning network for developing Winthistle: one
`bitcoind`, one node that stands in for yours (`alice`), and three peers
(`bob`, `carol`, `dave`) so batches up to *n*=3 are testable.

Regtest is the point: you mine on demand, so confirmation depth, the ten-minute
peer window, a deliberately stuck transaction, CPFP, and LND's funding-timeout
horizon are all reachable in seconds rather than hours. The failure branches are
most of the value here, and mainnet will not produce them on request.

## Requires

Docker and Docker Compose v2+. Nothing else — no chain to sync.

## Quickstart

```sh
make reset      # destroy, rebuild, fund, connect, extract creds, self-test
make info       # one line per node
make verify     # re-run the 2-of-2 self-test on its own
```

`make help` lists the rest. `make reset` is idempotent and takes well under a
minute; reach for it whenever state gets confusing rather than debugging it.

## What you get

| | |
|---|---|
| `alice` | your node — gRPC on `localhost:10009`, REST on `:8080` |
| `bob`, `carol`, `dave` | peers, already connected to alice — gRPC `:10010`–`:10012` |
| `bitcoind` | regtest, RPC on `localhost:18443`, user/pass `winthistle` |
| `creds/<node>/` | `tls.cert` and `admin.macaroon`, for tests to point at |
| `chain-init` | one-shot: matures the chain to 101 blocks, then exits |
| `cold1`, `cold2` | Core wallets each holding one key of a 2-of-2 |
| `cold-watch` | the watch-only "cold storage" wallet, funded with 4 UTXOs |

## The simulated cold wallet

`cold-wallet.py` builds a 2-of-2 multisig out of three Core wallets: `cold1` and
`cold2` each hold one private key, and `cold-watch` holds neither. That is the
stand-in for hardware, and it is what CI should use.

It matters because signing each partial separately and combining them *in the
app* is exactly the path invariant **I-2** depends on — no single party ever
holds a fully signed transaction. Testing with one wallet that can sign alone
would quietly skip the thing most worth testing.

```sh
./bin/bcli -rpcwallet=cold1 walletprocesspsbt <psbt> true ALL false
./bin/bcli -rpcwallet=cold2 walletprocesspsbt <psbt> true ALL false
# combine the two partials in the app, never with combinepsbt here
```

`./bin/bcli` and `./bin/lncli alice …` are thin wrappers for poking around by hand.

`make verify` proves the fixture actually holds: that neither signer alone can
complete a transaction, that the two partials combine into one that does, that
the mempool would accept it, and that every input is segwit. It broadcasts
nothing, so it is safe to re-run whenever you doubt the setup.

## Gotchas already handled

Two of these cost real debugging time; both fail in ways that look like your code.

- **A fresh regtest chain has zero blocks, and LND will not finish starting on
  one** — it sits at `Waiting for chain backend to finish sync` forever, so no
  healthcheck can ever pass. The one-shot `chain-init` service matures the chain
  to 101 blocks (past coinbase depth) before any LND node is allowed to start.
- **The LND data directory is an image detail, and it moved.** Polar's image ran
  LND as the `lnd` user with its data in `/home/lnd/.lnd`, while `docker compose
  exec` and healthchecks run as root, whose `~` is `/root` — so every `lncli`
  invocation needed `--lnddir=/home/lnd/.lnd` or it failed with a missing
  `tls.cert`, which reads like a TLS problem and is not one. Lightning Labs'
  image, which this harness uses from `v0.21.2-beta`, runs LND as root with its
  data in `/root/.lnd`, so the flag now happens to name `lncli`'s own default.
  It is still stated explicitly, in the compose healthcheck, in `bin/lncli` and
  in the `creds` target, because it is the image's choice and not ours.
- **Core does not auto-load non-default wallets.** After `make down && make up`,
  or any bitcoind restart, `miner` and the three cold wallets are still on disk
  but closed, and every wallet-scoped call fails with "Requested wallet does not
  exist or is not loaded" — which reads like the wallet was destroyed and was
  not. `make bootstrap` reloads them; `make reset` covers it too. The Go tests
  load what they need themselves, because a real node has the same behaviour and
  the abort path has to run on a node that has just come back up.
- **`verify.py` needs its shebang.** It is executable and invoked by path, so
  without one the kernel hands it to `/bin/sh`, which reads the backticks around
  `` `make verify` `` in the module docstring as command substitution and forks
  until the process table fills. The Makefile now calls `python3` explicitly so
  the recipe cannot depend on the shebang at all.
- **`-fallbackfee`** is set. Without it, regtest fee estimation fails outright
  because there is no fee history to estimate from.
- **`--tlsextraip=127.0.0.1`** is set, so gRPC from the host validates against
  each node's certificate.
- **`--maxpendingchannels=200`**, so re-arm cycles don't hit the per-peer
  default. This line said 10 until 2026-08-26; `.env` has set 200 for longer than
  that, and its own comment says "not the 10 Polar would give you" — so the two
  files disagreed about which number was the harness's and which was Polar's.
  `.env` is authoritative: it is the file the containers actually read.
- **LND is Lightning Labs' image; bitcoind is Polar's.** Polar publishes no LND
  tag past `0.20.0-beta`, and the harness has to run the version an operator
  actually runs. Three differences the compose file absorbs: the entrypoint is
  `lnd` itself, so `command` carries flags and not the binary name; the data
  directory is `/root/.lnd`, above; and there is no `USERID`/`GROUPID` entrypoint
  shim, which costs nothing because the state lives in named volumes and
  `docker cp` still lands `creds/` owned by you. Note the tags differ in shape
  too — Polar's LND tags had no `v`, Lightning Labs' do.
- **Named volumes, not bind mounts.** Avoids the uid-mismatch trap, and keeps
  this usable against a remote daemon — `docker context create stacker --docker
  host=ssh://stacker@host` then `docker --context stacker compose up -d`.
  Credentials come out via `make creds` rather than a mounted path.

## What regtest cannot test

Nothing this build does. The **descriptor-import rescan** and the
**prune-horizon check** could not be reached here — there is no history to
rescan, so a right birthday and a wrong one find precisely the same nothing, and
this node cannot be made meaningfully pruned — and a second harness in
`signet/` existed for them. Both paths belonged to `winthistle setup` and to the
descriptor import, which the 2026-08 rewrite deleted, so the signet harness went
with them.

This harness does not substitute for the mainnet cold probe, which proves your node,
your peers, your devices.
