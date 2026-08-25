# Winthistle — handoff

> ## ⚠ Direction changed on 2026-08-25. `docs/replan-2026-08.md` is the plan.
>
> **This file used to be ~3,960 lines of design record for a design that is
> being replaced.** Most of it has been deleted, deliberately, in the docs
> rewrite (replan build-order item 2). Git has it if you need it —
> `git log --follow HANDOFF.md`. What is kept below is the part that was never
> about the old design: hazards of this repository, this harness and this
> machine, which are as true after the replan as before.
>
> **The plan is now:** a CLI wizard around Sparrow and LND doing the two things
> those two cannot do between them — attribute the funding outputs to peers, and
> hold the I-1 gate. `CLAUDE.md` carries the invariants and the
> do-not-reintroduce list. `docs/replan-2026-08.md` carries the sequence, the
> two clocks, and the build order.

## Where the build actually is

**Item 1 is done and item 2 is this rewrite.** The inversion is proved on a
running node: `TestSkipFinalizeReachesChanPendingWithNothingSigned` in
`internal/arm/skip_finalize_regtest_test.go` took *n* = 2 channels to
`chan_pending` at the outpoints of an **unsigned** transaction, mempool clear,
in under a second. `skip_finalize` does not skip the gate. The rejected-list
entry that said it did was wrong in the source and wrong on the node, and it is
gone from `CLAUDE.md`.

**No code has moved yet.** Everything the replan cuts is still present and still
works: `setup`, `bump`, `serve`, `coldwallet`, `rehearsal`, `server`, `webrun`,
`signet/`, `internal/bitcoind`. Items 3 to 6 are what move them. Do not read the
docs' description of the new sequence as a description of the tree.

**Three collisions found while rewriting the docs**, none of them fixed here
because item 2 changes no code:

1. **`combine.ErrAlreadyFinalized` refuses the new happy path's own input.**
   `internal/combine` survives the replan, and `combine.go:94` refuses a device
   that returns a *finalized* input — with I-2 named in the comment and the
   error text reading "Only partial signatures may leave a signer". Step 7 is
   "sign in Sparrow", which returns exactly that. Settle it explicitly in item
   4; do not delete the check silently. The base-packet guard at
   `combine.go:249` is a different check and should stay.

2. **The receipt buffer fits the new sequence with zero headroom.**
   `req.Updates` is `make(chan *lnrpc.OpenStatusUpdate, 2)`
   (`lnd/server.go:5190`), and the funding manager blocks when it is full. Today
   `arm.Finalize` reads each receipt as it goes, so it never gets close. The new
   steps 5→6 verify all *n* and then collect *n* receipts — `psbt_fund` plus
   `chan_pending` is exactly 2 per stream. It fits, and nothing spare. Worth
   knowing before item 3.

3. **The custody-language change-output copy is still shipping.** `CLAUDE.md`
   explains at length why framing a stuck batch as a custody risk is dangerous,
   in the past tense, while three strings still say it — two of them
   operator-facing: `internal/plan/plan.go:395`, `internal/plan/report.go:101`,
   and the comment at `internal/plan/size.go:246`. Item 6's business, alongside
   demoting the finding.

**Four claims that were wrong today**, found by auditing rather than by working:
`handleFundingSigned` does not exist in LND v0.19.3-beta (it is
`funderProcessFundingSigned`, `funding/manager.go:2694`) and was cited in three
documents; there is no CI in this repository, though `CLAUDE.md` and `README.md`
both said there was; the harness is a hand-written compose file that borrows
Polar's images rather than Polar itself; and `docs/design.html`'s macaroon block
named two methods the build never calls while omitting two it does, with both
lists 17 long so the count matched. Check before writing "we do", not only
before writing "we cannot".

## The mainnet cold probe

**The one thing no harness substitutes for, and the replan does not change it.**
`winthistle run --stop-before-publish`: the whole production sequence with the
one call that broadcasts withheld, against real peers, with coins that never
move. Everything it needs exists — the steps up to publish are the production
code path, publish is one call inside one `if` that it does not make, and the
abort path it terminates through runs on every failure and is tested on both.

Composing it forced **exactly one branch**, an `if` between `arm.Finalize` and
`arm.Publish`, and that survived: the probe is the real run with the final call
withheld rather than a second path to the same place.

Two things the replan changes about it. The probe now terminates by abandoning
channels that reached `chan_pending` **with nothing signed** — and regtest
confirmed LND still refuses `pending_funding_shim_only` on those, so it still
costs an `i_know_what_i_am_doing` confirmation per channel. And the step
numbering moves: publish is step 8 in the replan's sequence, not step 9.

`--stop-before-publish` exits non-zero if the teardown does not finish. The probe
proving the sequence and then leaving channels pending is not a success, and a
probe is usually run from a terminal somebody walks away from.

## One live code gap

**Nothing bounds the `chan_pending` wait, and the fallback dies with the context
that ends it.** `arm.finalizeOne` blocks in `Recv` with no deadline of its own,
and `winthistle run` builds its context from `signal.NotifyContext` and nothing
else — so a peer that accepts and never answers parks the armed window until
`Ctrl-C`, with the rest of the batch already armed behind it. Then, because the
context that ended the wait is the one `isPending` is asked on, the lookup that
decides abandon-versus-cancel fails too, and the operator is told "this channel's
state is unknown" about a channel LND could still have answered for.

Not a safety failure: the batch is unpublishable and abortable throughout, and
`TestAPeerThatNeverAnswersFinalizeLeavesTheBatchUnarmed` pins both as current
behaviour rather than as correct behaviour. The `isPending` half is much smaller
than the deadline half and could be taken on its own — a `context.WithoutCancel`
plus a short timeout would let the one question that matters still be asked.

**This matters more after the replan, not less.** Step 6 is now the gate and the
only thing left under clock A.

## Docker on this machine

The login session predates the docker group, so `docker` in a fresh shell gets
`permission denied ... /var/run/docker.sock`. Group membership is fixed at
process start, so wrap it: `sg docker -c "make -C regtest reset"`. A re-login
fixes it permanently. On a new machine: `sudo addgroup --system docker; sudo
adduser $USER docker; sudo snap disable docker && sudo snap enable docker`.

## Watch out for

**Every bullet below is true of the repository today** — all of them were
re-checked against `internal/`, `cmd/`, `go.mod`, `.gitignore`, `regtest/` and
`lnd@v0.19.3-beta` during the docs rewrite. Roughly half are about packages the
replan deletes in item 5 (`server`, `webrun`, `coldwallet`, `setup`, `bump`,
`rehearsal`, `signet/`, Core in the application); those stop mattering when the
package does, and not before. The rest are about Go, LND, Core, the harness and
this repository's own habits, and they outlive the replan entirely.

Three are worth reading again in the new light rather than skipped as moot:
the legacy-address bullet (after the replan **nothing pre-excludes a legacy
coin** — Sparrow picks them, and `plan.Verify`'s `LegacyInput` refusal becomes
the only thing that notices); the stream-buffer bullet (see "Where the build
actually is"); and the 2-of-3-with-three-signatures bullet, because Sparrow can
over-sign and `internal/combine` survives.


- **`docs/design.html` is published as an artifact, and editing the file does not
  update the published page.** The two drifted five statements apart before
  anyone checked, across several slices, and the published page is the only copy
  an outside reader sees — the reviewer who wrote `docs/review-2026-08.md` worked
  from public documents alone. Five statements it was still making: that
  `replaceable: false` protects against RBF and should be surfaced as a UI
  toggle; that assisted single-sig is gated before the mode is selectable (that
  gate was never built — see the residual table); that a download hands the PSBT
  to Sparrow, as the only path; that animated QR was coming; and that a camera is
  a prerequisite. It was also missing "A batch that never confirms" and the
  seven-state recovery table entirely, which are the two sections an operator
  would most need under pressure.

  **So: any slice that changes `docs/design.html` republishes it in the same
  slice.** The artifact URL is not in this repository — `/artifacts` in Claude
  Code lists it, or ask the owner. Two things about doing it: the tool refuses a
  publish from a session that has not read the live version, which is the
  safeguard against clobbering and not an obstacle to route around; and *diff*
  the live source against the local file rather than assuming the local one is
  ahead. That diff is what found the five.

- **`Ctrl-C` on `winthistle serve` now cancels a run in flight, and it used to
  say it did not.** That row of decision 2's table changed in the seams slice and
  the reasoning is with it: a run left alive while the process exits is killed
  between two RPCs with *n* shims open. A tab closing is still nothing at all.
  Whatever else changes, do not let a *transport* event end a run.

- **A browser's same-origin form POST carries `Origin: null`, and a test may not
  invent that header.** Measured on Chrome 152, both referrer policies, real click
  and scripted submit. It is the opaque-origin serialisation and it means nothing
  in either direction, so the guard treats it as absent and leans on
  `Sec-Fetch-Site: same-origin` — which page JavaScript cannot set. Every test in
  `internal/server` had chosen a real `Origin`, which is why a `403` on every form
  in the UI shipped green. The helpers now send the measured headers and say why.

- **A device's transport must not be recomputed between the rounds.** The
  rehearsal's number predicts the armed window only because the two rounds go
  through the same transport, so `webrun.newSigners` resolves the choice once, off
  the configuration, and `Signers.Round` indexes it. If a later change makes that
  choice depend on anything that can move — a flag on the form, a probe of whether
  the command still exists, a fallback when it fails — that is the invariant
  breaking rather than a nicety, and the measurement quietly stops meaning
  anything. `TestTheTransportIsPerDeviceAndTheSameInBothRounds` is the guard.

- **A browser is installed on this machine.** Two handoffs said there was not,
  and that claim was never checked; `which chromium google-chrome firefox` finds
  three. Render before believing a page works. Three rendering sessions have now
  produced twelve defects between them, most of which no test would have caught,
  and four of which were false statements in operator copy.

- **`estimatesmartfee` can start answering on regtest, and one fees test has no
  guard for it.** `TestOnRegtestWithNoFloorThereIsNoRate` asserts there is no rate
  when nothing is configured, which holds only while Core has no estimate. Mine
  enough blocks carrying fee-paying transactions — a browser render followed by
  `make -C regtest mine N=2016` did it — and Core produces one for a while (1.11
  sat/vB, observed once) before its estimator decays back to "Insufficient data".
  The test then fails with "a rate of 1.11 sat/vB was produced with nothing behind
  it", which reads exactly like the refusal having been lost. It is harness state:
  re-run it. Note that its sibling above it,
  `TestRegtestHasNoFeeEstimateAndTheFloorCarriesIt`, has the `t.Skipf` guard for
  precisely this state and says so in its message; this one does not, and giving it
  the same guard is the fix if it recurs.

- **`go test ./...` has a second way to fail now, and it is not regtest.**
  `internal/webrun`'s `TestTheRoundsDeadlineIsSharedByEveryDevice` races two
  goroutines against a two-second poll, and under twenty package binaries
  competing for the CPU it lost once and reported "the seam never asked
  anything". It passes standalone and under `make check`. Same rule, second
  reason: `-p 1`, always.

- **`git checkout <file>` discards unstaged work, and this repository is worked
  on with everything unstaged.** One `git checkout internal/prose/recovery.go`,
  used to undo a two-line experiment, threw away six reapplied edits and cost
  fifteen minutes. Copy the file to the scratchpad and copy it back.

- **A `bool` that means "it failed" cannot also mean "we never tried".**
  `rehearsal.Measurement.Accepted` was both, and the report read the second as the
  first: a rehearsal that died in the finalizer announced that
  `testmempoolaccept` had refused it. `Asked` separates them now. When a report
  renders a verdict, check that the zero value is not a verdict.

- **A Core container restart unloads the harness wallets and the tests reload
  nothing.** Symptom: `Requested wallet does not exist or is not loaded`, from
  `getwalletinfo`. `./bin/bcli listwallets` will show only the
  `winthistle-*-test` wallets that earlier test runs created — `miner`,
  `cold-watch`, `cold1` and `cold2` are gone. `./bin/bcli loadwallet NAME` for
  each of the four is the quick fix; `make -C regtest bootstrap` is the blunt
  one. Distinct from the generation mismatch below, which bootstrap cannot fix.

- **The seams' bound is the 5:00 gate and never the peers' ten minutes.** The
  gate is ours and enforceable; the peers' clock is theirs, because
  `pruneZombieReservations` skips PSBT reservations. `config.Validate` already
  refuses a gate greater than or equal to `rehearsal.PeerWindow`, which is what
  makes a gate-derived deadline inside the peers' window by construction. If a
  seam ever needs a longer wait, the answer is not to reach for `PeerWindow` —
  `internal/webrun` is tested for not reading it.

- **The startup-token cookie is not port-scoped, and cookies never are.** Any
  other server on 127.0.0.1 that the operator's browser visits is sent this
  cookie, so a second local service can read the token out of its own request
  logs. That is not a regression on the threat model — `docs/design.html` is
  explicit that a localhost bind is not an authentication boundary and any
  process on the machine can reach the socket anyway — but do not describe the
  token as protecting against local software. It protects against a *web page*:
  DNS rebinding, a cross-origin form post, a stray `fetch`. `internal/server`'s
  package comment says so in those words; keep it saying so.

- **Not every question a batch asks is a signing round, and the clock sentence
  used to assume it was.** `waitingOn` called the blunt-abandon confirmation's
  deadline "the 5m0s signing gate" and said letting it pass cost "one more signing
  round" — during a teardown, where the round is over and what it actually costs is
  an abort finished by hand. It branches on `Question.Reply` now: non-empty means a
  packet is being asked for. If a fourth kind of question is ever added to a batch,
  that discriminator is what has to be revisited, not the copy.

- **A report written for a run says things that are false on a screen of its
  own.** `fees.Rate.Report()` closes on "there is no RBF on this transaction
  (I-4)", which is exact in Phase 0 and names nothing at `/fees` with nothing
  going. The cure is a note from the screen saying which transaction it would be,
  never an edit to the report: the report is right where it is called from, and
  editing it would break the caller that matters. Same shape as the pane rule
  below — what the page owes a report is framing, not rewriting.

- **`winthistle serve --connect` makes the doctor screen dial peers, and the
  report screens still do not.** `doctor.Options.Connect` is the server's, so a
  server started with that flag has one screen that connects and one that never
  will. Copy on `/peers` says exactly that, and the earlier draft — which put the
  connecting version in a terminal — was false about the tab next to it.

- **A served report is regularly wider than the pane, and the page must not fix
  it.** `internal/doctor`'s own pane test uses a synthetic report, and a real one
  against a real node contains tokens `prose.Wrap` cannot break: an absolute path
  to `winthistle.toml`, a 66-character pubkey, a descriptor with its checksum.
  A width assertion at the HTTP layer therefore measures `$TMPDIR`'s length —
  `server_regtest_test.go` had one for exactly one commit and it passed only
  because `t.TempDir()` is short. What the page owes the report is not to reflow
  it (`TestTheTextIsTheOracle`) and to soft-wrap what it cannot shorten
  (`TestALongLineSoftWraps`: `white-space: pre-wrap` and a `max-width` in the
  same column the text was written to). Fixing an overrun by rewrapping in HTML
  would break decision 3's oracle, which is the point of having one.

- **`getaddressinfo`'s `parent_desc` can name the wrong descriptor, and it is not
  a gate.** A Core wallet can hold two descriptors that derive the same address —
  the state a corrected setup leaves behind — and Core credits the address to the
  key manager created first, active or not. Measured: three of five addresses of a
  *correct* pair attributed to a rejected one. `AddressCheck.Recognised()` is what
  the setup question is gated on; `Attributed()` is reported. If you find yourself
  gating on `Consistent()` again, that is the bug — see "`winthistle setup`".

- **Core deactivates the descriptor it replaces and keeps its coins.** One active
  external and one active internal per wallet, no RPC to remove either, and
  `listunspent` still lists a deactivated descriptor's coins. So a corrected setup
  is a new wallet name, not a second import.

- **The CPFP child is replaceable and the funding transaction never is.** Two
  verifiers enforce opposite rules on purpose: `internal/plan` refuses a funding
  input below `MaxNonReplaceableSequence`, `internal/bump` requires exactly
  `MaxBIP125Sequence`. If you find yourself wanting one verifier with a flag,
  that is the invariant asking to be switched off — see "The second lift".

- **The child's sequence check is equality, not "anything replaceable".** The
  child is version 2, so a sequence with bit 31 clear is a BIP-68 relative
  timelock rather than an RBF signal, and a timelocked CPFP child cannot enter a
  mempool at all.

- **A second lift has to beat an absolute fee.** BIP-125 rule 3 compares totals,
  not rates, and Core says only `insufficient fee`. `bump.checkReplacement`
  refuses before a device is asked and names the target that would work.

- **`listunspent` cannot see a change output a mempool transaction has spent**,
  which is the state every second lift starts in. The change is reconstructed
  from the parent's outputs plus `getaddressinfo` — whose `hex` field is the
  witness script behind a P2WSH address, and whose `ismine` is true on a
  watch-only descriptor wallet while `iswatchonly` is false.

- **A CPFP child has a fee-rate ceiling of 10,000 sat/vB and this build cannot
  raise it.** btcwallet calls `SendRawTransaction(tx, false)` and rpcclient turns
  that into `maxfeerate: 0.1` BTC/kvB; no WalletKit parameter reaches it. Because
  a child concentrates the package's lift into ~150 vB, its own rate is roughly
  fifty times the package target on a three-channel batch — so a ~210 sat/vB
  package target is the practical limit, and lower on a bigger batch.
  `bump.Verify` refuses above it before any device is asked, and names
  `sendrawtransaction <hex> 0` as the way out. Full detail under
  "`winthistle bump`".

- **`bitcoind.Client.TestMempoolAccept` passes no `maxfeerate`**, so our own
  pre-flight enforces that same ceiling. Do not "fix" that by passing zero: the
  pre-flight should apply the ceiling the broadcast will, and the earlier,
  better-worded refusal is what an operator should hit first.

- **Core hides locked outputs from `listunspent`**, so a change output an
  unfinished bump is holding looks exactly like one that has already been spent —
  and the two call for opposite actions. `bump.Locate` consults the journal to
  tell them apart and names the holding bump. Nothing else can.

- **`getmempoolentry`'s ancestor figures include the transaction itself.** For a
  batch funded from confirmed coins they equal its own, which is the ordinary
  case and why the difference is easy to miss. `walletcreatefundedpsbt` charges
  for the whole ancestor package, so the arithmetic has to use the ancestor
  figures when `ancestorcount > 1`.

- **`journal.Open` has no migrations.** One `CREATE TABLE IF NOT EXISTS` block, no
  version table. A new column would silently not reach a journal an earlier build
  wrote, so schema growth means new tables — which is why a bump has three of its
  own rather than columns on `signers` and `locks`.

- **The peers do not forget an aborted batch, until something mines.** See
  finding 6: `AbandonChannel` touches only our own database. If the harness
  starts refusing opens with *"Number of pending channels exceed maximum"*, that
  is what it is — and the cheap cure is `make -C regtest mine N=2016`, about ten
  seconds, which times every stale pending channel out on every peer at once.
  `make harness` also works and is slower. `internal/plan`'s and
  `internal/peers`' regtest tests add to the pressure from the other side: they
  open shim streams and cancel every one without finalizing. A cancelled shim
  costs *us* nothing, but the peer has already sent `accept_channel` and holds
  its reservation until its own timeout, so a tight run of `make test` can still
  crowd a peer for ten minutes.
- **A shim probe that succeeds is not free.** The peer holds a reservation for
  about eleven minutes and `shim_cancel` does not tell it otherwise. Probing all
  *n* peers and then arming collides with itself against any peer running LND's
  default `--maxpendingchannels=1`. `peers.ReadyToArm` is the gate; the long
  version is in "Phase 0" above. A probe that is *refused* costs nothing at all,
  because every limit check runs before the peer creates a reservation.

- **`UpdateChannelPolicy` reports failure inside a success.** Nil error,
  `failed_updates` populated. Reading only `err` records a policy that was never
  applied. `settle.ApplyPolicy` is the only place this build calls it.

- **`minimum_depth` is not readable.** It is in `accept_channel` and in
  `OpenChannel.NumConfsRequired`, and in no RPC. `settle.ExpectedDepth` predicts
  what a stock LND peer will do and `State.ObservedDepth` records what it
  actually did, which is only knowable after the fact. Do not add a UI that
  promises "usable after k confirmations" up front — the design asks for it and
  it cannot be delivered.

- **`estimatesmartfee` succeeds when it has nothing to say.** No `feerate` field,
  an `errors` array, and a 200. Every regtest node is in that state permanently.
  `-fallbackfee` does not help: it is a wallet setting and `estimatesmartfee`
  never consults it.

- **`gettransaction` on a client with no wallet scope returns -19, not -5.**
  "Multiple wallets are loaded. Please select which wallet to use...".
  `bitcoind.Client.Confirmations` falls back to `getrawtransaction` on both, plus
  on a node built without wallet support — otherwise a node-level client can
  never report a confirmation count.

- **Mining and then acting needs a wait.** LND refuses to open a channel while
  its wallet is behind the chain — *"channels cannot be created before the wallet
  is fully synced"* — and one block is enough to trigger it. `regtestenv.Mine`
  now blocks until alice has caught up, which is why it takes a `*testing.T` and
  can fail.

- **Core does the CPFP arithmetic for you, and it is the same arithmetic.**
  `walletcreatefundedpsbt`'s `fee_rate` applies to the whole unconfirmed ancestor
  package. Passing "the rate I want the child to pay" would badly overpay; the
  right value is the package target. Verified to the satoshi against
  `plan.ChildFeeSat` at three rates.

- **`chan_pending` txids are chainhash bytes**, i.e. reversed relative to every
  txid a human or Core sees. `lnd.ChannelPointFromPending` handles it; hex-encoding
  those bytes directly yields a plausible txid that matches nothing.
- **`lnd@latest` resolves to `v0.0.2`**, a retracted tag — lnd's real versions are
  pre-releases. Pin `v0.19.3-beta`, matching `regtest/.env`.
- **lnd needs a forked protobuf.** `lnrpc` uses `protojson`'s `UseHexForBytes`,
  which only exists in Lightning Labs' fork. A dependency's `replace` does not
  apply transitively, so `go.mod` restates it. Do not remove it.
- **grpc is held at lnd's own pin (v1.59)**, which has no `grpc.NewClient`;
  `DialContext` is deliberate, not legacy.
- **`journal/` was an unanchored .gitignore pattern**, so `internal/journal/`
  matched it and the whole package was silently un-committable. Now `/journal/`
  and `/runs/`, anchored to the repo root where they were meant to be. Worth
  remembering the shape: unanchored gitignore patterns match at every level.
- **The call-site check sees typed client calls, not `conn.Invoke`.**
  `TestEveryLNDCallSiteIsRegistered` resolves method calls through go/types, so it
  finds a call on `lnrpc.LightningClient` and a call on a narrow interface that
  one satisfies — but a raw `conn.Invoke(ctx, "/lnrpc.Lightning/…", …)` with the
  path as a string is invisible to it. `internal/methods/bake_regtest_test.go`
  does exactly that on purpose, to test LND's own enforcement rather than ours;
  it is the one place that should.
- **A macaroon refusal is not `codes.PermissionDenied`.** It is
  `bakery.ErrPermissionDenied` — a plain `errgo.New("permission denied")` from
  gopkg.in/macaroon-bakery.v2 — passed through LND's interceptor with no gRPC
  status attached, so it arrives as `codes.Unknown` with that text. `winthistle
  doctor` has to match the text; matching the code would never fire. Proved in
  `TestTheBakedMacaroonWorksAndIsNarrow`, which logs if LND ever starts typing it.
- **`internal/reserve`'s tests lease every coin alice has**, on purpose, and give
  them back in `t.Cleanup`. If a later test fails with LND's reserved-value error
  and nothing explains it, check for a leaked lease: `lncli --network regtest
  wallet listleases`, and release with `wallet releaseoutput`. `-p 1` is what
  stops another package seeing that node mid-test.
- **btcd is now a direct dependency, at lnd's own pins.** `internal/plan` parses
  PSBTs and classifies scripts with `btcutil/psbt` and `txscript`, and it must be
  the same code LND runs or the verifier's answer stops predicting
  `psbt_verify`'s. `go mod tidy` only promoted the existing indirect entries; do
  not bump them independently of lnd.
- **A single send to a legacy address poisons a Core wallet for batching.** Core
  derives change of the same type as the payment, so paying one P2PKH address
  leaves a P2PKH change output behind — and the next batch built from that wallet
  fails at `psbt_verify` with *"not all inputs are SegWit spends"*, naming an
  input that has nothing to do with whatever produced it. A fixture that needed a
  legacy coin did exactly this to the miner wallet and broke every abort test.
  Two things now stop it: that fixture keeps its legacy coins inside its own
  wallet, and `regtestenv.BuildFundingPSBT` fences off the funding wallet's
  non-SegWit coins with `coldwallet.FenceOff` before handing over to
  `coldwallet.Build`. Worth knowing past the harness — a real cold wallet with any
  legacy history is in the same position, which is what the exclusion report in
  `Coins.Report` is for.

- **Locking is the only way to exclude a coin in Core.** There is no
  "do not spend these" option on `walletcreatefundedpsbt`, and Core does skip
  locked outputs. So `coldwallet.FenceOff` is `lockunspent`, and it inherits
  everything finding 2 above says about it.

- **A 2-of-3 with three signatures does not finalize.** btcd's
  `checkIsMultiSigScript` requires the number of partial signatures to equal the
  number the script demands, so asking one more device to sign "just in case"
  breaks the batch. `internal/combine` catches it before `MaybeFinalizeAll` and
  says so with the counts and the device labels; without that the operator gets
  "Unsupported script type". Worth knowing before designing the signing UI: it
  must collect exactly *m*.

- **A device that returns a *finalized* PSBT is refused, on purpose.** That is
  I-2: a finalized input is a complete witness, so that device held a
  broadcastable transaction. It is the right refusal and it is also the one most
  likely to surprise an operator in assisted mode, where a wallet's default "sign"
  button may finalize. The design's answer is to let the external wallet apply
  *m*−1 signatures and collect the last partial here.

- **`internal/arm`'s publish test leaves open channels and spends cold coins.**
  It is the only test that publishes, and it has to, because "the transaction
  reaches the network on exactly one line" is not a claim a dry run can make.
  Nothing can close what it opened — `CloseChannel` is on the never-list — so
  `make harness` is the reset.

- **A stream must be read promptly, or the funding manager waits.**
  `funderProcessFundingSigned` sends `chan_pending` on `resCtx.updates`, a channel
  with a buffer of 2 (`server.go`), and blocks on `f.quit` if it is full. One
  buffered slot is spent on `psbt_fund`, so a batch that finalized every channel
  before reading any receipt would be relying on that buffer. `arm.Finalize`
  finalizes one at a time and reads each receipt, so it never gets close — but
  this is why, and not merely tidiness.

- **A macaroon refusal has two shapes and they mean opposite things.**
  `codes.InvalidArgument` from `CheckMacaroonPermissions` is the answer about
  the macaroon in the request; an *untyped* error carrying the same "permission
  denied" text is the interceptor refusing the caller. Match both, as
  `doctor.tooNarrow` does — the code alone confuses them and the text alone does
  too.

- **`ExportAllChannelBackups` is node-wide.** The snapshot covers every channel
  the node has, not the batch's. A test may assert "at least the batch"; on the
  harness it comes back with 33.

- **"No device added a signature" is a harness fault, not a signing bug, and
  `bootstrap` does not fix it.** A container restart leaves Core's non-default
  wallets unloaded, which every wallet call reports as "Requested wallet does not
  exist" and `make -C regtest bootstrap` cures. A *different* failure looks like a
  code bug and is not: `internal/run` and `internal/settle` failing with
  "combining the cold wallet's partials: no device added a signature — every
  device returned the packet it was given" means `cold1`/`cold2` hold keys from a
  different generation than the descriptors in `cold-watch`, so
  `walletprocesspsbt` returns the packet untouched. Only `make -C regtest reset`
  (~1 min) fixes that, and `winthistle doctor` will report the coins as fine
  throughout, because they are. Reach for `reset` on that message rather than
  reading the combine path.

- **`internal/run`'s regtest tests open real channels and abort them.** Two
  channels to two peers for the cold-probe test, one stream for the failure
  test, all cancelled or abandoned — so they add to the pending-channel pressure
  every abort test creates. Same cure: `make -C regtest mine N=2016`.

- **The composition's failure path aborts without asking.** `run.Do` tears the
  batch down on any failure between `arm.Open` and the publish, because nothing
  has been broadcast and the answer is always the same. It does ask before using
  LND's blunt abandon flag — `prose.BluntConfirmation` — and that prompt is
  asked on **every** abort of a channel that reached `chan_pending`, because
  LND's safe flag rejects every channel this app opens.

  A piped answer is accepted; end of input is not. The first live CLI probe ran
  with stdin closed, refused its own teardown, and left two channels pending —
  correct by the old rule ("not a terminal, so no") and useless, because the
  operator then had to recover by hand from a run that had done everything
  right. The rule is now about whether anybody answered rather than about what
  kind of file stdin is: `echo y | winthistle recover <id>` works, EOF is no,
  and the guard that actually matters is unchanged — `abort.AbandonPending`
  asks `PendingChannels` itself and offers no confirmation at all for a channel
  that is not pending.

- **`--stop-before-publish` exits non-zero if the teardown does not finish.**
  The probe proving the sequence and then leaving two channels pending is not a
  success, and a probe is usually run from a terminal somebody walks away from.

- **The config reader refuses unknown keys, which makes it strict about its own
  history too.** Renaming a key is a breaking change to every operator's file,
  and the failure is loud rather than silent. That is the intent; it is also
  worth remembering before renaming one.

- **Waiting out the peers' window on a batch gives you no receipts, not one.**
  The obvious way to drive "a funding timeout expires with one receipt
  outstanding" is to open the batch, wait eleven minutes and finalize. It does
  not work: every peer's clock starts at its own `accept_channel`, so a batch
  opened together lapses together, and what you get is *n* dead reservations and
  a `psbt_verify` that already failed before any finalize. What produces one
  survivor is a **stagger** —
  `TestOnePeersWindowExpiringLeavesTheRestOfTheBatchArmed` opens the doomed
  stream six minutes before the other, verifies both while both are alive
  (`psbt_verify` is local and touches no timer, which is why it passes minutes
  before one member is gone), watches the first peer give up rather than sleeping
  past it, and finalizes afterwards. `WINTHISTLE_SLOW=1`, ~12 minutes, and it
  holds alice's streams and the cold wallet's coin locks for all of it — so do
  not run anything else harness-backed alongside it.

- **LND calls itself unsynced when regtest's tip is more than two hours old, and
  a long test can cross that line mid-run.** btcwallet's `IsSynced`: "if the
  timestamp on the best header is more than 2 hours in the past, then we're not
  yet synced" — exactly two hours, at v0.19.3-beta. `OpenChannel` then refuses
  with `channels cannot be created before the wallet is fully synced`, which
  reads like a node problem and is a *clock* problem: nothing has been mined.
  It bit the funding-timeout test on its first run, where the stream opened at
  t+0 and the one at t+6m did not. `env.Mine(t, 1)` at the start buys two hours
  of headroom and confirms nothing but itself. Any test that spans minutes on an
  idle cluster wants it.
