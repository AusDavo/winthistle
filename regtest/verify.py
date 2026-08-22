"""Smoke-test the harness, and specifically the 2-of-2 fixture.

Asserts the property the fixture exists for: neither signer alone can complete
the transaction, the two partials combine into one that does, the mempool would
accept it, and every input is segwit (I-3). Broadcasts nothing, so it is safe to
re-run at any time.

Run via `make verify`.
"""
import json, subprocess, sys
def cli(*a, wallet=None):
    c=['./bin/bcli']+([f'-rpcwallet={wallet}'] if wallet else [])+[str(x) for x in a]
    r=subprocess.run(c,capture_output=True,text=True)
    if r.returncode: sys.exit(f"FAIL {' '.join(c)[:80]}\n{r.stderr.strip()}")
    try: return json.loads(r.stdout.strip())
    except json.JSONDecodeError: return r.stdout.strip()

dest = cli('getnewaddress', wallet='miner')
built = cli('-named','walletcreatefundedpsbt',
            f'outputs=[{{"{dest}":1.5}}]','fee_rate=5','bip32derivs=true',
            wallet='cold-watch')
psbt = built['psbt']
print(f"built unsigned psbt, fee={built['fee']} BTC, changepos={built['changepos']}")

a = cli('walletprocesspsbt', psbt, 'true', 'ALL', 'false', wallet='cold1')
b = cli('walletprocesspsbt', psbt, 'true', 'ALL', 'false', wallet='cold2')
print(f"cold1 alone complete={a['complete']}   cold2 alone complete={b['complete']}")
assert a['complete'] is False and b['complete'] is False, "a single signer completed it — fixture is wrong"
assert a['psbt'] != b['psbt'], "signers produced identical psbts"

comb = cli('combinepsbt', json.dumps([a['psbt'], b['psbt']]))
fin  = cli('finalizepsbt', comb)
print(f"combined+finalized complete={fin['complete']}")
assert fin['complete'] is True and 'hex' in fin

acc = cli('testmempoolaccept', json.dumps([fin['hex']]))
print(f"testmempoolaccept allowed={acc[0]['allowed']}  vsize={acc[0].get('vsize')}")
assert acc[0]['allowed'] is True, acc[0]

# and prove the whole set of inputs was segwit, per I-3
dec = cli('decodepsbt', comb)
kinds = {i.get('witness_utxo',{}).get('scriptPubKey',{}).get('type') for i in dec['inputs']}
print(f"input script types: {kinds or 'NONE — non-segwit inputs!'}")
assert kinds and all(k and 'witness' in k for k in kinds), "non-segwit input present"

print("\n2-of-2 fixture verified: neither signer alone completes, combined does,")
print("mempool accepts it, and every input is segwit.")
