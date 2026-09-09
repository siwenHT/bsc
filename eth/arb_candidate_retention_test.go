package eth

import (
	"math/big"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/eth/arb"
	"github.com/holiman/uint256"
)

// TestDecodeMeasureTargetPairing proves the §389 pairing rule for the optional
// retention-measurement subject: executor + base_token are all-or-nothing, and each
// (when present) must be a valid address. The node NEVER guesses or hardcodes a
// beneficiary — a half pair or a malformed address is a hard client error, and both
// absent is the honest "don't measure" path (e.g. the arb contract isn't deployed).
func TestDecodeMeasureTargetPairing(t *testing.T) {
	exec := "0x000000000000000000000000000000000000dead"
	base := "0x000000000000000000000000000000000000beef"

	// Both absent -> measure OFF, no error.
	mt, err := decodeMeasureTarget("", "")
	if err != nil {
		t.Fatalf("both-absent must be OK, got %v", err)
	}
	if mt.measure {
		t.Fatal("both-absent must not enable measurement")
	}

	// Both valid -> measure ON with the exact decoded addresses.
	mt, err = decodeMeasureTarget(exec, base)
	if err != nil {
		t.Fatalf("both-valid must be OK, got %v", err)
	}
	if !mt.measure {
		t.Fatal("both-valid must enable measurement")
	}
	if mt.executor != common.HexToAddress(exec) || mt.baseToken != common.HexToAddress(base) {
		t.Fatalf("decoded subject mismatch: exec=%s base=%s", mt.executor.Hex(), mt.baseToken.Hex())
	}

	// Exactly one present -> rejected (both directions).
	if _, err := decodeMeasureTarget(exec, ""); err != errMeasureTargetPairing {
		t.Fatalf("executor-only must be rejected, got %v", err)
	}
	if _, err := decodeMeasureTarget("", base); err != errMeasureTargetPairing {
		t.Fatalf("base-only must be rejected, got %v", err)
	}

	// Malformed address (present but not a valid wire address) -> rejected.
	if _, err := decodeMeasureTarget("0xDEAD", base); err != errMeasureTargetPairing {
		t.Fatalf("malformed executor must be rejected, got %v", err)
	}
	if _, err := decodeMeasureTarget(exec, "not-hex"); err != errMeasureTargetPairing {
		t.Fatalf("malformed base_token must be rejected, got %v", err)
	}
}

// TestBalanceOfReadsTokenStorageNotNative proves the §391 retention primitive reads
// ERC20 balanceOf on the TOKEN contract — not the holder's native balance. The token
// contract is mocked to return a fixed balance B; the holder is separately given a
// DIFFERENT native balance. A correct BalanceOf returns B (the token read); a buggy
// implementation reading native balance would return the native value instead.
func TestBalanceOfReadsTokenStorageNotNative(t *testing.T) {
	sdb, err := state.New(types.EmptyRootHash, state.NewDatabaseForTesting())
	if err != nil {
		t.Fatal(err)
	}
	token := common.BytesToAddress([]byte("basetoken"))
	holder := common.BytesToAddress([]byte("executor"))

	tokenBal := big.NewInt(777_000_000_000)
	nativeBal := big.NewInt(123) // deliberately different from the token balance
	// balanceOf(...) returns a single uint256 word == tokenBal, regardless of args.
	deployReturner(sdb, token, w32(tokenBal))
	sdb.CreateAccount(holder)
	sdb.SetBalance(holder, uint256.MustFromBig(nativeBal), tracing.BalanceChangeUnspecified)

	budget := arb.NewReadBudget(func() time.Time { return time.Now() }, 1000, 0)
	pc := testPoolCaller(t, sdb, budget)

	got, err := pc.BalanceOf(token, holder)
	if err != nil {
		t.Fatalf("BalanceOf: %v", err)
	}
	if got.Cmp(tokenBal) != 0 {
		t.Fatalf("BalanceOf must return the TOKEN balance %v (read via balanceOf on the token), got %v", tokenBal, got)
	}
	if got.Cmp(nativeBal) == 0 {
		t.Fatal("BalanceOf must NOT read the holder's native balance")
	}
}

// TestBalanceOfSurfacesReadFailure proves BalanceOf never fabricates a balance: a token
// address with no code (nothing to return) surfaces an error, so measureRetention can
// distinguish "unmeasured" from "measured zero" and leave both fields empty (§381).
func TestBalanceOfSurfacesReadFailure(t *testing.T) {
	sdb, err := state.New(types.EmptyRootHash, state.NewDatabaseForTesting())
	if err != nil {
		t.Fatal(err)
	}
	emptyToken := common.BytesToAddress([]byte("no-code-token"))
	holder := common.BytesToAddress([]byte("executor"))

	budget := arb.NewReadBudget(func() time.Time { return time.Now() }, 1000, 0)
	pc := testPoolCaller(t, sdb, budget)

	// A call to an account with no code returns empty bytes; DecodeUint256 then fails
	// with ErrReturnTooShort rather than returning a fabricated zero.
	if _, err := pc.BalanceOf(emptyToken, holder); err == nil {
		t.Fatal("BalanceOf on a codeless token must error, never fabricate a balance")
	}
}

// TestRunCandidateT0T2Isolation proves the §391 two-point capture: PrefixPostState is
// frozen AFTER the signed prefix but BEFORE our candidate, and is isolated from the
// later candidate execution (which keeps mutating PostState). We read the sender's
// balance on both snapshots: our candidate is a value transfer FROM the funded sender,
// so its balance on the T0 snapshot must be strictly greater than on the T2 snapshot
// (value + gas spent between the two boundaries). Reading native balance directly here
// is only to observe the isolation invariant — the production path reads ERC20 base
// token via BalanceOf.
func TestRunCandidateT0T2Isolation(t *testing.T) {
	x, signer, bc := prefixFixture(t)
	defer bc.Stop()
	to := common.BytesToAddress([]byte("recipient"))
	bf := x.baseFee()

	key, _ := crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
	sender := crypto.PubkeyToAddress(key.PublicKey)

	prefix := []*types.Transaction{mustSignValueTx(t, signer, 0, to, big.NewInt(1000), bf)}
	ours := candidateEnvForKey1(1, to, big.NewInt(50_000), bf)

	res, err := x.RunCandidate(prefix, ours, PurposeDiagnostic, newReadBudget(1_000_000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if res.Candidate == nil || res.Candidate.Class.Status != arb.StatusSuccess {
		t.Fatal("candidate must succeed for this isolation test")
	}
	if res.PrefixPostState == nil {
		t.Fatal("completed prefix must capture a T0 snapshot")
	}
	if res.PostState == nil {
		t.Fatal("successful candidate must produce a T2 post state")
	}

	t0 := res.PrefixPostState.GetBalance(sender)
	t2 := res.PostState.GetBalance(sender)
	// T0 is before our candidate spent value+gas; T2 is after. If PrefixPostState were
	// aliased to the same live state as PostState (not an isolated Copy), the candidate's
	// mutation would show up in T0 too and the balances would be equal.
	if t0.Cmp(t2) <= 0 {
		t.Fatalf("T0 balance (%v) must exceed T2 balance (%v): PrefixPostState must be frozen before the candidate and isolated from it", t0, t2)
	}
	// Sanity: the recipient gained on T2 but the T0 snapshot must not show the candidate's transfer.
	toT0 := res.PrefixPostState.GetBalance(to)
	toT2 := res.PostState.GetBalance(to)
	if toT2.Cmp(toT0) <= 0 {
		t.Fatalf("recipient balance must grow from T0 (%v) to T2 (%v) by the candidate transfer", toT0, toT2)
	}
}
