// Result mapping + the remaining two simulate methods (arb_simulateCandidate and
// arb_getPostPoolState), plus small adapter helpers shared by the sim layer. The
// mapping functions turn an executor result (TargetResult/PrefixResult/
// CandidateResult) into the wire JobOutcome, and on a real success register the
// isolated PostState as a CHILD handle so a later arb_getPostPoolState can read
// pools on it (design §206: job completion != handle destruction; the post handle
// has its own lifetime, kept alive by the child ref on the parent lease).

package eth

import (
	"errors"
	"math/big"
	"strconv"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/eth/arb"
)

const (
	// defaultMaxReads bounds logical state reads per job (design §383 combined
	// limits; a fuller per-request override can be threaded later).
	defaultMaxReads = 200_000
	// postHandleTTL is how long a produced post handle stays borrowable before the
	// store closes it; the client releases sooner via releaseState.
	postHandleTTL = 30 * time.Second
)

func microsToDuration(us uint64) time.Duration { return time.Duration(us) * time.Microsecond }

// registerPostChild binds a successful run's isolated PostState as a child handle of
// the parent, returning the child id (the wire post_handle). The child keeps the
// parent's backend lease alive (§202) until released. Returns "" if post is nil
// (no quotable state on non-success, §256/§357) or the child cannot be created.
func (a *arbService) registerPostChild(parentHandle string, ident arb.HandleIdentity, post *state.StateDB, header *types.Header) string {
	if post == nil {
		return ""
	}
	childID, err := a.handles.CreateChild(parentHandle, ident, postHandleTTL)
	if err != nil {
		return ""
	}
	a.handleStates.put(childID, &pinnedState{header: header, root: header.Root, base: post})
	return childID
}

// meteringFrom builds the wire metering from a finished read budget and gas.
func meteringFrom(budget *arb.ReadBudget, gasUsed uint64) arb.Metering {
	var reads uint64
	if budget != nil {
		reads = budget.Reads()
	}
	return arb.Metering{StateReads: reads, GasUsed: gasUsed}
}

// mapTargetOutcome maps a single-target result. On success the post state becomes a
// child handle; otherwise post_handle is "".
func (api *ArbAPI) mapTargetOutcome(parentHandle string, ident arb.HandleIdentity, res *TargetResult, budget *arb.ReadBudget) *arb.JobOutcome {
	success := res.Class.Status == arb.StatusSuccess
	gas := uint64(0)
	if res.Class.ReceiptTrusted {
		gas = res.UsedGas
	}
	post := ""
	if success && res.PostState != nil {
		ps, _ := api.svc.handleStates.get(parentHandle)
		if ps != nil {
			post = api.svc.registerPostChild(parentHandle, ident, res.PostState, ps.header)
		}
	}
	return &arb.JobOutcome{
		Kind:       "target",
		Metering:   meteringFrom(budget, gas),
		Complete:   success,
		PostHandle: post,
		ErrCode:    nonSuccessCode(res.Class.Status),
	}
}

// mapPrefixOutcome maps a signed-bundle result. Post handle only on full completion.
func (api *ArbAPI) mapPrefixOutcome(parentHandle string, ident arb.HandleIdentity, res *PrefixResult, budget *arb.ReadBudget) *arb.JobOutcome {
	gas := uint64(0)
	for _, o := range res.Outcomes {
		if o.Class.ReceiptTrusted {
			gas += o.UsedGas
		}
	}
	post := ""
	if res.Completed && res.PostState != nil {
		ps, _ := api.svc.handleStates.get(parentHandle)
		if ps != nil {
			post = api.svc.registerPostChild(parentHandle, ident, res.PostState, ps.header)
		}
	}
	code := ""
	if !res.Completed {
		// The first non-success tx's status is the honest failure signal.
		if len(res.Outcomes) > 0 {
			code = nonSuccessCode(res.Outcomes[len(res.Outcomes)-1].Class.Status)
		} else {
			code = "empty_prefix"
		}
	}
	return &arb.JobOutcome{
		Kind:       "bundle",
		Metering:   meteringFrom(budget, gas),
		Complete:   res.Completed,
		PostHandle: post,
		ErrCode:    code,
	}
}

// mapCandidateOutcome maps a candidate result. Signed is always false (§8.3); the
// post handle appears only when prefix AND candidate all succeeded.
func (api *ArbAPI) mapCandidateOutcome(parentHandle string, ident arb.HandleIdentity, res *CandidateResult, budget *arb.ReadBudget) *arb.JobOutcome {
	gas := uint64(0)
	for _, o := range res.PrefixOutcomes {
		if o.Class.ReceiptTrusted {
			gas += o.UsedGas
		}
	}
	if res.Candidate != nil && res.Candidate.Class.ReceiptTrusted {
		gas += res.Candidate.UsedGas
	}
	success := res.PrefixCompleted && res.Candidate != nil && res.Candidate.Class.Status == arb.StatusSuccess
	post := ""
	if success && res.PostState != nil {
		ps, _ := api.svc.handleStates.get(parentHandle)
		if ps != nil {
			post = api.svc.registerPostChild(parentHandle, ident, res.PostState, ps.header)
		}
	}
	code := ""
	if !success {
		if !res.PrefixCompleted && len(res.PrefixOutcomes) > 0 {
			code = nonSuccessCode(res.PrefixOutcomes[len(res.PrefixOutcomes)-1].Class.Status)
		} else if res.Candidate != nil {
			code = nonSuccessCode(res.Candidate.Class.Status)
		} else {
			code = "candidate_not_run"
		}
	}
	return &arb.JobOutcome{
		Kind:       "candidate",
		Metering:   meteringFrom(budget, gas),
		Complete:   success,
		PostHandle: post,
		ErrCode:    code,
	}
}

// measureRetention fills out.RetainedT0/RetainedT2 with the §391 base-token balance of
// the beneficiary at the two isolated boundaries. Called ONLY on a successful candidate
// with a caller-named subject. It reads on private Copies of the T0/T2 states via the
// same budgeted read-only StaticCall path as pool reads, so it never mutates a quotable
// handle and cannot become a send permit. Best-effort and all-or-nothing: if either read
// fails/trips the budget, or a state snapshot is unexpectedly nil, BOTH fields stay ""
// (unmeasured) — an honest candidate result is never discarded for a missing gross seed,
// and a half-measured pair is never emitted (§389).
func (api *ArbAPI) measureRetention(x *targetExecutor, res *CandidateResult, mt measureTarget, budget *arb.ReadBudget, out *arb.JobOutcome) {
	t0Caller := x.PostStatePoolCaller(res.PrefixPostState, budget)
	t2Caller := x.PostStatePoolCaller(res.PostState, budget)
	if t0Caller == nil || t2Caller == nil {
		return // a snapshot was nil (should not happen on success) — stay unmeasured, never faked
	}
	t0, err := t0Caller.BalanceOf(mt.baseToken, mt.executor)
	if err != nil {
		return
	}
	t2, err := t2Caller.BalanceOf(mt.baseToken, mt.executor)
	if err != nil {
		return
	}
	// Both clean: emit the raw retention values. gross = T2 - T0 and the non-positive
	// rejection are the client's decision (§391), never inferred here.
	out.RetainedT0 = t0.String()
	out.RetainedT2 = t2.String()
}

// nonSuccessCode maps a non-success exec status to a wire error code; success -> "".
func nonSuccessCode(s arb.ExecStatus) string {
	if s == arb.StatusSuccess {
		return ""
	}
	return s.String()
}

// errMeasureTargetPairing: executor/base_token must be supplied TOGETHER or not at
// all, and each (when present) must be a valid address. Half a pair, or a malformed
// address, is a client mistake — rejected up front rather than silently unmeasured.
var errMeasureTargetPairing = errors.New("arb: executor and base_token must both be present valid addresses, or both absent")

// measureTarget names the §391 retention-measurement subject for a candidate sim.
type measureTarget struct {
	measure   bool           // false: caller asked for no retention measurement
	executor  common.Address // beneficiary whose base-token balance is read
	baseToken common.Address // standardized base token (e.g. WBNB) contract
}

// decodeMeasureTarget validates the optional executor/base_token pair. Both empty is
// the honest "don't measure" case (e.g. arb contract not deployed yet). Both valid is
// "measure". Anything else (one present, or a malformed address) is a client error —
// the node never guesses or hardcodes a beneficiary address (§389).
func decodeMeasureTarget(executor, baseToken string) (measureTarget, error) {
	if executor == "" && baseToken == "" {
		return measureTarget{measure: false}, nil
	}
	if !isAddressWire(executor) || !isAddressWire(baseToken) {
		return measureTarget{}, errMeasureTargetPairing
	}
	return measureTarget{
		measure:   true,
		executor:  common.HexToAddress(executor),
		baseToken: common.HexToAddress(baseToken),
	}, nil
}

// SimulateCandidate -> arb_simulateCandidate. Runs the signed target prefix then OUR
// unsigned envelope (no signature recovery, all other rules enforced, §237). Signed
// is always false in the result. When executor+base_token are supplied it also measures
// the §391 base-token retention at T0 (post-prefix, pre-candidate) and T2 (post-candidate).
func (api *ArbAPI) SimulateCandidate(args SimulateCandidateArgs) (*JobIDResult, error) {
	ident, err := api.commonSimValidate(args.RequestID, args.Stamp, args.ParentHandle, args.BudgetMicros)
	if err != nil {
		return nil, err
	}
	target, err := decodeRaw(args.TargetRaw)
	if err != nil {
		return nil, err
	}
	env, err := decodeUnsigned(args.UnsignedTx)
	if err != nil {
		return nil, err
	}
	mt, err := decodeMeasureTarget(args.Executor, args.BaseToken)
	if err != nil {
		return nil, err
	}
	// The measurement subject is part of request identity (§295): the same target+envelope
	// asked WITH vs WITHOUT retention measurement, or against a different beneficiary/base
	// token, are genuinely different requests and must not alias one job's cached result.
	dig := arb.NewDigest("arb.req.simulateCandidate.v1").
		FieldBytes([]byte(args.ParentHandle)).
		FieldBytes([]byte(args.Stamp)).
		FieldBytes(target.Hash().Bytes()).
		FieldBytes(env.From.Bytes()).
		FieldU64(env.Nonce).
		FieldU64(args.BudgetMicros)
	if mt.measure {
		dig.FieldBytes(mt.executor.Bytes()).FieldBytes(mt.baseToken.Bytes())
	}
	digest := hex32Str(dig.Finalize())

	run := func(budget *arb.ReadBudget) (*arb.JobOutcome, bool) {
		base, header, release, berr := api.svc.borrowBase(args.ParentHandle, ident)
		if berr != nil {
			return &arb.JobOutcome{Kind: "candidate", ErrCode: berr.Error()}, false
		}
		defer release()
		x := newTargetExecutor(api.svc.eth.blockchain, header, base)
		res, xerr := x.RunCandidateWithEnv([]*types.Transaction{target}, env, PurposeDiagnostic, budget, args.BlockEnv)
		if xerr != nil {
			return &arb.JobOutcome{Kind: "candidate", ErrCode: xerr.Error()}, false
		}
		out := api.mapCandidateOutcome(args.ParentHandle, ident, res, budget)
		// §391 retention: measure ONLY when the caller named a subject AND the candidate
		// actually succeeded (out.Complete). T0 reads the isolated post-prefix snapshot,
		// T2 the post-candidate state — same budgeted read-only path as pool reads. A read
		// failure leaves both fields "" (unmeasured) without failing the job: an honest
		// candidate result must not be discarded because the optional gross seed couldn't
		// be read (§389 — the client then treats gross as unmeasured, never fabricated).
		if mt.measure && out.Complete {
			api.measureRetention(x, res, mt, budget, out)
		}
		return out, out.Complete
	}

	jobID, err := api.submitAndEnqueue(args.RequestID, args.Stamp, digest, args.BudgetMicros, run)
	if err != nil {
		return nil, err
	}
	return &JobIDResult{JobID: jobID}, nil
}

// decodeUnsigned turns the wire unsigned envelope into a CandidateEnvelope. from must
// be an explicit non-zero sender (§7.1); amounts decode from decimal-or-hex strings.
func decodeUnsigned(u UnsignedEnvelope) (CandidateEnvelope, error) {
	if !isAddressWire(u.From) {
		return CandidateEnvelope{}, errBadSender
	}
	from := common.HexToAddress(u.From)
	if from == (common.Address{}) {
		return CandidateEnvelope{}, errors.New("arb: unsigned_tx.from must be explicit non-zero")
	}
	value, err := decodeBig(u.Value)
	if err != nil {
		return CandidateEnvelope{}, errors.New("arb: unsigned_tx.value invalid")
	}
	feeCap, err := decodeBig(u.GasFeeCap)
	if err != nil {
		return CandidateEnvelope{}, errors.New("arb: unsigned_tx.gas_fee_cap invalid")
	}
	tipCap, err := decodeBig(u.GasTipCap)
	if err != nil {
		return CandidateEnvelope{}, errors.New("arb: unsigned_tx.gas_tip_cap invalid")
	}
	var to *common.Address
	if u.To != "" {
		if !isAddressWire(u.To) {
			return CandidateEnvelope{}, errors.New("arb: unsigned_tx.to invalid")
		}
		t := common.HexToAddress(u.To)
		to = &t
	}
	var data []byte
	if u.Data != "" {
		b, derr := hexutil.Decode(u.Data)
		if derr != nil {
			return CandidateEnvelope{}, errors.New("arb: unsigned_tx.data invalid hex")
		}
		data = b
	}
	return CandidateEnvelope{
		From:      from,
		To:        to,
		Nonce:     u.Nonce,
		Value:     value,
		GasLimit:  u.GasLimit,
		GasFeeCap: feeCap,
		GasTipCap: tipCap,
		Data:      data,
	}, nil
}

// decodeBig parses a decimal or 0x-hex string into a non-negative big.Int. Empty
// string is treated as zero.
func decodeBig(s string) (*big.Int, error) {
	if s == "" {
		return big.NewInt(0), nil
	}
	if len(s) >= 2 && s[0] == '0' && s[1] == 'x' {
		v, err := hexutil.DecodeBig(s)
		if err != nil {
			return nil, err
		}
		return v, nil
	}
	v, ok := new(big.Int).SetString(s, 10)
	if !ok || v.Sign() < 0 {
		return nil, errors.New("bad integer")
	}
	return v, nil
}

// GetPostPoolStateArgs is arb_getPostPoolState params (wire contract v2). It reads the
// named pools on a previously-produced post handle (the child state from a successful
// simulate). v2 replaces v1's pool_locators (address-only) with structured pool_reads:
// the node holds NO pool registry, so the client must declare each pool's kind and
// (for v2) its token0/token1 — pool identity authority lives in the Rust PoolMeta, the
// node is a dumb executor that reads what it is told.
type GetPostPoolStateArgs struct {
	RequestID      string         `json:"request_id"`
	Stamp          string         `json:"stamp"`
	PostHandle     string         `json:"post_handle"`
	RegistryDigest string         `json:"registry_digest"`
	PoolReads      []PoolReadSpec `json:"pool_reads"`
	MissingParts   []any          `json:"missing_parts"`
	BudgetMicros   uint64         `json:"budget_us"`
}

// PoolReadSpec is one structured pool read (v2). Kind selects the read path: "v2" needs
// token0/token1 (getReserves + balanceOf on both tokens); "v3" needs only the locator
// (slot0 + liquidity). The node never guesses a kind or token identity (§389).
type PoolReadSpec struct {
	Locator string `json:"locator"`
	Kind    string `json:"kind"`
	Token0  string `json:"token0,omitempty"`
	Token1  string `json:"token1,omitempty"`
}

// errBadPoolRead: a pool_reads entry is malformed — bad locator, unknown kind, or a v2
// read missing/malforming its required token0/token1. Rejected up front; the node never
// substitutes a guessed identity.
var errBadPoolRead = errors.New("arb: pool_reads entry malformed (locator/kind/token invalid)")

// validPoolRead enforces the §389 no-guess rule on one read spec: the locator must be a
// valid address; kind must be "v2" or "v3"; a v2 read MUST carry valid token0/token1
// (the node has no registry to look them up); a v3 read must NOT carry tokens (they are
// meaningless for slot0/liquidity and their presence signals a client encoding bug).
func validPoolRead(pr *PoolReadSpec) bool {
	if !isAddressWire(pr.Locator) {
		return false
	}
	switch pr.Kind {
	case "v2":
		return isAddressWire(pr.Token0) && isAddressWire(pr.Token1)
	case "v3":
		return pr.Token0 == "" && pr.Token1 == ""
	default:
		return false
	}
}

// GetPostPoolState -> arb_getPostPoolState. Like the simulate methods it returns a
// job_id; the read runs on the post handle's state via the PostStatePoolCaller
// bridge, and the job's result carries the read snapshots (v1: metering + a
// completeness flag; the full snapshot payload rides the result once the wire result
// schema for pool batches is finalized).
func (api *ArbAPI) GetPostPoolState(args GetPostPoolStateArgs) (*JobIDResult, error) {
	if !isID32Wire(args.RequestID) {
		return nil, errBadRequestID
	}
	if !isHash32Wire(args.Stamp) {
		return nil, errBadStampWire
	}
	if !isID32Wire(args.PostHandle) {
		return nil, errBadHandleWire
	}
	if args.BudgetMicros == 0 {
		return nil, errZeroBudget
	}
	// Validate every pool read spec up front (§389: reject malformed, never guess).
	for i := range args.PoolReads {
		if !validPoolRead(&args.PoolReads[i]) {
			return nil, errBadPoolRead
		}
	}
	ident := arb.HandleIdentity{OwnerSession: args.RequestID, NodeBootID: api.svc.boot, Identity: args.RequestID}
	// v2 digest folds the structured read specs (kind + tokens), not bare locators, so
	// a v2 read is content-addressed distinctly from a v1 address-only read.
	d := arb.NewDigest("arb.req.getPostPoolState.v2").
		FieldBytes([]byte(args.PostHandle)).
		FieldBytes([]byte(args.Stamp)).
		FieldBytes([]byte(args.RegistryDigest)).
		FieldU64(args.BudgetMicros)
	for i := range args.PoolReads {
		pr := &args.PoolReads[i]
		d.FieldBytes([]byte(pr.Locator)).FieldBytes([]byte(pr.Kind)).
			FieldBytes([]byte(pr.Token0)).FieldBytes([]byte(pr.Token1))
	}
	digest := hex32Str(d.Finalize())

	run := func(budget *arb.ReadBudget) (*arb.JobOutcome, bool) {
		base, header, release, berr := api.svc.borrowBase(args.PostHandle, ident)
		if berr != nil {
			return &arb.JobOutcome{Kind: "post_pool", ErrCode: berr.Error()}, false
		}
		defer release()
		// Read pools on the post state via the same budgeted+StaticCall path used at
		// head (newPoolCaller over the borrowed post base). A read failure aborts the
		// WHOLE job with an error — never a partial or fabricated snapshot (§381).
		caller := api.svc.eth.newPoolCaller(base, header, budget)
		snaps := make([]arb.PoolSnapshot, 0, len(args.PoolReads))
		for i := range args.PoolReads {
			pr := &args.PoolReads[i]
			pool := common.HexToAddress(pr.Locator)
			switch pr.Kind {
			case "v3":
				s, rerr := caller.ReadV3Head(pool)
				if rerr != nil {
					return &arb.JobOutcome{Kind: "post_pool", ErrCode: rerr.Error()}, false
				}
				snaps = append(snaps, arb.PoolSnapshot{
					Locator:      pr.Locator,
					Kind:         "v3",
					SqrtPriceX96: s.SqrtPriceX96.String(),
					Tick:         strconv.FormatInt(int64(s.Tick), 10),
					Liquidity:    s.Liquidity.String(),
				})
			default: // "v2" (validPoolRead guaranteed kind ∈ {v2,v3} and v2 has tokens)
				s, rerr := caller.ReadV2(pool, common.HexToAddress(pr.Token0), common.HexToAddress(pr.Token1))
				if rerr != nil {
					return &arb.JobOutcome{Kind: "post_pool", ErrCode: rerr.Error()}, false
				}
				snaps = append(snaps, arb.PoolSnapshot{
					Locator:  pr.Locator,
					Kind:     "v2",
					Reserve0: s.Reserve0.String(),
					Reserve1: s.Reserve1.String(),
					Balance0: s.Balance0.String(),
					Balance1: s.Balance1.String(),
				})
			}
		}
		return &arb.JobOutcome{
			Kind:      "post_pool",
			Metering:  meteringFrom(budget, 0),
			Complete:  len(snaps) == len(args.PoolReads),
			Snapshots: snaps,
		}, true
	}

	jobID, err := api.submitAndEnqueue(args.RequestID, args.Stamp, digest, args.BudgetMicros, run)
	if err != nil {
		return nil, err
	}
	return &JobIDResult{JobID: jobID}, nil
}
