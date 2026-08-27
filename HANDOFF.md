# Winthistle — handoff

**This file holds two things: the runbook for a live mainnet batch, and the
hazards of this repository, this harness and Go/LND/Core as we use them.**
Everything else lives elsewhere and is not repeated here — `CLAUDE.md` has the
invariants, the rules and the current state; `docs/design.html` has the design;
git has the account of how any of it got this way.

Two live code gaps are recorded below because nothing else records them: they
are not defects with issues, they are shapes the build currently has.

---

## Running a live batch

The mainnet cold probe passed on 2026-08-26 and the first live batch published
the same evening, so this is no longer a description of something forthcoming.
It is what to know before doing it again. **A probe and a live batch are the
same run** — the probe is the real path with the final call withheld, and
composing it forced exactly one branch, an `if` before `arm.Publish`.

```
winthistle run --batch BATCH.toml --psbt FILE [--probe] [--stop-before-publish]
```

`--stop-before-publish` is the probe. `--probe` shim-probes every peer before
arming and then **waits out the ~11-minute hold it just created**, because
otherwise the probe collides with the open; it is worth adding the first time
you meet a peer and worth leaving off afterwards.

### The three costs, which are not the same size

The peer counts live reservations *plus* pending channels with no thaw height
against one `--maxpendingchannels` budget, so all three draw on the same number.

| What | Cost to the peer |
|---|---|
| A probe the peer **refuses** | Nothing at all. Every limit check runs before the peer creates a reservation, so you may probe downwards as often as you like |
| A **shim** probe the peer accepts | One slot for **~11 minutes**. `shim_cancel` deletes an entry in our own wallet and sends the peer nothing |
| A channel that reached `chan_pending` and was abandoned — **every channel in a probe** | One slot for **~2016 blocks**. `AbandonChannel` touches only our own database; the peer holds its side until `fundingTimeout` |

**LND's default `--maxpendingchannels` is 1**, it is not in gossip and cannot be
read, so assume 1 unless the peer has said otherwise. Against such a peer,
probing it and then opening a real channel with it are a fortnight apart.

### Before you start

- **Decide which peers you are willing to burn, and decide it first.** This is
  the only choice that cannot be taken back. Two defensible readings: probe with
  the peers you actually want, which proves *these* peers and costs you the
  wait; or probe with two you do not mind, which proves the code path against
  mainnet and leaves the real batch free to go the same day.
- **Know each peer's minimum, and find it out for free.** A size a peer rejects
  at step 2 is rejected on the clock. **The gossip graph is a poor proxy** — the
  smallest existing channel was wrong about the minimum for three of five peers
  in the first live batch, because a node's smallest channel may be one *it*
  opened outbound, which its own inbound minimum never constrained. Only
  `accept_channel` is authoritative, and it names the figure.
- **`arm.Open` is sequential and fails fast**, so ordering the peers most likely
  to refuse first makes a refusal free — nothing is opened behind it.
- **`winthistle doctor` clean**, against the mainnet node, with the baked
  macaroon rather than `admin.macaroon`. It checks the credential by asking
  `CheckMacaroonPermissions` rather than by calling anything, so a clean doctor
  is evidence and not a rehearsal.
- **`winthistle print-macaroon-command`**, and bake from *that* rather than from
  `docs/design.html`'s illustrative block, which has rotted before with the
  count matching while the membership did not.
- **There is no fee rate to supply.** Nothing estimates one, nothing asks for
  one, and `[fees]` is a retired section that the config reader refuses with a
  sentence. You choose the rate in Sparrow; step 5 computes what the transaction
  came out at and prints it, and grades nothing.

### What to watch

1. **Step 6 is the assertion.** *n* of *n* `chan_pending`, **with nothing
   signed**. That is the whole safety argument. Everything before it is
   reversible at no cost.
2. **The channel backups export while the channels are pending.** Step 6 does
   this, before publish and before signing, and it is the one thing the probe
   proves that the harness cannot fully vouch for.
3. **Sign for real at step 7.** The point is to exercise the devices and the
   descriptor, not to satisfy the tool. Then confirm the txid has not moved —
   I-3, and the only load-bearing check on what comes back.
4. **Expect the blunt abandon flag, once per channel.**
   `pending_funding_shim_only` is refused for every channel this app opens, so
   `i_know_what_i_am_doing` is the normal route rather than a warning sign: LND
   infers "shim funded" from `ThawHeight > 0` and a plain PSBT open sets none.
5. **`--stop-before-publish` exits non-zero if the teardown does not finish.** A
   probe that proved the sequence and then left channels pending is not a
   success, and it is usually run from a terminal somebody walks away from.
   **Check the exit code.**
6. **You cannot walk away from the teardown, but it will wait for you.** `--yes`
   skips the arming prompt and **not** the blunt-abandon confirmation, which
   `abort.AbandonPending` asks *per channel, at the moment of the rejection*.
   Sit with it, or pipe the answers in: `confirmBlunt`
   (`cmd/winthistle/main.go`) accepts a piped answer deliberately and refuses
   only end-of-input. **There is no clock on you while you decide** — the budget
   is per LND call (`abort.CallBudget`, 30s), and the call that runs *after* the
   confirmation takes neither the parent's deadline nor its cancellation.
7. **Settlement carries on per member.** One channel that will not take its
   policy is retried for `settle.RetryWindow` and then named at the end with its
   channel point; it never ends the loop for the rest. Check afterwards that
   every policy actually landed — `getchaninfo --chan_point TXID:N`, reading the
   side whose pubkey is ours.

### Reading the journal afterwards

**A probe that withheld the publish leaves no `raw_tx` on disk, and that is
correct.** The signed bytes reach the journal immediately before the publish RPC
and nowhere else, so `raw_tx` means "we may owe a rebroadcast" — and a probe owes
nothing. The pinned txid is there.

**Publish is step 8, not step 9.** It moved when the signing round moved, and
the withheld-publish screen says *"Step 8 was not made"*. A screen or a note
that calls it step 9 is pre-inversion copy.

**Funding addresses do not repeat between runs.** Every run derives fresh ones
on both sides, so there is no pre-staging a transaction against a previous run's
addresses.

### What would make it a failure

- A `chan_pending` that does not arrive and `PendingChannels` not explaining it.
  **Nothing bounds this wait** — see below — so a silent peer parks the run until
  `Ctrl-C` rather than timing out.
- A txid that moved between step 5 and step 7.
- A teardown that leaves a channel pending, i.e. a non-zero exit.
- Anything the verifier **refuses** at step 5. What it *reports* there is not a
  failure: a missing change output is the operator's business and the run
  continues past it deliberately.

---

## Two live code gaps, and neither is a safety failure

**Nothing bounds step 4 or step 7, and that is deliberate rather than
overlooked.** `run.FileWallet.wait` polls until the context ends, and the two
waits have different clocks above them that are not the transport's to enforce:
step 4 is inside the peers' ten minutes, where a deadline of ours would abort a
batch the peers were still holding, and step 7 has no deadline at all now that
the gate is open. What ends either is the operator, or the run's own context.
**If a countdown is ever added to the CLI, step 4 is the one it is for** — it is
the only step inside clock A that takes any time at all.

**Nothing bounds the `chan_pending` wait, and the fallback dies with the context
that ends it.** `arm.receiptFor` blocks in `Recv` with no deadline of its own,
and `winthistle run` builds its context from `signal.NotifyContext` and nothing
else — so a peer that accepts and never answers parks the armed window until
`Ctrl-C`, with the rest of the batch already armed behind it. Then, because the
context that ended the wait is the one `isPending` is asked on, the lookup that
decides abandon-versus-cancel fails too, and the operator is told "this
channel's state is unknown" about a channel LND could still have answered for.

Not a safety failure: the batch is unpublishable and abortable throughout, and
`TestAPeerThatNeverAnswersLeavesTheBatchUnarmed` pins both as *current*
behaviour rather than as correct behaviour. The `isPending` half is much smaller
than the deadline half and could be taken on its own — a `context.WithoutCancel`
plus a short timeout would let the one question that matters still be asked.
**This matters more after the inversion, not less**: step 6 is the gate and the
only thing left under clock A, and it is the step this gap sits inside.

**Its cousin.** A *crashed* process — one that died between a channel's
`psbt_verify` and its receipt — leaves a journal row saying `verified` for a
channel LND has probably already created, because `skip_finalize` makes the
verify complete the funding flow rather than park it. `Run.AbortTarget` then
lists it as a shim to cancel, and `abort.CancelShim` comes back `AlreadyGone` —
a *success* — for a channel still pending on the peer's side until clock B runs
out. Inside a run
this cannot happen: `arm.Receipts` asks `PendingChannels` whenever a receipt does
not arrive and journals what it finds. Closing it needs a lookup `AbortTarget`
cannot make — it has no client, and the journal cannot map a pending channel
point back to a pending channel id. The comment in `internal/journal/recover.go`
says so where somebody editing that switch would read it.

---

## Watch out for

### This repository

- **Republishing `docs/design.html` — the two things `CLAUDE.md`'s rule does not
  say.** The artifact URL is not in this repository: `/artifacts` in Claude Code
  lists it, or ask the owner. And the publish is refused from a session that has
  not read the live version, which is the safeguard against clobbering rather
  than an obstacle to route around.

- **`journal.Open` has no migrations.** One `CREATE TABLE IF NOT EXISTS` block,
  no version table. A new column would silently not reach a journal an earlier
  build wrote, so schema growth means **new tables**. A table whose package has
  since been deleted stays where it is, for the same reason.

- **The config reader refuses unknown keys, which makes it strict about its own
  history too.** `"[fees] has no key \"mode\""` reads like a typo rather than
  like the change it is, so a removed setting goes in `retiredKeys` or
  `retiredSections` in `internal/config/toml.go` with a sentence saying what
  happened to it. **Add the entry in the same commit as the removal**, or the
  refusal is the shrug.

- **The call-site check sees typed client calls, not `conn.Invoke`.**
  `TestEveryLNDCallSiteIsRegistered` resolves method calls through go/types, so
  it finds a call on `lnrpc.LightningClient` and a call on a narrow interface
  that one satisfies — but a raw `conn.Invoke(ctx, "/lnrpc.Lightning/…", …)`
  with the path as a string is invisible to it.
  `internal/methods/bake_regtest_test.go` does exactly that on purpose, to test
  LND's own enforcement rather than ours; it is the one place that should.

- **"directed mode" and "assisted mode" are pre-inversion vocabulary that
  survives in comments**, in `internal/plan`, `internal/settle` and the
  harness's `internal/regtestenv`. Neither mode exists. Fix them in place in
  whatever slice next touches those functions; they are comments, and hunting
  them down as a sweep is the process this repository retired.

- **When a report renders a verdict, check the zero value is not a verdict.** A
  `bool` that means "it failed" cannot also mean "we never tried", and a `switch`
  with no `default` arm renders an unknown value as "nothing happened". Both
  shipped here once, and both read as confident statements about something that
  had not been attempted.

### The harness

- **The peers do not forget an aborted batch until something mines.**
  `AbandonChannel` touches only our own database. If the harness starts refusing
  opens with *"Number of pending channels exceed maximum"*, that is what it is,
  and the cheap cure is `make -C regtest mine N=2016` — about ten seconds, and
  it times every stale pending channel out on every peer at once. `make harness`
  also works and is slower. Several packages' regtest tests add to the pressure
  from both sides: they open real channels and abort them, and they open shim
  streams and cancel every one. A cancelled shim costs *us* nothing, but the
  peer has already sent `accept_channel` and holds its reservation until its own
  timeout, so a tight run of `make test` can still crowd a peer for ten minutes.

  **On mainnet this is not an annoyance, it is a fortnight**, and there is no
  `mine N=2016` to reach for. It is the same fact that makes a probe cost one
  pending-channel slot per peer.

- **A fresh cluster gives alice zero channels**, which is the only state in which
  the anchor reserve is under LND's 100,000-sat cap and its arithmetic is
  observable at all. Rebuild deliberately when a measurement needs that.

- **`internal/arm`'s publish test leaves open channels and spends cold coins.**
  It is the only test that publishes a funding transaction, and it has to,
  because "the transaction reaches the network on exactly one line" is not a
  claim a dry run can make. Nothing can close what it opened — `CloseChannel` is
  on the never-list — so `make harness` is the reset. **"Insufficient funds" from
  the cold wallet is usually this**, and occasionally a leaked coin lock.

- **The harness owns its coin locks, and a leaked one is invisible.**
  `walletcreatefundedpsbt` is called with `lockUnspents` and the application
  takes no locks, so `Env.BuildPSBTPaying` registers the release itself and a
  test calling `coldwallet.Build` directly has to call
  `Env.ReleaseLocksAtCleanup`. The symptom names nothing:
  `walletcreatefundedpsbt: Insufficient funds` from a wallet whose
  `getbalances` is fine and whose `listunspent` is empty. Check
  `./bin/bcli -rpcwallet=cold-watch listlockunspent`; `lockunspent true` frees
  the lot. **A clean `make check` ends with zero locked outputs.**

- **`internal/reserve`'s tests lease every coin alice has**, on purpose, and give
  them back in `t.Cleanup`. If a later test fails with LND's reserved-value
  error and nothing explains it, check for a leaked lease: `lncli --network
  regtest wallet listleases`, and release with `wallet releaseoutput`.

- **A Core container restart unloads the harness wallets and the tests reload
  nothing.** Symptom: `Requested wallet does not exist or is not loaded`, from
  `getwalletinfo`. `./bin/bcli listwallets` will show only the
  `winthistle-*-test` wallets earlier runs created — `miner`, `cold-watch`,
  `cold1` and `cold2` are gone. `./bin/bcli loadwallet NAME` for each of the four
  is the quick fix; `make -C regtest bootstrap` is the blunt one.

- **Mining and then acting needs a wait.** LND refuses to open a channel while
  its wallet is behind the chain — *"channels cannot be created before the wallet
  is fully synced"* — and one block is enough to trigger it. `regtestenv.Mine`
  blocks until alice has caught up, which is why it takes a `*testing.T` and can
  fail.

- **LND calls itself unsynced when regtest's tip is more than two hours old, and
  a long test can cross that line mid-run.** btcwallet's `IsSynced`: *"if the
  timestamp on the best header is more than 2 hours in the past, then we're not
  yet synced"* — `isCurrentDelta` in btcwallet's `chain/interface.go`, still
  `2 * time.Hour` under lnd v0.21.2-beta. `OpenChannel` then refuses with a
  message that reads like a node problem and is a *clock* problem: nothing has
  been mined. `env.Mine(t, 1)` at the start buys two hours of headroom and
  confirms nothing but itself. Any test that spans minutes on an idle cluster
  wants it.

- **Waiting out the peers' window on a batch gives you no receipts, not one.**
  Every peer's clock starts at its own `accept_channel`, so a batch opened
  together lapses together. What produces one survivor is a **stagger** —
  `TestOnePeersWindowExpiringLeavesTheRestOfTheBatchArmed` opens the doomed
  stream six minutes before the other, verifies both while both are alive
  (`psbt_verify` is local and touches no timer), watches the first peer give up
  rather than sleeping past it, and finalizes afterwards. `WINTHISTLE_SLOW=1`,
  ~12 minutes, and it holds alice's streams and the cold wallet's coin locks for
  all of it — so do not run anything else harness-backed alongside it.

- **`ExportAllChannelBackups` is node-wide.** The snapshot covers every channel
  the node has, not the batch's. A test may assert "at least the batch" and
  nothing tighter.

### LND

- **A stream must be read promptly, or the funding manager waits — and this
  sequence spends the buffer exactly.** `resCtx.updates` has a buffer of 2
  (`server.go:5309`) and the funding manager blocks on `f.quit` when it is full.
  One slot goes to `psbt_fund`, read in `arm.Open`; `arm.Verify` then verifies
  all *n* streams and `arm.Receipts` reads the *n* receipts afterwards, so at the
  worst moment every stream holds one unread `chan_pending` in its one free slot.
  That fits, with nothing spare. It is safe because this flow produces exactly
  two updates per stream before confirmation and there is no third emitter —
  `funding/manager.go:2241`, `:2908` and `:4483` are every send site at
  v0.21.2-beta. **Add a third update per stream, or stop reading promptly, and
  the funding manager blocks; the failure reads as a dead peer.**
  `arm.Receipts`' doc comment carries this where it would be read.

- **`UpdateChannelPolicy` reports failure inside a success.** Nil error,
  `failed_updates` populated. Reading only `err` records a policy that was never
  applied. `UPDATE_FAILURE_UNKNOWN` is LND's catch-all and is transient, not a
  verdict. `settle.ApplyPolicy` is the only place this build calls it.

- **`minimum_depth` is readable, in one window.** It is in `accept_channel` and
  in `OpenChannel.NumConfsRequired`, and `PendingChannels` reports it back as
  `confirmations_until_active` for as long as the funding transaction is
  unconfirmed. `settle.State.PeerDepth` takes that reading once; `ExpectedDepth`
  is the prediction that stands in when the window was missed, and
  `ObservedDepth` is the harness's check from above. **Once the transaction
  confirms the same field is a countdown**, so a reading taken late is smaller
  than the peer's number and looks exactly like a peer that asked for less.

- **A macaroon refusal has two shapes and they mean opposite things.**
  `codes.InvalidArgument` from `CheckMacaroonPermissions` is the answer *about
  the macaroon in the request*; an **untyped** error carrying "permission denied"
  is the interceptor refusing *the caller* — `bakery.ErrPermissionDenied`, a
  plain `errgo` error with no gRPC status attached, so it arrives as
  `codes.Unknown`. Match both, as `doctor.refusedOverMacaroon` does; the code
  alone confuses them and so does the text alone.

- **`chan_pending` txids are chainhash bytes**, i.e. reversed relative to every
  txid a human or Core sees. `lnd.ChannelPointFromPending` handles it;
  hex-encoding those bytes directly yields a plausible txid that matches nothing.

- **`lnd@latest` resolves to `v0.0.2`**, a retracted tag — lnd's real versions
  are pre-releases. Pin the exact tag, matching `regtest/.env`.

- **lnd needs a forked protobuf, and the restatement does not follow the bump.**
  `lnrpc` uses `protojson`'s `UseHexForBytes`, which only exists in Lightning
  Labs' fork. A dependency's `replace` does not apply transitively, so `go.mod`
  restates it — and because it is a restatement, `go get` on lnd leaves it where
  it was. It sat at `v1.30.0-hex-display` against an lnd replacing at
  `v1.33.0-hex-display`. **Read lnd's own `go.mod` on every version bump** and
  match the line. Do not remove it.

- **grpc rides lnd's pin.** lnd v0.21.2-beta pins v1.79, where `DialContext` is
  deprecated and `NewClient` is the call. Nothing was lost in the move:
  `DialContext` without `WithBlock` never dialled either, so its 15-second
  context bounded nothing, and the `GetInfo` probe was always the thing that
  proved the connection.

- **btcd is a direct dependency, at lnd's own pins.** `internal/plan` parses
  PSBTs and classifies scripts with `btcutil/psbt` and `txscript`, and it must be
  the same code LND runs or the verifier's answer stops predicting
  `psbt_verify`'s. Do not bump them independently of lnd.

- **A 2-of-3 with three signatures does not finalize.** btcd's
  `checkIsMultiSigScript` requires the number of partial signatures to equal the
  number the script demands, so asking one more device to sign "just in case"
  breaks the packet. `combine.checkSignatureCounts` **skips an input that already
  carries a complete witness** — the batch's own path, where the signatures were
  counted by whatever finalized them and `executeWitnesses` is what checks the
  result. A wallet that over-signs and then finalizes is that wallet's problem;
  one that over-signs and hands back partials gets the counts and the labels.

- **`combine.Merge` refuses a finalized input and `combine.Accept` expects one,
  and the two are right for opposite reasons.** This used to be one rule and it
  used to be I-2. What survives is mechanical: a merge *unions* partial
  signatures and finalization discards them, so a packet that finalizes on its
  own leaves the rest of the round nothing to add to. `Accept` is the batch's
  path, where a complete witness is the expected input. **No production caller
  reaches `Merge`** — the harness's `Env.SignLikeSparrow` is what exercises it.
  **If you find yourself re-justifying either in I-2's words, stop**:
  `CLAUDE.md`'s I-2 section is the record of why that reasoning is gone.

### Bitcoin Core, as the harness uses it

The application dials no Bitcoin node. `internal/bitcoind` and
`internal/regtestenv/coldwallet` exist for the harness only, and everything here
is about that.

- **`getaddressinfo`'s `parent_desc` can name the wrong descriptor.** A wallet
  can hold two descriptors that derive the same address — the state a corrected
  setup leaves behind — and Core credits the address to the key manager created
  first, active or not. Measured: three of five addresses of a *correct* pair
  attributed to a rejected one. The harness's cold wallet does exactly this at
  `make harness`, so a bootstrap that looks wrong may be this rather than a
  broken descriptor.

- **Core deactivates the descriptor it replaces and keeps its coins.** One active
  external and one active internal per wallet, no RPC to remove either, and
  `listunspent` still lists a deactivated descriptor's coins. So a corrected
  setup is a new wallet name, not a second import.

- **Locking is the only way to exclude a coin.** There is no "do not spend these"
  option on `walletcreatefundedpsbt`, and Core does skip locked outputs. So
  `coldwallet.FenceOff` is `lockunspent`, and it inherits everything the coin-lock
  bullet above says.

- **Core hides locked outputs from `listunspent`**, so a coin something is
  holding looks exactly like one that has already been spent — and the two call
  for opposite actions.

- **A single send to a legacy address poisons a wallet for batching.** Core
  derives change of the same type as the payment, so paying one P2PKH address
  leaves a P2PKH change output behind — and the next batch built from that wallet
  fails at `psbt_verify` with *"not all inputs are SegWit spends"*, naming an
  input that has nothing to do with whatever produced it. A fixture that needed a
  legacy coin did exactly this to the miner wallet and broke every abort test.
  Two things stop it now: that fixture keeps its legacy coins inside its own
  wallet, and `regtestenv.BuildFundingPSBT` fences off the funding wallet's
  non-SegWit coins with `coldwallet.FenceOff`. **Worth knowing past the harness**
  — a real cold wallet with any legacy history is in the same position, and
  nothing in the application pre-excludes a legacy coin: Sparrow picks the coins,
  and `plan.Verify`'s `LegacyInput` refusal at step 5 is the only thing that
  notices. `coldwallet.Coins.Excluded` is the harness's own report of what it
  fenced off.

- **`gettransaction` on a client with no wallet scope returns -19, not -5.**
  *"Multiple wallets are loaded. Please select which wallet to use…"*.
  `bitcoind.Client.Confirmations` falls back to `getrawtransaction` on both, plus
  on a node built without wallet support — otherwise a node-level client can
  never report a confirmation count.
