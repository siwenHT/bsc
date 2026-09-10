package eth

import (
	"errors"
	"math/big"
	"strconv"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/eth/arb"
	"github.com/ethereum/go-ethereum/params"
)

const validHash32 = "0x00000000000000000000000000000000000000000000000000000000000000ab"

// validEnvForExecutor builds a wire block_env consistent with the executor's bound
// parent: real parent-hash linkage, number = parent+1, node-derived base_fee, and
// consistent ms/seconds timestamps. This is the env the RPC path would deliver.
func validEnvForExecutor(x *targetExecutor) arb.BlockEnv {
	num := new(big.Int).Add(x.parent.Number, big.NewInt(1))
	sec := x.parent.Time + 1
	author := common.BytesToAddress([]byte("real-block-author")) // explicit non-zero
	env := arb.BlockEnv{
		Number:           num.String(),
		ParentHash:       x.parent.Hash().Hex(),
		TimestampMs:      strconv.FormatUint(sec*1000, 10),
		TimestampSeconds: strconv.FormatUint(sec, 10),
		Author:           strings.ToLower(author.Hex()),
		GasLimit:         strconv.FormatUint(x.parent.GasLimit, 10),
		Difficulty:       x.parent.Difficulty.String(),
		MixDigest:        validHash32,
		ExtraData:        "0x",
		ForkRulesDigest:  validHash32,
	}
	// Include the node-derived base fee so the adapter's match check passes.
	if bf := x.baseFee(); bf != nil {
		s := bf.String()
		env.BaseFee = &s
	}
	return env
}

// TestExecuteTargetWithEnvSucceeds proves the env-AWARE path runs a value transfer
// under a real, verified N+1 block_env: the target succeeds, gas is the env-agnostic
// 21000, the coinbase is the env's explicit author (not the heuristic parent coinbase),
// and a post state is produced.
func TestExecuteTargetWithEnvSucceeds(t *testing.T) {
	x, signer, bc := prefixFixture(t)
	defer bc.Stop()
	to := common.BytesToAddress([]byte("recipient"))
	env := validEnvForExecutor(x)

	tx := mustSignValueTx(t, signer, 0, to, big.NewInt(1000), x.baseFee())
	res, err := x.ExecuteTargetWithEnv(tx, newReadBudget(1_000_000, 0), env)
	if err != nil {
		t.Fatalf("valid env must resolve: %v", err)
	}
	if res.Class.Status != arb.StatusSuccess {
		t.Fatalf("want success, got %s", res.Class.Status)
	}
	if res.UsedGas != params.TxGas {
		t.Fatalf("value transfer gas want %d, got %d", params.TxGas, res.UsedGas)
	}
	if res.PostState == nil {
		t.Fatal("success must yield a post state")
	}
}

// TestExecuteTargetWithEnvRejectsBadLinkage proves the adapter enforces the deferred
// node-dependent checks: parent-hash mismatch, wrong number, and a base_fee that does
// not match the node derivation are all HARD errors (never silently downgraded).
func TestExecuteTargetWithEnvRejectsBadLinkage(t *testing.T) {
	x, signer, bc := prefixFixture(t)
	defer bc.Stop()
	to := common.BytesToAddress([]byte("recipient"))
	tx := mustSignValueTx(t, signer, 0, to, big.NewInt(1000), x.baseFee())
	budget := newReadBudget(1_000_000, 0)

	t.Run("parent_hash mismatch", func(t *testing.T) {
		env := validEnvForExecutor(x)
		env.ParentHash = validHash32 // not the real parent hash
		_, err := x.ExecuteTargetWithEnv(tx, budget, env)
		if !errors.Is(err, ErrEnvParentHashMismatch) {
			t.Fatalf("want ErrEnvParentHashMismatch, got %v", err)
		}
	})

	t.Run("number not parent+1", func(t *testing.T) {
		env := validEnvForExecutor(x)
		bad := new(big.Int).Add(x.parent.Number, big.NewInt(2))
		env.Number = bad.String()
		_, err := x.ExecuteTargetWithEnv(tx, budget, env)
		if !errors.Is(err, ErrEnvNotParentPlusOne) {
			t.Fatalf("want ErrEnvNotParentPlusOne, got %v", err)
		}
	})

	t.Run("base_fee mismatch", func(t *testing.T) {
		env := validEnvForExecutor(x)
		if env.BaseFee == nil {
			t.Skip("parent has no base fee; mismatch check not applicable")
		}
		bogus := new(big.Int).Add(x.baseFee(), big.NewInt(1)).String()
		env.BaseFee = &bogus
		_, err := x.ExecuteTargetWithEnv(tx, budget, env)
		if !errors.Is(err, ErrEnvBaseFeeMismatch) {
			t.Fatalf("want ErrEnvBaseFeeMismatch, got %v", err)
		}
	})

	t.Run("zero author rejected by pure validator", func(t *testing.T) {
		env := validEnvForExecutor(x)
		env.Author = "0x0000000000000000000000000000000000000000"
		_, err := x.ExecuteTargetWithEnv(tx, budget, env)
		if !errors.Is(err, arb.ErrZeroAuthor) {
			t.Fatalf("want ErrZeroAuthor, got %v", err)
		}
	})
}

// TestEnvAwareCoinbaseIsAuthor proves the env's explicit author becomes the block
// coinbase in the env-aware path — a value transfer WITH a tip accrues that tip to the
// env author (distinct from the heuristic parent coinbase), observable in the post
// state. This confirms the env's author actually drives the block context, not a
// silent fallback.
func TestEnvAwareCoinbaseIsAuthor(t *testing.T) {
	x, _, bc := prefixFixture(t)
	defer bc.Stop()
	env := validEnvForExecutor(x)
	author := common.HexToAddress(env.Author)
	baseFee := x.baseFee()
	if baseFee == nil {
		t.Skip("chain has no base fee; tip-to-author accrual not exercisable")
	}

	// The env author must differ from the heuristic parent coinbase for this to be a
	// meaningful test.
	if author == x.author {
		t.Skip("env author coincides with heuristic author; nothing distinguishing to assert")
	}

	// Author starts with zero balance (a synthetic address); a dynamic-fee tx with a
	// positive tip must credit the tip to the env author.
	key, _ := crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
	tip := big.NewInt(1_000_000_000) // 1 gwei tip
	feeCap := new(big.Int).Add(baseFee, tip)
	to := common.BytesToAddress([]byte("recipient"))
	dynTx, err := types.SignNewTx(key, types.LatestSigner(x.chain.Config()), &types.DynamicFeeTx{
		ChainID:   x.chain.Config().ChainID,
		Nonce:     0,
		To:        &to,
		Value:     big.NewInt(1000),
		Gas:       params.TxGas,
		GasFeeCap: feeCap,
		GasTipCap: tip,
	})
	if err != nil {
		t.Fatal(err)
	}

	res, err := x.ExecuteTargetWithEnv(dynTx, newReadBudget(1_000_000, 0), env)
	if err != nil {
		t.Fatal(err)
	}
	if res.Class.Status != arb.StatusSuccess || res.PostState == nil {
		t.Fatalf("expected success with post state, got %s", res.Class.Status)
	}
	// The tip (gasUsed * tip) must have accrued to the ENV author, proving the env's
	// author drove the block coinbase.
	authorBal := res.PostState.GetBalance(author)
	if authorBal == nil || authorBal.IsZero() {
		t.Fatal("env author (coinbase) must have received the tx tip; got zero balance")
	}
}
