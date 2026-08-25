// Package signers is how a base64 PSBT gets to a cold-storage device and how
// the partial signature gets back.
//
// Two transports here, and a third that is not here: the browser's, which lives
// in internal/webrun because it is answered over the HTTP seam rather than by a
// process on this machine. It moves the same packet three ways — a read-only
// field to copy, a .psbt download of the same bytes, and a file input or a paste
// back — and it shares this package's rehearsal.Signer type for the reason given
// below. In-house animated QR is out of scope as of 2026-08-24, held open in
// docs/review-2026-08-triage.md rather than rejected: the contract is BIP174, so
// a QR-only signer is reached through a desktop wallet that already does the
// round trip. What stays here is everything that does not need a person watching
// a page:
//
//   - a command, which the operator names in winthistle.toml. It reads a base64
//     PSBT on stdin and writes one on stdout. That is enough for the simulated
//     multisig cold wallet CI uses, for hwi, and for any wrapper script.
//   - a file handshake, which is what an air-gapped device actually is: this
//     writes a file, prints what to do with it, and waits for the signed one to
//     appear. No socket, no daemon, no assumption about how it got across.
//
// # Mixing them, and where that choice is made
//
// The choice is per device rather than per run, and this package has always made
// it that way: Set.signer branches on config.Signer.Command, so one [[signer]]
// block naming a command and another naming none give one round two transports.
//
// A browser-driven run now makes the same choice — internal/webrun's Signers
// builds a one-device Set here for every signer whose block names a command, and
// the page is what answers for the rest. The file handshake is not in that mix
// and that is deliberate: on a browser-driven run the operator is at the page,
// which is a better place to be handed a packet than a directory they have to be
// told the path of.
//
// Whatever the rule, it has to be read off the configuration rather than
// recomputed, because of the measurement below: a device that took the command in
// the rehearsal and the page in the batch would make the rehearsal's number a
// prediction about a round that never happened.
//
// # What a signer in one of these rounds is not allowed to hand back
//
// A finalized PSBT. Nothing here enforces that — internal/combine does, and
// combine.Merge refuses one outright — but it is worth saying at the transport
// layer too, because this is where an operator's wallet software gets to choose.
//
// The reason is not the one it used to be. This said I-2: a finalized input is a
// complete witness, so that device held a broadcastable transaction, and no
// external party may ever be in that position. I-2 is dissolved — the gate now
// closes before anything is signed, so a wallet holding signed bytes front-runs
// nothing — and the refusal survives it mechanically. These rounds *union*
// partial signatures from m devices, and finalization discards them, so a device
// that finalizes on its own leaves the others nothing to add to. The batch's own
// wallet is asked through run.SigningWallet instead, and combine.Accept expects
// exactly the complete witness this refuses.
//
// The signature type is rehearsal.Signer, which is the same function the dress
// rehearsal measures. That is deliberate: the rehearsal's number is a
// prediction about the armed window only if the two rounds go through the same
// transport, and sharing the type is the cheapest way to keep them from
// drifting apart.
package signers

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/AusDavo/winthistle/internal/combine"
	"github.com/AusDavo/winthistle/internal/config"
	"github.com/AusDavo/winthistle/internal/prose"
	"github.com/AusDavo/winthistle/internal/rehearsal"
)

// DefaultPoll is how often the file handshake looks for the signed file.
const DefaultPoll = 2 * time.Second

// Options is everything the transports need that is not in the config.
type Options struct {
	// Dir is where the file handshake writes and waits. Required only if some
	// signer has no command.
	Dir string

	// Out is where instructions to the operator go. Required for the file
	// handshake; nil means os.Stdout.
	Out io.Writer

	// Poll is how often to look for the signed file. Zero means DefaultPoll.
	Poll time.Duration
}

// Set is the batch's signers.
type Set struct {
	signers []config.Signer
	opts    Options
}

// New builds the set from the [[signer]] blocks.
func New(sigs []config.Signer, opts Options) (*Set, error) {
	if len(sigs) == 0 {
		return nil, fmt.Errorf("no signers are configured. Add a [[signer]] block " +
			"per cold-storage device to winthistle.toml — and exactly as many as the " +
			"descriptor requires, because btcd's finalizer wants exactly m " +
			"signatures and a 2-of-3 carrying three partials does not finalize")
	}
	if opts.Out == nil {
		opts.Out = os.Stdout
	}
	if opts.Poll <= 0 {
		opts.Poll = DefaultPoll
	}
	for _, s := range sigs {
		if s.Command == "" && opts.Dir == "" {
			return nil, fmt.Errorf("signer %q has no command, so it signs by file "+
				"handshake, and no directory was given to do it in", s.Label)
		}
	}
	return &Set{signers: sigs, opts: opts}, nil
}

// Labels are the devices, in the order an operator would visit them.
func (s *Set) Labels() []string {
	out := make([]string, 0, len(s.signers))
	for _, d := range s.signers {
		out = append(out, d.Label)
	}
	return out
}

// Round returns the devices for one signing round.
//
// The round's name goes into the file names, and stale files under it are
// removed before anything is written. That is not tidiness: the dress rehearsal
// and the real batch are two rounds minutes apart, and a signed file left over
// from the first one, picked up by the second, would be a signature over the
// decoy — which internal/combine would refuse, at the worst possible moment,
// with a message about a moved txid rather than about a leftover file.
func (s *Set) Round(name string) []rehearsal.Device {
	out := make([]rehearsal.Device, 0, len(s.signers))
	for _, d := range s.signers {
		d := d
		out = append(out, rehearsal.Device{Label: d.Label, Sign: s.signer(name, d)})
	}
	return out
}

func (s *Set) signer(round string, d config.Signer) rehearsal.Signer {
	if d.Command != "" {
		return func(ctx context.Context, psbtB64 string) (combine.Part, error) {
			return runCommand(ctx, d, psbtB64)
		}
	}
	return func(ctx context.Context, psbtB64 string) (combine.Part, error) {
		return s.handshake(ctx, round, d.Label, psbtB64)
	}
}

// runCommand hands the PSBT to a command on stdin and reads one back.
//
// Through `sh -c`, so the operator can write a pipeline. That is a command from
// the operator's own configuration file running as the operator, which is what
// the tool does anyway — but it is worth being explicit that this file is as
// trusted as the binary.
func runCommand(ctx context.Context, d config.Signer, psbtB64 string) (combine.Part, error) {
	cmd := exec.CommandContext(ctx, "sh", "-c", d.Command)
	cmd.Stdin = strings.NewReader(psbtB64 + "\n")
	var out, errs bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errs

	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(errs.String())
		if detail != "" {
			return combine.Part{}, fmt.Errorf("signer %s (%s) failed: %w: %s",
				d.Label, d.Command, err, detail)
		}
		return combine.Part{}, fmt.Errorf("signer %s (%s) failed: %w",
			d.Label, d.Command, err)
	}
	raw, err := decode(out.Bytes())
	if err != nil {
		return combine.Part{}, fmt.Errorf("signer %s did not return a PSBT: %w",
			d.Label, err)
	}
	return combine.Part{Label: d.Label, PSBT: raw}, nil
}

// handshake writes the PSBT out and waits for a signed one to appear.
func (s *Set) handshake(ctx context.Context, round, label, psbtB64 string) (combine.Part, error) {
	req := filepath.Join(s.opts.Dir, fmt.Sprintf("%s-%s.psbt", round, label))
	got := filepath.Join(s.opts.Dir, fmt.Sprintf("%s-%s-signed.psbt", round, label))

	if err := os.MkdirAll(s.opts.Dir, 0o700); err != nil {
		return combine.Part{}, fmt.Errorf("making %s: %w", s.opts.Dir, err)
	}
	// Remove the answer before asking the question. See Round.
	if err := os.Remove(got); err != nil && !os.IsNotExist(err) {
		return combine.Part{}, fmt.Errorf("clearing %s: %w", got, err)
	}
	if err := os.WriteFile(req, []byte(psbtB64+"\n"), 0o600); err != nil {
		return combine.Part{}, fmt.Errorf("writing %s: %w", req, err)
	}

	fmt.Fprintf(s.opts.Out, "\n%s\n", label)
	fmt.Fprint(s.opts.Out, prose.Bullet("Take this file to the device and sign it. "+
		"Do not let the wallet finalize: this round collects a partial signature "+
		"from each device and unions them here, so a device that finalizes on its "+
		"own leaves the others nothing to add to and the packet is refused."))
	fmt.Fprintf(s.opts.Out, "      %s\n", req)
	fmt.Fprint(s.opts.Out, prose.Bullet("Then put the signed PSBT here. Base64 or "+
		"binary, either is read:"))
	fmt.Fprintf(s.opts.Out, "      %s\n", got)

	tick := time.NewTicker(s.opts.Poll)
	defer tick.Stop()
	for {
		body, err := os.ReadFile(got)
		switch {
		case err == nil:
			raw, err := decode(body)
			if err != nil {
				return combine.Part{}, fmt.Errorf("%s is not a PSBT this build can "+
					"read: %w", got, err)
			}
			return combine.Part{Label: label, PSBT: raw}, nil
		case !os.IsNotExist(err):
			return combine.Part{}, fmt.Errorf("reading %s: %w", got, err)
		}
		select {
		case <-ctx.Done():
			return combine.Part{}, fmt.Errorf("waiting for %s to sign: %w", label, ctx.Err())
		case <-tick.C:
		}
	}
}

// decode reads a PSBT that arrived as either base64 text or raw bytes.
//
// It is combine.Parse, and it moved there when the browser upload needed the
// same tolerance: three transports read files a wallet wrote, and two sniffers
// could disagree about one file. The wrapper stays because this package's
// callers read better with it, and because the file handshake is where the
// tolerance was first needed.
func decode(body []byte) ([]byte, error) { return combine.Parse(body) }
