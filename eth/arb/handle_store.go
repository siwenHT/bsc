// HandleStore lifecycle core (NODE shared infra, design §6.2). This is the
// node-agnostic borrow/refcount/status state machine that guarantees a backend
// state lease is released EXACTLY ONCE. It imports nothing from the parent eth
// package, so it compiles and unit-tests standalone (no CGO). The concrete binding
// (real StateDB.Copy under the per-handle lock, the actual backend Dereference)
// lives in the parent package (eth/arb_handles.go, a later slice); this core only
// owns the lifetime policy and tells the adapter WHEN to release the backend lease.
//
// Correctness contract (design §6.2 / §204, the trickiest surface in the service):
//
//   - Only a Parent handle carries a real backend lease. A PostTarget child handle
//     shares the parent's lease by reference count; while a child exists, releasing
//     the parent does NOT free the backend (§202).
//   - The backend lease is Dereferenced EXACTLY ONCE, when the owning Parent handle
//     is no longer Active AND its borrows AND its children have both reached zero.
//   - A second Release is idempotent: it never triggers a second Dereference (§204).
//   - Borrow validates Active + owner/boot/identity + TTL under the store lock;
//     once a handle is Closing (Release called or TTL elapsed) new borrows are
//     rejected, existing borrows drain (§204).
//
// The core returns a boolean "release the backend lease now" from the transitions
// that can make it true (ReleaseBorrow, ReleaseHandle, ReleaseChild). The adapter
// performs the single Dereference on a true return. Because the flag is latched in
// the entry (leaseReleased), the core can never signal it twice.
package arb

import (
	"errors"
	"time"
)

// HandleKind distinguishes a real parent-state handle from a post-target child.
type HandleKind uint8

const (
	// KindParent: the true parent-block final state; carries the backend lease.
	KindParent HandleKind = iota
	// KindPostTarget: created after the target tx succeeds (Finalise + error
	// checks); an in-memory post-state built on the parent's leased base. Shares
	// the parent's backend lease via refcount; holds no lease of its own.
	KindPostTarget
)

func (k HandleKind) String() string {
	switch k {
	case KindParent:
		return "parent"
	case KindPostTarget:
		return "post_target"
	default:
		return "unknown"
	}
}

// HandleStatus is the lifecycle state of a handle entry.
type HandleStatus uint8

const (
	// StatusActive: borrowable subject to identity/TTL checks.
	StatusActive HandleStatus = iota
	// StatusClosing: Release requested or TTL elapsed; new borrows rejected,
	// existing borrows/children drain before the backend lease is freed.
	StatusClosing
	// StatusReleased: fully torn down; the backend lease (if any) was Dereferenced.
	StatusReleased
)

func (s HandleStatus) String() string {
	switch s {
	case StatusActive:
		return "active"
	case StatusClosing:
		return "closing"
	case StatusReleased:
		return "released"
	default:
		return "unknown"
	}
}

// HandleIdentity binds a handle to its owner. Borrow must present a matching
// identity; a mismatch is rejected so one session cannot borrow another's handle
// (design §204 "验证...owner/boot/identity", wire arb_releaseState "不释放其他
// session句柄").
type HandleIdentity struct {
	OwnerSession string
	NodeBootID   string
	Identity     string // opaque binding digest (e.g. request/session identity)
}

// Errors surfaced by the store.
var (
	ErrHandleNotFound     = errors.New("arb: handle not found")
	ErrHandleNotActive    = errors.New("arb: handle not active (closing or released)")
	ErrIdentityMismatch   = errors.New("arb: handle identity mismatch")
	ErrHandleTTLElapsed   = errors.New("arb: handle TTL elapsed")
	ErrParentNotActive    = errors.New("arb: parent handle not active for child creation")
	ErrChildOnNonParent   = errors.New("arb: child handles may only extend a parent handle")
	ErrNoBorrowToRelease  = errors.New("arb: no active borrow to release")
	ErrNoChildToRelease   = errors.New("arb: no child to release")
)

type handleEntry struct {
	id       string
	kind     HandleKind
	ident    HandleIdentity
	parentID string // "" for KindParent
	expires  time.Time
	status   HandleStatus

	borrows  int
	children int
	// leaseReleased latches once the backend lease has been signaled for release,
	// so the core never signals a second Dereference.
	leaseReleased bool
}

// HandleStore owns all handle entries and their lifecycle. Not safe for concurrent
// use; the adapter serializes calls under its own lock (the design's HandleStore
// mutex). The clock is injectable for deterministic TTL tests.
type HandleStore struct {
	clock   Clock
	newID   func() string
	entries map[string]*handleEntry
}

// NewHandleStore builds a store with an injected clock and id generator. The id
// generator must produce unique opaque ids; the adapter supplies a crypto-random
// 32-byte (id32) generator, tests supply a counter.
func NewHandleStore(clock Clock, newID func() string) *HandleStore {
	return &HandleStore{
		clock:   clock,
		newID:   newID,
		entries: make(map[string]*handleEntry),
	}
}

// Pin registers a new parent-state handle and returns its id. The caller (adapter)
// has already acquired the backend lease for this base state; the store tracks its
// lifetime and will signal exactly one release when the handle fully drains.
func (s *HandleStore) Pin(ident HandleIdentity, ttl time.Duration) string {
	id := s.newID()
	s.entries[id] = &handleEntry{
		id:      id,
		kind:    KindParent,
		ident:   ident,
		expires: s.clock().Add(ttl),
		status:  StatusActive,
	}
	return id
}

// CreateChild registers a PostTarget child of an Active parent. It increments the
// parent's child count so the parent's backend lease stays alive until the child
// is released (§202). Returns the child id. The parent must be Active and of kind
// Parent.
func (s *HandleStore) CreateChild(parentID string, ident HandleIdentity, ttl time.Duration) (string, error) {
	p, ok := s.entries[parentID]
	if !ok {
		return "", ErrHandleNotFound
	}
	if p.kind != KindParent {
		return "", ErrChildOnNonParent
	}
	if p.status != StatusActive {
		return "", ErrParentNotActive
	}
	id := s.newID()
	s.entries[id] = &handleEntry{
		id:       id,
		kind:     KindPostTarget,
		ident:    ident,
		parentID: parentID,
		expires:  s.clock().Add(ttl),
		status:   StatusActive,
	}
	p.children++
	return id, nil
}

// Borrow validates the handle is Active, unexpired, and identity-matched, then
// increments its borrow count. On success the caller may (under its own lock)
// Copy the base state and run a worker, then MUST call ReleaseBorrow. Rejects with
// a specific error otherwise; TTL elapse also flips the handle to Closing.
func (s *HandleStore) Borrow(id string, ident HandleIdentity) error {
	e, ok := s.entries[id]
	if !ok {
		return ErrHandleNotFound
	}
	if e.status != StatusActive {
		return ErrHandleNotActive
	}
	if !s.clock().Before(e.expires) {
		// TTL elapsed: flip to Closing so no further borrows are admitted.
		e.status = StatusClosing
		return ErrHandleTTLElapsed
	}
	if e.ident != ident {
		return ErrIdentityMismatch
	}
	e.borrows++
	return nil
}

// ReleaseBorrow decrements the borrow count and returns whether the backend lease
// should be released now (the handle is draining and this was the last thing
// keeping it alive). The adapter performs the single Dereference on true.
func (s *HandleStore) ReleaseBorrow(id string) (releaseLease bool, err error) {
	e, ok := s.entries[id]
	if !ok {
		return false, ErrHandleNotFound
	}
	if e.borrows == 0 {
		return false, ErrNoBorrowToRelease
	}
	e.borrows--
	return s.maybeRelease(e), nil
}

// ReleaseHandle requests teardown of a handle: it flips Active -> Closing and, if
// nothing is borrowing it and it has no children, releases the backend lease now.
// Idempotent: a second call on a Closing/Released handle returns false (no double
// Dereference). For a PostTarget child, releasing it decrements the parent's child
// count and may in turn release the parent's lease.
func (s *HandleStore) ReleaseHandle(id string) (releaseLease bool, err error) {
	e, ok := s.entries[id]
	if !ok {
		return false, ErrHandleNotFound
	}
	if e.status == StatusReleased {
		return false, nil // already torn down; no second release
	}
	if e.status == StatusActive {
		e.status = StatusClosing
	}
	return s.maybeRelease(e), nil
}

// maybeRelease checks whether entry e is fully drained (Closing/Released, zero
// borrows, zero children) and, if so and not already released, latches the release
// and cascades to the parent for a PostTarget child. Returns true iff THIS call is
// the one that should Dereference a real backend lease (only a Parent handle owns
// one; a child returning true here means the cascade freed the parent).
func (s *HandleStore) maybeRelease(e *handleEntry) bool {
	if e.status == StatusActive || e.borrows != 0 || e.children != 0 || e.leaseReleased {
		return false
	}
	e.leaseReleased = true
	e.status = StatusReleased

	if e.kind == KindPostTarget {
		// A child holds no backend lease of its own; releasing it decrements the
		// parent's child count and may free the parent's lease.
		if p, ok := s.entries[e.parentID]; ok && p.children > 0 {
			p.children--
			return s.maybeRelease(p)
		}
		return false
	}
	// Parent handle: this is the single real backend-lease release.
	return true
}

// Status returns a handle's current status (for observability/tests).
func (s *HandleStore) Status(id string) (HandleStatus, bool) {
	e, ok := s.entries[id]
	if !ok {
		return StatusReleased, false
	}
	return e.status, true
}

// Borrows / Children expose current counts (tests/observability).
func (s *HandleStore) Borrows(id string) int {
	if e, ok := s.entries[id]; ok {
		return e.borrows
	}
	return 0
}

func (s *HandleStore) Children(id string) int {
	if e, ok := s.entries[id]; ok {
		return e.children
	}
	return 0
}
