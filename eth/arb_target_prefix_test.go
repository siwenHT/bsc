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

// prefixFixture builds a real-rules chain of `pre` empty blocks and returns an
// executor bound to the last block's post-state, plus a signer and the funded key.
// The empty parent lets us drive our OWN ordered prefix on top of it (the backrun
// [target, ours] shape) rather than replaying a native block.
func prefixFixture(t *testing.T) (*targetExecutor, types.Signer, *core.BlockChain) {
	t.Helper()
	key, _ := crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
	addr := crypto.PubkeyToAddress(key.PublicKey)
	genDb := rawdb.NewMemoryDatabase()
	db := rawdb.NewMemoryDatabase()

	gspec := &core.Genesis{
		Config: params.TestChainConfig,
		Alloc:  types.GenesisAlloc{addr: {Balance: new(big.Int).Mul(big.NewInt(1e18), big.NewInt(100))}},
	}
	genesis := gspec.MustCommit(genDb, triedb.NewDatabase(genDb, triedb.HashDefaults))
	chain, _ := core.GenerateChain(gspec.Config, genesis, ethash.NewFaker(), genDb, 1, func(i int, gen *core.BlockGen) {})

	bc, err := core.NewBlockChain(db, gspec, ethash.NewFaker(), core.DefaultConfig().WithStateScheme(rawdb.HashScheme))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bc.InsertChain(chain); err != nil {
		bc.Stop()
		t.Fatal(err)
	}
	parent := chain[0].Header()
	base, err := bc.StateAt(parent.Root)
	if err != nil {
		bc.Stop()
		t.Fatal(err)
	}
	return newTargetExecutor(bc, parent, base), types.LatestSigner(gspec.Config), bc
}

func mustSignValueTx(t *testing.T, signer types.Signer, nonce uint64, to common.Address, val, baseFee *big.Int) *types.Transaction {
	t.Helper()
	key, _ := crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
	tx, err := types.SignTx(types.NewTransaction(nonce, to, val, params.TxGas, baseFee, nil), signer, key)
	if err != nil {
		t.Fatal(err)
	}
	return tx
}

// TestExecutePrefixBothSucceed proves an ordered two-tx prefix runs on ONE isolated
// state with a SHARED gas pool: nonce 0 then nonce 1 from the same sender both apply
// (the second's nonce is only valid because the first mutated the shared state), the
// prefix Completes, and a quotable PostState is produced.
func TestExecutePrefixBothSucceed(t *testing.T) {
	x, signer, bc := prefixFixture(t)
	defer bc.Stop()
	to := common.BytesToAddress([]byte("recipient"))
	bf := x.baseFee()

	txs := []*types.Transaction{
		mustSignValueTx(t, signer, 0, to, big.NewInt(1000), bf),
		mustSignValueTx(t, signer, 1, to, big.NewInt(2000), bf),
	}
	res, err := x.ExecutePrefix(txs, newReadBudget(1_000_000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Completed {
		t.Fatal("both value transfers should complete the prefix")
	}
	if len(res.Outcomes) != 2 {
		t.Fatalf("want 2 outcomes, got %d", len(res.Outcomes))
	}
	for i, o := range res.Outcomes {
		if o.Class.Status != arb.StatusSuccess {
			t.Fatalf("tx %d: want success, got %s", i, o.Class.Status)
		}
		if o.UsedGas != params.TxGas {
			t.Fatalf("tx %d: want gas %d, got %d", i, params.TxGas, o.UsedGas)
		}
	}
	if res.PostState == nil {
		t.Fatal("completed prefix must yield a post state")
	}
}

// TestExecutePrefixStopsAtFirstFailure proves the prefix stops at the FIRST
// non-success tx and yields NO post state (§256/§357): a first tx with a bad nonce
// classifies invalid, the second tx is never attempted, and PostState stays nil.
func TestExecutePrefixStopsAtFirstFailure(t *testing.T) {
	x, signer, bc := prefixFixture(t)
	defer bc.Stop()
	to := common.BytesToAddress([]byte("recipient"))
	bf := x.baseFee()

	txs := []*types.Transaction{
		mustSignValueTx(t, signer, 99, to, big.NewInt(1000), bf), // nonce too high -> invalid
		mustSignValueTx(t, signer, 0, to, big.NewInt(2000), bf),  // never reached
	}
	res, err := x.ExecutePrefix(txs, newReadBudget(1_000_000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if res.Completed {
		t.Fatal("prefix with a failing first tx must not complete")
	}
	if len(res.Outcomes) != 1 {
		t.Fatalf("prefix must stop after the first tx, got %d outcomes", len(res.Outcomes))
	}
	if res.Outcomes[0].Class.Status != arb.StatusInvalid {
		t.Fatalf("first tx want invalid, got %s", res.Outcomes[0].Class.Status)
	}
	if res.PostState != nil {
		t.Fatal("early-stopped prefix must not yield a post state")
	}
}
