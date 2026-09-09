// Package arb contains the scheme-aware read-only state lifecycle policy for the
// backrun simulation service (NODE-00). It is intentionally free of any import of
// the parent `eth` package or of triedb/pathdb internals, so it compiles and unit
// tests standalone; the concrete binding to *eth.Ethereum lives in the parent
// package (eth/arb_backend.go) and implements the Backend interface here.
//
// Honesty contract (design §6.1, verified against eth/state_accessor.go and
// triedb/pathdb):
//
//   - hash scheme: triedb.Reference(root) is a real GC lease. We pin on acquire
//     and dereference on release. retention = Pinned.
//   - path scheme: there is NO pin primitive. Reference/Dereference/Cap return
//     "not supported" for pathdb, and StateDB / state readers re-resolve against
//     the live layer tree on every access. Once the writer flattens the root out
//     of the diff-layer window or marks the disk layer stale, reads fail with
//     errSnapshotStale. Therefore path handles are retention = BestEffort, bound
//     to the current head with a short TTL, and every read must be able to fail
//     (never return a guessed value). We do not claim long-lived pinning we cannot
//     back with a backend lease.
package arb

import (
	"errors"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

// StateScheme mirrors the node's trie storage scheme without importing rawdb,
// keeping this package dependency-light. The adapter maps rawdb.HashScheme /
// rawdb.PathScheme onto these values.
type StateScheme uint8

const (
	SchemeHash StateScheme = iota
	SchemePath
)

func (s StateScheme) String() string {
	switch s {
	case SchemeHash:
		return "hash"
	case SchemePath:
		return "path"
	default:
		return "unknown"
	}
}

// Retention is the strength of the readability guarantee a handle carries.
type Retention uint8

const (
	// RetentionPinned: backed by a real triedb reference (hash scheme). The root
	// stays readable until release regardless of concurrent import.
	RetentionPinned Retention = iota
	// RetentionBestEffort: no backend pin exists (path scheme). Readability is
	// only likely while the root remains within the live layer window and the
	// TTL has not elapsed; every read must tolerate failure.
	RetentionBestEffort
)

func (r Retention) String() string {
	switch r {
	case RetentionPinned:
		return "pinned"
	case RetentionBestEffort:
		return "best_effort"
	default:
		return "unknown"
	}
}

// Errors surfaced to callers. Ambiguity is never hidden: a read that cannot be
// proven fresh fails rather than returning a guessed value.
var (
	// ErrPinUnsupported: caller asked for a pinned lease but the scheme cannot
	// provide one (path scheme).
	ErrPinUnsupported = errors.New("arb: state pin unsupported for this scheme")
	// ErrStateUnavailable: the backend cannot resolve the requested root now.
	ErrStateUnavailable = errors.New("arb: requested state root unavailable")
	// ErrHandleExpired: the best-effort TTL elapsed; caller must re-acquire.
	ErrHandleExpired = errors.New("arb: state handle expired")
	// ErrHandleReleased: the handle was already released.
	ErrHandleReleased = errors.New("arb: state handle already released")
	// ErrStateStale: a previously-acquired best-effort root is no longer
	// readable (flattened/pruned by concurrent import).
	ErrStateStale = errors.New("arb: state root became stale")
	// ErrNotCurrentHead: a live-mode acquire was requested for a root that is
	// not the current ready head (path scheme cannot pin history reliably).
	ErrNotCurrentHead = errors.New("arb: live acquire requires current head root")
)

// Mode selects the acquisition intent.
type Mode uint8

const (
	// ModeLive: must be the current ready head. This is the only mode a path
	// backend can serve with any confidence (short TTL, best effort).
	ModeLive Mode = iota
	// ModeReplay: a historical root; hash scheme can pin it, path scheme only if
	// the backend proves a historic reader (checked via Backend.CanServeHistoric).
	ModeReplay
)

// Backend is the minimal, node-agnostic surface the policy layer needs. The real
// implementation (parent eth package) wraps *eth.Ethereum + blockchain + triedb.
type Backend interface {
	// Scheme reports the live trie scheme.
	Scheme() StateScheme
	// CurrentHeadRoot returns the current canonical head state root and number.
	CurrentHeadRoot() (common.Hash, uint64)
	// Pin places a GC reference on root. MUST error for path scheme.
	Pin(root common.Hash) error
	// Unpin removes a reference previously placed by Pin. MUST be idempotent-safe
	// at the policy layer (we guard double-release ourselves).
	Unpin(root common.Hash) error
	// Readable probes whether root resolves right now (single-shot; may race).
	Readable(root common.Hash) bool
	// CanServeHistoric reports whether the backend can serve the given non-head
	// root from an immutable source (e.g. state history / freezer). Path scheme
	// returns true only when a proven historic reader exists.
	CanServeHistoric(root common.Hash) bool
}

// Clock is injectable for deterministic tests. Monotonic time only.
type Clock func() time.Time

// StateHandle is an acquired, scheme-aware read lease. It does not embed a
// *StateDB — the adapter attaches the concrete state to the caller separately;
// this type owns only the lifetime/retention policy.
type StateHandle struct {
	Root      common.Hash
	Number    uint64
	Scheme    StateScheme
	Retention Retention
	Mode      Mode

	backend  Backend
	clock    Clock
	expires  time.Time
	pinned   bool // a real backend Pin is held (hash scheme only)
	released bool
}

// AcquireConfig bounds a best-effort acquisition.
type AcquireConfig struct {
	// TTL for best-effort (path) handles. Ignored for pinned handles beyond
	// being an upper bound the caller may still enforce. Must be > 0 for path.
	TTL time.Duration
}

// Acquire builds a scheme-aware handle for root at the given mode.
//
//   - hash + (live or replay): pin via backend.Pin, retention = Pinned.
//   - path + live: root must equal current head; retention = BestEffort, TTL bound.
//   - path + replay: only if backend.CanServeHistoric(root); retention = BestEffort.
//
// Never returns a handle whose readability it cannot at least probe.
func Acquire(b Backend, clock Clock, root common.Hash, mode Mode, cfg AcquireConfig) (*StateHandle, error) {
	if !b.Readable(root) {
		// For replay on path, a historic reader may still serve it even if the
		// live layer tree does not; consult the backend before giving up.
		if !(b.Scheme() == SchemePath && mode == ModeReplay && b.CanServeHistoric(root)) {
			return nil, ErrStateUnavailable
		}
	}

	h := &StateHandle{
		Root:    root,
		Scheme:  b.Scheme(),
		Mode:    mode,
		backend: b,
		clock:   clock,
	}

	switch b.Scheme() {
	case SchemeHash:
		// Real lease available in both modes.
		if err := b.Pin(root); err != nil {
			return nil, err
		}
		h.pinned = true
		h.Retention = RetentionPinned
		// Pinned handles have no intrinsic expiry; keep zero expires.

	case SchemePath:
		if cfg.TTL <= 0 {
			return nil, errors.New("arb: path handle requires positive TTL")
		}
		switch mode {
		case ModeLive:
			// Path cannot pin; only the current head is defensible.
			headRoot, headNum := b.CurrentHeadRoot()
			if headRoot != root {
				return nil, ErrNotCurrentHead
			}
			h.Number = headNum
		case ModeReplay:
			if !b.CanServeHistoric(root) {
				return nil, ErrStateUnavailable
			}
		}
		h.Retention = RetentionBestEffort
		h.expires = clock().Add(cfg.TTL)

	default:
		return nil, errors.New("arb: unknown scheme")
	}

	return h, nil
}

// CheckFresh must be called before every use of the underlying state. For pinned
// handles it only guards release; for best-effort handles it enforces TTL and
// re-probes readability, failing (never guessing) if the root went stale.
func (h *StateHandle) CheckFresh() error {
	if h.released {
		return ErrHandleReleased
	}
	if h.Retention == RetentionPinned {
		return nil
	}
	// Best-effort: TTL then staleness probe.
	if !h.expires.IsZero() && !h.clock().Before(h.expires) {
		return ErrHandleExpired
	}
	if !h.backend.Readable(h.Root) {
		if h.Mode == ModeReplay && h.backend.CanServeHistoric(h.Root) {
			return nil
		}
		return ErrStateStale
	}
	return nil
}

// Release drops the lease. Idempotent: a second call is a no-op. Only a real pin
// triggers a backend Unpin (guards against double-Dereference).
func (h *StateHandle) Release() {
	if h.released {
		return
	}
	h.released = true
	if h.pinned {
		_ = h.backend.Unpin(h.Root)
		h.pinned = false
	}
}

// IsReleased reports whether Release has been called.
func (h *StateHandle) IsReleased() bool { return h.released }
