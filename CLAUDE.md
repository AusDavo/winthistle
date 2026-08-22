# Winthistle

A local, guided web UI for batch-opening Lightning channels in one on-chain
transaction, funded from single-sig or multisig cold storage. LND only.

Full spec: `docs/design.html`. State and build order: `HANDOFF.md`.

**Status: design complete, unimplemented.** The safety model below is verified
against LND source but has not yet been tested against a running node.

---

## The four invariants

These are the product, not preferences. Each is enforced as a gate in code, and
each carries a source citation so you can verify it rather than trust this file.

**If you believe an invariant is wrong, say so and stop. Do not work around one,
and do not weaken one to make a test pass.**

### I-1 · Publish only once every channel is already recoverable

`no_publish` MUST be set on **every** channel in a batch. Finalize all *n*, wait
for all *n* `chan_pending`, then publish exactly once via
`WalletKit.PublishTransaction`.

Why it holds: in `funding/manager.go`, `handleFundingSigned` calls
`CompleteReservation(nil, commitSig)` — storing the peer's commitment signature —
*before* the broadcast block, and emits `chan_pending` *after* it. So each
`chan_pending` is a receipt that the channel is recoverable by force-close.
`no_publish` sets `NoFundingTxBit` (`lnwallet/reservation.go`), which skips only
the broadcast block, leaving `CompleteReservation`, `WatchNewChannel` and the
`chan_pending` emission intact.

NEVER change this to "all but the last", **even though that is what LND's own
docs recommend.** That idiom exists because `lncli` has no way to broadcast
afterwards — a CLI limitation, not a protocol constraint. LND's "DO NOT PUBLISH
… OR THE FUNDS CAN BE LOST" warning is about *ordering*, not authorship.

Consequence to preserve: `NoFundingTxBit` also gates `rebroadcastFundingTx`, so
we own rebroadcast. Publish through WalletKit (its wallet re-broadcasts on
startup until the tx confirms), and keep the raw tx in the journal.

### I-2 · The app holds the last signature

Signers return **partial** signatures; combining and finalizing happen in-app.
No external party may ever hold a broadcastable transaction, or it could publish
before the I-1 gate opens and defeat it from outside.

Free for *m*-of-*n*. Impossible for single-sig, which therefore requires a
genuinely air-gapped signer — enforce that in the UI, don't just document it.

### I-3 · The TXID must not move after verification

LND commits to the funding outpoint at `psbt_verify`: only signatures may be
added, never inputs or outputs. Hash the unsigned tx at verify and reject any
returned PSBT whose unsigned TXID differs, naming the offending device.

This is also why all inputs must be segwit — see `verifyAllInputsSegWit` in
`lnwallet/chanfunding/psbt_assembler.go` ("risk of malleability"). Filter legacy
UTXOs during coin selection and show which ones were excluded.

### I-4 · No RBF, ever

Replacing the funding tx changes the outpoints and destroys every channel in the
batch. Always include a change output we control, sized so a CPFP child stays
viable. Replaceability is disabled at construction and is not operator-adjustable.

---

## Rejected approaches — do not reintroduce

- **`skip_finalize`** on batch members. Skips the step that produces the
  `chan_pending` gate I-1 depends on. Non-negotiable.
- **`base_psbt` chaining.** An `lncli` ergonomic crutch; we build the tx ourselves.
- **Any broadcast path outside the gate.** `testmempoolaccept` validates without
  relaying and is the only pre-flight. There must be no other publish call site.
- **`AbandonChannel(i_know_what_i_am_doing)`** as the default. Use
  `pending_funding_shim_only`, which refuses unless the channel is both
  shim-funded and pending. Fall back to the blunt flag only on that specific
  rejection (it infers "shim funded" from `ThawHeight > 0` — see the `TODO` in
  `rpcserver.go`), and only with explicit confirmation.
- **Third-party APIs.** Fee rates come from Core's `estimatesmartfee`; peer facts
  from LND's local gossip graph via `GetNodeInfo`. Never a block explorer, fee
  API, or Lightning explorer — those log exactly the amounts, peers and timing
  we are trying not to leak. The rule is "no sockets of our own", not "no
  network": LND talking to peers is the point.
- **Hardcoding the macaroon permission list in docs.** Generate it from the
  method registry (`winthistle print-macaroon-command`) so it cannot drift.

---

## Build order — deliberately inverted

Abort and recovery paths ship **before** the happy path, because the
commissioning cold probe runs the real production flow and *terminates via the
abort path*.

1. `shim_cancel`, `AbandonChannel(pending_funding_shim_only)`, Core UTXO-lock
   release.
2. SQLite run journal: `pending_chan_id` ↔ funding outpoint ↔ signer state ↔
   finalized raw tx. Written *before* the publish call so a crash mid-publish
   leaves artifacts, not mystery.
3. Core descriptor plumbing (directed mode) and the batch-plan verifier
   (assisted mode).
4. The happy path on top.

---

## Development environments

- **regtest, via Polar** (Docker) — the inner loop. Mine on demand so
  confirmation depth, the 10-minute peer window, stuck transactions, CPFP and
  LND's ~2016-block forget horizon are all testable in seconds. Needs several
  LND nodes; Polar gives them in a few clicks.
- **Simulated multisig cold wallet** — two key-enabled Core wallets, xpubs
  assembled into `wsh(sortedmulti(2,…))`, imported watch-only, signed via
  `walletprocesspsbt` in each and `combinepsbt`. No hardware, fully scriptable,
  and it exercises exactly the partial-signature path I-2 depends on. This is
  what CI uses.
- **signet** — integration realism, and the only cheap way to test the
  descriptor-import **rescan** (regtest has no history; the rescan path needs an
  unpruned node, and signet's chain is small).
- **mainnet cold probe** — commissioning only, per `docs/design.html`. Proves
  this node, these peers, these devices. No signet/regtest substitute for it.

---

## Repo hygiene

This repo is **private and intended to go public** once the cold probe passes on
mainnet and the abort paths work. Assume every commit will eventually be public.

- **Never commit a mainnet xpub.** A single one deanonymises the whole cold
  wallet's history, permanently, and git history cannot be un-published. Use
  `tpub`/regtest keys in every fixture. `.gitignore` blocks descriptor and PSBT
  *files*, but an xpub pasted inline in a test will sail straight through it.
- No macaroons, certs, cookies, `winthistle.toml`, or `*.db` — all gitignored;
  keep it that way.
- Commit identity is `AusDavo <david@dpinkerton.com>`, not the client
  address.

## Style

Match the doc's register in user-facing copy: say what happened and what to do,
never euphemise a risk. The recovery screen's wording is the highest-stakes copy
in the product — write it before the happy path, not after.
