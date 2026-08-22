#!/usr/bin/env python3
"""Build a simulated 2-of-2 multisig cold wallet out of three Core wallets.

This is what CI uses in place of hardware. It creates:

  cold1       holds key A's private key  -> signs with walletprocesspsbt
  cold2       holds key B's private key  -> signs with walletprocesspsbt
  cold-watch  neither private key        -> the "cold storage" Winthistle sees

Signing partials in cold1 and cold2 and combining them in the app exercises
exactly the path invariant I-2 depends on: no single party ever holds a fully
signed transaction. No devices, no Sparrow, fully scriptable.
"""
import json
import re
import subprocess
import sys
from pathlib import Path

HERE = Path(__file__).parent
RANGE = [0, 999]
FUND_UTXOS = 4          # several inputs so coin selection has real choices
FUND_EACH = 2           # BTC per UTXO


def cli(*args, wallet=None):
    cmd = [str(HERE / "bin" / "bcli")]
    if wallet:
        cmd.append(f"-rpcwallet={wallet}")
    cmd += [str(a) for a in args]
    out = subprocess.run(cmd, capture_output=True, text=True)
    if out.returncode != 0:
        raise RuntimeError(f"{' '.join(cmd)}\n{out.stderr.strip()}")
    body = out.stdout.strip()
    try:
        return json.loads(body)
    except json.JSONDecodeError:
        return body


def make_wallet(name, private):
    """Create the wallet, tolerating one that already exists."""
    try:
        cli("-named", "createwallet", f"wallet_name={name}",
            f"disable_private_keys={str(not private).lower()}",
            f"blank={str(not private).lower()}", "descriptors=true")
    except RuntimeError as e:
        if "already exists" not in str(e) and "Database already exists" not in str(e):
            raise
        try:
            cli("loadwallet", name)
        except RuntimeError:
            pass


def key_expr(wallet, private):
    """Pull the external wpkh key expression, e.g. [fp/84h/1h/0h]tpub.../0/*"""
    descs = cli("listdescriptors", *(["true"] if private else []), wallet=wallet)
    for d in descs["descriptors"]:
        if d.get("active") and not d.get("internal") and d["desc"].startswith("wpkh("):
            m = re.match(r"^wpkh\((.+)\)#[a-z0-9]{8}$", d["desc"])
            if not m:
                raise RuntimeError(f"unparsed descriptor: {d['desc']}")
            return m.group(1)
    raise RuntimeError(f"{wallet}: no active external wpkh descriptor")


def branch(expr, index):
    """Swap the /0/* receive branch for /<index>/*."""
    if not expr.endswith("/0/*"):
        raise RuntimeError(f"unexpected branch on {expr}")
    return expr[: -len("/0/*")] + f"/{index}/*"


def multi(a, b, index):
    # sortedmulti sorts by pubkey at derivation time, so argument order here does
    # not change the addresses — all three wallets agree regardless.
    return f"wsh(sortedmulti(2,{branch(a, index)},{branch(b, index)}))"


def checksum(desc):
    return cli("getdescriptorinfo", desc)["descriptor"]


def import_pair(wallet, ext, internal):
    cli("importdescriptors", json.dumps([
        {"desc": checksum(ext), "active": True, "internal": False,
         "range": RANGE, "timestamp": 0},
        {"desc": checksum(internal), "active": True, "internal": True,
         "range": RANGE, "timestamp": 0},
    ]), wallet=wallet)


def main():
    print("==> creating wallets")
    make_wallet("cold1", private=True)
    make_wallet("cold2", private=True)
    make_wallet("cold-watch", private=False)

    print("==> extracting keys")
    pub_a, prv_a = key_expr("cold1", False), key_expr("cold1", True)
    pub_b, prv_b = key_expr("cold2", False), key_expr("cold2", True)

    print("==> importing 2-of-2 descriptors")
    # Each signer gets its own key private and the other's public.
    import_pair("cold1", multi(prv_a, pub_b, 0), multi(prv_a, pub_b, 1))
    import_pair("cold2", multi(pub_a, prv_b, 0), multi(pub_a, prv_b, 1))
    import_pair("cold-watch", multi(pub_a, pub_b, 0), multi(pub_a, pub_b, 1))

    print("==> funding cold-watch")
    miner_addr = cli("getnewaddress", wallet="miner")
    for _ in range(FUND_UTXOS):
        addr = cli("getnewaddress", wallet="cold-watch")
        cli("sendtoaddress", addr, FUND_EACH, wallet="miner")
    cli("generatetoaddress", 6, miner_addr)

    bal = cli("getbalances", wallet="cold-watch")["mine"]["trusted"]
    print(f"\n==> cold-watch holds {bal} BTC in {FUND_UTXOS} UTXOs")
    print("\nWatch-only descriptors — this is what Winthistle imports:\n")
    print("  external:", checksum(multi(pub_a, pub_b, 0)))
    print("  internal:", checksum(multi(pub_a, pub_b, 1)))
    print("\nSign a PSBT with:")
    print("  ./bin/bcli -rpcwallet=cold1 walletprocesspsbt <psbt> true ALL false")
    print("  ./bin/bcli -rpcwallet=cold2 walletprocesspsbt <psbt> true ALL false")
    print("  # then combine the two partials IN THE APP, never here (I-2)")


if __name__ == "__main__":
    try:
        main()
    except RuntimeError as e:
        sys.exit(f"error: {e}")
