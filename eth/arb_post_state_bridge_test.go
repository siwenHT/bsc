package eth

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/eth/arb"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/triedb"
)

// slotGetterSetterCode is a minimal storage-backed "pool": on an EMPTY-calldata call
// it returns SLOAD(slot0) as a 32-byte word (the getter path); on any non-empty
// calldata it does SSTORE(slot0, calldataload(0)) then STOP (the setter path). This
// lets a real tx mutate the slot and a read-only view-call observe it.
//
//	CALLDATASIZE PUSH1 0x0f JUMPI   ; if calldatasize != 0 -> setter
//	PUSH1 0 SLOAD PUSH1 0 MSTORE PUSH1 32 PUSH1 0 RETURN
//	JUMPDEST(0x0f) PUSH1 0 CALLDATALOAD PUSH1 0 SSTORE STOP
var slotGetterSetterCode = []byte{
	0x36, 0x60, 0x0f, 0x57, // CALLDATASIZE PUSH1 0x0f JUMPI
	0x60, 0x00, 0x54, 0x60, 0x00, 0x52, 0x60, 0x20, 0x60, 0x00, 0xf3, // getter
	0x5b, 0x60, 0x00, 0x35, 0x60, 0x00, 0x55, 0x00, // JUMPDEST setter
}

// TestPostStatePoolCallerReadsTargetEffect is the NODE-03→NODE-04 bridge proof: a tx
// that mutates a storage-backed pool slot is executed, and reading through the
// executor's PostState observes the NEW value while reading through the base observes
// the OLD value. This proves PostState carries the target's effects AND that reads are
// isolated (the quotable handle is read on a fresh Copy, base untouched).
func TestPostStatePoolCallerReadsTargetEffect(t *testing.T) {
	key, _ := crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
	addr := crypto.PubkeyToAddress(key.PublicKey)
	pool := common.BytesToAddress([]byte("storage-pool"))
	genDb := rawdb.NewMemoryDatabase()
	db := rawdb.NewMemoryDatabase()

	oldVal := big.NewInt(100)
	newVal := big.NewInt(999)

	gspec := &core.Genesis{
		Config: params.TestChainConfig,
		Alloc: types.GenesisAlloc{
			addr: {Balance: new(big.Int).Mul(big.NewInt(1e18), big.NewInt(100))},
			pool: {
				Code:    slotGetterSetterCode,
				Balance: common.Big0,
				Storage: map[common.Hash]common.Hash{{}: common.BigToHash(oldVal)},
			},
		},
	}
	genesis := gspec.MustCommit(genDb, triedb.NewDatabase(genDb, triedb.HashDefaults))
	chain, _ := core.GenerateChain(gspec.Config, genesis, ethash.NewFaker(), genDb, 1, func(i int, gen *core.BlockGen) {})

	bc, err := core.NewBlockChain(db, gspec, ethash.NewFaker(), core.DefaultConfig().WithStateScheme(rawdb.HashScheme))
	if err != nil {
		t.Fatal(err)
	}
	defer bc.Stop()
	if _, err := bc.InsertChain(chain); err != nil {
		t.Fatal(err)
	}

	parent := chain[0].Header()
	base, err := bc.StateAt(parent.Root)
	if err != nil {
		t.Fatal(err)
	}
	x := newTargetExecutor(bc, parent, base)
	signer := types.LatestSigner(gspec.Config)

	// A tx to the pool with 32-byte calldata = newVal triggers the setter (SSTORE).
	tx, err := types.SignTx(types.NewTransaction(
		0, pool, big.NewInt(0), 100_000, x.baseFee(), common.BigToHash(newVal).Bytes(),
	), signer, key)
	if err != nil {
		t.Fatal(err)
	}

	res, err := x.ExecuteTarget(tx, newReadBudget(1_000_000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if res.Class.Status != arb.StatusSuccess {
		t.Fatalf("setter tx want success, got %s", res.Class.Status)
	}
	if res.PostState == nil {
		t.Fatal("success must yield a post state")
	}

	readSlot := func(pc *poolCaller) *big.Int {
		t.Helper()
		ret, err := pc.staticCall(pool, nil) // empty calldata -> getter path
		if err != nil {
			t.Fatalf("getter staticCall: %v", err)
		}
		v, err := arb.DecodeUint256(ret)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		return v
	}

	// Read through the POST-target handle: must see the new value.
	postPC := x.PostStatePoolCaller(res.PostState, newReadBudget(1_000_000, 0))
	if postPC == nil {
		t.Fatal("PostStatePoolCaller must not be nil for a non-nil post state")
	}
	if got := readSlot(postPC); got.Cmp(newVal) != 0 {
		t.Fatalf("post-state read want %s, got %s", newVal, got)
	}

	// Read through a caller over the ORIGINAL base: must still see the old value
	// (the target's write is isolated to the post handle).
	basePC := newPoolCallerOver(bc, base, parent, newReadBudget(1_000_000, 0))
	if got := readSlot(basePC); got.Cmp(oldVal) != 0 {
		t.Fatalf("base read want %s (isolation), got %s", oldVal, got)
	}

	// Nil post state must yield a nil caller (never a silent caller over wrong state).
	if x.PostStatePoolCaller(nil, newReadBudget(1000, 0)) != nil {
		t.Fatal("nil post state must yield nil caller")
	}
}
