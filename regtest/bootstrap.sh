#!/usr/bin/env bash
# Bring a freshly-started harness to a usable state: mature the chain, fund every
# LND node, and connect alice to each peer. Idempotent — safe to re-run.
set -euo pipefail
cd "$(dirname "$0")"

PEERS=(bob carol dave)
NODES=(alice "${PEERS[@]}")
jqp() { python3 -c "import sys,json;print(json.load(sys.stdin)$1)"; }

echo "==> miner wallet"
./bin/bcli -named createwallet wallet_name=miner descriptors=true >/dev/null 2>&1 \
  || ./bin/bcli loadwallet miner >/dev/null 2>&1 || true
MINER=$(./bin/bcli -rpcwallet=miner getnewaddress)

# Core does not auto-load non-default wallets, so a restart leaves the cold
# wallets on disk but unloaded and every wallet call fails with "Requested wallet
# does not exist or is not loaded". Harmless before cold-wallet.py has ever run.
for w in cold1 cold2 cold-watch; do
  ./bin/bcli loadwallet "$w" >/dev/null 2>&1 || true
done

# Coinbase outputs need 100 confirmations before they are spendable.
HEIGHT=$(./bin/bcli getblockcount)
if [ "$HEIGHT" -lt 101 ]; then
  echo "==> mining $((101 - HEIGHT)) blocks to maturity"
  ./bin/bcli generatetoaddress $((101 - HEIGHT)) "$MINER" >/dev/null
fi

echo "==> funding LND nodes"
for n in "${NODES[@]}"; do
  addr=$(./bin/lncli "$n" newaddress p2tr | jqp "['address']")
  ./bin/bcli -rpcwallet=miner sendtoaddress "$addr" 5 >/dev/null
  echo "    $n  <- 5 BTC  ($addr)"
done
./bin/bcli generatetoaddress 6 "$MINER" >/dev/null

echo "==> connecting alice to peers"
for p in "${PEERS[@]}"; do
  pk=$(./bin/lncli "$p" getinfo | jqp "['identity_pubkey']")
  ./bin/lncli alice connect "$pk@$p:9735" >/dev/null 2>&1 || true
  echo "    alice -> $p  $pk"
done

echo
echo "==> ready"
for n in "${NODES[@]}"; do
  bal=$(./bin/lncli "$n" walletbalance | jqp "['confirmed_balance']")
  printf '    %-6s confirmed=%s sat\n' "$n" "$bal"
done
echo "    peers: $(./bin/lncli alice listpeers | jqp "['peers'].__len__()")"
