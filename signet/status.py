#!/usr/bin/env python3
"""Report the signet harness's state: sync progress, and where the coins are.

`--coins` adds the cold wallet's UTXOs *with their block heights*. That height
is the whole reason this harness exists — it is the reference the two birthday
tests are written against, one birthday before it and one after.
"""
import json
import subprocess
import sys
from pathlib import Path

HERE = Path(__file__).parent


def cli(node, *args, wallet=None):
    cmd = [str(HERE / "bin" / node)]
    if wallet:
        cmd.append(f"-rpcwallet={wallet}")
    cmd += [str(a) for a in args]
    out = subprocess.run(cmd, capture_output=True, text=True)
    if out.returncode != 0:
        raise RuntimeError(out.stderr.strip() or out.stdout.strip())
    try:
        return json.loads(out.stdout.strip())
    except json.JSONDecodeError:
        return out.stdout.strip()


def chain_line(label, node):
    try:
        c = cli(node, "getblockchaininfo")
    except RuntimeError as e:
        print(f"{label:<9} not answering — {str(e).splitlines()[-1][:70]}")
        return None
    extra = ""
    if c.get("pruned"):
        extra = f" pruneheight={c['pruneheight']}"
    print(f"{label:<9} height={c['blocks']} ibd={c['initialblockdownload']} "
          f"verified={c['verificationprogress']:.4f}{extra}")
    return c


def coins():
    try:
        utxos = cli("bcli", "listunspent", 0, 9999999, wallet="cold-watch")
    except RuntimeError:
        print("cold-watch  not created yet — run: make cold")
        return
    if not utxos:
        print("cold-watch  no coins yet — the faucet step has not landed")
        return
    for u in utxos:
        tx = cli("bcli", "gettransaction", u["txid"], wallet="cold-watch")
        h = tx.get("blockheight", "unconfirmed")
        print(f"cold-watch  {u['amount']} BTC at height {h} "
              f"({u['confirmations']} confirmations)  {u['txid']}:{u['vout']}")


def main():
    if "--coins-only" not in sys.argv:
        chain_line("unpruned", "bcli")
        chain_line("pruned", "bcli-pruned")
    if "--coins" in sys.argv or "--coins-only" in sys.argv:
        coins()


if __name__ == "__main__":
    main()
