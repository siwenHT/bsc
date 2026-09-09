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

// TestExecutorMatchesNativeProcess is the §441 prefix-parity harness (first
// iteration, env-independent scope). It generates a real-rules chain where each
// block contains exactly one value-transfer tx, takes the NATIVE per-tx receipts as
// authoritative, then runs the SAME tx through our targetExecutor on the SAME parent
// state and asserts gas_used + status match.
//
// HONEST SCOPE: value transfers cost exactly params.TxGas (21000) regardless of the
// block env (no TIMESTAMP/NUMBER/COINBASE opcodes), so gas parity here proves the
// execution path + preamble + classification are native-aligned for the
// env-independent case. Env-DEPENDENT parity (contract calls reading block fields)
// requires feeding the executor the real N+1 header via the RPC block_env path and
// is validated there, not here. Uses params.TestChainConfig + ethash faker; BSC
// parlia fee routing / system-contract specifics need a real BSC config on a
// maintenance-window node and are out of this synthetic harness's scope.
func TestExecutorMatchesNativeProcess(t *testing.T) {
	var (
		key1, _ = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
		addr1   = crypto.PubkeyToAddress(key1.PublicKey)
		addr2   = common.BytesToAddress([]byte("recipient"))
		genDb   = rawdb.NewMemoryDatabase()
		db      = rawdb.NewMemoryDatabase()
	)

	gspec := &core.Genesis{
		Config: params.TestChainConfig,
		Alloc:  types.GenesisAlloc{addr1: {Balance: new(big.Int).Mul(big.NewInt(1e18), big.NewInt(100))}},
	}
	genesis := gspec.MustCommit(genDb, triedb.NewDatabase(genDb, triedb.HashDefaults))

	const nBlocks = 4
	signer := types.LatestSigner(gspec.Config)
	chain, receipts := core.GenerateChain(gspec.Config, genesis, ethash.NewFaker(), genDb, nBlocks, func(i int, gen *core.BlockGen) {
		// one value-transfer tx per block
		tx, err := types.SignTx(types.NewTransaction(
			gen.TxNonce(addr1), addr2, big.NewInt(1000), params.TxGas, gen.BaseFee(), nil,
		), signer, key1)
		if err != nil {
			panic(err)
		}
		gen.AddTx(tx)
	})

	// Build a blockchain holding the generated states so we can StateAt(parent.Root).
	bc, err := core.NewBlockChain(db, gspec, ethash.NewFaker(), core.DefaultConfig().WithStateScheme(rawdb.HashScheme))
	if err != nil {
		t.Fatal(err)
	}
	defer bc.Stop()
	if n, err := bc.InsertChain(chain); err != nil {
		t.Fatalf("insert block %d: %v", n, err)
	}

	// For each generated block, the native receipt is authoritative. Run the same
	// single tx through our executor on the same parent post-state and compare.
	parents := append([]*types.Block{types.NewBlockWithHeader(genesis.Header())}, chain[:nBlocks-1]...)
	for i := 0; i < nBlocks; i++ {
		parent := parents[i].Header()
		nativeReceipt := receipts[i][0]
		tx := chain[i].Transactions()[0]

		base, err := bc.StateAt(parent.Root)
		if err != nil {
			t.Fatalf("block %d: StateAt(parent): %v", i, err)
		}
		x := newTargetExecutor(bc, parent, base)
		budget := newReadBudget(1_000_000, 0)
		res, err := x.ExecuteTarget(tx, budget)
		if err != nil {
			t.Fatalf("block %d: ExecuteTarget: %v", i, err)
		}

		if res.Class.Status != arb.StatusSuccess {
			t.Fatalf("block %d: want success, got %s", i, res.Class.Status)
		}
		if !res.Class.ReceiptTrusted {
			t.Fatalf("block %d: success must trust receipt", i)
		}
		if res.UsedGas != nativeReceipt.GasUsed {
			t.Fatalf("block %d: gas mismatch: executor=%d native=%d", i, res.UsedGas, nativeReceipt.GasUsed)
		}
		wantStatus := nativeReceipt.Status == types.ReceiptStatusSuccessful
		gotStatus := res.Class.Status == arb.StatusSuccess
		if wantStatus != gotStatus {
			t.Fatalf("block %d: status mismatch: executor=%v native=%v", i, gotStatus, wantStatus)
		}
		if res.PostState == nil {
			t.Fatalf("block %d: success must yield a post state", i)
		}
	}
}

// TestExecutorCoreErrorInvalid proves a tx that fails core validity (nonce too high)
// classifies as invalid (not reverted, not success) and yields no post state.
func TestExecutorCoreErrorInvalid(t *testing.T) {
	var (
		key1, _ = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
		addr1   = crypto.PubkeyToAddress(key1.PublicKey)
		addr2   = common.BytesToAddress([]byte("recipient"))
		genDb   = rawdb.NewMemoryDatabase()
		db      = rawdb.NewMemoryDatabase()
	)
	gspec := &core.Genesis{
		Config: params.TestChainConfig,
		Alloc:  types.GenesisAlloc{addr1: {Balance: new(big.Int).Mul(big.NewInt(1e18), big.NewInt(100))}},
	}
	genesis := gspec.MustCommit(genDb, triedb.NewDatabase(genDb, triedb.HashDefaults))
	// empty chain of 1 block to get a committed parent state
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
	signer := types.LatestSigner(gspec.Config)
	// nonce 99 is far too high -> ErrNonceTooHigh core error.
	tx, _ := types.SignTx(types.NewTransaction(99, addr2, big.NewInt(1), params.TxGas, parent.BaseFee, nil), signer, key1)

	x := newTargetExecutor(bc, parent, base)
	res, err := x.ExecuteTarget(tx, newReadBudget(1_000_000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if res.Class.Status != arb.StatusInvalid {
		t.Fatalf("want invalid, got %s", res.Class.Status)
	}
	if res.PostState != nil {
		t.Fatal("invalid tx must not yield a post state")
	}
	if res.Class.ReceiptTrusted {
		t.Fatal("invalid must not trust receipt")
	}
}
