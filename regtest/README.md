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
- **`docker compose exec` and healthchecks run as root**, whose `~` is `/root`,
  while LND runs as the `lnd` user with its data in `/home/lnd/.lnd`. So every
  `lncli` invocation needs `--lnddir=/home/lnd/.lnd` or it fails with a missing
  `tls.cert` — which reads like a TLS problem and is not one.
- **`-fallbackfee`** is set. Without it, regtest fee estimation fails outright
  because there is no fee history to estimate from.
- **`--tlsextraip=127.0.0.1`** is set, so gRPC from the host validates against
  each node's certificate.
- **`--maxpendingchannels=10`**, so re-arm cycles don't hit the per-peer default.
- **Named volumes, not bind mounts.** Avoids the uid-mismatch trap, and keeps
  this usable against a remote daemon — `docker context create stacker --docker
  host=ssh://stacker@host` then `docker --context stacker compose up -d`.
  Credentials come out via `make creds` rather than a mounted path.

## What regtest cannot test

The **descriptor-import rescan** — regtest has no history to rescan, and the
rescan path is precisely what needs an unpruned node. Use signet for that, where
a full unpruned node is only a few GB. And neither substitutes for the mainnet
cold probe, which proves your node, your peers, your devices.
