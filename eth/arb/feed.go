// Pending/head feed emitter core for the backrun node service (NODE-01, design
// §4.1/§4.2 and the FROZEN wire contract protocol/v1/wire.schema.json).
//
// This file is the node-agnostic EMITTER side: sequence/gap accounting, a bounded
// droppable queue that NEVER back-pressures the node, heartbeat that exposes tail
// loss, and frame serialization that matches the frozen wire schema byte-for-byte.
// It imports nothing from the parent eth package or txpool, so it compiles and
// unit-tests standalone (no CGO). The concrete binding to txpool.SubscribeTransactions
// + tx.MarshalBinary lives in the parent package (eth/arb_feed.go, a later slice).
//
// Wire contract authority (protocol/v1/wire.schema.json, arb_subscribe.frame_fields):
//
//	frame = { boot, stream_id, seq, kind, payload }
//
// Freeze rules honored here:
//   - integers on the wire are STRICT DECIMAL STRINGS (seq, stream_id) — never JSON
//     numbers, never hex. Zero is "0", no leading zeros, no sign for uint.
//   - hashes/ids are lowercase 0x-hex (boot = id32 = 32 bytes; tx_hash = hash32).
//   - raw signed tx keeps its type prefix, even-length lowercase hex.
//   - optional fields are emitted as JSON null when absent — never faked with zero
//     (design §4.1: "peer来源...缺失写null").
//
// Sequence semantics (design §4.2, mirrored by the Rust consumer's StreamTracker):
//   - seq is per-stream, assigned BEFORE an item enters the droppable queue and
//     starts at 1.
//   - a full queue still ADVANCES seq and accumulates a gap count; it does not stall
//     the drain. The consumer detects the gap from the seq jump and resyncs.
//   - even after the last pending item, a heartbeat carries the current seq and drop
//     count so the consumer can detect TAIL loss (items dropped after the last one
//     it received).
package arb

import (
	"encoding/json"
	"errors"
	"strconv"
)

// Kind is the subscription stream kind (wire: arb_subscribe params.kind enum).
type Kind string

const (
	KindPending Kind = "pending"
	KindHead    Kind = "head"
)

// PendingPayload is the payload for a KindPending frame (design §4.2 "Pending").
// Field names and null-for-absent are part of the wire contract with the Rust
// consumer; do not rename without bumping schema_version on both sides.
//
// Optional fields use pointer types WITHOUT omitempty so a nil serializes as JSON
// null (present-but-null), never dropped — the consumer distinguishes "unknown"
// (null) from any real value.
type PendingPayload struct {
	// TxHash is the canonical hash recomputed from the marshaled envelope (never
	// trusted from the pool index): lowercase 0x-hex hash32.
	TxHash string `json:"tx_hash"`
	// RawSignedTx is tx.MarshalBinary() output: even-length lowercase 0x-hex,
	// type prefix preserved for typed txs.
	RawSignedTx string `json:"raw_signed_tx"`
	// TxType is the EIP-2718 tx type byte as a decimal string ("0" legacy).
	TxType string `json:"tx_type"`
	// SourceKind is how we observed the tx. Phase 1 is always "txpool_event".
	SourceKind string `json:"source_kind"`
	// Validation is the trust level. txpool events are "txpool_event"; a future
	// P2P bypass would emit "unvalidated".
	Validation string `json:"validation"`
	// ObservedHeadHash is the canonical head hash when the tx was drained:
	// lowercase 0x-hex hash32.
	ObservedHeadHash string `json:"observed_head_hash"`
	// FirstFullSeenUnixNs is the emitter's monotonic-derived wall-clock ns when
	// the full tx was first seen, as a decimal string.
	FirstFullSeenUnixNs string `json:"first_full_seen_unix_ns"`
	// Sender is the recovered sender if cheaply available, else null. Phase 1
	// leaves this null (recovery is deferred to production simulation, design §4.1).
	Sender *string `json:"sender_optional"`
	// PeerTag is the origin peer tag if available, else null. txpool events do not
	// carry peer origin, so this is null in phase 1 (design §4.1).
	PeerTag *string `json:"peer_tag_optional"`
}

// HeartbeatPayload lets the consumer detect tail loss when no new items arrive.
// It carries the current high-water seq and cumulative drop count for the stream.
type HeartbeatPayload struct {
	// Kind is fixed "heartbeat" to disambiguate from real pending/head payloads.
	Kind string `json:"kind"`
	// LastSeq is the highest seq assigned so far (decimal string); 0 => nothing
	// emitted yet.
	LastSeq string `json:"last_seq"`
	// Dropped is the cumulative count of items dropped due to capacity
	// (decimal string). A rise since the consumer's last frame means tail loss.
	Dropped string `json:"dropped"`
}

// Frame is one StreamEnvelope on the wire. Field order in the struct is irrelevant
// to JSON; the json tags are the contract.
type Frame struct {
	Boot     string          `json:"boot"`      // id32, lowercase 0x-hex (32 bytes)
	StreamID string          `json:"stream_id"` // per-connection stream id, decimal string
	Seq      string          `json:"seq"`       // per-stream sequence, decimal string, from "1"
	Kind     Kind            `json:"kind"`
	Payload  json.RawMessage `json:"payload"`
}

// Errors surfaced by the emitter.
var (
	// ErrEmitterClosed: the emitter was closed; no further frames are produced.
	ErrEmitterClosed = errors.New("arb: feed emitter closed")
	// ErrBadBoot: boot id is not a 0x-prefixed 32-byte lowercase hex string.
	ErrBadBoot = errors.New("arb: boot id must be 0x + 64 lowercase hex chars")
)

// queued is an item awaiting delivery, tagged with the seq assigned at enqueue.
type queued struct {
	seq     uint64
	bytes   int
	kind    Kind
	payload json.RawMessage
}

// StreamEmitter owns the sequence/gap state and bounded queue for ONE subscription
// stream. It is not safe for concurrent use; the drain loop that owns it calls
// Enqueue, and the same goroutine (or a mutex-guarded caller) calls Pop/Heartbeat.
// The design keeps the fast txpool drain and the per-client serialization in
// separate goroutines connected by this bounded queue, so a slow client socket can
// never stall the drain.
type StreamEmitter struct {
	boot     string
	streamID string

	// nextSeq is the seq to assign to the NEXT enqueued item (starts at 1).
	nextSeq uint64
	// lastSeq is the highest seq assigned so far (0 until first enqueue).
	lastSeq uint64
	// dropped is the cumulative count of items dropped for capacity.
	dropped uint64

	// bounded FIFO queue with a count cap and total-byte cap. Phase-1 pending
	// items share a uniform priority, so eviction is drop-oldest (FIFO head).
	// Priority-class eviction (matching the Rust BoundedFeed) can be layered on
	// when head/pending are multiplexed; documented as a follow-up.
	q        []queued
	maxItems int
	maxBytes int
	curBytes int

	closed bool
}

// NewStreamEmitter validates boot and returns an emitter with the given caps.
// maxItems/maxBytes must be > 0. boot must be a 32-byte lowercase 0x-hex id32.
func NewStreamEmitter(boot, streamID string, maxItems, maxBytes int) (*StreamEmitter, error) {
	if !isID32(boot) {
		return nil, ErrBadBoot
	}
	if maxItems <= 0 || maxBytes <= 0 {
		return nil, errors.New("arb: emitter caps must be positive")
	}
	return &StreamEmitter{
		boot:     boot,
		streamID: streamID,
		nextSeq:  1,
		maxItems: maxItems,
		maxBytes: maxBytes,
	}, nil
}

// LastSeq returns the highest seq assigned so far (0 if none).
func (e *StreamEmitter) LastSeq() uint64 { return e.lastSeq }

// Dropped returns the cumulative capacity-drop count.
func (e *StreamEmitter) Dropped() uint64 { return e.dropped }

// Len returns the number of items currently queued.
func (e *StreamEmitter) Len() int { return len(e.q) }

// Bytes returns the total queued payload bytes.
func (e *StreamEmitter) Bytes() int { return e.curBytes }

// Enqueue assigns the next seq to payload and places it in the bounded queue.
// It NEVER blocks: if the queue is full (by count or bytes) it drops the oldest
// queued item(s) and counts a gap, but the seq is still consumed so the consumer
// observes the jump. Returns the assigned seq. Returns ErrEmitterClosed if closed.
//
// Enqueue advances seq even when the item itself is immediately dropped (single
// item larger than the byte cap), because the seq was already committed to this
// item — the consumer must see it as a gap, never as a silently reused number.
func (e *StreamEmitter) Enqueue(kind Kind, payload json.RawMessage) (uint64, error) {
	if e.closed {
		return 0, ErrEmitterClosed
	}
	seq := e.nextSeq
	e.nextSeq++
	e.lastSeq = seq

	b := len(payload)
	// A single item exceeding the byte cap can never be held: drop it, count gap.
	// seq is still consumed (assigned above) so the consumer detects the loss.
	if b > e.maxBytes {
		e.dropped++
		return seq, nil
	}

	// Evict oldest items until this one fits by both count and bytes.
	for len(e.q)+1 > e.maxItems || e.curBytes+b > e.maxBytes {
		if len(e.q) == 0 {
			break // nothing left to evict; byte check above guarantees fit
		}
		old := e.q[0]
		e.q = e.q[1:]
		e.curBytes -= old.bytes
		e.dropped++
	}

	e.q = append(e.q, queued{seq: seq, bytes: b, kind: kind, payload: payload})
	e.curBytes += b
	return seq, nil
}

// Pop removes and returns the oldest queued item as a wire Frame, or (nil,false)
// if the queue is empty. FIFO delivery order preserves the seq ordering the
// consumer expects (monotonic, with gaps where drops occurred).
func (e *StreamEmitter) Pop() (*Frame, bool) {
	if len(e.q) == 0 {
		return nil, false
	}
	item := e.q[0]
	e.q = e.q[1:]
	e.curBytes -= item.bytes
	f := &Frame{
		Boot:     e.boot,
		StreamID: e.streamID,
		Seq:      strconv.FormatUint(item.seq, 10),
		Kind:     item.kind,
		Payload:  item.payload,
	}
	return f, true
}

// Heartbeat builds a heartbeat Frame carrying the current high-water seq and drop
// count WITHOUT consuming a new seq — its Seq field echoes lastSeq so the consumer
// can compare against what it has received and detect tail loss. A heartbeat is
// never a gap: it does not advance nextSeq.
func (e *StreamEmitter) Heartbeat() (*Frame, error) {
	hb := HeartbeatPayload{
		Kind:    "heartbeat",
		LastSeq: strconv.FormatUint(e.lastSeq, 10),
		Dropped: strconv.FormatUint(e.dropped, 10),
	}
	raw, err := json.Marshal(hb)
	if err != nil {
		return nil, err
	}
	return &Frame{
		Boot:     e.boot,
		StreamID: e.streamID,
		Seq:      strconv.FormatUint(e.lastSeq, 10),
		Kind:     KindPending, // heartbeat rides the same stream kind; payload.kind disambiguates
		Payload:  raw,
	}, nil
}

// Close marks the emitter closed; subsequent Enqueue returns ErrEmitterClosed.
// Queued items may still be Pop'd/flushed by the caller before teardown.
func (e *StreamEmitter) Close() { e.closed = true }

// isID32 reports whether s is a 0x-prefixed 32-byte lowercase hex string, matching
// the wire schema's id32/hash32 pattern ^0x[0-9a-f]{64}$.
func isID32(s string) bool {
	if len(s) != 66 || s[0] != '0' || s[1] != 'x' {
		return false
	}
	for i := 2; i < 66; i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}
