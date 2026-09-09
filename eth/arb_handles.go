// The concrete-state side of NODE-04 handle management. The pure HandleStore (in
// package arb) owns the lifecycle — borrow counting, exactly-once backend lease
// release, TTL, identity — but it deliberately does NOT hold a *state.StateDB
// (design §200: "不把 *StateDB 暴露给 RPC 调用者或多个 worker 共用"; the adapter
// attaches the concrete state separately). This file is that attachment: a side
// table keyed by handle id that holds the frozen parent header, its state root,
// and the pinned base *state.StateDB, guarded so a worker takes an isolated Copy
// under the lock and never shares the base.
//
// It lives in package eth to touch *state.StateDB and the blockchain directly.

package eth

import (
	"errors"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/eth/arb"
)

var (
	// errUnknownHandle: a simulate/getPostPoolState named a parent/post handle the
	// side table has no concrete state for (never pinned, or already released).
	errUnknownHandle = errors.New("arb: no concrete state bound for handle")
	// errParentUnreadable: StateAt failed for the requested parent hash — the node
	// cannot back a pin at that root (pruned, or not canonical). Honest failure,
	// never a guessed state (design §182 StateUnavailable).
	errParentUnreadable = errors.New("arb: parent state not readable at requested hash")
	// errUnknownParentHash: the requested parent_hash is not a known header.
	errUnknownParentHash = errors.New("arb: parent_hash is not a known block header")
)

// pinnedState is the concrete state bound to one Parent handle. base is the pinned
// StateDB at header.Root; a worker Copies it under mu (a StateDB.Copy shares the
// underlying reader, so serializing the Copy avoids concurrent lazy-cache races,
// design §200/§204).
type pinnedState struct {
	header *types.Header
	root   common.Hash
	base   *state.StateDB
}

// handleTable is arbService's side table: handle id -> concrete state. The pure
// HandleStore governs whether a handle is Active/borrowable; this table just holds
// the state the store cannot. Both are updated together under the service's use.
type handleTable struct {
	mu     sync.Mutex
	states map[string]*pinnedState
}

func newHandleTable() *handleTable {
	return &handleTable{states: make(map[string]*pinnedState)}
}

func (t *handleTable) put(id string, ps *pinnedState) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.states[id] = ps
}

func (t *handleTable) get(id string) (*pinnedState, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	ps, ok := t.states[id]
	return ps, ok
}

func (t *handleTable) drop(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.states, id)
}

// pinParent implements the concrete side of arb_pinState: resolve the parent
// header by hash, read its state (which pins nothing on path scheme, best-effort;
// pins a GC reference on hash scheme via the backend lease), register a Parent
// handle in the pure store, and bind the concrete base state in the side table.
//
// Returns the handle id, the resolved state root, and the granted TTL. On any
// failure NOTHING is registered (no dangling handle, no half-pin).
func (a *arbService) pinParent(ident arb.HandleIdentity, parentHash common.Hash, ttl time.Duration) (handleID string, root common.Hash, err error) {
	header := a.eth.blockchain.GetHeaderByHash(parentHash)
	if header == nil {
		return "", common.Hash{}, errUnknownParentHash
	}
	base, err := a.eth.blockchain.StateAt(header.Root)
	if err != nil {
		return "", common.Hash{}, errParentUnreadable
	}
	// Pin the backend lease (hash scheme: real GC reference; path scheme: best-
	// effort no-op) BEFORE registering, so a hash-scheme pin failure aborts cleanly.
	backend := a.eth.NewArbStateBackend()
	if backend.Scheme() == arb.SchemeHash {
		if perr := backend.Pin(header.Root); perr != nil {
			return "", common.Hash{}, perr
		}
	}
	id := a.handles.Pin(ident, ttl)
	a.handleStates.put(id, &pinnedState{header: header, root: header.Root, base: base})
	return id, header.Root, nil
}

// releaseParent implements arb_releaseState: request teardown of the handle in the
// pure store; if that returns releaseLease==true (fully drained, this was the
// owning Parent), unpin the backend lease exactly once and drop the concrete state.
// Idempotent: releasing an already-released handle returns released=true with no
// second Unpin (the store latches the single release).
func (a *arbService) releaseParent(handleID string) (released bool, err error) {
	ps, ok := a.handleStates.get(handleID)
	releaseLease, herr := a.handles.ReleaseHandle(handleID)
	if herr != nil {
		// Unknown handle in the store: treat as already-released (idempotent) only
		// if we also have no concrete state; otherwise surface the store error.
		if errors.Is(herr, arb.ErrHandleNotFound) && !ok {
			return true, nil
		}
		return false, herr
	}
	if releaseLease {
		if ok {
			backend := a.eth.NewArbStateBackend()
			if backend.Scheme() == arb.SchemeHash {
				_ = backend.Unpin(ps.root)
			}
			a.handleStates.drop(handleID)
		}
	}
	return true, nil
}

// borrowBase borrows the handle in the pure store (validating Active/owner/boot/
// TTL/identity) and returns an ISOLATED Copy of the pinned base for a worker to
// execute on, plus a release func the worker MUST defer. The Copy is taken under
// the table lock so concurrent borrows never share a base StateDB (§200). The
// release func returns the borrow to the store and, if that drains the handle,
// unpins the lease once.
func (a *arbService) borrowBase(handleID string, ident arb.HandleIdentity) (base *state.StateDB, header *types.Header, release func(), err error) {
	if err := a.handles.Borrow(handleID, ident); err != nil {
		return nil, nil, nil, err
	}
	ps, ok := a.handleStates.get(handleID)
	if !ok {
		// Borrow succeeded in the store but we have no concrete state: release the
		// borrow and fail. This should not happen (put precedes Active), but never
		// hand back a nil base as if it were valid.
		_, _ = a.handles.ReleaseBorrow(handleID)
		return nil, nil, nil, errUnknownHandle
	}
	a.handleStates.mu.Lock()
	isolated := ps.base.Copy()
	a.handleStates.mu.Unlock()

	release = func() {
		releaseLease, rerr := a.handles.ReleaseBorrow(handleID)
		if rerr != nil {
			return
		}
		if releaseLease {
			backend := a.eth.NewArbStateBackend()
			if backend.Scheme() == arb.SchemeHash {
				_ = backend.Unpin(ps.root)
			}
			a.handleStates.drop(handleID)
		}
	}
	return isolated, ps.header, release, nil
}
