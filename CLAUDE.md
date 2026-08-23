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

### I-4 · No RBF on the funding transaction, ever

Replacing the funding tx changes the outpoints and destroys every channel in the
batch. Always include a change output we control, sized so a CPFP child stays
viable. **The funding transaction's** replaceability is disabled at construction
(`coldwallet.Build` passes `replaceable: false`), is not operator-adjustable, and
`internal/plan` refuses any funding input below `MaxNonReplaceableSequence`.
There is no code path in this repository that replaces a funding transaction.

The heading used to read "No RBF, ever" and the last sentence used to say
"replaceability is disabled at construction" with no subject. That was true when
this build made one transaction. It now makes two, so the subject matters:

- **The funding transaction: never.** *n* peers hold commitment signatures
  against its outpoints. Everything above applies to it, unchanged.
- **The CPFP child: always.** `settle.buildChildAt` sets
  `plan.MaxBIP125Sequence` and `replaceable: true`, and `internal/bump`'s
  verifier *requires* it. Nobody has committed to anything about a child — it
  spends the batch's change and pays cold storage back — so replacing one moves
  nothing anyone depends on, and only cold storage can sign the replacement.
  What it buys is the second lift: a batch needing acceleration twice gets an
  ordinary RBF of the child instead of a grandchild paying for a longer chain.

This is a clarification of I-4's scope, not a relaxation of it. If a change ever
makes a *funding* transaction replaceable, that is the invariant breaking and the
answer is to stop, not to edit this section. Two verifiers is what lets both
rules be stated at once; one verifier with a flag on it would be a switch on the
invariant.

---

## Rejected approaches — do not reintroduce

- **`skip_finalize`** on batch members. Skips the step that produces the
  `chan_pending` gate I-1 depends on. Non-negotiable.
- **`base_psbt` chaining.** An `lncli` ergonomic crutch; we build the tx ourselves.
- **Any broadcast path that could carry a funding transaction outside the gate.**
  `testmempoolaccept` validates without relaying and is the only pre-flight.

  The rule used to read "there must be no other publish call site", and it was
  enforced by nothing — the claim lived in five prose comments while
  `internal/methods`' call-site test grouped by method and checked
  registered-versus-called in both directions without ever counting. There are
  now **exactly two** calls to `WalletKit.PublishTransaction`, and the count is
  enforced: `Method.CallSites` pins it at 2 and
  `TestEveryLNDCallSiteIsRegistered` fails on a third.

  1. `arm.Publish` — the funding transaction, behind the I-1 gate.
  2. `bump.Publish` — the CPFP child of a batch that is already public. It has
     no gate to sit behind and needs none: by the time a child can be built the
     parent is in a mempool, every channel reached `chan_pending` before that,
     and the child spends the batch's change, which no channel depends on.
     There is no "early" for it to be published in.

  What keeps them apart is the type system, not a convention. `arm.Publish`
  takes an `*arm.Armed` and `bump.Publish` takes a `*bump.Signed`; each has its
  raw transaction in an unexported field that exactly one constructor fills,
  after that constructor's own checks. Neither line can be handed the other's
  bytes.

  Changing the count is a deliberate change to what this build promises about
  reaching the network. Change `CallSites`, this bullet, and `docs/design.html`
  in the same commit, or do not change it.

- **Bumping the funding transaction, by any route.** I-4. `winthistle bump`
  builds a child; nothing in this repository replaces a parent.
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
