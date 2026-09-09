package eth

import (
	"math/big"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/eth/arb"
	"github.com/ethereum/go-ethereum/params"
	"github.com/holiman/uint256"
)

// TestApplyMessageThroughBudgetedWrapper is the write-path risk probe promised in
// the NODE-03 plan. StaticCall (read-only) already survives the wrapper; this proves
// the FULL state-mutating path (ApplyMessage -> SSTORE -> journal/snapshot ->
// Finalise) also survives it — i.e. geth does NOT type-assert to a concrete
// *state.StateDB on the write path in a way that breaks the vm.StateDB interface
// wrapper. If this ever panics or the write is lost, the executor must NOT reuse the
// interface wrapper for budgeting on the write path and must fall back to a
// tracer/OnOpcode counter instead.
func TestApplyMessageThroughBudgetedWrapper(t *testing.T) {
	sdb, err := state.New(types.EmptyRootHash, state.NewDatabaseForTesting())
	if err != nil {
		t.Fatal(err)
	}

	// Contract: SSTORE(0, 7) then STOP. PUSH1 7 PUSH1 0 SSTORE STOP.
	contract := common.BytesToAddress([]byte("sstore-contract"))
	code := []byte{0x60, 0x07, 0x60, 0x00, 0x55, 0x00}
	sdb.CreateAccount(contract)
	sdb.SetCode(contract, code, tracing.CodeChangeUnspecified)

	// Fund the sender so nonce/balance core checks pass.
	sender := common.BytesToAddress([]byte("sender"))
	sdb.CreateAccount(sender)
	sdb.AddBalance(sender, uint256.NewInt(1_000_000_000_000_000_000), tracing.BalanceChangeUnspecified)

	budget := arb.NewReadBudget(func() time.Time { return time.Now() }, 1_000_000, 0)
	wrapped := newBudgetedStateDB(sdb, budget)

	blockCtx := vm.BlockContext{
		CanTransfer: core.CanTransfer,
		Transfer:    core.Transfer,
		GetHash:     func(uint64) common.Hash { return common.Hash{} },
		Coinbase:    common.BytesToAddress([]byte("coinbase")),
		BlockNumber: big.NewInt(1),
		Time:        1,
		Difficulty:  big.NewInt(1),
		GasLimit:    30_000_000,
		BaseFee:     big.NewInt(0),
	}
	evm := vm.NewEVM(blockCtx, wrapped, params.TestChainConfig, vm.Config{})
	wrapped.SetCancel(evm.Cancel)

	msg := &core.Message{
		To:                &contract,
		From:              sender,
		Nonce:             0,
		Value:             big.NewInt(0),
		GasLimit:          100_000,
		GasPrice:          big.NewInt(0),
		GasFeeCap:         big.NewInt(0),
		GasTipCap:         big.NewInt(0),
		Data:              nil,
		SkipNonceChecks:   false,
		SkipTransactionChecks: false,
	}
	gp := new(core.GasPool).AddGas(30_000_000)

	// SetTxContext as the design requires per tx (§8.2).
	sdb.SetTxContext(common.HexToHash("0x01"), 0)

	res, err := core.ApplyMessage(evm, msg, gp)
	if err != nil {
		t.Fatalf("ApplyMessage core error (unexpected): %v", err)
	}
	if res.Err != nil {
		t.Fatalf("execution error (unexpected revert/OOG): %v", res.Err)
	}
	if res.UsedGas == 0 {
		t.Fatal("expected non-zero gas used for an SSTORE")
	}

	// The write must have landed: slot 0 of the contract == 7. Read it back through
	// the concrete state (proves the wrapper did not swallow or misroute the write).
	got := sdb.GetState(contract, common.Hash{})
	if got.Big().Cmp(big.NewInt(7)) != 0 {
		t.Fatalf("SSTORE did not persist through wrapper: slot0=%s", got.Hex())
	}

	// The write path also issued logical reads that were counted (budget healthy).
	if budget.Reads() == 0 {
		t.Fatal("expected some logical reads charged on the write path")
	}
	if budget.Failed() {
		t.Fatal("budget must not be tripped on a well-funded simple SSTORE")
	}
	t.Logf("write-path OK: gas=%d reads=%d", res.UsedGas, budget.Reads())
}
