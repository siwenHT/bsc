package arb

import (
	"encoding/json"
	"strings"
	"testing"
)

func mkBoot(b byte) string {
	// build a valid id32: 0x + 64 lowercase hex chars
	hexb := []byte("0123456789abcdef")
	var sb strings.Builder
	sb.WriteString("0x")
	for i := 0; i < 32; i++ {
		if i == 0 {
			sb.WriteByte(hexb[b>>4])
			sb.WriteByte(hexb[b&0xf])
		} else {
			sb.WriteString("00")
		}
	}
	return sb.String()
}

func payload(n int) json.RawMessage {
	// n-byte JSON string payload (n includes the quotes)
	if n < 2 {
		n = 2
	}
	return json.RawMessage(`"` + strings.Repeat("x", n-2) + `"`)
}

func TestNewStreamEmitterRejectsBadBoot(t *testing.T) {
	for _, bad := range []string{"", "0xABCD", "abc", "0x" + strings.Repeat("g", 64), strings.Repeat("0", 66)} {
		if _, err := NewStreamEmitter(bad, "1", 10, 1000); err != ErrBadBoot {
			t.Fatalf("boot %q: want ErrBadBoot, got %v", bad, err)
		}
	}
	if _, err := NewStreamEmitter(mkBoot(1), "1", 0, 1000); err == nil {
		t.Fatal("want error for zero maxItems")
	}
}

func TestSeqStartsAtOneAssignedBeforeQueue(t *testing.T) {
	e, err := NewStreamEmitter(mkBoot(1), "s1", 10, 10_000)
	if err != nil {
		t.Fatal(err)
	}
	s1, _ := e.Enqueue(KindPending, payload(10))
	s2, _ := e.Enqueue(KindPending, payload(10))
	if s1 != 1 || s2 != 2 {
		t.Fatalf("want seq 1,2 got %d,%d", s1, s2)
	}
	if e.LastSeq() != 2 {
		t.Fatalf("lastSeq want 2 got %d", e.LastSeq())
	}
	f, ok := e.Pop()
	if !ok || f.Seq != "1" {
		t.Fatalf("first frame seq want \"1\" got %+v", f)
	}
	if f.Boot != mkBoot(1) || f.StreamID != "s1" {
		t.Fatalf("frame identity wrong: %+v", f)
	}
}

func TestFullQueueDropsOldestButAdvancesSeqAndCountsGap(t *testing.T) {
	// cap 2 items. enqueue 3; oldest dropped, seq keeps advancing, dropped=1.
	e, _ := NewStreamEmitter(mkBoot(2), "s", 2, 10_000)
	e.Enqueue(KindPending, payload(10)) // seq1
	e.Enqueue(KindPending, payload(10)) // seq2
	s3, _ := e.Enqueue(KindPending, payload(10)) // seq3 evicts seq1
	if s3 != 3 {
		t.Fatalf("seq must advance to 3, got %d", s3)
	}
	if e.Dropped() != 1 {
		t.Fatalf("want 1 drop, got %d", e.Dropped())
	}
	if e.Len() != 2 {
		t.Fatalf("want 2 queued, got %d", e.Len())
	}
	// remaining should be seq2 then seq3 (FIFO, seq1 was evicted => gap)
	f1, _ := e.Pop()
	f2, _ := e.Pop()
	if f1.Seq != "2" || f2.Seq != "3" {
		t.Fatalf("want seq 2,3 remaining, got %s,%s", f1.Seq, f2.Seq)
	}
}

func TestByteCapEvicts(t *testing.T) {
	// byte cap 30; each payload 20 bytes => only one fits at a time.
	e, _ := NewStreamEmitter(mkBoot(3), "s", 100, 30)
	e.Enqueue(KindPending, payload(20)) // seq1, 20 bytes
	e.Enqueue(KindPending, payload(20)) // seq2 needs 40>30 => evict seq1
	if e.Len() != 1 || e.Bytes() > 30 {
		t.Fatalf("byte cap not enforced: len=%d bytes=%d", e.Len(), e.Bytes())
	}
	if e.Dropped() != 1 {
		t.Fatalf("want 1 drop from byte cap, got %d", e.Dropped())
	}
	f, _ := e.Pop()
	if f.Seq != "2" {
		t.Fatalf("want seq2 survivor, got %s", f.Seq)
	}
}

func TestOversizeItemDroppedButSeqConsumed(t *testing.T) {
	// single item larger than byte cap: dropped immediately, seq still consumed.
	e, _ := NewStreamEmitter(mkBoot(4), "s", 10, 30)
	s, err := e.Enqueue(KindPending, payload(100)) // 100 > 30
	if err != nil {
		t.Fatal(err)
	}
	if s != 1 {
		t.Fatalf("seq want 1 got %d", s)
	}
	if e.Len() != 0 {
		t.Fatalf("oversize must not be queued, len=%d", e.Len())
	}
	if e.Dropped() != 1 {
		t.Fatalf("want 1 drop, got %d", e.Dropped())
	}
	// next seq must be 2, not reused 1 (no silent seq reuse).
	s2, _ := e.Enqueue(KindPending, payload(10))
	if s2 != 2 {
		t.Fatalf("seq must not be reused; want 2 got %d", s2)
	}
}

func TestHeartbeatExposesTailLossWithoutAdvancingSeq(t *testing.T) {
	e, _ := NewStreamEmitter(mkBoot(5), "s", 2, 10_000)
	e.Enqueue(KindPending, payload(10)) // seq1
	e.Enqueue(KindPending, payload(10)) // seq2
	e.Enqueue(KindPending, payload(10)) // seq3 evicts seq1 => dropped=1
	before := e.LastSeq()
	hb, err := e.Heartbeat()
	if err != nil {
		t.Fatal(err)
	}
	if e.LastSeq() != before {
		t.Fatalf("heartbeat must not advance seq: before=%d after=%d", before, e.LastSeq())
	}
	if hb.Seq != "3" {
		t.Fatalf("heartbeat seq should echo lastSeq 3, got %s", hb.Seq)
	}
	var hp HeartbeatPayload
	if err := json.Unmarshal(hb.Payload, &hp); err != nil {
		t.Fatal(err)
	}
	if hp.Kind != "heartbeat" || hp.LastSeq != "3" || hp.Dropped != "1" {
		t.Fatalf("heartbeat payload wrong: %+v", hp)
	}
}

func TestClosedEmitterRejects(t *testing.T) {
	e, _ := NewStreamEmitter(mkBoot(6), "s", 10, 10_000)
	e.Close()
	if _, err := e.Enqueue(KindPending, payload(10)); err != ErrEmitterClosed {
		t.Fatalf("want ErrEmitterClosed, got %v", err)
	}
}

// TestWireContractShape verifies a pending frame serializes to exactly the frozen
// wire fields (boot, stream_id, seq, kind, payload), with seq as a decimal STRING
// and optional payload fields present-but-null when absent.
func TestWireContractShape(t *testing.T) {
	e, _ := NewStreamEmitter(mkBoot(7), "42", 10, 10_000)
	pp := PendingPayload{
		TxHash:              "0x" + strings.Repeat("11", 32),
		RawSignedTx:         "0x02abcd",
		TxType:              "2",
		SourceKind:          "txpool_event",
		Validation:          "txpool_event",
		ObservedHeadHash:    "0x" + strings.Repeat("22", 32),
		FirstFullSeenUnixNs: "1700000000000000000",
		Sender:              nil, // must serialize as null
		PeerTag:             nil, // must serialize as null
	}
	raw, err := json.Marshal(pp)
	if err != nil {
		t.Fatal(err)
	}
	e.Enqueue(KindPending, raw)
	f, _ := e.Pop()
	out, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	// top-level keys exactly the frozen set
	var top map[string]json.RawMessage
	if err := json.Unmarshal(out, &top); err != nil {
		t.Fatal(err)
	}
	want := []string{"boot", "stream_id", "seq", "kind", "payload"}
	if len(top) != len(want) {
		t.Fatalf("frame has %d keys, want %d: %s", len(top), len(want), out)
	}
	for _, k := range want {
		if _, ok := top[k]; !ok {
			t.Fatalf("frame missing key %q: %s", k, out)
		}
	}
	// seq must be a JSON string, not a number
	if string(top["seq"]) != `"1"` {
		t.Fatalf("seq must be decimal string \"1\", got %s", top["seq"])
	}
	// optional fields present as null
	s := string(out)
	if !strings.Contains(s, `"sender_optional":null`) {
		t.Fatalf("sender_optional must be null when absent: %s", s)
	}
	if !strings.Contains(s, `"peer_tag_optional":null`) {
		t.Fatalf("peer_tag_optional must be null when absent: %s", s)
	}
}
