// The four async simulate methods (arb_simulateTarget / simulateCandidate /
// simulateSignedBundle / getPostPoolState). Each validates wire shape, decodes the
// raw tx(s) and env, computes a request_digest (the content-addressed idempotency
// key — same request_id + same digest returns the existing job; a different digest
// is a conflict), submits to the registry, and — for a fresh job — builds the
// executor closure and enqueues it. Every method returns only {job_id}; the result
// is read later via arb_getJob (design §293 async submit contract).
//
// The executor closures reuse the ALREADY-VERIFIED env-aware entries
// (ExecuteTargetWithEnv / ExecutePrefixWithEnv / RunCandidateWithEnv), so no new
// simulation logic is introduced here — only decode + dispatch + result mapping.

package eth

import (
	"errors"

	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/eth/arb"
)

// JobIDResult is the shared {job_id} response of every simulate method.
type JobIDResult struct {
	JobID string `json:"job_id"`
}

// SimulateTargetArgs is arb_simulateTarget params.
type SimulateTargetArgs struct {
	RequestID    string       `json:"request_id"`
	Stamp        string       `json:"stamp"`
	ParentHandle string       `json:"parent_handle"`
	BlockEnv     arb.BlockEnv `json:"block_env"`
	TargetRaw    string       `json:"target_raw"`
	BudgetMicros uint64       `json:"budget_us"`
}

// SimulateCandidateArgs is arb_simulateCandidate params. unsigned_tx is OUR own
// unsigned arbitrage envelope (explicit sender, no signature); the executor runs it
// without signature recovery but enforces every other rule (§237).
type SimulateCandidateArgs struct {
	RequestID    string          `json:"request_id"`
	Stamp        string          `json:"stamp"`
	ParentHandle string          `json:"parent_handle"`
	BlockEnv     arb.BlockEnv    `json:"block_env"`
	TargetRaw    string          `json:"target_raw"`
	UnsignedTx   UnsignedEnvelope `json:"unsigned_tx"`
	BudgetMicros uint64          `json:"budget_us"`
	// Executor + BaseToken are OPTIONAL and, when present, must BOTH be valid lowercase
	// hex addresses. They name the candidate's beneficiary and the standardized base
	// token (e.g. WBNB) whose retention delta is the §391 gross seed. When both are set
	// the node measures RetainedT0/RetainedT2 (before/after our candidate); when both are
	// empty it measures nothing (the arb contract may not be deployed yet — the node never
	// hardcodes an address, and the client then treats gross as unmeasured, §389). Supplying
	// exactly one, or a malformed address, is a client error and is rejected.
	Executor     string          `json:"executor,omitempty"`
	BaseToken    string          `json:"base_token,omitempty"`
}

// UnsignedEnvelope is the wire shape of our unsigned candidate tx. All amounts are
// decimal-or-hex strings decoded to big.Int; from is an explicit non-zero sender.
type UnsignedEnvelope struct {
	From      string `json:"from"`
	To        string `json:"to"`
	Nonce     uint64 `json:"nonce"`
	Value     string `json:"value"`
	GasLimit  uint64 `json:"gas_limit"`
	GasFeeCap string `json:"gas_fee_cap"`
	GasTipCap string `json:"gas_tip_cap"`
	Data      string `json:"data"`
}

// SimulateSignedBundleArgs is arb_simulateSignedBundle params. v1 requires exactly
// two raws [target, our].
type SimulateSignedBundleArgs struct {
	RequestID    string       `json:"request_id"`
	Stamp        string       `json:"stamp"`
	ParentHandle string       `json:"parent_handle"`
	BlockEnv     arb.BlockEnv `json:"block_env"`
	Raws         []string     `json:"raws"`
	BudgetMicros uint64       `json:"budget_us"`
}

var (
	errBadStampWire   = errors.New("arb: stamp must be a 0x + 64 lowercase hex hash32")
	errBadHandleWire  = errors.New("arb: parent/post handle must be a 0x + 64 lowercase hex id32")
	errBadRawTx       = errors.New("arb: raw tx is not valid hex or fails to decode")
	errBundleArity    = errors.New("arb: v1 signed bundle must be exactly [target, our] (2 raws)")
	errZeroBudget     = errors.New("arb: budget_us must be > 0")
)

// decodeRaw decodes a 0x-hex raw signed tx into a *types.Transaction.
func decodeRaw(raw string) (*types.Transaction, error) {
	b, err := hexutil.Decode(raw)
	if err != nil {
		return nil, errBadRawTx
	}
	tx := new(types.Transaction)
	if err := tx.UnmarshalBinary(b); err != nil {
		return nil, errBadRawTx
	}
	return tx, nil
}

// commonSimValidate checks the fields shared by the three parent-handle simulate
// methods and returns the identity for borrowing.
func (api *ArbAPI) commonSimValidate(requestID, stamp, parentHandle string, budgetUs uint64) (arb.HandleIdentity, error) {
	if !isID32Wire(requestID) {
		return arb.HandleIdentity{}, errBadRequestID
	}
	if !isHash32Wire(stamp) {
		return arb.HandleIdentity{}, errBadStampWire
	}
	if !isID32Wire(parentHandle) {
		return arb.HandleIdentity{}, errBadHandleWire
	}
	if budgetUs == 0 {
		return arb.HandleIdentity{}, errZeroBudget
	}
	return arb.HandleIdentity{
		OwnerSession: requestID,
		NodeBootID:   api.svc.boot,
		Identity:     requestID,
	}, nil
}

// submitAndEnqueue computes the request digest, submits to the registry (idempotent
// on request_id), and enqueues a fresh job's closure. Returns the job id.
func (api *ArbAPI) submitAndEnqueue(requestID, stamp, digest string, maxWallUs uint64, run func(budget *arb.ReadBudget) (*arb.JobOutcome, bool)) (string, error) {
	jobID, _, existing, err := api.svc.jobs.Submit(requestID, stamp, digest)
	if err != nil {
		return "", err
	}
	job := arbJob{
		jobID:    jobID,
		run:      run,
		maxReads: defaultMaxReads,
		wall:     microsToDuration(maxWallUs),
	}
	if !api.svc.enqueue(job, existing) {
		// Capacity refusal was recorded on the job (Failed/queue_full); return the
		// job id so the client polls and sees the terminal refusal.
		return jobID, nil
	}
	return jobID, nil
}

// SimulateTarget -> arb_simulateTarget. Decodes the target, digests the request,
// and enqueues a closure that runs ExecuteTargetWithEnv on a borrowed isolated base.
func (api *ArbAPI) SimulateTarget(args SimulateTargetArgs) (*JobIDResult, error) {
	ident, err := api.commonSimValidate(args.RequestID, args.Stamp, args.ParentHandle, args.BudgetMicros)
	if err != nil {
		return nil, err
	}
	target, err := decodeRaw(args.TargetRaw)
	if err != nil {
		return nil, err
	}
	digest := hex32Str(arb.NewDigest("arb.req.simulateTarget.v1").
		FieldBytes([]byte(args.ParentHandle)).
		FieldBytes([]byte(args.Stamp)).
		FieldBytes(target.Hash().Bytes()).
		FieldU64(args.BudgetMicros).
		Finalize())

	run := func(budget *arb.ReadBudget) (*arb.JobOutcome, bool) {
		base, header, release, berr := api.svc.borrowBase(args.ParentHandle, ident)
		if berr != nil {
			return &arb.JobOutcome{Kind: "target", ErrCode: berr.Error()}, false
		}
		defer release()
		x := newTargetExecutor(api.svc.eth.blockchain, header, base)
		res, xerr := x.ExecuteTargetWithEnv(target, budget, args.BlockEnv)
		if xerr != nil {
			return &arb.JobOutcome{Kind: "target", ErrCode: xerr.Error()}, false
		}
		return api.mapTargetOutcome(args.ParentHandle, ident, res, budget), res.Class.Status == arb.StatusSuccess
	}

	jobID, err := api.submitAndEnqueue(args.RequestID, args.Stamp, digest, args.BudgetMicros, run)
	if err != nil {
		return nil, err
	}
	return &JobIDResult{JobID: jobID}, nil
}

// SimulateSignedBundle -> arb_simulateSignedBundle. Requires exactly [target, our];
// runs ExecutePrefixWithEnv (stops at first non-success, §357).
func (api *ArbAPI) SimulateSignedBundle(args SimulateSignedBundleArgs) (*JobIDResult, error) {
	ident, err := api.commonSimValidate(args.RequestID, args.Stamp, args.ParentHandle, args.BudgetMicros)
	if err != nil {
		return nil, err
	}
	if len(args.Raws) != 2 {
		return nil, errBundleArity
	}
	txs := make([]*types.Transaction, 0, 2)
	d := arb.NewDigest("arb.req.simulateBundle.v1").
		FieldBytes([]byte(args.ParentHandle)).
		FieldBytes([]byte(args.Stamp)).
		FieldU64(args.BudgetMicros)
	for _, raw := range args.Raws {
		tx, derr := decodeRaw(raw)
		if derr != nil {
			return nil, derr
		}
		txs = append(txs, tx)
		d.FieldBytes(tx.Hash().Bytes())
	}
	digest := hex32Str(d.Finalize())

	run := func(budget *arb.ReadBudget) (*arb.JobOutcome, bool) {
		base, header, release, berr := api.svc.borrowBase(args.ParentHandle, ident)
		if berr != nil {
			return &arb.JobOutcome{Kind: "bundle", ErrCode: berr.Error()}, false
		}
		defer release()
		x := newTargetExecutor(api.svc.eth.blockchain, header, base)
		res, xerr := x.ExecutePrefixWithEnv(txs, budget, args.BlockEnv)
		if xerr != nil {
			return &arb.JobOutcome{Kind: "bundle", ErrCode: xerr.Error()}, false
		}
		return api.mapPrefixOutcome(args.ParentHandle, ident, res, budget), res.Completed
	}

	jobID, err := api.submitAndEnqueue(args.RequestID, args.Stamp, digest, args.BudgetMicros, run)
	if err != nil {
		return nil, err
	}
	return &JobIDResult{JobID: jobID}, nil
}
