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

// returnerCode builds bytecode for a contract that ignores calldata and returns the
// given data bytes (padded/stored word-aligned). Bytecode:
//   for each 32-byte chunk i: PUSH32 <chunk> PUSH1 <32*i> MSTORE
//   PUSH2 <len> PUSH1 0 RETURN
func returnerCode(data []byte) []byte {
	padded := make([]byte, (len(data)+31)/32*32)
	copy(padded, data)
	code := []byte{}
	for i := 0; i < len(padded); i += 32 {
		code = append(code, 0x7f) // PUSH32
		code = append(code, padded[i:i+32]...)
		code = append(code, 0x60, byte(i)) // PUSH1 offset
		code = append(code, 0x52)          // MSTORE
	}
	// PUSH2 len ; PUSH1 0 ; RETURN
	code = append(code, 0x61, byte(len(padded)>>8), byte(len(padded)))
	code = append(code, 0x60, 0x00, 0xf3)
	return code
}

// deployReturner deploys returnerCode(data) at addr.
func deployReturner(sdb *state.StateDB, addr common.Address, data []byte) {
	sdb.CreateAccount(addr)
	sdb.SetCode(addr, returnerCode(data), tracing.CodeChangeUnspecified)
}

// deploySstore deploys a contract that does SSTORE(0,1) then STOP — used to prove
// StaticCall forbids writes.
func deploySstore(sdb *state.StateDB, addr common.Address) {
	code := []byte{0x60, 0x01, 0x60, 0x00, 0x55, 0x00} // PUSH1 1 PUSH1 0 SSTORE STOP
	sdb.CreateAccount(addr)
	sdb.SetCode(addr, code, tracing.CodeChangeUnspecified)
}

func testPoolCaller(t *testing.T, sdb *state.StateDB, budget *arb.ReadBudget) *poolCaller {
	t.Helper()
	wrapped := newBudgetedStateDB(sdb, budget)
	blockCtx := vm.BlockContext{
		CanTransfer: func(vm.StateDB, common.Address, *uint256.Int) bool { return true },
		Transfer:    func(vm.StateDB, common.Address, common.Address, *uint256.Int) {},
		GetHash:     func(uint64) common.Hash { return common.Hash{} },
		BlockNumber: big.NewInt(1),
		Time:        1,
		Difficulty:  big.NewInt(1),
		GasLimit:    30_000_000,
		BaseFee:     big.NewInt(0),
	}
	evm := vm.NewEVM(blockCtx, wrapped, params.TestChainConfig, vm.Config{})
	wrapped.SetCancel(evm.Cancel)
	return &poolCaller{evm: evm, wrapped: wrapped, caller: common.BytesToAddress([]byte("t")), gasCap: viewGas}
}

func w32(v *big.Int) []byte {
	b := v.Bytes()
	out := make([]byte, 32)
	copy(out[32-len(b):], b)
	return out
}

// TestReadV2EndToEnd deploys a mock pool + two mock tokens and verifies the full
// StaticCall -> ABI decode path over the budgeted wrapper on a real EVM.
func TestReadV2EndToEnd(t *testing.T) {
	sdb, err := state.New(types.EmptyRootHash, state.NewDatabaseForTesting())
	if err != nil {
		t.Fatal(err)
	}
	pool := common.BytesToAddress([]byte("pool"))
	tok0 := common.BytesToAddress([]byte("tok0"))
	tok1 := common.BytesToAddress([]byte("tok1"))

	r0 := big.NewInt(1_000_000)
	r1 := big.NewInt(4_000_000)
	// getReserves returns 3 words
	reservesRet := append(append(w32(r0), w32(r1)...), w32(big.NewInt(1700000000))...)
	deployReturner(sdb, pool, reservesRet)
	// tokens return balanceOf == reserves (so §9.3 balances==reserves holds)
	deployReturner(sdb, tok0, w32(r0))
	deployReturner(sdb, tok1, w32(r1))

	budget := arb.NewReadBudget(func() time.Time { return time.Now() }, 1000, 0)
	pc := testPoolCaller(t, sdb, budget)

	snap, err := pc.ReadV2(pool, tok0, tok1)
	if err != nil {
		t.Fatalf("ReadV2: %v", err)
	}
	if snap.Reserve0.Cmp(r0) != 0 || snap.Reserve1.Cmp(r1) != 0 {
		t.Fatalf("reserves wrong: %v %v", snap.Reserve0, snap.Reserve1)
	}
	if snap.Balance0.Cmp(r0) != 0 || snap.Balance1.Cmp(r1) != 0 {
		t.Fatalf("balances wrong: %v %v", snap.Balance0, snap.Balance1)
	}
	// 3 reads charged (getReserves + 2 balanceOf), none should trip budget.
	if budget.Failed() {
		t.Fatal("budget must not be tripped")
	}
}

// TestStaticCallForbidsWrites proves the read-only guarantee (§6.1): a contract that
// attempts SSTORE under StaticCall must fail, and our staticCall surfaces the error
// rather than silently succeeding.
func TestStaticCallForbidsWrites(t *testing.T) {
	sdb, err := state.New(types.EmptyRootHash, state.NewDatabaseForTesting())
	if err != nil {
		t.Fatal(err)
	}
	writer := common.BytesToAddress([]byte("writer"))
	deploySstore(sdb, writer)

	budget := arb.NewReadBudget(func() time.Time { return time.Now() }, 1000, 0)
	pc := testPoolCaller(t, sdb, budget)

	// A write attempt under StaticCall must revert; staticCall reports it as failed.
	if _, err := pc.staticCall(writer, []byte{0x00, 0x00, 0x00, 0x00}); err != ErrViewCallReverted {
		t.Fatalf("SSTORE under StaticCall must fail as reverted, got %v", err)
	}
}

// TestReadV2BudgetTripSurfacesError proves that if the read budget trips mid-read,
// ReadV2 fails rather than returning a partial/fabricated snapshot.
func TestReadV2BudgetTripSurfacesError(t *testing.T) {
	sdb, err := state.New(types.EmptyRootHash, state.NewDatabaseForTesting())
	if err != nil {
		t.Fatal(err)
	}
	pool := common.BytesToAddress([]byte("pool"))
	tok0 := common.BytesToAddress([]byte("tok0"))
	tok1 := common.BytesToAddress([]byte("tok1"))
	deployReturner(sdb, pool, append(append(w32(big.NewInt(1)), w32(big.NewInt(2))...), w32(big.NewInt(3))...))
	deployReturner(sdb, tok0, w32(big.NewInt(1)))
	deployReturner(sdb, tok1, w32(big.NewInt(2)))

	// The returner code does an MSTORE-heavy path but no SLOAD; the counted reads
	// are GetCode on the callee (charged once per call via GetCode). Set a cap of 1
	// so the second call trips the budget.
	budget := arb.NewReadBudget(func() time.Time { return time.Now() }, 1, 0)
	pc := testPoolCaller(t, sdb, budget)

	_, err = pc.ReadV2(pool, tok0, tok1)
	if err != ErrReadBudgetTripped {
		t.Fatalf("want budget tripped error, got %v", err)
	}
}
