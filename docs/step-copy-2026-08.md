# The step rail

A live plan. It is deleted when it lands — the slice-by-slice account goes to git
history and PR bodies, per `CLAUDE.md`.

## The finding

Every section heading the run prints, in order:

```
Phase 0 — the peers
Phase 0 — the shim probe
Phase 0 — the anchor reserve
Phase 1 — the armed window
Step 4 — build the transaction in your wallet
The batch plan
Step 5 — does it match the plan?
Step 7 — sign it
Phase 2 — settlement
```

Two vocabularies interleaved, and neither complete. Steps 1, 2, 3, 8 and 9 have
no heading. **Step 6 has none**, and step 6 is the gate — the moment the whole
inversion exists to produce goes past unnamed. An operator reading down the
transcript cannot say where they are or how much is left.

The prose is not the problem. The paragraphs at steps 5, 6 and 7 are the best
copy in the product. What is missing is the rail they hang on.

## The spine

Three acts and nine steps, numbered as `docs/design.html` numbers them so the app
and the design teach one model. Each step heading is followed by a single
standing line: **what it costs to stop here.**

That line is the whole design. It changes value exactly three times in a run,
and each change is a thing the operator must understand — so the rail teaches the
gate rather than describing it.

| After | The line reads |
|---|---|
| step 1 | nothing is lost; no peer has been asked for anything |
| steps 2–5 | the batch is torn down: shims cancelled, each peer holds a slot ~11 min |
| step 6 | the batch is abandoned: channels pending and recoverable, peers give up at block N |
| step 8 | it cannot be stopped; the transaction is public |

Clock A is a duration, clock B is blocks, and the line says each in its own
units. Recovery copy already does this and does not change.

## The headings

Act banners print once each.

**Act I — nothing has been asked of anyone**

- `Step 1 — who you are opening to` (was `Phase 0 — the peers`)
- `Step 1 — the shim probe` (was `Phase 0 — the shim probe`; prints only under `--probe`)
- `Step 1 — this node's anchor reserve` (was `Phase 0 — the anchor reserve`)

**Act II — the peers' ten minutes, and nothing is signed**

- `Step 2 — asking each peer to hold a slot` (was `Phase 1 — the armed window`)
- `Step 3 — the plan` — the recipients table (was under step 4's heading)
- `Step 4 — build it in your wallet, and do not sign` — the instruction and the wait
- `Step 5 — does it match the plan?` — opens with the plan document (was the orphan `The batch plan`)
- `Step 6 — every channel becomes recoverable` **(new)**

**Act III — past the gate**

- `Step 7 — sign it` (unchanged)
- `Step 8 — publish, once` **(new)**
- `Step 9 — watch it confirm, and apply the policies` (was `Phase 2 — settlement`)

Off the rail, and staying off it: `Armed`, `Step 8 was not made`,
`The publish call failed…`, `Taking the batch apart`. Those are reports and the
abort path, not positions in the sequence.

## Slices

1. **The rail, mechanically.** Rename the nine headings, add the two missing
   ones, print the three act banners. No prose touched. `Env.RecipientsIn` reads
   the step-4 table off the transcript and is the check that this stayed legible.
2. **The standing line.** One helper in `prose`, four values. Printed where its
   value changes rather than under every heading — see slice 2 below.
3. **Step 6's own screen.** The paragraph exists and is good; it currently
   arrives under no heading, after step 5's output, reading as a continuation of
   the verify. Give it the heading and the line, and nothing else.
4. **Step 8.** The publish has no heading at all today. One heading, one line,
   and the txid it re-checked.

## Slice 1 — landed

Headings, act banners, and two ordering defects the rail made visible.

**The run counted 4, then 3, then 5.** The recipients table printed under step
4's heading, and the batch plan — which cannot be assembled until the packet
exists, because until then there is nothing to say about the change output —
printed under an unnumbered heading after it. Unnumbered, the contradiction was
invisible; numbered, it was the first thing you saw. The table is step 3, because
the table is the attribution; the instruction and the wait are step 4; and the
plan document now opens step 5, which is what it is — the thing the verification
runs against. `regtestenv.RecipientsIn` scrapes that table and its anchor moved
with it.

**Step 6's heading is in the future tense, and prints before the wait.** Both are
deliberate. Nothing bounds that wait — a silent peer parks it until Ctrl-C — so a
heading printed afterwards leaves the operator watching a blank screen with no
idea what for. And *"every channel is now recoverable"*, printed before the
receipts arrive, would assert the one thing the step exists to establish. What
happened is the paragraph below it, which counts the channels itself.

**Two things fixed in place.** `section` underlined by `len(title)`, and every
heading has an em dash in it — three bytes, one column — so every rule in the
product was two characters long. It counts runes now. And the cold probe's test
was named `…WithholdsStepNine` while asserting on *"Step 8 was not made"*.

**Not covered by any test:** the step 8 and step 9 headings. No test takes
`run.Do` past the publish — the two that arm set `StopBeforePublish`, and the
other three fail inside the armed window on purpose. That gap predates this
slice.

## Slice 2 — landed

`prose.StoppingHere(Stage, n)`, four values, printed at four sites.

**Not under every heading — where the value changes.** Steps 2 through 5 share
one answer, so repeating it on four consecutive screens would be noise. And a
line printed under a heading would be printed before the thing it claims: the
Act II banner prints before `arm.Open`, so a line there would say the shims are
cancellable before any shim exists. Each stage prints immediately after the call
that makes it true — Act I's banner, `arm.Open`'s "streams open" line,
`arm.Receipts`, and `arm.Publish`.

**No stage line names an LND default.** The eleven minutes and the 2016 blocks
stay on the screens that own them, where `StockLNDNote` attributes them; a
figure repeated across four more screens would owe four more attributions, and
the rule exists to stop noise rather than license it.
`TestNoStageLineRepeatsAnLNDDefault` is the check.

**The unknown-`Stage` default is loud.** A switch falling through to `""` would
render as a screen with no standing line, which reads exactly like a screen where
stopping is free. It says it cannot say, and it asserts nothing about the batch,
because at that point it knows nothing about the batch.

**One repeat removed.** Step 6's paragraph ended *"To stop here, Ctrl-C — the
teardown abandons"*, and the standing line four lines below says what stopping
costs. The paragraph keeps the key and the hazard that is its own — LND does not
refuse a force-close on a pending channel — and drops the clause.

**Still not covered by any test:** the `StagePublished` line, for the same reason
the step 8 and step 9 headings are not. Its wording is asserted in
`internal/prose`; its call site is not reached.

## What this pass may not do

- **No prose rewrite and no copy sweep.** Fix a wrong sentence in place; do not
  go looking. That process filed ~5 issues per 1 closed.
- **Do not repeat LND's defaults.** `prose.StockLNDNote` attributes the eleven
  minutes and the 2016 blocks once per screen. The act banners name neither.
- **Do not assert a cause the program cannot know.** The standing line states a
  cost, never a risk, and never a reason.
- **The act banner is not a claim about state.** It is a position in the
  sequence. What is true of the batch is the standing line's business, and the
  standing line is written from what has been established.

## What this leaves for a TUI

The rail is static text; the live regions are not, and stdout cannot have them:
clock A counting down in place, *k* of *n* receipts arriving, and an elapsed
time on the two unbounded waits — the saved file, and a silent peer at step 6.

If a TUI is taken up it renders **inline, not fullscreen**. The scrollback is the
record: the step-3 table is the last place the attribution is legible to a human,
and after an abort the transcript is what gets read. An alt-screen that discards
it is a regression in the moment comprehension matters most.

Sequencing: the rail before the public flip, the TUI after it, over this same
step model — see the held-open questions in git history for why a second front
end is an audit-surface decision and not a UI one.
