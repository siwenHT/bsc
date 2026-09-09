// This file binds the node-agnostic HeadCoordinator (eth/arb/head.go, NODE-02) onto
// the concrete *Ethereum node. It subscribes to ChainEvent and ChainHeadEvent as
// "verify now" wake-ups (design §5.1: batch import / SetHead / merged head
// notifications mean an arbitrary event is NOT itself proof of a tradable head),
// resolves the CURRENT canonical head on each wake-up, probes its state root for
// readability through the NODE-00 backend, and feeds the coordinator.
//
// It lives in package eth (not eth/arb) so it can touch *core.BlockChain and
// *types.Header directly. eth/arb stays node-agnostic and unit-testable.
//
// Honest timestamps (design §5.1 HeadStateReady): BSC post-Lorentz stores the
// millisecond remainder in MixDigest, exposed via Header.MilliTimestamp()
// (= Time*1000 + ms). We emit consensus_timestamp_ms = MilliTimestamp() and
// evm_timestamp_seconds = Time — never seconds*1000 faked as millisecond precision.
//
// Phase 1 is identity_only: the coordinator emits head identity plus epoch/seq/
// reset; it does NOT read pool state here. Pool snapshots (V2 reserves, V3
// slot0/bitmap/ticks at a fixed parent) are the second NODE-02 slice, which needs
// the NODE-00 state handle and lives separately.
//
// This file does NOT start any service, open any port, or restart anything. Wiring
// the coordinator loop into node startup is a later, separately-authorized task.
package eth

import (
	"sync"

	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/eth/arb"
	"github.com/ethereum/go-ethereum/event"
)

// arbHeadDrain owns the chain subscriptions and drives the HeadCoordinator. The
// coordinator is single-owner; this drain's loop goroutine is its only mutator,
// and Take/state reads by a serving side are guarded by mu.
type arbHeadDrain struct {
	eth     *Ethereum
	backend arb.Backend
	coord   *arb.HeadCoordinator

	mu sync.Mutex // guards coord (Observe in loop vs. Take on serving side)

	evCh    chan core.ChainEvent
	headCh  chan core.ChainHeadEvent
	evSub   event.Subscription
	headSub event.Subscription
	quit    chan struct{}
	stopped chan struct{}
}

// NewArbHeadDrain builds a head-stream drain over the node's arb backend and a
// fresh coordinator. Not started until Start.
func (s *Ethereum) NewArbHeadDrain() *arbHeadDrain {
	return &arbHeadDrain{
		eth:     s,
		backend: s.NewArbStateBackend(),
		coord:   arb.NewHeadCoordinator(),
		evCh:    make(chan core.ChainEvent, 32),
		headCh:  make(chan core.ChainHeadEvent, 32),
		quit:    make(chan struct{}),
		stopped: make(chan struct{}),
	}
}

// Start installs both subscriptions BEFORE consuming (no subscribe/read gap) and
// launches the drive loop.
func (d *arbHeadDrain) Start() {
	d.evSub = d.eth.blockchain.SubscribeChainEvent(d.evCh)
	d.headSub = d.eth.blockchain.SubscribeChainHeadEvent(d.headCh)
	go d.loop()
}

// Stop unsubscribes and waits for the loop to exit.
func (d *arbHeadDrain) Stop() {
	if d.evSub != nil {
		d.evSub.Unsubscribe()
	}
	if d.headSub != nil {
		d.headSub.Unsubscribe()
	}
	close(d.quit)
	<-d.stopped
}

// loop treats every event as a wake-up and re-evaluates the current canonical head.
func (d *arbHeadDrain) loop() {
	defer close(d.stopped)
	for {
		select {
		case <-d.evCh:
			d.evaluate()
		case <-d.headCh:
			d.evaluate()
		case <-d.evSub.Err():
			return
		case <-d.headSub.Err():
			return
		case <-d.quit:
			return
		}
	}
}

// evaluate resolves the current canonical head and feeds the coordinator. It does
// NOT trust the event payload as the head; it reads CurrentBlock fresh (design
// §5.1 step 1). If the head's state root is not readable, it does not publish a
// Ready — an unreadable root cannot back a tradable head. Overflow of the internal
// wake-up channels is surfaced to the coordinator as a reset signal (design §5.2:
// never silently lose a head delta).
func (d *arbHeadDrain) evaluate() {
	h := d.eth.blockchain.CurrentBlock()
	if h == nil {
		return // no canonical head yet -> NotReady, nothing to publish
	}

	// A readable root is required before we call a head tradable (design §5.1
	// step 2: check the backend explicitly, no historic reexec here).
	if !d.backend.Readable(h.Root) {
		return
	}

	sig := arb.ObserveSignal{
		// If either wake-up channel is saturated, we may have collapsed
		// intermediate heads before re-evaluating; tell the coordinator to latch
		// reset so the consumer rebuilds rather than trusting a contiguous stream.
		DownstreamOverflow: len(d.evCh) == cap(d.evCh) || len(d.headCh) == cap(d.headCh),
	}

	in := headInputFromHeader(h, d.eth.blockchain.CurrentFinalBlock())

	d.mu.Lock()
	d.coord.Observe(in, sig)
	d.mu.Unlock()
}

// TakeReady returns the staged HeadStateReady (if any) with reset_required
// resolved. Guarded against the loop's Observe.
func (d *arbHeadDrain) TakeReady() *arb.HeadStateReady {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.coord.Take()
}

// Epoch/CanonicalSeq expose current coordinator counters for observability.
func (d *arbHeadDrain) Epoch() uint64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.coord.Epoch()
}

// headInputFromHeader maps a canonical header (and optional finalized header) onto
// the pure HeadInput. Millisecond and second timestamps come from distinct honest
// sources (see file header). PoolRegistryRevision is 0 in phase 1 (identity_only;
// the registry batch is wired in the second slice).
func headInputFromHeader(h *types.Header, finalized *types.Header) arb.HeadInput {
	var finHash *string
	if finalized != nil {
		s := finalized.Hash().Hex()
		finHash = &s
	}
	return arb.HeadInput{
		Hash:                 h.Hash().Hex(),
		Number:               h.Number.Uint64(),
		ParentHash:           h.ParentHash.Hex(),
		StateRoot:            h.Root.Hex(),
		ConsensusTimestampMs: h.MilliTimestamp(), // real ms (Time*1000 + MixDigest ms)
		EvmTimestampSeconds:  h.Time,             // EVM TIMESTAMP opcode value (seconds)
		FinalizedHash:        finHash,
		PoolRegistryRevision: 0,
	}
}
