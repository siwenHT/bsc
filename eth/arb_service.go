// The arb service: the CGO adapter that welds the pure NODE-04 cores (JobRegistry,
// WorkerPool, HandleStore — all in package arb, node-agnostic and unit-tested) onto
// the concrete *Ethereum node, plus the fixed set of runner goroutines that pull
// from a bounded queue and drive the already-verified target/prefix/candidate
// executors under a per-job read budget and cooperative cancel.
//
// This file lives in package eth (like arb_backend.go / arb_feed.go / arb_head.go)
// so it can touch *Ethereum, *state.StateDB and the executor directly without an
// import cycle. It holds NO simulation logic of its own — every subtle invariant
// (queue capacity, exactly-once slot release, cancel/complete race, idempotency,
// result TTL) already lives in the pure cores and is locked by their tests. Here we
// only: mint the boot id, run workers, decode wire requests into executor calls,
// and map executor results into JobOutcome.
//
// Cancellation model (v1): cooperative, per the wire contract ("arb_cancelJob does
// NOT promise the worker has exited; resource-release finality is observed via
// arb_getJob", §401). RequestCancel flips the job to CancelRequested and the worker
// holding the job's EVM cancel func trips it at the next cooperative check; the
// worker then runs to a bounded stop and Complete resolves the race to Cancelled,
// discarding any success evidence. A hard context interrupt is a later refinement.

package eth

import (
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/eth/arb"
	"github.com/ethereum/go-ethereum/log"
)

// arbJob is one unit of queued work: the registry job id plus the closure that
// actually runs the simulation under a budget. The closure is built by the RPC
// method from the decoded request; the runner does not know which of the three
// executor variants it is. It returns the wire outcome and whether the job
// succeeded (success gates Succeeded vs Failed; a cancel that won the race turns
// either into Cancelled inside the registry).
type arbJob struct {
	jobID string
	run   func(budget *arb.ReadBudget) (*arb.JobOutcome, bool)
	// maxReads / wall bound the per-job read budget; the adapter builds the budget
	// fresh per run so a requeue never shares a latched budget.
	maxReads uint64
	wall     time.Duration
}

// arbService is the node-bound owner of the NODE-04 surface. One per node boot.
type arbService struct {
	eth  *Ethereum
	boot string

	jobs    *arb.JobRegistry
	pool    *arb.WorkerPool
	handles *arb.HandleStore
	// handleStates is the concrete-state side table (arb_handles.go); the pure
	// HandleStore governs lifecycle, this binds the actual *state.StateDB.
	handleStates *handleTable

	queue   chan arbJob
	workers int

	quit    chan struct{}
	stopped chan struct{}
	wg      sync.WaitGroup

	// sweepEvery is how often terminal jobs past their result TTL are evicted.
	sweepEvery time.Duration
}

// arbServiceConfig carries the tunables so tests and the node ctor can vary them
// without touching the wiring. All have sane defaults applied in newArbService.
type arbServiceConfig struct {
	Workers    int           // number of fixed runner goroutines
	QueueDepth int           // bounded queue capacity (in-flight cap = workers+queue)
	ResultTTL  time.Duration // how long a terminal job's result is retained
	SweepEvery time.Duration // eviction sweep cadence
}

func (c arbServiceConfig) withDefaults() arbServiceConfig {
	if c.Workers <= 0 {
		c.Workers = 4
	}
	if c.QueueDepth <= 0 {
		c.QueueDepth = 64
	}
	if c.ResultTTL <= 0 {
		c.ResultTTL = 10 * time.Second // §417 JobResultTTLMillis = 10000
	}
	if c.SweepEvery <= 0 {
		c.SweepEvery = time.Second
	}
	return c
}

// newArbService builds the service and its pure cores bound to a fresh boot id.
// It does NOT start the workers (call Start). The in-flight capacity handed to the
// worker pool is workers+queueDepth: that many jobs may be admitted (queued or
// running) before arb_simulate* is refused with a capacity failure.
func (s *Ethereum) newArbService(cfg arbServiceConfig) (*arbService, error) {
	cfg = cfg.withDefaults()
	boot := arb.NewBootID()
	clock := arb.Clock(func() time.Time { return time.Now() })

	jobs, err := arb.NewJobRegistry(boot, clock, arb.RandomID32, cfg.ResultTTL)
	if err != nil {
		return nil, err
	}
	pool, err := arb.NewWorkerPool(cfg.Workers + cfg.QueueDepth)
	if err != nil {
		return nil, err
	}
	handles := arb.NewHandleStore(clock, arb.RandomID32)

	return &arbService{
		eth:          s,
		boot:         boot,
		jobs:         jobs,
		pool:         pool,
		handles:      handles,
		handleStates: newHandleTable(),
		queue:        make(chan arbJob, cfg.QueueDepth),
		workers:    cfg.Workers,
		quit:       make(chan struct{}),
		stopped:    make(chan struct{}),
		sweepEvery: cfg.SweepEvery,
	}, nil
}

// Boot returns the node boot id stamped into jobs and feed frames this boot.
func (a *arbService) Boot() string { return a.boot }

// Start launches the fixed runner goroutines and the eviction sweeper. Idempotent
// guard is the caller's responsibility (the node starts it once).
func (a *arbService) Start() error {
	for i := 0; i < a.workers; i++ {
		a.wg.Add(1)
		go a.runner()
	}
	a.wg.Add(1)
	go a.sweeper()
	go func() { a.wg.Wait(); close(a.stopped) }()
	log.Info("arb service started", "boot", a.boot, "workers", a.workers)
	return nil
}

// Stop signals shutdown and waits for all runners and the sweeper to exit. Design
// §101: stop accepting new work first, then let in-flight workers drain. We close
// quit (runners stop pulling new queue items and exit) and wait. Jobs still queued
// but not yet started are abandoned in the channel; their slots are reclaimed at
// process exit. In-flight jobs finish their current run (cooperative).
func (a *arbService) Stop() error {
	close(a.quit)
	<-a.stopped
	log.Info("arb service stopped", "boot", a.boot)
	return nil
}

// runner is one fixed worker. It pulls a job, marks it Running, runs the closure
// under a fresh per-job budget, reports the terminal outcome, and ALWAYS releases
// the pool slot exactly once (design §401: the slot returns only when the worker
// truly exits this job). A panic in the closure is recovered and reported as a
// failed job so one bad request never takes down a worker.
func (a *arbService) runner() {
	defer a.wg.Done()
	for {
		select {
		case <-a.quit:
			return
		case job := <-a.queue:
			a.execute(job)
		}
	}
}

// execute drives one job through Start -> run -> Complete -> Release. It is a
// method (not inline) so a deferred slot-release and panic recovery bracket the
// whole run regardless of how the closure exits.
func (a *arbService) execute(job arbJob) {
	// Release the pool slot exactly once when this worker is fully done with the
	// job, whatever the outcome (success/fail/cancel/panic).
	defer func() {
		if err := a.pool.Release(job.jobID); err != nil {
			log.Warn("arb job slot release", "job", job.jobID, "err", err)
		}
	}()

	// Claim the job. If it was cancelled while queued, Start fails and we must not
	// run it — just release the slot (deferred) and drop it.
	if err := a.jobs.Start(job.jobID); err != nil {
		return
	}

	budget := arb.NewReadBudget(func() time.Time { return time.Now() }, job.maxReads, job.wall)

	var outcome *arb.JobOutcome
	var success bool
	func() {
		defer func() {
			if r := recover(); r != nil {
				log.Error("arb job panicked", "job", job.jobID, "recover", r)
				outcome = &arb.JobOutcome{ErrCode: "internal_panic"}
				success = false
			}
		}()
		outcome, success = job.run(budget)
	}()

	if err := a.jobs.Complete(job.jobID, success, outcome); err != nil {
		log.Warn("arb job complete", "job", job.jobID, "err", err)
	}
}

// sweeper periodically evicts terminal jobs past their result TTL, so a client that
// never reads a completed job does not pin memory forever.
func (a *arbService) sweeper() {
	defer a.wg.Done()
	t := time.NewTicker(a.sweepEvery)
	defer t.Stop()
	for {
		select {
		case <-a.quit:
			return
		case <-t.C:
			a.jobs.Sweep()
		}
	}
}

// enqueue admits a job to the worker pool (capacity gate) and pushes it onto the
// bounded queue. On capacity rejection it marks the registry job Failed with a
// queue_full code (Reject) and returns false — the caller reports that terminal
// state, NOT a fresh error, so the client can poll the job and see the refusal.
// A jobID that already holds a slot (idempotent resubmit that returned an existing
// running/queued job) is not re-enqueued.
func (a *arbService) enqueue(job arbJob, alreadyExisting bool) bool {
	if alreadyExisting {
		// Existing job: it is already queued/running/terminal; do not double-enqueue
		// or double-admit. Idempotent Admit is a no-op for a held slot.
		return true
	}
	if err := a.pool.Admit(job.jobID); err != nil {
		// At capacity: honest refusal recorded on the job itself.
		if rerr := a.jobs.Reject(job.jobID, "queue_full"); rerr != nil {
			log.Warn("arb reject at capacity", "job", job.jobID, "err", rerr)
		}
		return false
	}
	select {
	case a.queue <- job:
		return true
	case <-a.quit:
		// Shutting down: release the slot we just took and refuse.
		_ = a.pool.Release(job.jobID)
		_ = a.jobs.Reject(job.jobID, "shutting_down")
		return false
	}
}
