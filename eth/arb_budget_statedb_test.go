package eth

import (
	"math/big"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/eth/arb"
	"github.com/ethereum/go-ethereum/params"
	"github.com/holiman/uint256"
)

// realClock for budget wall-clock (we use no deadline here, so it is only nominal).
func realClock() arb.Clock { return func() time.Time { return time.Now() } }

// newTestStateWithSloadContract builds an in-memory StateDB and deploys, at addr, a
// contract whose code performs `sloads` SLOAD operations then STOP. Returns the
// concrete state and the contract address.
//
// bytecode per SLOAD: PUSH1 0x00 (0x6000) SLOAD (0x54) => "6000 54". Repeated, then
// STOP (0x00). Each SLOAD invokes StateDB.GetState once.
func newTestStateWithSloadContract(t *testing.T, sloads int) (*state.StateDB, common.Address) {
	t.Helper()
	sdb, err := state.New(types.EmptyRootHash, state.NewDatabaseForTesting())
	if err != nil {
		t.Fatalf("state.New: %v", err)
	}
	code := make([]byte, 0, sloads*3+1)
	for i := 0; i < sloads; i++ {
		code = append(code, 0x60, 0x00, 0x54) // PUSH1 0x00 ; SLOAD
		code = append(code, 0x50)             // POP (keep stack bounded)
	}
	code = append(code, 0x00) // STOP
	addr := common.BytesToAddress([]byte("sload-contract"))
	sdb.CreateAccount(addr)
	sdb.SetCode(addr, code, tracing.CodeChangeUnspecified)
	return sdb, addr
}

// runEVMCall builds an EVM over the given (wrapped) StateDB and Calls addr. Returns
// the wrapper so the test can inspect the budget and cancel latch.
func runEVMCall(t *testing.T, base vm.StateDB, addr common.Address, budget *arb.ReadBudget) *budgetedStateDB {
	t.Helper()
	wrapped := newBudgetedStateDB(base, budget)

	blockCtx := vm.BlockContext{
		CanTransfer: func(vm.StateDB, common.Address, *uint256.Int) bool { return true },
		Transfer:    func(vm.StateDB, common.Address, common.Address, *uint256.Int) {},
		GetHash:     func(uint64) common.Hash { return common.Hash{} },
		Coinbase:    common.Address{},
		BlockNumber: big.NewInt(1),
		Time:        1,
		Difficulty:  big.NewInt(1),
		GasLimit:    30_000_000,
		BaseFee:     big.NewInt(0),
	}
	evm := vm.NewEVM(blockCtx, wrapped, params.TestChainConfig, vm.Config{})
	wrapped.SetCancel(evm.Cancel)

	origin := common.BytesToAddress([]byte("origin"))
	// A cancelled EVM legitimately returns an abort error, so we do NOT fatal on
	// err here; each test asserts on the budget/cancel state instead. The point of
	// this test is that the wrapper survives a real EVM run and counts/cancels
	// correctly, not that the call always succeeds.
	_, _, _ = evm.Call(origin, addr, nil, 10_000_000, uint256.NewInt(0))
	return wrapped
}

// TestBudgetedStateDBCountsRealSloads proves the wrapper survives a real EVM run
// (no internal *state.StateDB type-assertion breaks it) and that each SLOAD is
// charged as a logical storage read.
func TestBudgetedStateDBCountsRealSloads(t *testing.T) {
	const sloads = 5
	sdb, addr := newTestStateWithSloadContract(t, sloads)
	budget := arb.NewReadBudget(realClock(), 1_000_000, 0) // effectively unbounded
	w := runEVMCall(t, sdb, addr, budget)

	if got := w.Budget().ReadsOfKind(arb.ReadStorage); got != sloads {
		t.Fatalf("want %d storage reads charged, got %d", sloads, got)
	}
	if w.Budget().Failed() {
		t.Fatal("unbounded budget must not be failed")
	}
}

// TestBudgetedStateDBLatchesCancelOnOverflow proves that exceeding max_state_reads
// latches the budget failure AND cooperatively cancels the EVM — without ever
// fabricating a value (the reads that ran returned real state; the job is failed by
// the latched budget, not by a faked zero).
func TestBudgetedStateDBLatchesCancelOnOverflow(t *testing.T) {
	const sloads = 10
	sdb, addr := newTestStateWithSloadContract(t, sloads)
	// Cap below the number of storage reads so the budget trips mid-run.
	budget := arb.NewReadBudget(realClock(), 3, 0)
	w := runEVMCall(t, sdb, addr, budget)

	if !w.Budget().Failed() {
		t.Fatal("over-budget run must latch budget failure")
	}
	if w.Budget().Err() != arb.ErrReadBudgetExceeded {
		t.Fatalf("want ErrReadBudgetExceeded, got %v", w.Budget().Err())
	}
	if !w.cancelled {
		t.Fatal("over-budget must have invoked EVM cancel")
	}
	// The cap itself must not be exceeded in the counter (never overshoots).
	if w.Budget().Reads() != 3 {
		t.Fatalf("counter must stop at cap 3, got %d", w.Budget().Reads())
	}
}
