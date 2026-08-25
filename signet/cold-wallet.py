#!/usr/bin/env python3
"""Build the signet simulated 2-of-2 multisig cold wallet.

Same construction as regtest/cold-wallet.py — three Core wallets, two holding a
private key each, their external wpkh key expressions reassembled into
wsh(sortedmulti(2,...)) and imported watch-only:

  cold1       holds key A's private key
  cold2       holds key B's private key
  cold-watch  neither private key  -> the "cold storage" Winthistle imports

It is a separate script rather than a parameter on that one because everything
around the descriptor assembly differs: there is no miner wallet here, no
generatetoaddress, and the coins come from a faucet with a human in the middle.
Nothing depends on the two scripts agreeing — the Go tests read the descriptors
off the wallet, never off this file — so the worst a drift could do is produce a
different but equally valid 2-of-2.

What this wallet is for is the rescan. Signet has real history, so a birthday
before the coin's block and a birthday after it find different things, which is
the one property regtest cannot show.

Signet keys are tpubs. A descriptor printed here may be pasted into a test
fixture; a mainnet one never may.
"""
import json
import re
import subprocess
import sys
from pathlib import Path

HERE = Path(__file__).parent
RANGE = [0, 999]


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
    # timestamp 0 is genesis, the same as regtest/cold-wallet.py uses.
    #
    # It is deliberately not "now", and not only because coldwallet.Config
    # refuses that. This script can be run while the node is still downloading
    # the chain — the faucet step below has a human in it and is worth starting
    # early — and a wallet imported from *now* on a node that is behind would
    # record a birthday later than blocks it went on to scan. Genesis is true
    # whenever it is run.
    cli("importdescriptors", json.dumps([
        {"desc": checksum(ext), "active": True, "internal": False,
         "range": RANGE, "timestamp": 0},
        {"desc": checksum(internal), "active": True, "internal": True,
         "range": RANGE, "timestamp": 0},
    ]), wallet=wallet)


def chain_state():
    """Report where the node is, without refusing one that is still catching up.

    Being behind does not spoil this fixture: these wallets are imported from
    genesis, and Core hands a loaded wallet every block it connects afterwards,
    so a faucet payment made now is picked up whenever the download reaches it.
    What matters is that the *tests* refuse a node in initial block download —
    signetenv.Start does, because a rescan against a chain that is still
    arriving finds whatever has arrived, silently.
    """
    info = cli("getblockchaininfo")
    if info["chain"] != "signet":
        raise RuntimeError(f"this node is on {info['chain']}, not signet")
    return info


def main():
    print("==> checking the node")
    chain = chain_state()
    print(f"    signet, height {chain['blocks']} of {chain['headers']}")
    if chain["initialblockdownload"]:
        print("    still downloading — that is fine for building the fixture, but")
        print("    the Go tests will skip until `make sync` reports ibd=False.")

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

    addr = cli("getnewaddress", wallet="cold-watch")

    print("\nWatch-only descriptors — this is what Winthistle imports:\n")
    print("  external:", checksum(multi(pub_a, pub_b, 0)))
    print("  internal:", checksum(multi(pub_a, pub_b, 1)))
    print("\n==> the wallet is empty, and the next step is a human one.")
    print(f"\n  Send signet coins to: {addr}\n")
    print("  Faucet:   https://alt.signetfaucet.com/   (worked 2026-08-25)")
    print("            https://signetfaucet.com/       (queued, never paid)")
    print("\n  They are rate-limited, often down, and they rot — see the faucet")
    print("  section of README.md. A retry elsewhere is normal and is not a")
    print("  failure. When the payment confirms, run")
    print("  `make height` — that block height is the reference both birthday")
    print("  tests are written against.")


if __name__ == "__main__":
    try:
        main()
    except RuntimeError as e:
        sys.exit(f"error: {e}")
