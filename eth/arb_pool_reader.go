// Fixed-parent pool state reader (NODE-02.2, design §9.3/§9.5, §6.1). It binds the
// pure ABI codec (eth/arb/pool_abi.go) and the budgeted state wrapper
// (eth/arb_budget_statedb.go) onto a real EVM over a FIXED parent state, and reads
// pool state by read-only view calls. It lives in package eth so it can touch the
// node's EVM, blockchain, and state types.
//
// Read-only guarantee (design §6.1 "模拟禁止调用Commit/向canonical TrieDB写入"):
// every pool accessor is issued via evm.StaticCall, which forbids ALL state writes
// at the EVM level — the strongest honest read-only guarantee, stronger than merely
// not calling Commit. We also run against a StateDB.Copy of the borrowed base so
// even lazy cache fills never touch the canonical/base object shared elsewhere.
//
// Honesty (design §381/§9.3):
//   - a malformed/short view-call return is a hard error (from the ABI codec); we
//     never fabricate a zero reserve/price.
//   - V2 reserves are only trusted for standard-math quoting when the pool's real
//     token balances equal the reserves (§9.3); we read balanceOf for both tokens
//     and let the caller/Rust gate on balances_match_reserves. We report balances
//     as read; we do not overwrite reserves with balances.
//   - if the read budget trips mid-read, the wrapper cancels and we surface the
//     budget error; the partial result is discarded, never returned as success.
package eth

import (
	"errors"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/eth/arb"
)

// poolCaller wraps a budgeted EVM over a fixed parent state for issuing read-only
// pool view calls. Build it once per snapshot batch; reuse across pools sharing the
// same parent to amortize the state Copy and block context.
type poolCaller struct {
	evm     *vm.EVM
	wrapped *budgetedStateDB
	caller  common.Address // an arbitrary EOA origin for the static calls
	gasCap  uint64
}

// viewGas bounds a single view call. Pool accessors are cheap; this only guards a
// pathological contract, and the read budget is the real limiter.
const viewGas = 2_000_000

// PoolReadError wraps a budget/exec failure so callers can distinguish it from a
// clean "pool does not implement this" decode error.
var (
	ErrViewCallReverted = errors.New("arb: pool view call reverted or failed")
	ErrReadBudgetTripped = errors.New("arb: read budget tripped during pool read")
)

// newPoolCallerOver builds a caller over a Copy of base at the given parent header,
// depending only on core.ChainContext (config + engine + header lookups) so it works
// over EITHER the canonical head state OR an executor's isolated post-target state
// (the §256 quotable post handle) — the NODE-03→NODE-04 bridge. The author is resolved
// explicitly (design §7.1: never let a failed Author extraction become the zero
// address); for a read-only snapshot the coinbase does not affect pool getters, but we
// still bind it honestly from the header's real author when available.
func newPoolCallerOver(chain core.ChainContext, base *state.StateDB, parent *types.Header, budget *arb.ReadBudget) *poolCaller {
	// Work on a private copy so lazy cache fills never mutate the borrowed base
	// (design §6.1: Copy shares underlying readers; we still isolate the mutable
	// in-memory objects, and StaticCall prevents writes regardless).
	work := base.Copy()
	wrapped := newBudgetedStateDB(work, budget)

	// Explicit author: prefer the consensus engine's author for the (already
	// validated) parent header; fall back to the header coinbase. Never zero via a
	// silently-ignored error.
	author := parent.Coinbase
	if a, err := chain.Engine().Author(parent); err == nil {
		author = a
	}

	blockCtx := core.NewEVMBlockContext(parent, chain, &author)
	evm := vm.NewEVM(blockCtx, wrapped, chain.Config(), vm.Config{})
	wrapped.SetCancel(evm.Cancel)

	return &poolCaller{
		evm:     evm,
		wrapped: wrapped,
		caller:  common.BytesToAddress([]byte("arb-pool-reader")),
		gasCap:  viewGas,
	}
}

// newPoolCaller is the *Ethereum convenience wrapper over newPoolCallerOver, binding
// the node's own blockchain as the chain context.
func (s *Ethereum) newPoolCaller(base *state.StateDB, parent *types.Header, budget *arb.ReadBudget) *poolCaller {
	return newPoolCallerOver(s.blockchain, base, parent, budget)
}

// staticCall issues one read-only call to addr with calldata, returning the raw
// return bytes. It fails if the budget tripped (cancel latched) or the call errored
// (revert / out-of-gas). It NEVER returns a fabricated value on failure.
func (c *poolCaller) staticCall(addr common.Address, calldata []byte) ([]byte, error) {
	ret, _, err := c.evm.StaticCall(c.caller, addr, calldata, c.gasCap)
	if c.wrapped.Budget().Failed() {
		return nil, ErrReadBudgetTripped
	}
	if err != nil {
		return nil, ErrViewCallReverted
	}
	return ret, nil
}

// V2Snapshot is the read result for a V2 pool: reserves plus the real token
// balances (design §9.3). The caller maps these onto arb-types V2State and gates
// quoting on balances == reserves.
type V2Snapshot struct {
	Reserve0 *big.Int
	Reserve1 *big.Int
	Balance0 *big.Int
	Balance1 *big.Int
}

// ReadV2 reads getReserves() on the pool and balanceOf(pool) on both tokens. token0
// and token1 are the pool's token addresses from the registry (not re-read here;
// the registry is the identity authority). Any decode/budget/revert failure aborts
// with an error — no partial or fabricated snapshot.
func (c *poolCaller) ReadV2(pool, token0, token1 common.Address) (*V2Snapshot, error) {
	ret, err := c.staticCall(pool, arb.CallGetReserves())
	if err != nil {
		return nil, err
	}
	res, err := arb.DecodeGetReserves(ret)
	if err != nil {
		return nil, err
	}

	bal := func(token common.Address) (*big.Int, error) {
		var raw [20]byte
		copy(raw[:], pool.Bytes())
		out, err := c.staticCall(token, arb.CallBalanceOf(raw))
		if err != nil {
			return nil, err
		}
		return arb.DecodeUint256(out)
	}
	b0, err := bal(token0)
	if err != nil {
		return nil, err
	}
	b1, err := bal(token1)
	if err != nil {
		return nil, err
	}

	return &V2Snapshot{
		Reserve0: res.Reserve0,
		Reserve1: res.Reserve1,
		Balance0: b0,
		Balance1: b1,
	}, nil
}

// V3Snapshot is the read result for a V3 pool head: slot0 (sqrtPriceX96, tick) and
// liquidity. Bitmap words and ticks are read on demand elsewhere (design §9.5); the
// head is what the identity_only + on-demand model needs first.
type V3Snapshot struct {
	SqrtPriceX96 *big.Int
	Tick         int32
	Liquidity    *big.Int
}

// ReadV3Head reads slot0() and liquidity() on the pool.
func (c *poolCaller) ReadV3Head(pool common.Address) (*V3Snapshot, error) {
	ret, err := c.staticCall(pool, arb.CallSlot0())
	if err != nil {
		return nil, err
	}
	s0, err := arb.DecodeSlot0(ret)
	if err != nil {
		return nil, err
	}
	lret, err := c.staticCall(pool, arb.CallLiquidity())
	if err != nil {
		return nil, err
	}
	liq, err := arb.DecodeUint128(lret)
	if err != nil {
		return nil, err
	}
	return &V3Snapshot{SqrtPriceX96: s0.SqrtPriceX96, Tick: s0.Tick, Liquidity: liq}, nil
}

// NewPoolCallerAtHead builds a pool caller at the current canonical head state, for
// integration use. It probes readability through the arb backend contract (a
// readable root is required, design §5.1). ttl/budget bound the read session.
func (s *Ethereum) NewPoolCallerAtHead(maxReads uint64, wall time.Duration) (*poolCaller, error) {
	h := s.blockchain.CurrentBlock()
	if h == nil {
		return nil, errors.New("arb: no canonical head")
	}
	base, err := s.blockchain.StateAt(h.Root)
	if err != nil {
		return nil, err
	}
	budget := arb.NewReadBudget(func() time.Time { return time.Now() }, maxReads, wall)
	return s.newPoolCaller(base, h, budget), nil
}
