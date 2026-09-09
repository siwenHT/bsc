// NODE-04 admission control: the pure capacity accounting behind the bounded job
// queue and fixed worker set (design §92 "JobRegistry -> bounded queue -> fixed
// Runner workers", §383 combined limits, §401 "只有 worker 真退出才归还配额").
//
// This file is package arb and node-agnostic on purpose: it owns ONLY the slot
// accounting — how many jobs may be in flight (queued + running) at once, and the
// invariant that a slot is released EXACTLY ONCE, and only when a worker has truly
// exited (terminal), never optimistically on cancel-request. The eth/ adapter owns
// the actual goroutine pool, the channels, and the EVM cancel wiring; it consults
// this gate to decide admit-or-reject and to return a slot when a worker returns.
//
// Keeping this pure lets the subtle invariants — no admission past cap, no double
// free, no slot returned before the worker exits — be locked by a deterministic
// unit test rather than chased through goroutine races.

package arb

import (
	"errors"
	"sync"
)

var (
	// ErrPoolAtCapacity: admission refused because in-flight (queued+running) has
	// reached the configured cap. The adapter maps this to a Failed job with a
	// "queue_full" code (JobRegistry.Reject), NOT a Cancelled job — the client did
	// not cancel, the node refused capacity (§383). Retriable with a fresh request.
	ErrPoolAtCapacity = errors.New("arb: job pool at capacity")
	// ErrSlotAlreadyReleased: Release called for a job whose slot was already
	// returned. Latched to guarantee exactly-once accounting (§401): a worker's
	// terminal exit frees its slot once; a duplicate release is a caller bug and
	// is surfaced, never silently double-counted.
	ErrSlotAlreadyReleased = errors.New("arb: slot already released for this job")
	// ErrSlotNotHeld: Release called for a job that never held a slot (never
	// admitted, or an unknown id).
	ErrSlotNotHeld = errors.New("arb: no slot held for this job")
	// ErrBadCapacity: NewWorkerPool given a non-positive capacity.
	ErrBadCapacity = errors.New("arb: worker pool capacity must be positive")
)

// WorkerPool is the pure in-flight slot accountant. cap is the maximum number of
// simultaneously admitted jobs (queued + running); a slot is held from Admit
// until the job's worker truly exits and Release is called. All state changes are
// serialized under mu; the adapter must not hold mu across EVM work.
type WorkerPool struct {
	mu   sync.Mutex
	cap  int
	live int             // count of currently-held (true) slots; the capacity gate
	held map[string]bool // job_id -> slot live (true) / released-latch (false)
}

// NewWorkerPool builds a pool with the given positive capacity.
func NewWorkerPool(capacity int) (*WorkerPool, error) {
	if capacity <= 0 {
		return nil, ErrBadCapacity
	}
	return &WorkerPool{cap: capacity, held: make(map[string]bool)}, nil
}

// Admit reserves a slot for jobID if the pool is below capacity. It returns
// ErrPoolAtCapacity when full (honest backpressure, no silent drop). Admitting a
// jobID that already holds a slot is a no-op success — Admit is keyed by job_id so
// an idempotent resubmit that returns an existing job does not double-count.
func (p *WorkerPool) Admit(jobID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.held[jobID] {
		return nil // already counted; idempotent for existing-job resubmits
	}
	if p.live >= p.cap {
		return ErrPoolAtCapacity
	}
	p.held[jobID] = true
	p.live++
	return nil
}

// Release returns the slot held by jobID. It is exactly-once: the first call frees
// the slot, a second call returns ErrSlotAlreadyReleased. Releasing a job that
// never held a slot returns ErrSlotNotHeld. The adapter calls Release only when
// the worker has truly exited (terminal), so capacity reflects real in-flight
// work, never a cancel-requested job still holding its StateDB/gas (§401).
func (p *WorkerPool) Release(jobID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	held, seen := p.held[jobID]
	if !seen {
		return ErrSlotNotHeld
	}
	if !held {
		return ErrSlotAlreadyReleased
	}
	p.held[jobID] = false // latch: seen but no longer held
	p.live--
	return nil
}

// InFlight reports the number of slots currently held (queued+running jobs).
func (p *WorkerPool) InFlight() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.live
}

// Capacity returns the configured maximum in-flight count.
func (p *WorkerPool) Capacity() int { return p.cap }

// Forget drops all accounting for jobID (both the held flag and the release
// latch), so its id may be reused after the job has been fully evicted from the
// registry. It must only be called for a job whose slot is already released; it
// returns ErrSlotAlreadyReleased-free no-op if the id is unknown, and refuses to
// forget a job still holding a live slot.
func (p *WorkerPool) Forget(jobID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.held[jobID] {
		return errors.New("arb: cannot forget a job still holding a slot")
	}
	delete(p.held, jobID)
	return nil
}
