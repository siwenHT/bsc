package arb

import (
	"testing"
	"time"
)

// counter id generator for deterministic tests.
func counterIDs() func() string {
	n := 0
	return func() string {
		n++
		return "h" + string(rune('0'+n))
	}
}

func idn(sess string) HandleIdentity {
	return HandleIdentity{OwnerSession: sess, NodeBootID: "boot1", Identity: "id-" + sess}
}

// leaseTracker counts backend Dereference calls the adapter WOULD make, driven by
// the release-now signal from the store. Proves exactly-once.
type leaseTracker struct{ derefs int }

func (l *leaseTracker) onRelease(release bool) {
	if release {
		l.derefs++
	}
}

func TestPinBorrowReleaseSingleLease(t *testing.T) {
	clk := &fixedClock{now: time.Unix(1000, 0)}
	s := NewHandleStore(clk.Clock(), counterIDs())
	lt := &leaseTracker{}

	id := s.Pin(idn("a"), time.Minute)
	if st, _ := s.Status(id); st != StatusActive {
		t.Fatalf("want active, got %s", st)
	}
	if err := s.Borrow(id, idn("a")); err != nil {
		t.Fatalf("borrow: %v", err)
	}
	if s.Borrows(id) != 1 {
		t.Fatalf("want 1 borrow, got %d", s.Borrows(id))
	}
	// Releasing the borrow while still Active must NOT release the lease.
	rel, err := s.ReleaseBorrow(id)
	if err != nil {
		t.Fatal(err)
	}
	lt.onRelease(rel)
	if rel {
		t.Fatal("active handle with zero borrows must not release lease until ReleaseHandle")
	}
	// Now request teardown: drained + closing => single release.
	rel, err = s.ReleaseHandle(id)
	if err != nil {
		t.Fatal(err)
	}
	lt.onRelease(rel)
	if !rel {
		t.Fatal("drained closing parent must release lease")
	}
	if lt.derefs != 1 {
		t.Fatalf("want exactly 1 deref, got %d", lt.derefs)
	}
	// Idempotent: second ReleaseHandle must not release again.
	rel, _ = s.ReleaseHandle(id)
	lt.onRelease(rel)
	if lt.derefs != 1 {
		t.Fatalf("double release must not deref again, got %d", lt.derefs)
	}
	if st, _ := s.Status(id); st != StatusReleased {
		t.Fatalf("want released, got %s", st)
	}
}

func TestReleaseWithOutstandingBorrowDefersLease(t *testing.T) {
	clk := &fixedClock{now: time.Unix(1000, 0)}
	s := NewHandleStore(clk.Clock(), counterIDs())
	lt := &leaseTracker{}

	id := s.Pin(idn("a"), time.Minute)
	s.Borrow(id, idn("a"))
	// Release requested while a borrow is outstanding: flips to Closing, no lease yet.
	rel, _ := s.ReleaseHandle(id)
	lt.onRelease(rel)
	if rel {
		t.Fatal("must not release lease while a borrow is outstanding")
	}
	if st, _ := s.Status(id); st != StatusClosing {
		t.Fatalf("want closing, got %s", st)
	}
	// New borrow rejected while closing.
	if err := s.Borrow(id, idn("a")); err != ErrHandleNotActive {
		t.Fatalf("closing handle must reject new borrow, got %v", err)
	}
	// The outstanding borrow returning now frees the lease exactly once.
	rel, _ = s.ReleaseBorrow(id)
	lt.onRelease(rel)
	if lt.derefs != 1 {
		t.Fatalf("want 1 deref after last borrow drains, got %d", lt.derefs)
	}
}

func TestChildKeepsParentLeaseAlive(t *testing.T) {
	clk := &fixedClock{now: time.Unix(1000, 0)}
	s := NewHandleStore(clk.Clock(), counterIDs())
	lt := &leaseTracker{}

	parent := s.Pin(idn("a"), time.Minute)
	child, err := s.CreateChild(parent, idn("a"), time.Minute)
	if err != nil {
		t.Fatalf("create child: %v", err)
	}
	if s.Children(parent) != 1 {
		t.Fatalf("want 1 child, got %d", s.Children(parent))
	}
	// Releasing the parent while a child exists must NOT free the backend (§202).
	rel, _ := s.ReleaseHandle(parent)
	lt.onRelease(rel)
	if rel {
		t.Fatal("parent with a live child must not release backend lease")
	}
	if st, _ := s.Status(parent); st != StatusClosing {
		t.Fatalf("parent should be closing, got %s", st)
	}
	// Releasing the child cascades and frees the parent's lease exactly once.
	rel, err = s.ReleaseHandle(child)
	if err != nil {
		t.Fatal(err)
	}
	lt.onRelease(rel)
	if !rel {
		t.Fatal("releasing last child of a closing parent must cascade to release parent lease")
	}
	if lt.derefs != 1 {
		t.Fatalf("want exactly 1 deref via cascade, got %d", lt.derefs)
	}
	if st, _ := s.Status(child); st != StatusReleased {
		t.Fatalf("child should be released, got %s", st)
	}
}

func TestChildReleasedBeforeParentStillSingleDeref(t *testing.T) {
	clk := &fixedClock{now: time.Unix(1000, 0)}
	s := NewHandleStore(clk.Clock(), counterIDs())
	lt := &leaseTracker{}

	parent := s.Pin(idn("a"), time.Minute)
	child, _ := s.CreateChild(parent, idn("a"), time.Minute)
	// Child released first: parent still Active, no release.
	rel, _ := s.ReleaseHandle(child)
	lt.onRelease(rel)
	if rel {
		t.Fatal("releasing child while parent still Active must not free parent lease")
	}
	if s.Children(parent) != 0 {
		t.Fatalf("parent child count should be 0, got %d", s.Children(parent))
	}
	// Now parent teardown frees lease once.
	rel, _ = s.ReleaseHandle(parent)
	lt.onRelease(rel)
	if lt.derefs != 1 {
		t.Fatalf("want 1 deref, got %d", lt.derefs)
	}
}

func TestIdentityMismatchRejected(t *testing.T) {
	clk := &fixedClock{now: time.Unix(1000, 0)}
	s := NewHandleStore(clk.Clock(), counterIDs())
	id := s.Pin(idn("a"), time.Minute)
	if err := s.Borrow(id, idn("b")); err != ErrIdentityMismatch {
		t.Fatalf("want identity mismatch, got %v", err)
	}
}

func TestTTLElapseFlipsClosingAndRejectsBorrow(t *testing.T) {
	clk := &fixedClock{now: time.Unix(1000, 0)}
	s := NewHandleStore(clk.Clock(), counterIDs())
	id := s.Pin(idn("a"), time.Second)
	clk.advance(time.Second) // reach expiry boundary
	if err := s.Borrow(id, idn("a")); err != ErrHandleTTLElapsed {
		t.Fatalf("want TTL elapsed, got %v", err)
	}
	if st, _ := s.Status(id); st != StatusClosing {
		t.Fatalf("TTL elapse must flip to closing, got %s", st)
	}
}

func TestChildRequiresActiveParent(t *testing.T) {
	clk := &fixedClock{now: time.Unix(1000, 0)}
	s := NewHandleStore(clk.Clock(), counterIDs())
	parent := s.Pin(idn("a"), time.Minute)
	// close the parent (drained) -> released
	s.ReleaseHandle(parent)
	if _, err := s.CreateChild(parent, idn("a"), time.Minute); err != ErrParentNotActive {
		t.Fatalf("want parent-not-active, got %v", err)
	}
	// child on non-existent parent
	if _, err := s.CreateChild("nope", idn("a"), time.Minute); err != ErrHandleNotFound {
		t.Fatalf("want not-found, got %v", err)
	}
}

func TestReleaseBorrowWithoutBorrowErrors(t *testing.T) {
	clk := &fixedClock{now: time.Unix(1000, 0)}
	s := NewHandleStore(clk.Clock(), counterIDs())
	id := s.Pin(idn("a"), time.Minute)
	if _, err := s.ReleaseBorrow(id); err != ErrNoBorrowToRelease {
		t.Fatalf("want no-borrow error, got %v", err)
	}
}

func TestUnknownHandleErrors(t *testing.T) {
	clk := &fixedClock{now: time.Unix(1000, 0)}
	s := NewHandleStore(clk.Clock(), counterIDs())
	if err := s.Borrow("x", idn("a")); err != ErrHandleNotFound {
		t.Fatalf("want not-found, got %v", err)
	}
	if _, err := s.ReleaseBorrow("x"); err != ErrHandleNotFound {
		t.Fatalf("want not-found, got %v", err)
	}
	if _, err := s.ReleaseHandle("x"); err != ErrHandleNotFound {
		t.Fatalf("want not-found, got %v", err)
	}
}
