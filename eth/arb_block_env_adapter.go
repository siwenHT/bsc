// Block-env adapter (NODE shared infra, design §7.1). It turns a wire arb.BlockEnv —
// already shape/consistency-validated by arb.ValidateBlockEnv — into the CONCRETE geth
// values the executor needs, and performs the node-dependent checks that the pure core
// deliberately deferred (file header of eth/arb/block_env.go):
//
//   - parent LINKAGE: env.parent_hash must equal the bound parent's hash, and
//     env.number must be exactly parent.Number+1 (this is an N+1 simulation).
//   - base_fee: RECOMPUTED from parent+fork rules via eip1559.CalcBaseFee and, when the
//     env carries one, checked to MATCH — a client-supplied base_fee is never trusted
//     as an arbitrary value, it is verified against the node's own derivation (§7.1).
//
// Honesty: author is taken from the env verbatim (already proven non-zero by the pure
// validator); we never silently substitute a zero. Timestamps use the env's ms/seconds
// (already proven consistent). Nothing here fabricates a deferred check.
package eth

import (
	"errors"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/misc/eip1559"
	"github.com/ethereum/go-ethereum/eth/arb"
)

// resolvedBlockEnv is the concrete, node-verified target block environment. Every
// field is ready to drop into a vm.BlockContext / message build; no wire strings.
type resolvedBlockEnv struct {
	number     *big.Int
	timeSec    uint64
	timeMs     uint64
	author     common.Address
	gasLimit   uint64
	difficulty *big.Int
	baseFee    *big.Int // node-derived (verified against env when env supplied one)
}

// Errors surfaced by env resolution (adapter-side, node-dependent). Wire-shape errors
// still come from arb.ValidateBlockEnv (errors.Is ErrBlockEnvBadField etc.).
var (
	ErrEnvParentHashMismatch = errors.New("arb: block_env parent_hash does not match bound parent")
	ErrEnvNotParentPlusOne   = errors.New("arb: block_env number must be parent.Number+1 (N+1 simulation)")
	ErrEnvBaseFeeMismatch    = errors.New("arb: block_env base_fee does not match node-derived base fee")
	ErrEnvParseField         = errors.New("arb: block_env field failed concrete parse")
)

// resolveBlockEnv validates + links + parses a wire env against the given parent. On
// success the returned resolvedBlockEnv is authoritative for building the target block
// context. It is the ONE place the deferred node-dependent checks live.
func (x *targetExecutor) resolveBlockEnv(env arb.BlockEnv) (*resolvedBlockEnv, error) {
	// 1) Pure wire-shape + node-independent consistency (author non-zero, ts, etc.).
	if err := arb.ValidateBlockEnv(env); err != nil {
		return nil, err
	}

	// 2) Concrete parses. isUintStr already proved these are clean decimal uints, so a
	//    SetString failure would be an internal contradiction; we still check honestly.
	number, ok := new(big.Int).SetString(env.Number, 10)
	if !ok {
		return nil, fmt.Errorf("%w: number", ErrEnvParseField)
	}
	difficulty, ok := new(big.Int).SetString(env.Difficulty, 10)
	if !ok {
		return nil, fmt.Errorf("%w: difficulty", ErrEnvParseField)
	}
	gasLimit, err := parseUint64(env.GasLimit)
	if err != nil {
		return nil, fmt.Errorf("%w: gas_limit", ErrEnvParseField)
	}
	timeSec, err := parseUint64(env.TimestampSeconds)
	if err != nil {
		return nil, fmt.Errorf("%w: timestamp_seconds", ErrEnvParseField)
	}
	timeMs, err := parseUint64(env.TimestampMs)
	if err != nil {
		return nil, fmt.Errorf("%w: timestamp_ms", ErrEnvParseField)
	}

	// 3) Parent linkage (deferred by the pure core): this is an N+1 sim on THIS parent.
	if !equalHashHex(env.ParentHash, x.parent.Hash()) {
		return nil, ErrEnvParentHashMismatch
	}
	wantNum := new(big.Int).Add(x.parent.Number, big.NewInt(1))
	if number.Cmp(wantNum) != 0 {
		return nil, ErrEnvNotParentPlusOne
	}

	// 4) base_fee: recompute from parent+fork rules; when the env supplied one, it must
	//    MATCH the node derivation (never trust a client value as arbitrary, §7.1).
	var baseFee *big.Int
	if x.parent.BaseFee != nil {
		baseFee = eip1559.CalcBaseFee(x.chain.Config(), x.parent)
	}
	if env.BaseFee != nil {
		supplied, ok := new(big.Int).SetString(*env.BaseFee, 10)
		if !ok {
			return nil, fmt.Errorf("%w: base_fee", ErrEnvParseField)
		}
		switch {
		case baseFee == nil:
			// Parent has no base fee but env supplied one: contradiction.
			return nil, ErrEnvBaseFeeMismatch
		case baseFee.Cmp(supplied) != 0:
			return nil, ErrEnvBaseFeeMismatch
		}
	}

	return &resolvedBlockEnv{
		number:     number,
		timeSec:    timeSec,
		timeMs:     timeMs,
		author:     common.HexToAddress(env.Author),
		gasLimit:   gasLimit,
		difficulty: difficulty,
		baseFee:    baseFee,
	}, nil
}

// parseUint64 parses a clean decimal string (already isUintStr-validated upstream) into
// a uint64, erroring on overflow rather than silently wrapping.
func parseUint64(s string) (uint64, error) {
	v, ok := new(big.Int).SetString(s, 10)
	if !ok || !v.IsUint64() {
		return 0, ErrEnvParseField
	}
	return v.Uint64(), nil
}

// equalHashHex compares a lowercase 0x-hex hash string to a common.Hash without
// allocating a new string form (Hash.Hex lowercases, so a direct compare is fine).
func equalHashHex(hex string, h common.Hash) bool {
	return hex == h.Hex()
}
