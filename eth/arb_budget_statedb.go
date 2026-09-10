// Budgeted vm.StateDB wrapper (NODE shared infra adapter, design §379-381/§399).
// It binds the node-agnostic ReadBudget (eth/arb/budget_reader.go) onto a real
// state.StateDB so that logical state reads issued by the EVM are counted against
// max_state_reads, and a budget-exhausted simulation is cooperatively cancelled and
// FAILED — never allowed to return a fabricated value as a successful read.
//
// Why a wrapper and not a tracing hook (design §379): tracing.Hooks has
// OnStorageChange but NO OnStorageRead / read-count / read-byte callback, so hooks
// cannot implement max_state_reads. We wrap vm.StateDB and count the logical read
// methods explicitly. The same budget also backs the typed PoolReader.
//
// Honesty contract (design §381/§399):
//   - The counted read methods (GetState, GetBalance, GetCode, ...) do NOT return an
//     error in the vm.StateDB interface, so on over-budget we CANNOT surface an
//     error from the read itself. Instead we latch budget failure and call the EVM
//     cancel func, so the interpreter stops at its next cooperative check. For the
//     read already in flight we return the REAL underlying value — we never
//     fabricate a zero and call it success (§381).
//   - Logical read count != disk IO count; a cached read still counts one. The
//     wrapper bounds admitted logical work, not latency, and cannot abort an
//     already-blocked disk read (documented limitation, §381).
//   - After the run, the caller MUST check Budget().Failed() (and evm.Cancelled(),
//     ctx, StateDB.Error) and reject the job if any is set, so a cancel is never
//     mistaken for success (§399).
package eth

import (
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/eth/arb"
	"github.com/holiman/uint256"
)

// budgetedStateDB embeds a real vm.StateDB and intercepts the logical read methods
// to charge a ReadBudget. All non-read methods are promoted from the embedded
// interface unchanged. It is owned by a single EVM worker (not concurrent), but the
// cancel latch is guarded so a bounded-frequency OnOpcode check can also trip it.
type budgetedStateDB struct {
	vm.StateDB // embedded real state; promoted methods pass through

	budget *arb.ReadBudget

	mu        sync.Mutex
	cancel    func() // set post-construction (EVM created after statedb)
	cancelled bool
}

// newBudgetedStateDB wraps base with the given budget. The EVM cancel func is
// attached later via SetCancel, because vm.NewEVM(blockCtx, statedb, ...) needs the
// statedb before the EVM exists.
func newBudgetedStateDB(base vm.StateDB, budget *arb.ReadBudget) *budgetedStateDB {
	return &budgetedStateDB{StateDB: base, budget: budget}
}

// SetCancel attaches the EVM's cancel func (design §399: each worker holds the
// current EVM cancel function). Safe to call once after NewEVM.
func (s *budgetedStateDB) SetCancel(cancel func()) {
	s.mu.Lock()
	s.cancel = cancel
	s.mu.Unlock()
}

// Budget exposes the underlying budget so the worker can check Failed()/Err() after
// the run.
func (s *budgetedStateDB) Budget() *arb.ReadBudget { return s.budget }

// charge accounts one logical read; on failure it latches cooperative cancel exactly
// once. It never changes the value the caller returns — callers still return the
// real underlying read (no fabricated zero).
func (s *budgetedStateDB) charge(kind arb.ReadKind) {
	if err := s.budget.ChargeRead(kind); err != nil {
		s.mu.Lock()
		if !s.cancelled && s.cancel != nil {
			s.cancel()
			s.cancelled = true
		}
		s.mu.Unlock()
	}
}

// ---- counted logical reads (real value always returned; budget latches on overflow) ----

func (s *budgetedStateDB) GetBalance(addr common.Address) *uint256.Int {
	s.charge(arb.ReadBalance)
	return s.StateDB.GetBalance(addr)
}

func (s *budgetedStateDB) GetNonce(addr common.Address) uint64 {
	s.charge(arb.ReadNonce)
	return s.StateDB.GetNonce(addr)
}

func (s *budgetedStateDB) GetCodeHash(addr common.Address) common.Hash {
	s.charge(arb.ReadCode)
	return s.StateDB.GetCodeHash(addr)
}

func (s *budgetedStateDB) GetCode(addr common.Address) []byte {
	s.charge(arb.ReadCode)
	return s.StateDB.GetCode(addr)
}

func (s *budgetedStateDB) GetCodeSize(addr common.Address) int {
	s.charge(arb.ReadCode)
	return s.StateDB.GetCodeSize(addr)
}

func (s *budgetedStateDB) GetState(addr common.Address, slot common.Hash) common.Hash {
	s.charge(arb.ReadStorage)
	return s.StateDB.GetState(addr, slot)
}

// GetStateAndCommittedState yields both the dirty and committed value from one
// logical access; charged once as a committed-state read.
func (s *budgetedStateDB) GetStateAndCommittedState(addr common.Address, slot common.Hash) (common.Hash, common.Hash) {
	s.charge(arb.ReadCommitted)
	return s.StateDB.GetStateAndCommittedState(addr, slot)
}

func (s *budgetedStateDB) GetStorageRoot(addr common.Address) common.Hash {
	s.charge(arb.ReadStorageRoot)
	return s.StateDB.GetStorageRoot(addr)
}

// compile-time assertion that the wrapper still satisfies vm.StateDB.
var _ vm.StateDB = (*budgetedStateDB)(nil)
