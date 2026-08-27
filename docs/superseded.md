# Superseded designs, and why each went

Four arguments used to live in `README.md`. Each is about a design this tool no
longer has, and a reader arriving cold had to learn the discarded version in
order to understand the current one. They are here instead, because *why a thing
was removed* is still worth having written down — it is what stops it being
reintroduced by somebody who only sees the gap.

The plan that removed most of them was `docs/replan-2026-08.md`, finished on
2026-08-26 and deleted once it was; git has it, and the account of each item as
built is in the pull requests that did the work.

---

## The invariant about partial signatures

The README keeps a compressed version of this one, because the abandonment is
the reason the tool's shape changed. The full argument:

An earlier design required that signers return **partial** signatures only, so
that no external wallet ever held a transaction it could broadcast. Combining and
finalizing happened in-app. It existed for exactly one reason: to stop anything
broadcasting *before the gate opened* and defeating it from outside. It was
called I-2, and it specified an *m*−1 signing dance to arrange it.

After the inversion there is no "before the gate opens". Every channel reaches
`chan_pending` at step 6, and you do not sign until step 7. A wallet holding a
fully signed transaction front-runs nothing — every channel is already
recoverable by force-close. The invariant was not relaxed; it had nothing left to
protect against, and the signing dance went with it.

**The single-sig caveat became moot rather than unmet.** It used to say that a
single-sig wallet returns a complete transaction, so there is no partial-signature
path to hold it up in, and that on single-sig the property rested on the
operator's setup rather than on a gate. That was an honest limit on an invariant
that now has nothing to reach: which kind of wallet you sign with stopped
mattering to this program. It is not a weaker caveat that survives; it is gone.

**What did not dissolve with it.** The step 4 refusal of a signed packet is *not*
this invariant reappearing — step 4 genuinely is before the gate opens, and that
refusal belongs to I-1. Neither is `combine.Merge`'s refusal of a finalized
input, which exists because a merge unions partial signatures and finalization
discards them.

## The fee estimate, and then the declared fee rate

Fee rates came from Bitcoin Core's `estimatesmartfee`. Removing Core removed the
estimate, and nothing replaced it, because the no-third-party rule forbids the
obvious substitute: a fee API is handed the size of what you are building and the
moment you are building it, which together are most of what this tool exists not
to leak.

So it asked *you* instead, in a `[fees] target_sat_per_vb` key with a
`--fee-rate N` flag — and that turned out to be theatre. This program does not
build the transaction and does not choose the fee, so the only number it could
have held you to was the same one you had already typed into your wallet, entered
a second time for a report to compare against the first. The key went on
2026-08-26, and `FeeTooLow`, `FeeTooHigh` and `ChangeTooSmall` went with the
number they judged against.

**Do not add a fee target back** — not as a key, not as a flag, and not as an
estimate.

## The sequence-number check

The batch verifier used to refuse a transaction whose inputs signalled
replaceability, on the reading that a non-replaceable funding transaction was a
defence of I-4.

It is not one. Verified against Bitcoin Core v29: full-RBF is unconditional.
`mempoolfullrbf` does not exist even as a hidden debug option, and
`getmempoolinfo` reports `"fullrbf": true` with no way to turn it off. A
higher-fee conflict relays regardless of what the sequence numbers signal, so
refusing over that signal was a lint wearing an invariant's clothes.

**Removing it did not relax I-4, and the two are the same diff to a fast reader.**
Nothing was weakened, because the lint never held anything. What holds I-4 is that
only you can sign those inputs and nothing in this repository builds a
replacement. `InputView.Sequence` still records what each input said, so a report
can show it; what catches a signer that edited one is the txid pin, because a
changed sequence is a changed txid.

## Grading the size of the change output

The verifier used to compute a floor for the change output, sized against what a
CPFP child would cost at the declared fee rate, and report a change output that
came out under it.

Both halves of that went on 2026-08-26. This build constructs no CPFP child, so
computing a floor for one was an opinion about the operator's arrangements
dressed as arithmetic; and the declared rate it measured against no longer
exists. `ChangeFloor`, `ChildFeeSat` and `Fee.CPFPTarget()` went with
`ChangeTooSmall`.

What survives is the reason the change output is worth having, which is a fact
about I-4 rather than a number: replace the batch and every outpoint moves, so a
child spending the change is the only lever there will ever be on it. Saying that
is informing. Measuring your change against a target and grading it was judging.
