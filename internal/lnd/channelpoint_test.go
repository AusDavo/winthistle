package lnd

import "testing"

func TestParseChannelPoint(t *testing.T) {
	const txid = "1e0f2c4a6b8d0e1f2a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f8091"

	cp, err := ParseChannelPoint(txid + ":3")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cp.TxID != txid || cp.Index != 3 {
		t.Fatalf("got %+v", cp)
	}
	if got := cp.String(); got != txid+":3" {
		t.Fatalf("String() round trip: %s", got)
	}

	for _, bad := range []string{
		"", txid, txid + ":", txid + ":x", txid + ":-1", "abc:0", txid + "ff:0",
	} {
		if _, err := ParseChannelPoint(bad); err == nil {
			t.Errorf("ParseChannelPoint(%q) should have failed", bad)
		}
	}
}

// The reversal is the whole point of ChannelPointFromPending: LND sends
// chainhash bytes, everything else speaks display order.
func TestChannelPointFromPendingReversesTxid(t *testing.T) {
	internal := make([]byte, 32)
	for i := range internal {
		internal[i] = byte(i)
	}

	cp, err := ChannelPointFromPending(internal, 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	const want = "1f1e1d1c1b1a191817161514131211100f0e0d0c0b0a09080706050403020100"
	if cp.TxID != want {
		t.Fatalf("txid not reversed:\n got %s\nwant %s", cp.TxID, want)
	}

	if _, err := ChannelPointFromPending(internal[:31], 0); err == nil {
		t.Error("a 31-byte txid should have been rejected")
	}
}

func TestPendingChanIDRoundTrip(t *testing.T) {
	id, err := NewPendingChanID()
	if err != nil {
		t.Fatalf("NewPendingChanID: %v", err)
	}
	back, err := ParsePendingChanID(id.String())
	if err != nil {
		t.Fatalf("ParsePendingChanID: %v", err)
	}
	if back != id {
		t.Fatalf("round trip changed the id: %s -> %s", id, back)
	}

	// Bytes must copy, or a protobuf could alias the journal's identifier.
	b := id.Bytes()
	b[0] ^= 0xff
	if id[0] == b[0] {
		t.Error("Bytes() aliased the identifier")
	}

	for _, bad := range []string{"", "zz", id.String() + "00", id.String()[:62]} {
		if _, err := ParsePendingChanID(bad); err == nil {
			t.Errorf("ParsePendingChanID(%q) should have failed", bad)
		}
	}
}
