package arb

import (
	"testing"
	"time"
)

func TestChargeReadUpToCapThenFails(t *testing.T) {
	clk := &fixedClock{now: time.Unix(1000, 0)}
	b := NewReadBudget(clk.Clock(), 3, 0) // no deadline
	for i := 0; i < 3; i++ {
		if err := b.ChargeRead(ReadStorage); err != nil {
			t.Fatalf("read %d should be admitted, got %v", i, err)
		}
	}
	if b.Reads() != 3 {
		t.Fatalf("want 3 reads, got %d", b.Reads())
	}
	// 4th read exceeds cap.
	if err := b.ChargeRead(ReadStorage); err != ErrReadBudgetExceeded {
		t.Fatalf("want budget exceeded, got %v", err)
	}
	// Counter must not overshoot the cap.
	if b.Reads() != 3 {
		t.Fatalf("counter overshot cap: %d", b.Reads())
	}
	if !b.Failed() || b.Err() != ErrReadBudgetExceeded {
		t.Fatalf("budget must be latched failed")
	}
}

func TestExceededIsStickyNoFakeSuccess(t *testing.T) {
	clk := &fixedClock{now: time.Unix(1000, 0)}
	b := NewReadBudget(clk.Clock(), 1, 0)
	b.ChargeRead(ReadStorage)              // ok, at cap
	if b.ChargeRead(ReadBalance) != ErrReadBudgetExceeded {
		t.Fatal("second read should exceed")
	}
	// Every subsequent read of any kind must keep failing (no recovery).
	for _, k := range []ReadKind{ReadStorage, ReadCode, ReadCommitted, ReadNonce} {
		if err := b.ChargeRead(k); err != ErrReadBudgetExceeded {
			t.Fatalf("kind %s after exceed must fail, got %v", k, err)
		}
	}
}

func TestDeadlineLatches(t *testing.T) {
	clk := &fixedClock{now: time.Unix(1000, 0)}
	b := NewReadBudget(clk.Clock(), 1000, 500*time.Millisecond)
	if err := b.ChargeRead(ReadStorage); err != nil {
		t.Fatalf("first read before deadline: %v", err)
	}
	clk.advance(500 * time.Millisecond) // reach deadline boundary
	if err := b.ChargeRead(ReadStorage); err != ErrDeadlineExceeded {
		t.Fatalf("want deadline exceeded, got %v", err)
	}
	// Sticky: even a kind check keeps failing.
	if err := b.CheckDeadline(); err != ErrDeadlineExceeded {
		t.Fatalf("deadline must stay latched, got %v", err)
	}
	if !b.Failed() {
		t.Fatal("must be failed after deadline")
	}
}

func TestDeadlineTakesPrecedenceOverCap(t *testing.T) {
	clk := &fixedClock{now: time.Unix(1000, 0)}
	b := NewReadBudget(clk.Clock(), 1000, time.Second)
	clk.advance(time.Second)
	// Even with read budget remaining, an elapsed deadline fails first.
	if err := b.ChargeRead(ReadStorage); err != ErrDeadlineExceeded {
		t.Fatalf("deadline should take precedence, got %v", err)
	}
}

func TestPerKindCounts(t *testing.T) {
	clk := &fixedClock{now: time.Unix(1000, 0)}
	b := NewReadBudget(clk.Clock(), 100, 0)
	b.ChargeRead(ReadStorage)
	b.ChargeRead(ReadStorage)
	b.ChargeRead(ReadBalance)
	b.ChargeRead(ReadCode)
	if b.ReadsOfKind(ReadStorage) != 2 {
		t.Fatalf("want 2 storage, got %d", b.ReadsOfKind(ReadStorage))
	}
	if b.ReadsOfKind(ReadBalance) != 1 || b.ReadsOfKind(ReadCode) != 1 {
		t.Fatal("per-kind balance/code counts wrong")
	}
	if b.ReadsOfKind(ReadNonce) != 0 {
		t.Fatal("unused kind should be 0")
	}
}

func TestNoDeadlineNeverExpires(t *testing.T) {
	clk := &fixedClock{now: time.Unix(1000, 0)}
	b := NewReadBudget(clk.Clock(), 5, 0)
	clk.advance(24 * time.Hour)
	if err := b.CheckDeadline(); err != nil {
		t.Fatalf("no deadline must never expire, got %v", err)
	}
	if err := b.ChargeRead(ReadStorage); err != nil {
		t.Fatalf("read should still work with no deadline, got %v", err)
	}
}

func TestHealthyBudgetErrNil(t *testing.T) {
	clk := &fixedClock{now: time.Unix(1000, 0)}
	b := NewReadBudget(clk.Clock(), 5, time.Minute)
	if b.Failed() || b.Err() != nil {
		t.Fatal("fresh budget must be healthy")
	}
}
