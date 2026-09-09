// This file binds the node-agnostic feed emitter (eth/arb/feed.go, NODE-01) onto
// the concrete *Ethereum node: it subscribes to the txpool's new-transaction event
// and drains it into a bounded StreamEmitter. It lives in package eth (not eth/arb)
// so it can touch *txpool.TxPool and *types.Transaction directly without an import
// cycle — eth/arb stays node-agnostic and unit-testable, this adapter binds it.
//
// NODE-01: pending feed drain. Design §4.1/§4.2 and the FROZEN wire contract
// protocol/v1/wire.schema.json (arb_subscribe frame = boot/stream_id/seq/kind/payload).
//
// Honesty / non-back-pressure contract:
//   - the drain loop consumes core.NewTxsEvent as fast as it arrives and pushes into
//     a BOUNDED emitter queue via Enqueue, which NEVER blocks: a slow downstream can
//     only cause capacity drops (counted as gaps), never stall the node's txpool.
//   - the subscription goroutine does NOT write client sockets synchronously; it only
//     marshals and enqueues. Per-client serialization / RPC serving is a separate
//     slice (NODE-02) that pops from the emitter.
//   - source_kind is always "txpool_event" and peer/sender are null in phase 1 — we
//     do not fake origin we did not observe (design §4.1).
//
// This file does NOT start any service, open any port, or restart anything. Wiring
// the drain into node startup is a later, separately-authorized NODE task.
package eth

import (
	"encoding/json"
	"strconv"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/eth/arb"
	"github.com/ethereum/go-ethereum/event"
)

// arbFeedDrain owns a txpool subscription and drains new transactions into a
// bounded StreamEmitter. It is created stopped; Start installs the subscription
// and launches the drain goroutine, Stop tears it down. The emitter is guarded by
// mu so a future pop-side (RPC serialization) can share it safely.
type arbFeedDrain struct {
	eth     *Ethereum
	emitter *arb.StreamEmitter

	mu      sync.Mutex // guards emitter (Enqueue vs. future Pop/Heartbeat)
	sub     event.Subscription
	txCh    chan core.NewTxsEvent
	quit    chan struct{}
	stopped chan struct{}
}

// NewArbFeedDrain builds a drain for the given boot id and stream, with the bounded
// queue caps. boot must be a 32-byte lowercase 0x-hex id32 (validated by the
// emitter). The drain is not started until Start is called.
func (s *Ethereum) NewArbFeedDrain(boot, streamID string, maxItems, maxBytes int) (*arbFeedDrain, error) {
	em, err := arb.NewStreamEmitter(boot, streamID, maxItems, maxBytes)
	if err != nil {
		return nil, err
	}
	return &arbFeedDrain{
		eth:     s,
		emitter: em,
		txCh:    make(chan core.NewTxsEvent, 64), // small buffer; drain must keep up
		quit:    make(chan struct{}),
		stopped: make(chan struct{}),
	}, nil
}

// Start installs the txpool subscription (reorgs=true so resurrected txs are also
// observed, per design §4.1) and launches the drain goroutine. The subscription is
// installed BEFORE the loop begins consuming so there is no startup gap between
// subscribe and drain.
func (d *arbFeedDrain) Start() {
	d.sub = d.eth.txPool.SubscribeTransactions(d.txCh, true)
	go d.loop()
}

// Stop unsubscribes and waits for the drain goroutine to exit. Idempotent-safe only
// once; callers should not double-Stop.
func (d *arbFeedDrain) Stop() {
	if d.sub != nil {
		d.sub.Unsubscribe()
	}
	close(d.quit)
	<-d.stopped
	d.mu.Lock()
	d.emitter.Close()
	d.mu.Unlock()
}

// loop is the fast drain. It only marshals and enqueues — never blocks on a client.
func (d *arbFeedDrain) loop() {
	defer close(d.stopped)
	for {
		select {
		case ev := <-d.txCh:
			d.drain(ev)
		case <-d.sub.Err():
			return // subscription closed/errored
		case <-d.quit:
			return
		}
	}
}

// drain converts a batch of new transactions into pending frames and enqueues them.
// Each tx is marshaled to its canonical signed envelope (type prefix preserved) and
// its hash is recomputed from the tx object (not trusted from any pool index).
func (d *arbFeedDrain) drain(ev core.NewTxsEvent) {
	// Snapshot the observed head once per batch; all txs in this batch share it.
	var headHex string
	if h := d.eth.blockchain.CurrentBlock(); h != nil {
		headHex = h.Hash().Hex()
	}
	nowNs := strconv.FormatInt(time.Now().UnixNano(), 10)

	for _, tx := range ev.Txs {
		raw, err := tx.MarshalBinary()
		if err != nil {
			// A tx that cannot be marshaled cannot be forwarded honestly; skip it.
			// It is not a seq gap (nothing was ever assigned).
			continue
		}
		pp := arb.PendingPayload{
			TxHash:              tx.Hash().Hex(),
			RawSignedTx:         hexutil.Encode(raw),
			TxType:              strconv.FormatUint(uint64(tx.Type()), 10),
			SourceKind:          "txpool_event",
			Validation:          "txpool_event",
			ObservedHeadHash:    headHex,
			FirstFullSeenUnixNs: nowNs,
			Sender:              nil, // txpool events carry no peer/sender origin (phase 1)
			PeerTag:             nil,
		}
		payload, err := json.Marshal(pp)
		if err != nil {
			continue
		}
		d.mu.Lock()
		_, _ = d.emitter.Enqueue(arb.KindPending, json.RawMessage(payload))
		d.mu.Unlock()
	}
}

// PopFrame removes the highest-priority queued frame, if any (for the RPC/serialize
// side). Guarded so it is safe against the drain goroutine's Enqueue.
func (d *arbFeedDrain) PopFrame() (*arb.Frame, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.emitter.Pop()
}

// HeartbeatFrame builds a heartbeat frame exposing current seq/drop counts.
func (d *arbFeedDrain) HeartbeatFrame() (*arb.Frame, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.emitter.Heartbeat()
}

// Dropped reports the cumulative capacity-drop count (observability).
func (d *arbFeedDrain) Dropped() uint64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.emitter.Dropped()
}
