package arb

import "testing"

func TestWorkerPoolRejectsNonPositiveCapacity(t *testing.T) {
	if _, err := NewWorkerPool(0); err != ErrBadCapacity {
		t.Fatalf("cap 0 err=%v want ErrBadCapacity", err)
	}
	if _, err := NewWorkerPool(-1); err != ErrBadCapacity {
		t.Fatalf("cap -1 err=%v want ErrBadCapacity", err)
	}
}

func TestAdmitUpToCapacityThenReject(t *testing.T) {
	p, _ := NewWorkerPool(2)
	if err := p.Admit("j1"); err != nil {
		t.Fatalf("admit j1: %v", err)
	}
	if err := p.Admit("j2"); err != nil {
		t.Fatalf("admit j2: %v", err)
	}
	if p.InFlight() != 2 {
		t.Fatalf("InFlight=%d want 2", p.InFlight())
	}
	// Third admission over cap is honest backpressure, not a silent drop.
	if err := p.Admit("j3"); err != ErrPoolAtCapacity {
		t.Fatalf("admit j3 err=%v want ErrPoolAtCapacity", err)
	}
}

func TestReleaseFreesSlotForNextAdmit(t *testing.T) {
	p, _ := NewWorkerPool(1)
	p.Admit("j1")
	if err := p.Admit("j2"); err != ErrPoolAtCapacity {
		t.Fatalf("j2 should be rejected at cap 1, err=%v", err)
	}
	// Worker for j1 truly exits -> slot returns -> j2 admissible.
	if err := p.Release("j1"); err != nil {
		t.Fatalf("release j1: %v", err)
	}
	if p.InFlight() != 0 {
		t.Fatalf("InFlight=%d want 0 after release", p.InFlight())
	}
	if err := p.Admit("j2"); err != nil {
		t.Fatalf("admit j2 after release: %v", err)
	}
}

func TestReleaseIsExactlyOnce(t *testing.T) {
	p, _ := NewWorkerPool(2)
	p.Admit("j1")
	if err := p.Release("j1"); err != nil {
		t.Fatalf("first release: %v", err)
	}
	// A second release must NOT double-count capacity back (§401 exactly-once).
	if err := p.Release("j1"); err != ErrSlotAlreadyReleased {
		t.Fatalf("second release err=%v want ErrSlotAlreadyReleased", err)
	}
	if p.InFlight() != 0 {
		t.Fatalf("InFlight=%d want 0 (double release must not go negative/positive)", p.InFlight())
	}
}

func TestReleaseUnknownJobIsError(t *testing.T) {
	p, _ := NewWorkerPool(2)
	if err := p.Release("never-admitted"); err != ErrSlotNotHeld {
		t.Fatalf("release unknown err=%v want ErrSlotNotHeld", err)
	}
}

func TestAdmitIsIdempotentForExistingJob(t *testing.T) {
	p, _ := NewWorkerPool(1)
	p.Admit("j1")
	// An idempotent resubmit returns the SAME job_id; re-admitting must not consume
	// a second slot (which at cap 1 would falsely reject).
	if err := p.Admit("j1"); err != nil {
		t.Fatalf("re-admit same job err=%v want nil (idempotent)", err)
	}
	if p.InFlight() != 1 {
		t.Fatalf("InFlight=%d want 1 (idempotent admit must not double-count)", p.InFlight())
	}
}

func TestForgetOnlyAfterRelease(t *testing.T) {
	p, _ := NewWorkerPool(2)
	p.Admit("j1")
	// Cannot forget a job that still holds a live slot.
	if err := p.Forget("j1"); err == nil {
		t.Fatal("Forget of a live-slot job must fail")
	}
	p.Release("j1")
	// After release, forgetting clears the latch so the id can be reused fresh.
	if err := p.Forget("j1"); err != nil {
		t.Fatalf("Forget after release: %v", err)
	}
	// Re-admitting the forgotten id starts clean (not ErrSlotAlreadyReleased on next release).
	if err := p.Admit("j1"); err != nil {
		t.Fatalf("re-admit forgotten id: %v", err)
	}
	if err := p.Release("j1"); err != nil {
		t.Fatalf("release re-admitted id err=%v want nil (latch was cleared)", err)
	}
}
