package lnd

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// PendingChanID is the 32-byte handle the *client* generates for a funding
// stream. LND does not hand it to us — we choose it and pass it in — which is
// what lets the app own every handle in a batch from the first call, and what
// makes an abort possible without having to ask LND what it thinks is in flight.
type PendingChanID [32]byte

// NewPendingChanID draws a fresh identifier. The bytes only need to be unique
// per in-flight funding stream, but they come from crypto/rand because a
// predictable identifier would let anyone who can reach the RPC cancel our shims.
func NewPendingChanID() (PendingChanID, error) {
	var id PendingChanID
	if _, err := rand.Read(id[:]); err != nil {
		return id, fmt.Errorf("generating pending channel id: %w", err)
	}
	return id, nil
}

// ParsePendingChanID reads the hex form written to the journal.
func ParsePendingChanID(s string) (PendingChanID, error) {
	var id PendingChanID
	b, err := hex.DecodeString(s)
	if err != nil {
		return id, fmt.Errorf("pending channel id %q is not hex: %w", s, err)
	}
	if len(b) != len(id) {
		return id, fmt.Errorf("pending channel id %q is %d bytes, want %d", s, len(b), len(id))
	}
	copy(id[:], b)
	return id, nil
}

func (id PendingChanID) String() string { return hex.EncodeToString(id[:]) }

// Bytes returns a copy, so a caller handing it to a protobuf cannot alias the
// journal's copy of the identifier.
func (id PendingChanID) Bytes() []byte {
	out := make([]byte, len(id))
	copy(out, id[:])
	return out
}
