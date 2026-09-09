// Fixed-parent target executor (NODE-03, design §8.1/§8.2/§277/§357). It runs a
// single signed target transaction on an ISOLATED copy of a fixed parent state,
// after reproducing the block PREAMBLE (BSC system-contract upgrade, EIP-4788
// beacon root, EIP-2935 parent block hash) so the base is "post-preamble, pre-
// target" exactly as design §357 requires — never a bare ApplyMessage on raw parent
// state. Reads are metered through the budgeted vm.StateDB wrapper (write-path
// survival already proven by TestApplyMessageThroughBudgetedWrapper).
//
// It lives in package eth for access to core execution APIs and *state.StateDB.
//
// Honesty (design §277/§333/§349): the Go error from ApplyTransactionWithEVM is
// mapped to ExecSignals via errors.Is on the known core-error sentinels (never by
// parsing English strings), and infra/cancel/budget signals are collected so the
// pure Classify (eth/arb/exec_outcome.go) can impose the honest priority — a
// cancelled/errored run's receipt is never mistaken for a real revert/success.
package eth

import (
	"errors"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/misc/eip1559"
	"github.com/ethereum/go-ethereum/consensus/misc/eip4844"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/systemcontracts"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/eth/arb"
)

// TargetResult is the outcome of executing one target tx on the isolated copy.
type TargetResult struct {
	Class   arb.Classification
	Receipt *types.Receipt // only trustworthy when Class.ReceiptTrusted
	UsedGas uint64         // only trustworthy when Class.ReceiptTrusted
	// PostState is the isolated, post-target StateDB — non-nil ONLY on a real
	// success (design §256/§357: target revert creates no quotable post handle).
	PostState *state.StateDB
	// Budget is the read budget after the run (metering echo / observability).
	Budget *arb.ReadBudget
}

// PrefixTxOutcome is the classified outcome of one tx within a signed prefix.
type PrefixTxOutcome struct {
	Class   arb.Classification
	Receipt *types.Receipt // trustworthy only when Class.ReceiptTrusted
	UsedGas uint64         // this tx alone (receipt.GasUsed), trustworthy same condition
}

// PrefixResult is the outcome of executing an ordered signed prefix on one isolated
// state (e.g. the backrun bundle [target, ours], design §357). Execution stops at the
// FIRST non-success tx: a backrun's later txs are sized for the prior txs' effects, so
// building on a reverted/failed prefix is meaningless and forbidden.
type PrefixResult struct {
	// Outcomes holds one entry per tx ACTUALLY ATTEMPTED (len < len(input) when the
	// prefix stopped early). Index i lines up with input tx i.
	Outcomes []PrefixTxOutcome
	// PostState is the isolated state after the FULL prefix, non-nil ONLY when every
	// input tx succeeded (design §256/§357). Early-stop yields no quotable handle.
	PostState *state.StateDB
	// Completed is true iff every input tx was applied and classified success.
	Completed bool
	// Budget is the shared read budget after the whole prefix (metering echo).
	Budget *arb.ReadBudget
}

// CandidatePurpose is the §8.3 purpose tag echoed on a candidate simulation. It is
// diagnostic/audit metadata ONLY — the executor never signs and never emits a signing
// permit for EITHER purpose; the signer entrypoint is what rejects Diagnostic.
type CandidatePurpose int

const (
	// PurposeDiagnostic collects a measured_gross_seed with B=0/minRetained=0 (§10#3).
	PurposeDiagnostic CandidatePurpose = iota
	// PurposeTradeCandidate is the build-phase revision sim with real B/guard/fees.
	PurposeTradeCandidate
)

func (p CandidatePurpose) String() string {
	switch p {
	case PurposeDiagnostic:
		return "diagnostic"
	case PurposeTradeCandidate:
		return "trade_candidate"
	default:
		return "unknown"
	}
}

// CandidateEnvelope is OUR OWN unsigned arbitrage tx for a candidate simulation
// (§8.3). It carries an EXPLICIT real sender so execution can proceed WITHOUT
// signature recovery, while nonce/balance/gas/fee/current-BSC-rule checks are all
// still enforced (design §237: "跳过的只有我方签名恢复"). It is a dynamic-fee tx.
type CandidateEnvelope struct {
	From      common.Address  // real reserved sender (never a silent zero, §7.1)
	To        *common.Address // nil == contract creation (not used by our executor)
	Nonce     uint64
	Value     *big.Int
	GasLimit  uint64
	GasFeeCap *big.Int
	GasTipCap *big.Int
	Data      []byte
}

// CandidateResult is the outcome of a candidate simulation (§8.3): the signed target
// prefix outcomes plus OUR candidate's outcome. It is diagnostic/build-phase data for
// the §10 gas model and four-point ledger — NEVER a signing proof (Signed is always
// false; the final admission path re-simulates the exact signed raw via a bundle sim).
type CandidateResult struct {
	Purpose CandidatePurpose
	// PrefixOutcomes is one entry per signed target tx ACTUALLY attempted (stops early
	// if a target tx is non-success — then Candidate stays nil).
	PrefixOutcomes []PrefixTxOutcome
	// PrefixCompleted is true iff every signed target tx succeeded.
	PrefixCompleted bool
	// Candidate is OUR unsigned tx's outcome, non-nil ONLY when the prefix completed.
	Candidate *PrefixTxOutcome
	// Signed is ALWAYS false: a candidate sim can never be a signing artifact (§8.3).
	Signed bool
	// PostState is the isolated post-candidate state, non-nil ONLY when the prefix AND
	// the candidate all succeeded. It is a quotable read handle, not a send permit.
	PostState *state.StateDB
	// PrefixPostState is an isolated snapshot taken AFTER the signed prefix completed
	// but BEFORE our unsigned candidate ran — the §391 T0 boundary for the candidate's
	// standardized base-token retention. Non-nil ONLY when the prefix completed (it is
	// captured before we know whether the candidate will succeed). It is a Copy, so the
	// later candidate execution never mutates it; reads run on it via the same read-only
	// PostStatePoolCaller path. Isolating T0 here (not the base state) means the retention
	// delta measured against PostState reflects OUR candidate alone, excluding any effect
	// the victim prefix may have had on the beneficiary.
	PrefixPostState *state.StateDB
	Budget          *arb.ReadBudget
}

// targetExecutor binds a fixed parent header + base state and executes targets. It
// depends only on core.ChainContext (config + engine + header lookups) so it is
// testable against a generated chain (the §441 harness) without a full *Ethereum.
type targetExecutor struct {
	chain  core.ChainContext
	parent *types.Header
	base   *state.StateDB // borrowed base; each ExecuteTarget works on a Copy
	author common.Address
}

// newTargetExecutor resolves the explicit author (never a silent zero, design §7.1)
// and captures the base. The base must already be the parent's post-final state.
func newTargetExecutor(chain core.ChainContext, parent *types.Header, base *state.StateDB) *targetExecutor {
	author := parent.Coinbase
	if a, err := chain.Engine().Author(parent); err == nil {
		author = a
	}
	return &targetExecutor{chain: chain, parent: parent, base: base, author: author}
}

// applyPreamble reproduces the pre-transaction block system calls on the given EVM
// (over the isolated copy), mirroring core.Process before its tx loop. This makes
// the state "post-preamble, pre-target". num/timeSec are the TARGET block's number and
// timestamp (real values from a verified block_env, or the parent+1 heuristic) and
// gate the fork-specific calls exactly as Process does — so the preamble's fork
// decisions always match the block context the txs run under.
func (x *targetExecutor) applyPreamble(evm *vm.EVM, work *state.StateDB, num *big.Int, timeSec uint64) {
	cfg := x.chain.Config()

	// BSC built-in system contract code upgrades at block begin (parlia-specific).
	systemcontracts.TryUpdateBuildInSystemContract(cfg, num, x.parent.Time, timeSec, work, true)

	// EIP-4788 beacon root (if the built env carries one) and EIP-2935 parent hash
	// (Prague/Verkle). We build against the parent, so the parent block hash is the
	// history entry to store.
	if cfg.IsPrague(num, timeSec) || cfg.IsVerkle(num, timeSec) {
		core.ProcessParentBlockHash(x.parent.Hash(), evm)
	}
}

// blockTime returns the target block timestamp. First iteration: parent time + 1s
// as a placeholder; the real env's consensus timestamp is supplied via block_env in
// the RPC path (design §7.1). Kept explicit so it is never a silent zero.
func (x *targetExecutor) blockTime() uint64 { return x.parent.Time + 1 }

// execSession is one isolated execution context: a private Copy of the base, its
// budgeted wrapper, the EVM over the fixed target block env, and the SHARED block gas
// pool. The block preamble (§357) is replayed exactly ONCE at construction, so the
// session state is "post-preamble, pre-first-tx". Both ExecuteTarget (single tx) and
// ExecutePrefix (ordered bundle) drive the SAME session so the two paths never drift.
type execSession struct {
	x       *targetExecutor
	work    *state.StateDB
	evm     *vm.EVM
	num     *big.Int
	signer  types.Signer
	baseFee *big.Int
	gp      *core.GasPool // shared across the whole prefix, like a real block
	budget  *arb.ReadBudget
}

// newExecSession isolates the base, builds the target-env EVM with the EXPLICIT
// author (§7.1), and replays the block preamble once. When env is non-nil it supplies
// the REAL N+1 block environment (number/time/author/gasLimit/difficulty/baseFee) from
// a node-verified block_env, closing the env-dependent parity gap; when env is nil the
// executor falls back to the parent+1 heuristic (env-INDEPENDENT scope, e.g. the §441
// harness and value transfers whose gas is env-agnostic).
func (x *targetExecutor) newExecSession(budget *arb.ReadBudget, env *resolvedBlockEnv) *execSession {
	work := x.base.Copy() // isolate; lazy cache fills never touch the borrowed base
	wrapped := newBudgetedStateDB(work, budget)

	// Resolve the target block env: real values from a verified block_env, else the
	// honest parent+1 heuristic.
	var (
		num        *big.Int
		timeSec    uint64
		author     common.Address
		gasLimit   uint64
		difficulty *big.Int
		baseFee    *big.Int
		getHash    vm.GetHashFunc
	)
	if env != nil {
		num = new(big.Int).Set(env.number)
		timeSec = env.timeSec
		author = env.author
		gasLimit = env.gasLimit
		difficulty = new(big.Int).Set(env.difficulty)
		baseFee = env.baseFee
		// A verified env gives us the true parent hash linkage, so a synthetic N+1
		// header (ParentHash = parent.Hash(), Number = N+1) makes GetHashFn resolve
		// BLOCKHASH(parent.number) correctly — closing the off-by-one the heuristic
		// path documents below.
		n1 := &types.Header{Number: new(big.Int).Set(num), ParentHash: x.parent.Hash()}
		getHash = core.GetHashFn(n1, x.chain)
	} else {
		num = new(big.Int).Add(x.parent.Number, big.NewInt(1))
		timeSec = x.blockTime()
		author = x.author
		gasLimit = x.parent.GasLimit
		difficulty = new(big.Int).Set(x.parent.Difficulty)
		baseFee = x.baseFee()
		// BLOCKHASH resolution (honest limitation of the heuristic path): GetHashFn
		// walks back from ref.ParentHash, so using the parent header as ref resolves
		// BLOCKHASH for numbers < parent.number correctly but is off-by-one for
		// BLOCKHASH(parent.number) in an N+1 simulation. Target txs that read BLOCKHASH
		// of the immediate parent are therefore not exact without a block_env; the
		// env path above closes this. Simple value/DEX target txs do not use BLOCKHASH.
		getHash = core.GetHashFn(x.parent, x.chain)
	}

	// BlobBaseFee:core state_transition.go:573 在 BSC Cancun 后跑 blobFee.Mul(_, BlobBaseFee),
	// 必解引用它;手拼 BlockContext 漏填=nil → 每个 target 100% panic(空指针)。镜像
	// core.NewEVMBlockContext 的 guard:parent 有 ExcessBlobGas(BSC 现网 Cancun 已激活)才算,
	// 否则留 nil 且此时 IsCancun=false,core 不会走到 573。用 parent 的 excess 作 N+1 近似
	// (启发式,与 parent+1 一致;block_env 未来可带 excess_blob_gas 求精确)。
	var blobBaseFee *big.Int
	if x.parent.ExcessBlobGas != nil {
		blobBaseFee = eip4844.CalcBlobFee(x.chain.Config(), x.parent)
	}
	blockCtx := vm.BlockContext{
		CanTransfer: core.CanTransfer,
		Transfer:    core.Transfer,
		GetHash:     getHash,
		Coinbase:    author, // EXPLICIT author, never a silent zero (§7.1)
		BlockNumber: num,
		Time:        timeSec,
		Difficulty:  difficulty,
		GasLimit:    gasLimit,
		BaseFee:     baseFee,
		BlobBaseFee: blobBaseFee,
	}
	evm := vm.NewEVM(blockCtx, wrapped, x.chain.Config(), vm.Config{})
	wrapped.SetCancel(evm.Cancel)

	x.applyPreamble(evm, work, num, timeSec)

	return &execSession{
		x:       x,
		work:    work,
		evm:     evm,
		num:     num,
		signer:  types.MakeSigner(x.chain.Config(), num, timeSec),
		baseFee: baseFee,
		gp:      new(core.GasPool).AddGas(blockCtx.GasLimit),
		budget:  budget,
	}
}

// PostStatePoolCaller builds a read-only pool caller over a successful execution's
// PostState (the §256 quotable post-target handle) — the NODE-03→NODE-04 bridge that
// backs arb_getPostPoolState's on-demand refills. It reuses the SAME budgeted-wrapper +
// StaticCall read path as the head reader, so post-target pool reads inherit the same
// read-only guarantee and budgeting. Reads run on a fresh Copy of post, so the quotable
// handle itself is never mutated by a read. Returns nil if post is nil (never a silent
// caller over the wrong state).
//
// A read-only view call ignores block coinbase/number/time, so binding the executor's
// parent header for the block context is correct for pool getters (getReserves/slot0);
// the post-target STATE is what carries the target's effects, which is the point.
func (x *targetExecutor) PostStatePoolCaller(post *state.StateDB, budget *arb.ReadBudget) *poolCaller {
	if post == nil {
		return nil
	}
	return newPoolCallerOver(x.chain, post, x.parent, budget)
}

// applyOne applies a single signed tx at ordinal `index` within the session and
// returns its fully classified outcome. It mutates the session's shared state and gas
// pool, so a caller running a prefix must apply txs in order and stop at the first
// non-success (§357). A message-build failure classifies as a core/validity error
// (never infra).
func (s *execSession) applyOne(tx *types.Transaction, index int) PrefixTxOutcome {
	// Build the message against the TARGET env (never pool head, design §273). This
	// path RECOVERS the sender from the signature (TransactionToMessage), so it is
	// only for SIGNED txs (the target prefix). Our own unsigned candidate goes through
	// applyMessage with an injected sender instead.
	msg, err := core.TransactionToMessage(tx, s.signer, s.baseFee)
	if err != nil {
		return PrefixTxOutcome{Class: arb.Classify(arb.ExecSignals{CoreError: true})}
	}
	return s.applyMessage(msg, tx, index)
}

// applyMessage is the ONE low-level application path shared by every mode: it drives
// ApplyTransactionWithEVM with an explicit *core.Message on the session's shared state
// and gas pool, then classifies honestly. The `tx` is used only for receipt metadata
// (hash/type/context) — execution uses `msg` (so msg.From is the acting sender, which
// is how the candidate path injects a real sender WITHOUT signature recovery).
func (s *execSession) applyMessage(msg *core.Message, tx *types.Transaction, index int) PrefixTxOutcome {
	// Per-tx context (§8.2): establish txHash/index before applying.
	s.work.SetTxContext(tx.Hash(), index)

	var usedGas uint64
	receipt, applyErr := core.ApplyTransactionWithEVM(
		msg, s.gp, s.work, s.num, common.Hash{}, s.x.blockTime(), tx, &usedGas, s.evm,
	)

	sig := s.x.collectSignals(s.budget, s.evm, s.work, receipt, applyErr)
	class := arb.Classify(sig)

	out := PrefixTxOutcome{Class: class}
	if class.ReceiptTrusted {
		out.Receipt = receipt
		out.UsedGas = receipt.GasUsed
	}
	return out
}

// ExecuteTarget runs one signed target tx on a fresh isolated copy of the base,
// under a read budget. It returns a fully classified result. On a real success the
// PostState is the isolated post-target state (caller may build a post handle); on
// any non-success it is nil (design §256).
func (x *targetExecutor) ExecuteTarget(tx *types.Transaction, budget *arb.ReadBudget) (*TargetResult, error) {
	return x.executeTarget(tx, budget, nil)
}

// ExecuteTargetWithEnv is the env-AWARE entry (RPC path, §7.1): it resolves + verifies
// the wire block_env against the bound parent (parent-hash linkage, N+1 number,
// node-derived base_fee match) and runs the target under the REAL N+1 environment,
// closing the env-dependent parity gap. A malformed/unlinked env is a hard error (never
// silently downgraded to the heuristic).
func (x *targetExecutor) ExecuteTargetWithEnv(tx *types.Transaction, budget *arb.ReadBudget, env arb.BlockEnv) (*TargetResult, error) {
	renv, err := x.resolveBlockEnv(env)
	if err != nil {
		return nil, err
	}
	return x.executeTarget(tx, budget, renv)
}

func (x *targetExecutor) executeTarget(tx *types.Transaction, budget *arb.ReadBudget, env *resolvedBlockEnv) (*TargetResult, error) {
	s := x.newExecSession(budget, env)
	out := s.applyOne(tx, 0)

	res := &TargetResult{Class: out.Class, Budget: budget}
	if out.Class.ReceiptTrusted {
		res.Receipt = out.Receipt
		res.UsedGas = out.UsedGas
	}
	if out.Class.Status == arb.StatusSuccess {
		res.PostState = s.work // isolated post-target state for a quotable handle
	}
	return res, nil
}

// ExecutePrefix runs an ordered signed prefix (e.g. the backrun bundle [target, ours],
// design §357) on ONE isolated state under ONE shared read budget and ONE shared block
// gas pool — mirroring how the txs would sit together in a real block. It stops at the
// FIRST non-success tx: a backrun's later txs are sized for the earlier txs' on-chain
// effects, so a post state built atop a reverted/failed prefix is meaningless and is
// never produced (§256). PostState is non-nil ONLY when the WHOLE prefix succeeded.
func (x *targetExecutor) ExecutePrefix(txs []*types.Transaction, budget *arb.ReadBudget) (*PrefixResult, error) {
	return x.executePrefix(txs, budget, nil)
}

// ExecutePrefixWithEnv is the env-AWARE prefix entry (RPC path, §7.1): resolves +
// verifies the wire block_env against the bound parent, then runs the ordered prefix
// under the REAL N+1 environment. A malformed/unlinked env is a hard error.
func (x *targetExecutor) ExecutePrefixWithEnv(txs []*types.Transaction, budget *arb.ReadBudget, env arb.BlockEnv) (*PrefixResult, error) {
	renv, err := x.resolveBlockEnv(env)
	if err != nil {
		return nil, err
	}
	return x.executePrefix(txs, budget, renv)
}

func (x *targetExecutor) executePrefix(txs []*types.Transaction, budget *arb.ReadBudget, env *resolvedBlockEnv) (*PrefixResult, error) {
	s := x.newExecSession(budget, env)
	res := &PrefixResult{Budget: budget, Outcomes: make([]PrefixTxOutcome, 0, len(txs))}

	for i, tx := range txs {
		out := s.applyOne(tx, i)
		res.Outcomes = append(res.Outcomes, out)
		if out.Class.Status != arb.StatusSuccess {
			// Stop the prefix; no quotable post handle on a non-success (§256/§357).
			return res, nil
		}
	}

	res.Completed = true
	res.PostState = s.work // full-prefix isolated post state for a quotable handle
	return res, nil
}

// candidateMessage builds a *core.Message for OUR unsigned envelope with an EXPLICIT
// sender (no signature recovery), replicating geth's effective-gas-price rule
// (min(feeCap, tipCap+baseFee)). SkipNonceChecks/SkipTransactionChecks stay FALSE so
// nonce, EOA, gas-limit and fee-cap-vs-basefee checks are ALL still enforced (§237).
func (s *execSession) candidateMessage(env CandidateEnvelope) *core.Message {
	feeCap := env.GasFeeCap
	if feeCap == nil {
		feeCap = new(big.Int)
	}
	tipCap := env.GasTipCap
	if tipCap == nil {
		tipCap = new(big.Int)
	}
	// Effective gas price = min(feeCap, tipCap + baseFee), matching TransactionToMessage.
	gasPrice := new(big.Int).Set(feeCap)
	if s.baseFee != nil {
		eff := new(big.Int).Add(tipCap, s.baseFee)
		if eff.Cmp(feeCap) < 0 {
			gasPrice = eff
		}
	}
	val := env.Value
	if val == nil {
		val = new(big.Int)
	}
	return &core.Message{
		To:                    env.To,
		From:                  env.From,
		Nonce:                 env.Nonce,
		Value:                 val,
		GasLimit:              env.GasLimit,
		GasPrice:              gasPrice,
		GasFeeCap:             feeCap,
		GasTipCap:             tipCap,
		Data:                  env.Data,
		SkipNonceChecks:       false, // §237: nonce IS checked
		SkipTransactionChecks: false, // §237: EOA + gaslimit + fee rules ALL checked
	}
}

// RunCandidate simulates the signed target prefix followed by OUR OWN unsigned
// arbitrage envelope on one isolated state (§8.3 arb_simulateCandidate). It exists to
// collect diagnostic/build-phase gas + ledger data — it NEVER signs and NEVER yields a
// send permit (Signed stays false); final admission re-simulates the exact signed raw
// via a bundle sim. Our candidate runs ONLY if every signed target tx succeeded first
// (a candidate sized for the target's effects is meaningless atop a failed prefix).
//
// A candidate that FAILS validity (nonce/balance/gas/fee/BSC rule) or reverts is a
// legitimate, honestly-classified outcome (the build phase learns from it), NOT an
// error — so err is reserved for nothing here; the classification carries the truth.
func (x *targetExecutor) RunCandidate(
	signedPrefix []*types.Transaction, ours CandidateEnvelope,
	purpose CandidatePurpose, budget *arb.ReadBudget,
) (*CandidateResult, error) {
	return x.runCandidate(signedPrefix, ours, purpose, budget, nil)
}

// RunCandidateWithEnv is the env-AWARE candidate entry (RPC path, §7.1): resolves +
// verifies the wire block_env against the bound parent, then runs the signed prefix and
// our unsigned envelope under the REAL N+1 environment. A malformed/unlinked env is a
// hard error. Signed stays false regardless (§8.3).
func (x *targetExecutor) RunCandidateWithEnv(
	signedPrefix []*types.Transaction, ours CandidateEnvelope,
	purpose CandidatePurpose, budget *arb.ReadBudget, env arb.BlockEnv,
) (*CandidateResult, error) {
	renv, err := x.resolveBlockEnv(env)
	if err != nil {
		return nil, err
	}
	return x.runCandidate(signedPrefix, ours, purpose, budget, renv)
}

func (x *targetExecutor) runCandidate(
	signedPrefix []*types.Transaction, ours CandidateEnvelope,
	purpose CandidatePurpose, budget *arb.ReadBudget, env *resolvedBlockEnv,
) (*CandidateResult, error) {
	s := x.newExecSession(budget, env)
	res := &CandidateResult{
		Purpose:        purpose,
		Signed:         false, // §8.3: a candidate sim is never a signing artifact
		Budget:         budget,
		PrefixOutcomes: make([]PrefixTxOutcome, 0, len(signedPrefix)),
	}

	// 1) Signed target prefix, in order, stopping at the first non-success.
	for i, tx := range signedPrefix {
		out := s.applyOne(tx, i)
		res.PrefixOutcomes = append(res.PrefixOutcomes, out)
		if out.Class.Status != arb.StatusSuccess {
			return res, nil // prefix failed -> no candidate, no post handle
		}
	}
	res.PrefixCompleted = true
	// §391 T0 boundary: snapshot the post-prefix state BEFORE our candidate mutates it.
	// Copy() isolates it so the candidate execution below never changes what T0 reads
	// (s.work keeps advancing into the T2/post-candidate state). Captured unconditionally
	// once the prefix completed; the RPC layer decides whether to actually read it.
	res.PrefixPostState = s.work.Copy()

	// 2) OUR unsigned candidate, with an injected real sender (no sig recovery).
	msg := s.candidateMessage(ours)
	// Its ordinal follows the prefix; the receipt hash uses a synthetic marker since an
	// unsigned envelope has no canonical tx hash. We build a minimal typed tx purely
	// for receipt metadata (execution uses msg, not this tx's signature).
	metaTx := types.NewTx(&types.DynamicFeeTx{
		Nonce:     ours.Nonce,
		To:        ours.To,
		Value:     msg.Value,
		Gas:       ours.GasLimit,
		GasFeeCap: msg.GasFeeCap,
		GasTipCap: msg.GasTipCap,
		Data:      ours.Data,
	})
	candOut := s.applyMessage(msg, metaTx, len(signedPrefix))
	res.Candidate = &candOut

	if candOut.Class.Status == arb.StatusSuccess {
		res.PostState = s.work // quotable post-candidate handle (NOT a send permit)
	}
	return res, nil
}

// collectSignals maps raw execution outputs to ExecSignals with honest priority
// inputs. Order of extraction does not matter (Classify imposes priority); we only
// set each boolean truthfully.
func (x *targetExecutor) collectSignals(
	budget *arb.ReadBudget, evm *vm.EVM, work *state.StateDB,
	receipt *types.Receipt, applyErr error,
) arb.ExecSignals {
	var sig arb.ExecSignals

	// Budget latch distinguishes deadline vs read-cap.
	switch budget.Err() {
	case arb.ErrDeadlineExceeded:
		sig.DeadlineExceeded = true
	case arb.ErrReadBudgetExceeded:
		sig.ReadBudgetExhausted = true
	}
	// Explicit EVM cancel not attributable to the budget deadline is a control cancel.
	if evm.Cancelled() && !sig.DeadlineExceeded && !sig.ReadBudgetExhausted {
		sig.ControlCancelled = true
	}
	// Backend read error surfaced by the state DB (excludes the budget/cancel cases
	// above, which are not infra failures).
	if err := work.Error(); err != nil && !budget.Failed() {
		sig.BackendReadError = true
	}
	// Core/validity error from application (nonce/balance/fee/intrinsic/gaspool).
	if applyErr != nil {
		sig.CoreError = isCoreError(applyErr)
		// A non-core apply error with no budget/infra signal is still not a success;
		// treat unknown apply errors conservatively as infra failure.
		if !sig.CoreError && !sig.DeadlineExceeded && !sig.ReadBudgetExhausted &&
			!sig.ControlCancelled && !sig.BackendReadError {
			sig.BackendReadError = true
		}
	}
	// Applied + receipt only meaningful when there was no apply error.
	if applyErr == nil && receipt != nil {
		sig.Applied = true
		sig.ReceiptFailed = receipt.Status == types.ReceiptStatusFailed
	}
	return sig
}

// classifyOnly builds a result from signals with no receipt (early-exit paths).
func (x *targetExecutor) classifyOnly(sig arb.ExecSignals, budget *arb.ReadBudget) *TargetResult {
	return &TargetResult{Class: arb.Classify(sig), Budget: budget}
}

// baseFee returns the TARGET (N+1) block base fee, derived from the parent via the
// same EIP-1559 rule the block builder uses (consensus/misc/eip1559.CalcBaseFee) —
// NOT a stale copy of the parent's own base fee. Copying the parent value is wrong on
// any block where the base fee moves (it trips ErrFeeCapTooLow against a tx priced
// for the real N+1 fee). When the chain has no base fee (pre-London), this is nil.
// The RPC path may override with the exact consensus value via block_env (design
// §7.1); this derivation is the honest default and is what the §441 harness compares
// against.
func (x *targetExecutor) baseFee() *big.Int {
	if x.parent.BaseFee == nil {
		return nil
	}
	return eip1559.CalcBaseFee(x.chain.Config(), x.parent)
}

// isCoreError reports whether err is a consensus/validity core error (as opposed to
// a receipt-level revert/OOG, which is not an error here). Uses errors.Is against
// the known sentinels, never string parsing (design §349).
func isCoreError(err error) bool {
	for _, target := range []error{
		core.ErrNonceTooLow,
		core.ErrNonceTooHigh,
		core.ErrInsufficientFunds,
		core.ErrInsufficientFundsForTransfer,
		core.ErrIntrinsicGas,
		core.ErrGasLimitReached,
		core.ErrTipVeryHigh,
		core.ErrFeeCapVeryHigh,
		core.ErrTipAboveFeeCap,
		core.ErrFeeCapTooLow,
		core.ErrSenderNoEOA,
	} {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

// NewTargetExecutorAtHead builds an executor at the current canonical head, probing
// readability first (design §5.1). Provided for integration; the harness builds its
// own executor over a generated chain's parent state.
func (s *Ethereum) NewTargetExecutorAtHead() (*targetExecutor, error) {
	h := s.blockchain.CurrentBlock()
	if h == nil {
		return nil, errors.New("arb: no canonical head")
	}
	base, err := s.blockchain.StateAt(h.Root)
	if err != nil {
		return nil, err
	}
	return newTargetExecutor(s.blockchain, h, base), nil
}

// newReadBudget is a small helper for callers that want a wall-clock-bounded budget.
func newReadBudget(maxReads uint64, wall time.Duration) *arb.ReadBudget {
	return arb.NewReadBudget(func() time.Time { return time.Now() }, maxReads, wall)
}
