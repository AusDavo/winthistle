package webrun

import (
	"context"
	"encoding/base64"
	"fmt"
	"time"

	"github.com/AusDavo/winthistle/internal/combine"
	"github.com/AusDavo/winthistle/internal/prose"
	"github.com/AusDavo/winthistle/internal/run"
	"github.com/AusDavo/winthistle/internal/server"
)

// SigningWindow is how long a browser-driven run holds step 7's question open.
//
// Deliberately not the gate, and for the reason AddressCheckWindow is not: it is
// not a safety bound. The gate exists because a signing round used to sit inside
// the peers' ten minutes with reservations against outpoints and a clock running
// whether or not anybody was ready. Step 7 has none of that any more — the gate
// opened at step 6, every channel is already recoverable by force-close, and
// nothing is in any mempool.
//
// So the only thing this number bounds is how long the registry's one-at-a-time
// question slot is held while the operator is at the safe, and the design's own
// words for step 7 are "take an hour if the device is in another building". An
// hour, then. Lapsing costs an abort of n pending channels — one of each peer's
// pending slots for 2016 blocks, which is real and is not funds.
const SigningWindow = time.Hour

// BuildWindow is how long step 4's question stays open.
//
// This one *is* inside a clock, and not ours: the peers' ten minutes are running
// from arm.Open and step 4 is the only step inside them that takes any time at
// all. The gate is the right number here for once — it is what this product
// already calls "as long as an operator at the machine gets to do a thing" — and
// letting it lapse costs n shim cancels and nothing else, because LND has not
// been shown a transaction yet.
func (l *Launcher) buildWindow() time.Duration { return l.gate() }

// The two choices step 4 and step 7 offer. Matched affirmatively, never
// negatively: a submission naming something else reaches here verbatim, so
// `== ChoiceBuilt` is safe for any string a form could carry and `!= cannot`
// would read a typo as consent.
const (
	// ChoiceBuilt is step 4's affirmative.
	ChoiceBuilt = "built"

	// ChoiceCannotBuild stops the run before anything is pinned.
	ChoiceCannotBuild = "cannot-build"
)

// pageWallet is run.SigningWallet over the browser.
//
// One wallet, two questions, and the same Ask/Reply seam every other adapter in
// this package uses. It replaces the n-device round for the batch and not for the
// CPFP child, which is still m devices returning m partials — see Signer.
//
// # Why the packet arrives as an upload rather than as a path
//
// `winthistle run --psbt FILE` names a path and the file transport reads it.
// This cannot: a path that came in over HTTP is a browser choosing which file
// this process opens, which is the objection already written down for
// setup.Options.Descriptors. So the packet comes through the form, pasted or
// uploaded, and combine.Parse settles binary-or-base64 the way it does for every
// other transport here.
type pageWallet struct {
	run   *server.Run
	build time.Duration
	sign  time.Duration
}

func newPageWallet(r *server.Run, build, sign time.Duration) *pageWallet {
	return &pageWallet{run: r, build: build, sign: sign}
}

// Built is step 4: the operator builds the transaction in their wallet and brings
// it back.
//
// The recipients are already on the transcript above this question — internal/run
// prints them, in full, because they are copy-pasted rather than typed — so the
// prompt says what to do with them rather than repeating them. A second renderer
// of the addresses would be a second thing to keep in step with the plan.
func (w *pageWallet) Built(ctx context.Context, _ []run.Recipient) ([]byte, error) {
	a, err := w.run.Ask(ctx, server.Question{
		Prompt: "Step 4 — build the transaction in your wallet\n\n" +
			prose.Para("Enter the recipients above in your wallet, choose your "+
				"coins and the fee, and save the PSBT — then paste or upload it here. "+
				"Do not sign it yet: nothing is recoverable until step 6, so a "+
				"transaction signed and broadcast before then would confirm one 2-of-2 "+
				"output per channel with no channel behind any of them. This step is "+
				"inside the peers' ten minutes and it is the only step inside them that "+
				"takes any time; letting it lapse costs a restart and nothing else."),
		Reply: "paste the unsigned PSBT, base64",
		Choices: []server.Choice{
			{Value: ChoiceBuilt, Label: "This is the transaction I built"},
			{Value: ChoiceCannotBuild, Label: "I cannot build it — stop here"},
		},
		Deadline: deadlineFor(ctx, time.Now().Add(w.build)),
	})
	if err != nil {
		return nil, err
	}
	if a.Choice == ChoiceCannotBuild {
		return nil, fmt.Errorf("the transaction was not built, so the batch is " +
			"being taken apart. Nothing was pinned, nothing was signed and nothing " +
			"is at risk")
	}
	raw, err := packetIn(a, ChoiceBuilt, "there is nothing to verify")
	if err != nil {
		return nil, err
	}
	if err := combine.Unsigned(raw); err != nil {
		return nil, fmt.Errorf("that packet carries signatures already: %w.\n\n"+
			"Nothing is at risk yet — no channel has been told about this "+
			"transaction. Build it again without signing it. Then sign at step 7, "+
			"when every channel is already recoverable and broadcasting early "+
			"cannot cost anything", err)
	}
	return raw, nil
}

// Signed is step 7.
func (w *pageWallet) Signed(ctx context.Context, unsigned []byte) ([]byte, error) {
	a, err := w.run.Ask(ctx, server.Question{
		Prompt: "Step 7 — sign it\n\n" +
			prose.Para("Sign it. Every channel in the batch is already "+
				"recoverable by force-close and nothing is in any mempool, so there is no "+
				"clock on this step that costs a restart — take it to the device and take "+
				"as long as it needs. Let the wallet finalize: a complete transaction is "+
				"what this step wants now."),
		Payload:         base64.StdEncoding.EncodeToString(unsigned),
		PayloadLabel:    "the transaction to sign (base64 PSBT)",
		PayloadFilename: "batch-unsigned.psbt",
		Reply:           "paste the signed PSBT, base64",
		Choices: []server.Choice{
			{Value: ChoiceSigned, Label: "This is the signed transaction"},
			{Value: ChoiceCannot, Label: "It cannot be signed — stop here"},
		},
		Deadline: deadlineFor(ctx, time.Now().Add(w.sign)),
	})
	if err != nil {
		return nil, err
	}
	if a.Choice == ChoiceCannot {
		return nil, fmt.Errorf("the batch was not signed, so it is being taken " +
			"apart. Nothing was published: every channel reached chan_pending over " +
			"an unsigned transaction and is now abandoned")
	}
	return packetIn(a, ChoiceSigned, "there is nothing to publish")
}

// packetIn reads the packet off an answer, pasted or uploaded.
//
// An unanswered question is an error rather than an empty packet, which is the
// only safe reading: a "declined quietly" would become a refusal further down
// blamed on the wrong thing.
func packetIn(a server.Answer, affirmative, consequence string) ([]byte, error) {
	if a.Choice != affirmative || (a.Text == "" && len(a.Upload) == 0) {
		return nil, fmt.Errorf("the form came back with no packet in it, so %s",
			consequence)
	}
	// Pasted or uploaded, one function reads both. combine.Parse settles
	// binary-or-base64 on BIP174's five-byte magic, so a wallet that writes a
	// binary .psbt and one that writes base64 are both read without asking the
	// operator which theirs does.
	body, where := []byte(a.Text), "the pasted packet"
	if len(a.Upload) > 0 {
		body, where = a.Upload, "the uploaded file"
		if a.UploadName != "" {
			where = a.UploadName
		}
	}
	raw, err := combine.Parse(body)
	if err != nil {
		return nil, fmt.Errorf("%s is not a PSBT this build can read: %w", where, err)
	}
	return raw, nil
}
