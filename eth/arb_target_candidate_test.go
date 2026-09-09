package eth

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/eth/arb"
	"github.com/ethereum/go-ethereum/params"
)

// candidateEnvForKey1 builds a value-transfer unsigned envelope whose injected sender
// is key1's address (the funded account). No signature is produced — RunCandidate must
// still enforce nonce/balance/gas/fee.
func candidateEnvForKey1(nonce uint64, to common.Address, val, baseFee *big.Int) CandidateEnvelope {
	key, _ := crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
	from := crypto.PubkeyToAddress(key.PublicKey)
	tip := big.NewInt(1)
	feeCap := new(big.Int).Add(baseFee, tip) // exactly covers base fee + 1 tip
	return CandidateEnvelope{
		From:      from,
		To:        &to,
		Nonce:     nonce,
		Value:     val,
		GasLimit:  params.TxGas,
		GasFeeCap: feeCap,
		GasTipCap: tip,
		Data:      nil,
	}
}

// TestRunCandidateDiagnostic proves the §8.3 candidate path: a signed target prefix
// (nonce 0) completes, then OUR unsigned envelope (injected sender, nonce 1) executes
// WITHOUT signature recovery yet succeeds under real nonce/fee checks, yielding a
// quotable post state. Signed must be false regardless of purpose.
func TestRunCandidateDiagnostic(t *testing.T) {
	x, signer, bc := prefixFixture(t)
	defer bc.Stop()
	to := common.BytesToAddress([]byte("recipient"))
	bf := x.baseFee()

	prefix := []*types.Transaction{mustSignValueTx(t, signer, 0, to, big.NewInt(1000), bf)}
	ours := candidateEnvForKey1(1, to, big.NewInt(2000), bf)

	res, err := x.RunCandidate(prefix, ours, PurposeDiagnostic, newReadBudget(1_000_000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if res.Signed {
		t.Fatal("a candidate simulation must NEVER be a signing artifact (§8.3)")
	}
	if !res.PrefixCompleted {
		t.Fatal("signed target prefix should complete")
	}
	if res.Candidate == nil {
		t.Fatal("completed prefix must attempt our candidate")
	}
	if res.Candidate.Class.Status != arb.StatusSuccess {
		t.Fatalf("candidate want success, got %s", res.Candidate.Class.Status)
	}
	if res.Candidate.UsedGas != params.TxGas {
		t.Fatalf("candidate gas want %d, got %d", params.TxGas, res.Candidate.UsedGas)
	}
	if res.PostState == nil {
		t.Fatal("successful candidate must yield a quotable post state")
	}
	if res.Purpose != PurposeDiagnostic {
		t.Fatalf("purpose echo mismatch: %s", res.Purpose)
	}
}

// TestRunCandidateNonceEnforced proves that injecting the sender (skipping our
// signature recovery) does NOT skip the nonce check: an envelope with a too-high nonce
// classifies invalid and yields no post state (§237: only signature recovery is
// skipped, nonce/balance/gas/fee stay enforced).
func TestRunCandidateNonceEnforced(t *testing.T) {
	x, signer, bc := prefixFixture(t)
	defer bc.Stop()
	to := common.BytesToAddress([]byte("recipient"))
	bf := x.baseFee()

	prefix := []*types.Transaction{mustSignValueTx(t, signer, 0, to, big.NewInt(1000), bf)}
	// After the prefix consumes nonce 0, the correct candidate nonce is 1; use 99.
	ours := candidateEnvForKey1(99, to, big.NewInt(2000), bf)

	res, err := x.RunCandidate(prefix, ours, PurposeTradeCandidate, newReadBudget(1_000_000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if !res.PrefixCompleted {
		t.Fatal("signed target prefix should still complete")
	}
	if res.Candidate == nil {
		t.Fatal("candidate should have been attempted")
	}
	if res.Candidate.Class.Status != arb.StatusInvalid {
		t.Fatalf("bad-nonce candidate want invalid, got %s", res.Candidate.Class.Status)
	}
	if res.PostState != nil {
		t.Fatal("invalid candidate must not yield a post state")
	}
	if res.Signed {
		t.Fatal("candidate is never signed")
	}
}

// TestRunCandidateSkipsOnFailedPrefix proves a candidate is NOT attempted when the
// signed target prefix fails: our candidate is sized for the target's effects, so on a
// failed prefix Candidate stays nil and no post state is produced.
func TestRunCandidateSkipsOnFailedPrefix(t *testing.T) {
	x, signer, bc := prefixFixture(t)
	defer bc.Stop()
	to := common.BytesToAddress([]byte("recipient"))
	bf := x.baseFee()

	// A target tx with a too-high nonce fails the prefix immediately.
	prefix := []*types.Transaction{mustSignValueTx(t, signer, 99, to, big.NewInt(1000), bf)}
	ours := candidateEnvForKey1(0, to, big.NewInt(2000), bf)

	res, err := x.RunCandidate(prefix, ours, PurposeDiagnostic, newReadBudget(1_000_000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if res.PrefixCompleted {
		t.Fatal("prefix with a bad target tx must not complete")
	}
	if res.Candidate != nil {
		t.Fatal("candidate must NOT be attempted on a failed prefix")
	}
	if res.PostState != nil {
		t.Fatal("failed prefix must not yield a post state")
	}
}
