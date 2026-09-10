package arb

import (
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

// mockBackend is a controllable Backend for policy tests. No node, no triedb.
type mockBackend struct {
	scheme      StateScheme
	headRoot    common.Hash
	headNum     uint64
	readable    map[common.Hash]bool
	historic    map[common.Hash]bool
	pinned      map[common.Hash]int // net pin count
	pinErr      error
}

func newMock(scheme StateScheme) *mockBackend {
	return &mockBackend{
		scheme:   scheme,
		readable: make(map[common.Hash]bool),
		historic: make(map[common.Hash]bool),
		pinned:   make(map[common.Hash]int),
	}
}

func (m *mockBackend) Scheme() StateScheme                 { return m.scheme }
func (m *mockBackend) CurrentHeadRoot() (common.Hash, uint64) { return m.headRoot, m.headNum }
func (m *mockBackend) Readable(root common.Hash) bool      { return m.readable[root] }
func (m *mockBackend) CanServeHistoric(root common.Hash) bool { return m.historic[root] }

func (m *mockBackend) Pin(root common.Hash) error {
	if m.pinErr != nil {
		return m.pinErr
	}
	m.pinned[root]++
	return nil
}

func (m *mockBackend) Unpin(root common.Hash) error {
	m.pinned[root]--
	return nil
}

func h(b byte) common.Hash {
	var out common.Hash
	out[0] = b
	return out
}

// fixedClock returns a controllable clock.
type fixedClock struct{ now time.Time }

func (c *fixedClock) Clock() Clock       { return func() time.Time { return c.now } }
func (c *fixedClock) advance(d time.Duration) { c.now = c.now.Add(d) }

func TestHashSchemePinsAndReleases(t *testing.T) {
	m := newMock(SchemeHash)
	root := h(1)
	m.readable[root] = true
	clk := &fixedClock{now: time.Unix(1000, 0)}

	handle, err := Acquire(m, clk.Clock(), root, ModeLive, AcquireConfig{})
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if handle.Retention != RetentionPinned {
		t.Fatalf("want pinned, got %s", handle.Retention)
	}
	if m.pinned[root] != 1 {
		t.Fatalf("want 1 pin, got %d", m.pinned[root])
	}
	// Fresh regardless of time (no TTL for pinned).
	clk.advance(time.Hour)
	if err := handle.CheckFresh(); err != nil {
		t.Fatalf("pinned should stay fresh: %v", err)
	}
	handle.Release()
	if m.pinned[root] != 0 {
		t.Fatalf("want deref to 0, got %d", m.pinned[root])
	}
	// Double release is a no-op (no double-deref).
	handle.Release()
	if m.pinned[root] != 0 {
		t.Fatalf("double release must not deref again, got %d", m.pinned[root])
	}
	if err := handle.CheckFresh(); err != ErrHandleReleased {
		t.Fatalf("want released error, got %v", err)
	}
}

func TestPathLiveRequiresCurrentHead(t *testing.T) {
	m := newMock(SchemePath)
	head := h(2)
	other := h(3)
	m.headRoot, m.headNum = head, 100
	m.readable[head] = true
	m.readable[other] = true
	clk := &fixedClock{now: time.Unix(1000, 0)}

	// Non-head live acquire rejected.
	if _, err := Acquire(m, clk.Clock(), other, ModeLive, AcquireConfig{TTL: time.Second}); err != ErrNotCurrentHead {
		t.Fatalf("want ErrNotCurrentHead, got %v", err)
	}
	// Head live acquire succeeds, best-effort, never pins.
	handle, err := Acquire(m, clk.Clock(), head, ModeLive, AcquireConfig{TTL: time.Second})
	if err != nil {
		t.Fatalf("acquire head: %v", err)
	}
	if handle.Retention != RetentionBestEffort {
		t.Fatalf("path must be best-effort, got %s", handle.Retention)
	}
	if len(m.pinned) != 0 {
		t.Fatalf("path must never pin")
	}
	if handle.Number != 100 {
		t.Fatalf("want head number 100, got %d", handle.Number)
	}
}

func TestPathTTLExpiry(t *testing.T) {
	m := newMock(SchemePath)
	head := h(4)
	m.headRoot = head
	m.readable[head] = true
	clk := &fixedClock{now: time.Unix(1000, 0)}

	handle, err := Acquire(m, clk.Clock(), head, ModeLive, AcquireConfig{TTL: 500 * time.Millisecond})
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := handle.CheckFresh(); err != nil {
		t.Fatalf("should be fresh immediately: %v", err)
	}
	clk.advance(500 * time.Millisecond) // reach expiry boundary
	if err := handle.CheckFresh(); err != ErrHandleExpired {
		t.Fatalf("want expired at TTL boundary, got %v", err)
	}
}

func TestPathStaleDetectedBeforeExpiry(t *testing.T) {
	m := newMock(SchemePath)
	head := h(5)
	m.headRoot = head
	m.readable[head] = true
	clk := &fixedClock{now: time.Unix(1000, 0)}

	handle, _ := Acquire(m, clk.Clock(), head, ModeLive, AcquireConfig{TTL: time.Minute})
	// Concurrent import flattens root out of the live tree: no longer readable.
	m.readable[head] = false
	if err := handle.CheckFresh(); err != ErrStateStale {
		t.Fatalf("want stale (not a guessed value), got %v", err)
	}
}

func TestPathReplayNeedsHistoricReader(t *testing.T) {
	m := newMock(SchemePath)
	root := h(6)
	// Not in live tree, no historic reader -> unavailable.
	clk := &fixedClock{now: time.Unix(1000, 0)}
	if _, err := Acquire(m, clk.Clock(), root, ModeReplay, AcquireConfig{TTL: time.Second}); err != ErrStateUnavailable {
		t.Fatalf("want unavailable, got %v", err)
	}
	// With a proven historic reader, replay acquire succeeds best-effort.
	m.historic[root] = true
	handle, err := Acquire(m, clk.Clock(), root, ModeReplay, AcquireConfig{TTL: time.Second})
	if err != nil {
		t.Fatalf("historic replay acquire: %v", err)
	}
	if handle.Retention != RetentionBestEffort {
		t.Fatalf("want best-effort, got %s", handle.Retention)
	}
	// Historic root stays fresh even if not in the live layer tree.
	if err := handle.CheckFresh(); err != nil {
		t.Fatalf("historic should stay fresh: %v", err)
	}
}

func TestHashPinErrorPropagates(t *testing.T) {
	m := newMock(SchemeHash)
	root := h(7)
	m.readable[root] = true
	m.pinErr = ErrPinUnsupported
	clk := &fixedClock{now: time.Unix(1000, 0)}
	if _, err := Acquire(m, clk.Clock(), root, ModeLive, AcquireConfig{}); err != ErrPinUnsupported {
		t.Fatalf("want pin error propagated, got %v", err)
	}
}

func TestUnreadableRootRejected(t *testing.T) {
	m := newMock(SchemeHash)
	root := h(8) // not readable
	clk := &fixedClock{now: time.Unix(1000, 0)}
	if _, err := Acquire(m, clk.Clock(), root, ModeLive, AcquireConfig{}); err != ErrStateUnavailable {
		t.Fatalf("want unavailable, got %v", err)
	}
}
