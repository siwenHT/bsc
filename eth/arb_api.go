// The arb JSON-RPC surface (namespace "arb"), the thin request/response layer over
// the arbService. geth turns each exported method Foo into arb_foo. Every method
// here only: validates wire shape, decodes into executor inputs, and delegates to
// the service (which owns the pure cores + runners). No simulation logic lives here.
//
// Wire authority: protocol/v1/wire.schema.json. This file implements the control
// plane (getCapabilities/pinState/releaseState/getJob/cancelJob/getTargetSlot); the
// four async simulate methods live in arb_api_sim.go.

package eth

import (
	"errors"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/eth/arb"
)

// arbCommit is the node build commit echoed in getCapabilities so a client can
// detect a binary change across a reconnect. Set to the gethsrc HEAD.
const arbCommit = "664fca292d5c"

// arbSchemaVersion is the wire schema version this node implements (protocol/v1).
const arbSchemaVersion = 1

// ArbAPI is the RPC service bound to one arbService (one node boot).
type ArbAPI struct {
	svc *arbService
}

// NewArbAPI builds the RPC service. Exported for APIs() registration.
func NewArbAPI(svc *arbService) *ArbAPI { return &ArbAPI{svc: svc} }

// --- shared param/result shapes (JSON tags match wire.schema.json exactly) ---

// CapabilitiesArgs is arb_getCapabilities params.
type CapabilitiesArgs struct {
	SchemaVersion uint64 `json:"schema_version"`
}

// CapabilitiesResult mirrors arb_getCapabilities result_fields.
type CapabilitiesResult struct {
	Boot              string         `json:"boot"`
	Commit            string         `json:"commit"`
	ChainConfigDigest string         `json:"chain_config_digest"`
	StateScheme       string         `json:"state_scheme"`
	SupportedTxTypes  []uint64       `json:"supported_tx_types"`
	SupportedVersions []uint64       `json:"supported_versions"`
	PinSupported      bool           `json:"pin_supported"`
	ResourceLimits    map[string]any `json:"resource_limits"`
}

// PinStateArgs is arb_pinState params.
type PinStateArgs struct {
	RequestID  string `json:"request_id"`
	ParentHash string `json:"parent_hash"`
	TTLMicros  uint64 `json:"ttl_us"`
}

// PinStateResult mirrors arb_pinState result_fields.
type PinStateResult struct {
	RequestID     string `json:"request_id"`
	ParentHandle  string `json:"parent_handle"`
	ParentHash    string `json:"parent_hash"`
	StateRoot     string `json:"state_root"`
	Boot          string `json:"boot"`
	GrantedTTLUs  uint64 `json:"granted_ttl_us"`
}

// ReleaseStateArgs is arb_releaseState params.
type ReleaseStateArgs struct {
	RequestID string `json:"request_id"`
	HandleID  string `json:"handle_id"`
	Boot      string `json:"boot"`
}

// ReleaseStateResult mirrors arb_releaseState result_fields.
type ReleaseStateResult struct {
	RequestID string `json:"request_id"`
	Released  bool   `json:"released"`
}

// JobArgs is arb_getJob / arb_cancelJob params.
type JobArgs struct {
	JobID string `json:"job_id"`
	Boot  string `json:"boot"`
}

// JobResult mirrors arb_getJob result_fields. Metering/completeness are echoed
// from the frozen outcome; result_kind is "" until known. Outcome-derived fields
// are omitted (nil) when the job has no result yet.
// JobResult mirrors arb_getJob result_fields (wire contract v2). The frozen v1 six
// fields are unchanged; v2 adds three: post_handle_or_null / error_code_or_null
// (correcting the v1 drift where the server already returned post_handle/error_code
// outside the frozen list — now first-class present-but-null fields) and
// payload_or_null (the result_kind-dispatched result payload). All three use
// present-but-null (pointer / any, NO omitempty) so absence serializes as JSON null,
// never a dropped or zero-faked field (hashing.md optional discipline).
type JobResult struct {
	Status           string         `json:"status"`
	Stamp            string         `json:"stamp"`
	RequestDigest    string         `json:"request_digest"`
	ResultKind       string         `json:"result_kind"`
	Metering         map[string]any `json:"metering"`
	Completeness     bool           `json:"completeness"`
	PostHandleOrNull *string        `json:"post_handle_or_null"`
	ErrorCodeOrNull  *string        `json:"error_code_or_null"`
	// PayloadOrNull is the result_kind-dispatched payload: candidate -> {retained_t0,
	// retained_t2} (§391) or null; post_pool -> {snapshots:[...]} or null; target/
	// bundle -> null. Null whenever the job has no result yet, or the kind produced no
	// payload (e.g. candidate retention unmeasured), never a fabricated value.
	PayloadOrNull    any            `json:"payload_or_null"`
}

// CancelJobResult mirrors arb_cancelJob result_fields.
type CancelJobResult struct {
	CancelRequested bool `json:"cancel_requested"`
}

// TargetSlotArgs is arb_getTargetSlot params.
type TargetSlotArgs struct {
	RequestID string `json:"request_id"`
	Sender    string `json:"sender"`
	Nonce     uint64 `json:"nonce"`
}

// TargetSlotResult mirrors arb_getTargetSlot result_fields. current_raw/current_hash
// are nil when the pool holds no tx at that (sender,nonce) — never zero-faked.
type TargetSlotResult struct {
	Boot            string  `json:"boot"`
	SlotRevision    uint64  `json:"slot_revision"`
	CurrentRawOrNil *string `json:"current_raw_or_null"`
	CurrentHashOrNil *string `json:"current_hash_or_null"`
}

// --- errors surfaced to RPC callers ---

var (
	errUnsupportedSchema = errors.New("arb: unsupported schema_version")
	errBadRequestID      = errors.New("arb: request_id must be a 0x + 64 lowercase hex id32")
	errBadParentHash     = errors.New("arb: parent_hash must be a 0x + 64 lowercase hex hash32")
	errBadHandleID       = errors.New("arb: handle_id must be a 0x + 64 lowercase hex id32")
	errBadSender         = errors.New("arb: sender must be a 0x + 40 lowercase hex address")
)

// GetCapabilities -> arb_getCapabilities. Reports boot/commit/scheme and limits so a
// client can gate on binary/boot changes and read the pin support actually verified
// by NODE-00 (pin_supported true only on hash scheme; path scheme is best-effort).
func (api *ArbAPI) GetCapabilities(args CapabilitiesArgs) (*CapabilitiesResult, error) {
	if args.SchemaVersion != arbSchemaVersion {
		return nil, errUnsupportedSchema
	}
	backend := api.svc.eth.NewArbStateBackend()
	scheme := backend.Scheme()
	cfg := api.svc.eth.blockchain.Config()
	// chain_config_digest: a versioned digest of the chain id (a fuller fork digest
	// is computed per-env as fork_rules_digest; this is the stable chain identity).
	ccd := arb.NewDigest("arb.chaincfg.v1").FieldU64(cfg.ChainID.Uint64()).Finalize()
	return &CapabilitiesResult{
		Boot:              api.svc.boot,
		Commit:            arbCommit,
		ChainConfigDigest: hex32Str(ccd),
		StateScheme:       scheme.String(),
		SupportedTxTypes:  []uint64{0, 1, 2}, // legacy/access-list/dynamic-fee
		SupportedVersions: []uint64{arbSchemaVersion},
		PinSupported:      scheme == arb.SchemeHash,
		ResourceLimits: map[string]any{
			"workers":     api.svc.workers,
			"queue_depth": cap(api.svc.queue),
			"inflight_cap": api.svc.pool.Capacity(),
		},
	}, nil
}

// PinState -> arb_pinState. Pins the parent state and returns a handle the simulate
// methods borrow. TTL is granted as-requested (bounded by the store's own policy).
func (api *ArbAPI) PinState(args PinStateArgs) (*PinStateResult, error) {
	if !isID32Wire(args.RequestID) {
		return nil, errBadRequestID
	}
	if !isHash32Wire(args.ParentHash) {
		return nil, errBadParentHash
	}
	ident := arb.HandleIdentity{
		OwnerSession: args.RequestID,
		NodeBootID:   api.svc.boot,
		Identity:     args.RequestID,
	}
	ttl := time.Duration(args.TTLMicros) * time.Microsecond
	id, root, err := api.svc.pinParent(ident, common.HexToHash(args.ParentHash), ttl)
	if err != nil {
		return nil, err
	}
	return &PinStateResult{
		RequestID:    args.RequestID,
		ParentHandle: id,
		ParentHash:   args.ParentHash,
		StateRoot:    root.Hex(),
		Boot:         api.svc.boot,
		GrantedTTLUs: args.TTLMicros,
	}, nil
}

// ReleaseState -> arb_releaseState. Idempotent on boot match: releasing an already-
// released handle returns released=true; a wrong boot is rejected.
func (api *ArbAPI) ReleaseState(args ReleaseStateArgs) (*ReleaseStateResult, error) {
	if !isID32Wire(args.RequestID) {
		return nil, errBadRequestID
	}
	if !isID32Wire(args.HandleID) {
		return nil, errBadHandleID
	}
	if args.Boot != api.svc.boot {
		return nil, arb.ErrBootMismatch
	}
	released, err := api.svc.releaseParent(args.HandleID)
	if err != nil {
		return nil, err
	}
	return &ReleaseStateResult{RequestID: args.RequestID, Released: released}, nil
}

// GetJob -> arb_getJob. Reports the wire status (CancelRequested surfaces as Running)
// and, once terminal with a result, the frozen metering/completeness/kind.
func (api *ArbAPI) GetJob(args JobArgs) (*JobResult, error) {
	view, err := api.svc.jobs.Get(args.JobID, args.Boot)
	if err != nil {
		return nil, err
	}
	res := &JobResult{
		Status:        view.Status.WireStatus(),
		Stamp:         view.Stamp,
		RequestDigest: view.Digest,
		ResultKind:    view.Kind,
	}
	if view.Outcome != nil {
		o := view.Outcome
		res.Completeness = o.Complete
		res.PostHandleOrNull = strPtrOrNil(o.PostHandle) // "" -> JSON null, never dropped
		res.ErrorCodeOrNull = strPtrOrNil(o.ErrCode)
		res.PayloadOrNull = jobResultPayload(o) // result_kind-dispatched, or nil
		res.Metering = map[string]any{
			"state_reads":  o.Metering.StateReads,
			"gas_used":     o.Metering.GasUsed,
			"wall_us":      o.Metering.WallMicros,
			"result_bytes": o.Metering.ResultBytes,
		}
	}
	return res, nil
}

// strPtrOrNil maps "" -> nil (serialized as JSON null under a no-omitempty pointer
// field, the wire's present-but-null convention) and any non-empty string to itself.
func strPtrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// jobResultPayload builds the v2 arb_getJob payload_or_null by result_kind. Returns
// nil (JSON null) whenever the kind carries no payload or the payload was not measured
// — never a fabricated value (§389/§381). Only Kind-relevant fields are emitted so
// each wire object stays additionalProperties:false.
func jobResultPayload(o *arb.JobOutcome) any {
	switch o.Kind {
	case "candidate":
		// §391 retention rides only when measured (both present, set together).
		if o.RetainedT0 == "" || o.RetainedT2 == "" {
			return nil
		}
		return map[string]any{
			"retained_t0": o.RetainedT0,
			"retained_t2": o.RetainedT2,
		}
	case "post_pool":
		// A complete read carries one snapshot per requested pool, in request order.
		// An incomplete/failed read leaves ErrCode set and payload null (§381), so
		// only emit when the outcome actually completed.
		if !o.Complete {
			return nil
		}
		snaps := make([]any, 0, len(o.Snapshots))
		for _, s := range o.Snapshots {
			snaps = append(snaps, poolSnapshotWire(s))
		}
		return map[string]any{"snapshots": snaps}
	default:
		return nil // target / bundle: no extra payload
	}
}

// poolSnapshotWire shapes one PoolSnapshot into its Kind-specific wire object, emitting
// only the fields meaningful for that kind (V2 reserve/balance pairs, or V3 slot0/
// liquidity) so each object is additionalProperties:false per the v2 schema side.
func poolSnapshotWire(s arb.PoolSnapshot) map[string]any {
	switch s.Kind {
	case "v3":
		return map[string]any{
			"locator":        s.Locator,
			"kind":           "v3",
			"sqrt_price_x96": s.SqrtPriceX96,
			"tick":           s.Tick,
			"liquidity":      s.Liquidity,
		}
	default: // "v2"
		return map[string]any{
			"locator":  s.Locator,
			"kind":     "v2",
			"reserve0": s.Reserve0,
			"reserve1": s.Reserve1,
			"balance0": s.Balance0,
			"balance1": s.Balance1,
		}
	}
}

// CancelJob -> arb_cancelJob. Requests cooperative cancellation; does not promise
// the worker has exited (observe finality via GetJob).
func (api *ArbAPI) CancelJob(args JobArgs) (*CancelJobResult, error) {
	req, err := api.svc.jobs.RequestCancel(args.JobID, args.Boot)
	if err != nil {
		return nil, err
	}
	return &CancelJobResult{CancelRequested: req}, nil
}

// GetTargetSlot -> arb_getTargetSlot. Returns the node's CURRENT authoritative pool
// selection for (sender,nonce) — never a Rust-guessed price-bump (frozen §4). A slot
// with no current tx returns nils, not a faked zero.
func (api *ArbAPI) GetTargetSlot(args TargetSlotArgs) (*TargetSlotResult, error) {
	if !isID32Wire(args.RequestID) {
		return nil, errBadRequestID
	}
	if !isAddressWire(args.Sender) {
		return nil, errBadSender
	}
	sender := common.HexToAddress(args.Sender)
	pending, queued := api.svc.eth.txPool.ContentFrom(sender)
	res := &TargetSlotResult{Boot: api.svc.boot, SlotRevision: 0}
	// Find the tx at exactly this nonce among pending (executable) then queued. The
	// pool's current selection is authoritative (frozen §4); we do not synthesize a
	// price-bump or guess a replacement.
	for _, group := range [][]*types.Transaction{pending, queued} {
		for _, tx := range group {
			if tx.Nonce() != args.Nonce {
				continue
			}
			bin, err := tx.MarshalBinary()
			if err != nil {
				return nil, err
			}
			raw := hexutil.Encode(bin)
			h := tx.Hash().Hex()
			res.CurrentRawOrNil = &raw
			res.CurrentHashOrNil = &h
			return res, nil
		}
	}
	return res, nil // no tx at that slot: nils, honest absence
}
