// Package signers is how a base64 PSBT gets to a cold-storage device and how
// the partial signature gets back.
//
// Two transports, because the design's third — the browser's file up/down and
// animated QR — belongs to the UI that does not exist yet:
//
//   - a command, which the operator names in winthistle.toml. It reads a base64
//     PSBT on stdin and writes one on stdout. That is enough for the simulated
//     multisig cold wallet CI uses, for hwi, and for any wrapper script.
//   - a file handshake, which is what an air-gapped device actually is: this
//     writes a file, prints what to do with it, and waits for the signed one to
//     appear. No socket, no daemon, no assumption about how it got across.
//
// # What a signer is not allowed to hand back
//
// A finalized PSBT. Nothing here enforces that — internal/combine does, and it
// refuses one outright — but it is worth saying at the transport layer too,
// because this is where an operator's wallet software gets to choose. A
// finalized input is a complete witness, which means that device held a
// broadcastable transaction, and I-2 is that no external party ever does. In
// assisted mode the answer is to let the external wallet apply m−1 signatures
// and collect the last partial here.
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
	"encoding/base64"
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

// magic is a PSBT's five-byte prefix, "psbt" and 0xff. It is how a file written
// by Sparrow (binary) is told from one written by Core (base64) without asking
// the operator which their wallet does.
var magic = []byte{0x70, 0x73, 0x62, 0x74, 0xff}

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
		"Do not let the wallet finalize: this tool combines the partial signatures "+
		"itself, and a device that hands back a finalized transaction held a "+
		"broadcastable one, which is refused (I-2)."))
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
// Sparrow writes binary .psbt files and Core writes base64, and an operator
// moving files by hand should not have to know which this tool wanted. The
// five-byte magic settles it without guessing.
func decode(body []byte) ([]byte, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("it is empty")
	}
	if bytes.HasPrefix(trimmed, magic) {
		return trimmed, nil
	}
	raw, err := combine.ParseBase64(string(trimmed))
	if err != nil {
		// Not base64 either. Say what was actually there rather than repeating
		// base64's complaint, which is about padding and tells nobody anything.
		if _, decErr := base64.StdEncoding.DecodeString(string(trimmed)); decErr != nil {
			return nil, fmt.Errorf("it is neither base64 nor a PSBT: it starts %q",
				firstBytes(trimmed))
		}
		return nil, err
	}
	return raw, nil
}

func firstBytes(b []byte) string {
	if len(b) > 24 {
		b = b[:24]
	}
	return string(b)
}
