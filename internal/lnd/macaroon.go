package lnd

import (
	"errors"
	"fmt"
	"os"

	"gopkg.in/macaroon.v2"
)

// ErrMacaroonFile marks a failure that belongs to the credential *file* rather
// than to the node, the address or the credential's permissions. Nothing has
// asked LND anything by the time one of these is returned, so a caller that
// renders a cause may say "this file" and must not say "this node" or "too
// narrow".
var ErrMacaroonFile = errors.New("the macaroon file")

// ReadMacaroon reads the baked credential and proves that what came back is a
// macaroon, before anything presents it as one.
//
// os.ReadFile is happy with a short read, and hex.EncodeToString turns whatever
// came back into a well-formed credential carrying the wrong bytes —
// hex.EncodeToString(nil) is "", a perfectly well-formed empty credential. LND
// then refuses it, and every sentence downstream is about authentication: the
// address, the permission list, re-baking. None of those is the cause, and one
// of them — re-baking — is the action that re-opens the window this closes.
//
// The window is real and this build prints the command that opens it:
// `winthistle print-macaroon-command --save-to PATH | sh` is lncli bakemacaroon
// --save_to writing this exact path. Re-bake in one pane while a run starts in
// another and the race is live. As in issue #8 the wide half is zero bytes — the
// file sits empty for the whole gap between the writer's open and its write.
//
// There is no retry here and no polling, which is where this parts from
// run.readWhole. That one polls for a file the operator's wallet is still
// writing, inside clock A, so it has to tell "not finished yet" from "wrong".
// Dial is a one-shot at a moment nothing is waiting on: a credential that is
// wrong must fail on the first look, and a credential that is half written is
// wrong on this look. Read it again after the bake has finished.
//
// The tls.cert read two lines above the call site has never needed this.
// AppendCertsFromPEM returns false on a truncated PEM, so that path already
// fails with "no certificate found in %s" — loud and correctly diagnosed. This
// is the macaroon path being held to the standard already in the file.
func ReadMacaroon(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%w %s could not be read: %w", ErrMacaroonFile, path, err)
	}
	if len(b) == 0 {
		return nil, fmt.Errorf("%w %s is empty. No macaroon is zero bytes, so "+
			"this is the file and not the credential — an empty file is what a "+
			"bake looks like part-way through, and what an interrupted one "+
			"leaves behind. Let the bake finish and run this again",
			ErrMacaroonFile, path)
	}
	if err := (&macaroon.Macaroon{}).UnmarshalBinary(b); err != nil {
		return nil, fmt.Errorf("%w %s is %d bytes and does not decode as a "+
			"macaroon: %v. Nothing has been asked of LND yet, so this is the "+
			"file rather than what the credential is allowed to do",
			ErrMacaroonFile, path, len(b), err)
	}
	return b, nil
}
