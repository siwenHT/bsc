package arb

import (
	"encoding/json"
	"strings"
	"testing"
)

func hh(b byte) string {
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

// headAt builds a HeadInput extending parent (by hash) at the given number.
func headAt(hash string, num uint64, parent string) HeadInput {
	return HeadInput{
		Hash:                 hash,
		Number:               num,
		ParentHash:           parent,
		StateRoot:            hh(0xee),
		ConsensusTimestampMs: 1700000000000,
		EvmTimestampSeconds:  1700000000,
		PoolRegistryRevision: 7,
	}
}

func TestFirstHeadEpochAndSeqStartAtOne(t *testing.T) {
	c := NewHeadCoordinator()
	class := c.Observe(headAt(hh(1), 100, hh(0)), ObserveSignal{})
	if class != ClassFirst {
		t.Fatalf("want ClassFirst, got %s", class)
	}
	r := c.Take()
	if r == nil {
		t.Fatal("want a staged head")
	}
	if r.StateEpoch != "1" || r.CanonicalSeq != "1" {
		t.Fatalf("want epoch/seq 1/1, got %s/%s", r.StateEpoch, r.CanonicalSeq)
	}
	if r.Number != "100" {
		t.Fatalf("number must be decimal string 100, got %s", r.Number)
	}
	if r.PoolStateMode != ModeIdentityOnly || r.DataComplete {
		t.Fatalf("identity_only must not claim complete: %+v", r)
	}
	if r.ResetRequired {
		t.Fatal("first head should not require reset")
	}
}

func TestDirectExtensionKeepsEpochAdvancesSeq(t *testing.T) {
	c := NewHeadCoordinator()
	c.Observe(headAt(hh(1), 100, hh(0)), ObserveSignal{})
	c.Take()
	class := c.Observe(headAt(hh(2), 101, hh(1)), ObserveSignal{}) // parent == hh(1)
	if class != ClassDirectExtension {
		t.Fatalf("want ClassDirectExtension, got %s", class)
	}
	r := c.Take()
	if r.StateEpoch != "1" {
		t.Fatalf("direct extension must keep epoch 1, got %s", r.StateEpoch)
	}
	if r.CanonicalSeq != "2" {
		t.Fatalf("seq must advance to 2, got %s", r.CanonicalSeq)
	}
}

func TestReorgSameHeightBumpsEpoch(t *testing.T) {
	c := NewHeadCoordinator()
	c.Observe(headAt(hh(1), 100, hh(0)), ObserveSignal{})
	c.Take()
	// same height 100, different hash => reorg, epoch++.
	class := c.Observe(headAt(hh(9), 100, hh(0)), ObserveSignal{})
	if class != ClassReorg {
		t.Fatalf("want ClassReorg, got %s", class)
	}
	r := c.Take()
	if r.StateEpoch != "2" {
		t.Fatalf("reorg must bump epoch to 2, got %s", r.StateEpoch)
	}
}

func TestNonLinearForwardJumpIsReorg(t *testing.T) {
	c := NewHeadCoordinator()
	c.Observe(headAt(hh(1), 100, hh(0)), ObserveSignal{})
	c.Take()
	// jump to 105 whose parent is not the last hash => reorg (not direct ext).
	class := c.Observe(headAt(hh(5), 105, hh(4)), ObserveSignal{})
	if class != ClassReorg {
		t.Fatalf("want ClassReorg for non-linear jump, got %s", class)
	}
}

func TestDuplicateHeadIsStaleNoOp(t *testing.T) {
	c := NewHeadCoordinator()
	c.Observe(headAt(hh(1), 100, hh(0)), ObserveSignal{})
	c.Take()
	class := c.Observe(headAt(hh(1), 100, hh(0)), ObserveSignal{}) // same hash
	if class != ClassStale {
		t.Fatalf("want ClassStale, got %s", class)
	}
	if c.HasPending() {
		t.Fatal("stale head must not stage anything")
	}
	if c.CanonicalSeq() != 1 || c.Epoch() != 1 {
		t.Fatalf("stale must not move seq/epoch: seq=%d epoch=%d", c.CanonicalSeq(), c.Epoch())
	}
}

func TestCollapsedUndeliveredHeadLatchesReset(t *testing.T) {
	c := NewHeadCoordinator()
	c.Observe(headAt(hh(1), 100, hh(0)), ObserveSignal{})
	// do NOT Take: the next observe overwrites the undelivered pending => a head
	// was collapsed, consumer must be told to rebuild.
	c.Observe(headAt(hh(2), 101, hh(1)), ObserveSignal{})
	if !c.ResetPending() {
		t.Fatal("collapsing an undelivered head must latch reset")
	}
	r := c.Take()
	if !r.ResetRequired {
		t.Fatalf("delivered head must carry reset_required after collapse")
	}
	// latch cleared after delivery.
	if c.ResetPending() {
		t.Fatal("reset latch must clear once delivered")
	}
}

func TestOverflowAndReorgDepthLatchReset(t *testing.T) {
	for _, sig := range []ObserveSignal{
		{DownstreamOverflow: true},
		{ReorgDepthExceeded: true},
	} {
		c := NewHeadCoordinator()
		c.Observe(headAt(hh(1), 100, hh(0)), sig)
		if !c.ResetPending() {
			t.Fatalf("signal %+v must latch reset", sig)
		}
		r := c.Take()
		if !r.ResetRequired {
			t.Fatalf("signal %+v: delivered head must carry reset_required", sig)
		}
		if c.ResetPending() {
			t.Fatalf("signal %+v: latch must clear after delivery", sig)
		}
	}
}

func TestStickyResetSurvivesUntilDelivered(t *testing.T) {
	c := NewHeadCoordinator()
	c.Observe(headAt(hh(1), 100, hh(0)), ObserveSignal{DownstreamOverflow: true})
	// overwrite before delivery; reset stays latched (and re-latched by collapse).
	c.Observe(headAt(hh(2), 101, hh(1)), ObserveSignal{})
	if !c.ResetPending() {
		t.Fatal("reset must stay latched until a head is actually delivered")
	}
	r := c.Take()
	if !r.ResetRequired {
		t.Fatal("sticky reset must ride the delivered head")
	}
}

func TestWireShapeHeadReady(t *testing.T) {
	c := NewHeadCoordinator()
	fin := hh(0xfa)
	h := headAt(hh(1), 100, hh(0))
	h.FinalizedHash = &fin
	c.Observe(h, ObserveSignal{})
	r := c.Take()
	out, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	// integers are decimal strings
	if !strings.Contains(s, `"number":"100"`) || !strings.Contains(s, `"state_epoch":"1"`) {
		t.Fatalf("integers must be decimal strings: %s", s)
	}
	// bools are real JSON bools, not strings
	if !strings.Contains(s, `"data_complete":false`) || !strings.Contains(s, `"reset_required":false`) {
		t.Fatalf("bools must be JSON bools: %s", s)
	}
	// finalized present
	if !strings.Contains(s, `"finalized_hash_optional":"`+fin+`"`) {
		t.Fatalf("finalized hash must be present: %s", s)
	}
	// batch_id null in identity_only
	if !strings.Contains(s, `"batch_id":null`) {
		t.Fatalf("batch_id must be null in identity_only: %s", s)
	}
	if !strings.Contains(s, `"pool_state_mode":"identity_only"`) {
		t.Fatalf("mode must be identity_only: %s", s)
	}
}

func TestFinalizedNullWhenAbsent(t *testing.T) {
	c := NewHeadCoordinator()
	c.Observe(headAt(hh(1), 100, hh(0)), ObserveSignal{}) // no finalized
	r := c.Take()
	out, _ := json.Marshal(r)
	if !strings.Contains(string(out), `"finalized_hash_optional":null`) {
		t.Fatalf("absent finalized must be null: %s", out)
	}
}

func TestTakeEmptyReturnsNil(t *testing.T) {
	c := NewHeadCoordinator()
	if c.Take() != nil {
		t.Fatal("Take with nothing staged must return nil")
	}
}
