// HeadReady / canonical state-stream coordinator core (NODE-02, design §5.1/§5.2
// and the FROZEN wire contract protocol/v1/wire.schema.json arb_subscribe kind="head").
//
// This is the node-agnostic EMITTER core for the head stream. It imports nothing
// from the parent eth package, so it compiles and unit-tests standalone (no CGO).
// The concrete binding (subscribe ChainEvent+ChainHeadEvent, resolve the current
// canonical head, probe root readability via the NODE-00 backend) lives in the
// parent package (eth/arb_head.go).
//
// Why the head stream is NOT the pending BoundedFeed (design §5.2):
//
//	The pending stream treats every tx as independent: a full queue drops the
//	lowest-priority item and counts a gap, then keeps going. The head stream MUST
//	NOT silently lose deltas. Only the LATEST canonical head matters for trading,
//	so intermediate heads MAY be collapsed — but a collapse (an undelivered head
//	overwritten by a newer one, an internal overflow, or a reorg deeper than the
//	tracked depth) MUST be signaled to the consumer via a STICKY reset_required so
//	it drops stale evaluations and rebuilds from the latest head. Dropping a head
//	without that signal would let the consumer trade on a superseded state.
//
// Epoch / sequence semantics (mirrored by the Rust consumer's coordinator gate,
// crates/coordinator/src/gate.rs §8.9):
//
//   - canonical_seq: monotonic per PUBLISHED head, from 1. The head stream has no
//     lossy queue, so delivered seqs are contiguous; a jump only occurs across a
//     reconnect (consumer resyncs) — never from a silent internal drop.
//   - state_epoch: from 1; increments on any NON-direct extension (a reorg — a
//     same-height hash swap, a shorter-chain reorg, or a non-linear jump). A direct
//     linear extension keeps the epoch. The consumer uses an epoch change to cancel
//     evaluations bound to the old head.
//   - reset_required: latched (sticky) on overflow / missed-head collapse / a reorg
//     the adapter reports as deeper than the configured tracking depth. It is
//     delivered on the next published head and cleared once delivered, because in
//     identity_only mode that single published head IS the fresh baseline.
package arb

import "strconv"

// PoolStateMode is the completeness class of a HeadStateReady (design §5.1).
type PoolStateMode string

const (
	// ModeIdentityOnly: the Ready carries only head identity; EVM state is usable
	// but the Rust pool cache is NOT prepared. The consumer must call
	// arb_getPoolSnapshot for this head before quoting. data_complete is false.
	ModeIdentityOnly PoolStateMode = "identity_only"
	// ModeCompleteRegisteredBatch: the Ready carries a complete registered-pool
	// batch. Only then may data_complete be true. (Second slice; needs state reads.)
	ModeCompleteRegisteredBatch PoolStateMode = "complete_registered_batch"
)

// HeadInput is the identity of an observed canonical head, resolved by the adapter
// from the blockchain. Pure data — no node types — so the core stays testable.
// By construction the adapter always passes the CURRENT canonical head (chain
// events are treated as "verify now" wake-ups, not trusted deltas), so a lower
// number with a different hash is a genuine shorter-chain reorg, not a late event.
type HeadInput struct {
	Hash                 string  // hash32, lowercase 0x-hex
	Number               uint64  //
	ParentHash           string  // hash32
	StateRoot            string  // hash32
	ConsensusTimestampMs uint64  //
	EvmTimestampSeconds  uint64  //
	FinalizedHash        *string // optional; nil => absent (emitted as null)
	PoolRegistryRevision uint64  //
}

// ObserveSignal carries adapter-detected conditions the pure core cannot compute
// itself (they need ancestor walks / downstream state).
type ObserveSignal struct {
	// DownstreamOverflow: the delivery side could not keep up and an internal
	// buffer overflowed. Latches reset_required.
	DownstreamOverflow bool
	// ReorgDepthExceeded: the adapter walked the parent chain and the reorg is
	// deeper than the configured tracking depth (design §5.2: do not scan
	// infinitely — signal reset instead). Latches reset_required.
	ReorgDepthExceeded bool
}

// HeadStateReady is the wire payload for a kind="head" frame (design §5.1).
// Integers are strict decimal strings (freeze §2); hashes lowercase 0x-hex;
// finalized_hash / batch_id are JSON null when absent. data_complete and
// reset_required are genuine booleans (not integers), so they ride as JSON bools.
type HeadStateReady struct {
	Hash                 string        `json:"hash"`
	Number               string        `json:"number"`
	ParentHash           string        `json:"parent_hash"`
	StateRoot            string        `json:"state_root"`
	ConsensusTimestampMs string        `json:"consensus_timestamp_ms"`
	EvmTimestampSeconds  string        `json:"evm_timestamp_seconds"`
	StateEpoch           string        `json:"state_epoch"`
	CanonicalSeq         string        `json:"canonical_seq"`
	FinalizedHash        *string       `json:"finalized_hash_optional"`
	PoolRegistryRevision string        `json:"pool_registry_revision"`
	BatchID              *string       `json:"batch_id"`
	PoolStateMode        PoolStateMode `json:"pool_state_mode"`
	DataComplete         bool          `json:"data_complete"`
	ResetRequired        bool          `json:"reset_required"`
}

// HeadClass classifies an observed head relative to the last published one.
type HeadClass uint8

const (
	// ClassStale: exact duplicate of the last published head (a redundant wake-up).
	// Nothing is published; seq/epoch unchanged.
	ClassStale HeadClass = iota
	// ClassFirst: nothing published yet; this becomes the baseline.
	ClassFirst
	// ClassDirectExtension: number == last+1 and parent == last hash. Epoch kept.
	ClassDirectExtension
	// ClassReorg: any other change (same-height hash swap, shorter-chain reorg, or
	// a non-linear forward jump). Epoch incremented.
	ClassReorg
)

func (c HeadClass) String() string {
	switch c {
	case ClassStale:
		return "stale"
	case ClassFirst:
		return "first"
	case ClassDirectExtension:
		return "direct_extension"
	case ClassReorg:
		return "reorg"
	default:
		return "unknown"
	}
}

// HeadCoordinator is the single-owner canonical-head state machine. Not safe for
// concurrent use; the adapter serializes Observe/Take under its own lock.
type HeadCoordinator struct {
	// last published head identity (empty until first publish).
	lastHash   string
	lastNumber uint64
	lastParent string
	hasLast    bool

	epoch        uint64 // current state_epoch (0 until first publish -> 1)
	canonicalSeq uint64 // current canonical_seq (0 until first publish -> 1)

	// stickyReset latches until a head carrying reset_required=true is delivered.
	stickyReset bool

	// pending is the latest observed-but-undelivered HeadStateReady. Overwriting a
	// non-nil pending means an intermediate head was collapsed -> latch reset.
	pending *HeadStateReady
}

// NewHeadCoordinator returns a fresh coordinator with no baseline.
func NewHeadCoordinator() *HeadCoordinator { return &HeadCoordinator{} }

// Epoch returns the current state_epoch (0 before the first publish).
func (c *HeadCoordinator) Epoch() uint64 { return c.epoch }

// CanonicalSeq returns the current canonical_seq (0 before the first publish).
func (c *HeadCoordinator) CanonicalSeq() uint64 { return c.canonicalSeq }

// ResetPending reports whether reset_required is currently latched.
func (c *HeadCoordinator) ResetPending() bool { return c.stickyReset }

// classify compares h against the last published head.
func (c *HeadCoordinator) classify(h HeadInput) HeadClass {
	if !c.hasLast {
		return ClassFirst
	}
	if h.Hash == c.lastHash {
		return ClassStale
	}
	if h.Number == c.lastNumber+1 && h.ParentHash == c.lastHash {
		return ClassDirectExtension
	}
	return ClassReorg
}

// Observe ingests the current canonical head and the adapter's signals, updating
// the coordinator and staging a HeadStateReady for delivery. It returns the
// classification (for observability/logging). A ClassStale observation is a no-op
// beyond possibly latching reset from the signals.
//
// identity_only mode: the staged Ready carries only head identity —
// pool_state_mode = identity_only, data_complete = false, batch_id = null. The
// consumer must fetch pool snapshots for this head before quoting (design §5.1).
func (c *HeadCoordinator) Observe(h HeadInput, sig ObserveSignal) HeadClass {
	if sig.DownstreamOverflow || sig.ReorgDepthExceeded {
		c.stickyReset = true
	}

	class := c.classify(h)
	if class == ClassStale {
		return class
	}

	switch class {
	case ClassFirst:
		c.epoch = 1
	case ClassDirectExtension:
		// epoch unchanged
	case ClassReorg:
		c.epoch++
	}
	c.canonicalSeq++

	// Collapsing an undelivered head loses an intermediate delta -> must signal.
	if c.pending != nil {
		c.stickyReset = true
	}

	c.lastHash = h.Hash
	c.lastNumber = h.Number
	c.lastParent = h.ParentHash
	c.hasLast = true

	c.pending = &HeadStateReady{
		Hash:                 h.Hash,
		Number:               strconv.FormatUint(h.Number, 10),
		ParentHash:           h.ParentHash,
		StateRoot:            h.StateRoot,
		ConsensusTimestampMs: strconv.FormatUint(h.ConsensusTimestampMs, 10),
		EvmTimestampSeconds:  strconv.FormatUint(h.EvmTimestampSeconds, 10),
		StateEpoch:           strconv.FormatUint(c.epoch, 10),
		CanonicalSeq:         strconv.FormatUint(c.canonicalSeq, 10),
		FinalizedHash:        h.FinalizedHash,
		PoolRegistryRevision: strconv.FormatUint(h.PoolRegistryRevision, 10),
		BatchID:              nil,
		PoolStateMode:        ModeIdentityOnly,
		DataComplete:         false, // identity_only never claims complete data (§5.1)
		ResetRequired:        false, // filled at Take from the sticky latch
	}
	return class
}

// Take returns the staged HeadStateReady (if any) with reset_required resolved
// from the sticky latch, and clears the staging slot. Delivering a head with
// reset_required=true clears the latch: that head is the fresh baseline. Returns
// nil when nothing is staged.
func (c *HeadCoordinator) Take() *HeadStateReady {
	if c.pending == nil {
		return nil
	}
	r := *c.pending
	c.pending = nil
	r.ResetRequired = c.stickyReset
	if c.stickyReset {
		c.stickyReset = false
	}
	return &r
}

// HasPending reports whether a staged head awaits Take.
func (c *HeadCoordinator) HasPending() bool { return c.pending != nil }
