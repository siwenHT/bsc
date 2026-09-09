// NODE-04 job registry: the pure state machine behind the four async simulate
// methods (arb_simulateTarget/Candidate/SignedBundle + arb_getPostPoolState) and
// their observation/cancellation surface (arb_getJob / arb_cancelJob).
//
// This file lives in package arb and is deliberately node-agnostic: it owns ONLY
// the job lifecycle (state transitions, idempotency, boot scoping, result TTL
// eviction, and the cancel/complete race resolution). It touches no EVM, no
// goroutine, no *Ethereum — the eth/ adapter binds a real worker goroutine and
// EVM cancel func on top of this, exactly as arb_backend.go binds the state
// capability and arb_feed.go binds the emitter. That keeps this keystone unit-
// testable with a fake clock and a counter id generator.
//
// Wire contract (protocol/v1/wire.schema.json, authoritative):
//   - The four simulate methods take a client request_id (id32) and return a
//     server-generated job_id (result_fields:["job_id"]). request_id is the
//     idempotency key; the server owns job_id.
//   - arb_getJob(job_id, boot) reports status ∈ Queued/Running/Succeeded/Failed/
//     Cancelled plus stamp/request_digest/result_kind/metering/completeness. An
//     evicted/unknown job is JobNotFound — the client must NOT treat unknown as
//     "never ran" and must NOT expect an automatic rerun (§295).
//   - arb_cancelJob(job_id, boot) returns cancel_requested; it does NOT promise
//     the worker has exited — resource-release finality is observed via getJob
//     (§ wire note, §401).
//
// Design semantics (bsc_node_backrun_modification_design.md §9.1/§397/§401):
//   - Queued -> Running -> Succeeded/Failed/Cancelled. Running may enter an
//     internal CancelRequested; it stays externally "Running" until the worker
//     actually exits and reports a terminal outcome, because the worker still
//     holds its StateDB/gas/read budget and the slot must not be re-handed out
//     before it truly returns (§401 "只有 worker 真退出才归还配额").
//   - cancel/complete race is decided serially under the registry lock: if cancel
//     arrived before completion, the success/fail evidence is discarded and the
//     job becomes Cancelled (§401 "结果完成前已取消则不发成功证据"); if the job
//     was already terminal, cancel is a no-op returning the existing terminal
//     state ("若早已完成则返回既有终态").
//   - boot scoping: this registry belongs to one node boot; getJob/cancel from a
//     stale boot are rejected, so a client that missed a restart cannot read or
//     cancel across the boot boundary.

package arb

import (
	"errors"
	"sync"
	"time"
)

// JobStatus is the internal lifecycle state. CancelRequested is internal only;
// it never crosses the wire (WireStatus maps it to "Running").
type JobStatus uint8

const (
	// JobQueued: submitted, no worker has started it yet.
	JobQueued JobStatus = iota
	// JobRunning: a worker has claimed it and holds its resources.
	JobRunning
	// JobCancelRequested: cancel arrived while Running; the worker has been asked
	// to stop but has not yet reported a terminal outcome. Externally "Running".
	JobCancelRequested
	// JobSucceeded / JobFailed / JobCancelled: terminal. Result (if any) frozen.
	JobSucceeded
	JobFailed
	JobCancelled
)

func (s JobStatus) String() string {
	switch s {
	case JobQueued:
		return "Queued"
	case JobRunning:
		return "Running"
	case JobCancelRequested:
		return "CancelRequested"
	case JobSucceeded:
		return "Succeeded"
	case JobFailed:
		return "Failed"
	case JobCancelled:
		return "Cancelled"
	default:
		return "Unknown"
	}
}

// WireStatus maps the internal state onto the wire enum (Queued/Running/
// Succeeded/Failed/Cancelled). CancelRequested is reported as Running because the
// worker still holds resources and has not reached a terminal state (§397).
func (s JobStatus) WireStatus() string {
	if s == JobCancelRequested {
		return "Running"
	}
	return s.String()
}

// IsTerminal reports whether the job has reached a frozen terminal state.
func (s JobStatus) IsTerminal() bool {
	return s == JobSucceeded || s == JobFailed || s == JobCancelled
}

// Metering is the wire-reportable resource accounting for a finished job. The
// adapter fills it from the executor / read budget; the pure layer only stores
// and echoes it.
type Metering struct {
	StateReads  uint64
	GasUsed     uint64
	WallMicros  uint64
	ResultBytes uint64
}

// JobOutcome is the wire-reportable result of a completed job. It carries no EVM
// types on purpose — result_kind, metering and completeness are all the client
// observes via arb_getJob; PostHandle (id32 or "") is the optional output state
// handle whose lifetime is tracked separately in the HandleStore (§206: job
// completion != handle destruction). ErrCode is set on Failed.
type JobOutcome struct {
	Kind       string // result_kind, e.g. "target"/"candidate"/"bundle"/"post_pool"
	Metering   Metering
	Complete   bool   // completeness: was the result fully computed
	PostHandle string // output handle id (id32) or "" if none
	ErrCode    string // failure code when the job Failed; "" otherwise
	// RetainedT0/RetainedT2 carry the §391 standardized-base-token retention of the
	// candidate's beneficiary (executor) measured at two isolated points: T0 = after
	// the signed prefix completed but BEFORE our unsigned candidate ran; T2 = after
	// our candidate succeeded. Both are decimal-string uint256 (no EVM types in this
	// pure layer). They are set TOGETHER or not at all: "" (nil-on-wire) whenever the
	// caller supplied no executor/base_token, the candidate did not succeed, or a
	// balance read failed/tripped the budget (§389: never fabricate a T2 boundary —
	// the Rust side then conservatively treats gross as unmeasured). gross = T2 - T0
	// and the non-positive rejection are the CLIENT's call, never inferred here.
	RetainedT0 string // executor base-token balance before our candidate; "" if unmeasured
	RetainedT2 string // executor base-token balance after our candidate; "" if unmeasured
	// Snapshots carries the post_pool result payload (v2 wire result_kind=post_pool):
	// one entry per requested pool read, in request order, on a successful complete
	// read. Empty for every other kind, and empty (not partial) if any read failed —
	// the whole job fails instead of returning a fabricated/partial snapshot (§381).
	// Pure decimal-string values only (no EVM types in this layer); the adapter shapes
	// each into its v2/v3 wire object by Kind.
	Snapshots []PoolSnapshot
}

// PoolSnapshot is one pool's post-state read, wire-reportable as decimal strings
// (§9.3/§9.5). Kind selects which fields are meaningful: "v2" fills reserve/balance
// pairs (V3 fields ""); "v3" fills sqrt_price/tick/liquidity (V2 fields ""). The
// adapter emits only the Kind-relevant fields onto the wire (additionalProperties
// false per side). Carries no EVM types on purpose — same discipline as JobOutcome.
type PoolSnapshot struct {
	Locator string // pool contract address, lowercase 0x-hex
	Kind    string // "v2" | "v3"
	// V2 (empty when Kind=="v3"):
	Reserve0 string
	Reserve1 string
	Balance0 string
	Balance1 string
	// V3 (empty when Kind=="v2"):
	SqrtPriceX96 string
	Tick         string // signed decimal (int24)
	Liquidity    string
}

// JobView is the read-only projection returned by Get, mirroring arb_getJob
// result_fields. Outcome is nil until the job is terminal (Succeeded/Failed) with
// a result; a Cancelled job may carry nil Outcome (cancel won the race).
type JobView struct {
	JobID   string
	Status  JobStatus
	Stamp   string // hash32 bound at submit (env/config stamp, §9.1)
	Digest  string // request_digest (hash32)
	Kind    string // result_kind, "" until known
	Outcome *JobOutcome
}

type jobEntry struct {
	jobID      string
	requestID  string
	stamp      string
	digest     string
	status     JobStatus
	outcome    *JobOutcome
	terminalAt time.Time // set when status becomes terminal; drives TTL eviction
}

// Job registry errors. Observable; never silently swallowed.
var (
	// ErrIdempotencyConflict: same request_id resubmitted with a different
	// request_digest — the two requests disagree on content (§295). The client
	// must use a fresh request_id for a genuinely new request.
	ErrIdempotencyConflict = errors.New("arb: request_id reused with a different request_digest")
	// ErrJobNotFound: unknown job_id, or a terminal job already evicted after its
	// result TTL. NOT the same as "never ran"; the client must not auto-assume.
	ErrJobNotFound = errors.New("arb: job not found (unknown or result TTL elapsed)")
	// ErrBootMismatch: getJob/cancel presented a boot id other than this
	// registry's boot — the client missed a node restart.
	ErrBootMismatch = errors.New("arb: boot id does not match this node boot")
	// ErrJobNotQueued: Start called on a job that is not Queued (already running,
	// terminal, or cancelled while queued).
	ErrJobNotQueued = errors.New("arb: job is not in Queued state")
	// ErrJobNotRunning: Complete called on a job that is not Running/CancelRequested.
	ErrJobNotRunning = errors.New("arb: job is not running")
	// ErrBadRequestID / ErrBadJobID / ErrBadStamp / ErrBadDigest: malformed id32/
	// hash32 inputs (shape enforced at the boundary, not deep in the state logic).
	ErrBadRequestID = errors.New("arb: request_id must be a 0x + 64 lowercase hex id32")
	ErrBadJobID     = errors.New("arb: job_id must be a 0x + 64 lowercase hex id32")
	ErrBadStamp     = errors.New("arb: stamp must be a 0x + 64 lowercase hex hash32")
	ErrBadDigest    = errors.New("arb: request_digest must be a 0x + 64 lowercase hex hash32")
)

// JobRegistry is the per-boot job state machine. All mutation and observation is
// serialized under mu; the lock guards only in-memory state swaps — the adapter
// must never hold it across EVM execution, disk reads, or socket writes (§97).
type JobRegistry struct {
	mu        sync.Mutex
	boot      string
	clock     Clock
	newID     func() string
	resultTTL time.Duration
	entries   map[string]*jobEntry // key = job_id
	byRequest map[string]string    // request_id -> job_id (idempotency index)
}

// NewJobRegistry builds a registry bound to one node boot. clock is monotonic and
// injectable; newID produces opaque unique id32 job ids (the adapter supplies a
// crypto-random generator, tests supply a counter). resultTTL is how long a
// terminal job's result is retained before eviction (§417 JobResultTTLMillis).
func NewJobRegistry(boot string, clock Clock, newID func() string, resultTTL time.Duration) (*JobRegistry, error) {
	if !isID32(boot) {
		return nil, ErrBadBoot
	}
	return &JobRegistry{
		boot:      boot,
		clock:     clock,
		newID:     newID,
		resultTTL: resultTTL,
		entries:   make(map[string]*jobEntry),
		byRequest: make(map[string]string),
	}, nil
}

// Boot returns the node boot id this registry is scoped to.
func (r *JobRegistry) Boot() string { return r.boot }

// Submit registers a simulate request and returns its server-generated job_id.
//
// Idempotency (§295): the key is request_id. A resubmit of the same request_id
// with the same request_digest returns the existing job_id and its current status
// (no new worker, existing=true). A resubmit with a DIFFERENT digest is an
// ErrIdempotencyConflict — the two requests disagree on content. A brand-new
// request_id mints a fresh Queued job.
//
// stamp and digest are the env/config stamp (hash32) and the request digest
// (hash32) computed by the adapter from the full request; they are echoed back
// via getJob and (digest) used for the idempotency comparison.
func (r *JobRegistry) Submit(requestID, stamp, digest string) (jobID string, status JobStatus, existing bool, err error) {
	if !isID32(requestID) {
		return "", 0, false, ErrBadRequestID
	}
	if !isHash32(stamp) {
		return "", 0, false, ErrBadStamp
	}
	if !isHash32(digest) {
		return "", 0, false, ErrBadDigest
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.evictExpiredLocked()

	if jid, ok := r.byRequest[requestID]; ok {
		e := r.entries[jid]
		// e is guaranteed present while byRequest points at it (eviction removes
		// both together).
		if e.digest != digest {
			return "", 0, false, ErrIdempotencyConflict
		}
		return e.jobID, e.status, true, nil
	}

	jid := r.newID()
	r.entries[jid] = &jobEntry{
		jobID:     jid,
		requestID: requestID,
		stamp:     stamp,
		digest:    digest,
		status:    JobQueued,
	}
	r.byRequest[requestID] = jid
	return jid, JobQueued, false, nil
}

// Start transitions a Queued job to Running when a worker claims it. It fails if
// the job is not Queued (e.g. it was cancelled while queued, or already started),
// so a worker never runs a job that has moved on.
func (r *JobRegistry) Start(jobID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[jobID]
	if !ok {
		return ErrJobNotFound
	}
	if e.status != JobQueued {
		return ErrJobNotQueued
	}
	e.status = JobRunning
	return nil
}

// Complete is the worker's terminal report after it has actually exited and
// released its resources. The registry decides the outcome serially:
//   - if cancel arrived first (status CancelRequested), the result is discarded
//     and the job becomes Cancelled with no success/fail evidence (§401);
//   - otherwise the job becomes Succeeded or Failed per `success`, freezing the
//     outcome for retrieval until the result TTL elapses.
//
// It fails if the job is not Running/CancelRequested (a worker must not report a
// job it does not hold, and must not report twice).
func (r *JobRegistry) Complete(jobID string, success bool, outcome *JobOutcome) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[jobID]
	if !ok {
		return ErrJobNotFound
	}
	if e.status != JobRunning && e.status != JobCancelRequested {
		return ErrJobNotRunning
	}
	if e.status == JobCancelRequested {
		// Cancel won the race: no success evidence, resources now released.
		e.status = JobCancelled
		e.outcome = nil
	} else if success {
		e.status = JobSucceeded
		e.outcome = outcome
	} else {
		e.status = JobFailed
		e.outcome = outcome
	}
	e.terminalAt = r.clock()
	return nil
}

// RequestCancel handles arb_cancelJob(job_id, boot). It returns whether a cancel
// was newly requested (cancel_requested). It does NOT promise the worker exited.
//   - Queued: no worker holds it, so it goes straight to Cancelled (terminal).
//   - Running: flips to CancelRequested; the worker will observe the flag (the
//     adapter wires evm.Cancel) and eventually Complete -> Cancelled.
//   - CancelRequested: idempotent, still cancel_requested=true.
//   - terminal: no-op, cancel_requested=false (already finished, §401 既有终态).
//
// boot is validated against this registry's boot; a stale boot is rejected.
func (r *JobRegistry) RequestCancel(jobID, boot string) (cancelRequested bool, err error) {
	if boot != r.boot {
		return false, ErrBootMismatch
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.evictExpiredLocked()
	e, ok := r.entries[jobID]
	if !ok {
		return false, ErrJobNotFound
	}
	switch e.status {
	case JobQueued:
		e.status = JobCancelled
		e.terminalAt = r.clock()
		return true, nil
	case JobRunning:
		e.status = JobCancelRequested
		return true, nil
	case JobCancelRequested:
		return true, nil
	default: // terminal
		return false, nil
	}
}

// Reject transitions a Queued job directly to Failed with an error code, without
// ever running a worker. It exists for admission control: when the bounded job
// queue is full, the pool rejects the freshly-submitted job honestly as Failed
// (not Cancelled — the client did not cancel; the node refused capacity, §383).
// A Failed capacity job is retriable with a fresh request_id. Fails if the job is
// not Queued (a running/terminal job is never capacity-rejected).
func (r *JobRegistry) Reject(jobID, errCode string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[jobID]
	if !ok {
		return ErrJobNotFound
	}
	if e.status != JobQueued {
		return ErrJobNotQueued
	}
	e.status = JobFailed
	e.outcome = &JobOutcome{Kind: "", Complete: false, ErrCode: errCode}
	e.terminalAt = r.clock()
	return nil
}

// Get handles arb_getJob(job_id, boot). It validates boot, evicts any expired
// terminal jobs first, and returns the wire-facing view (CancelRequested is
// reported as Running via WireStatus at the boundary). An unknown or evicted job
// is ErrJobNotFound.
func (r *JobRegistry) Get(jobID, boot string) (JobView, error) {
	if boot != r.boot {
		return JobView{}, ErrBootMismatch
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.evictExpiredLocked()
	e, ok := r.entries[jobID]
	if !ok {
		return JobView{}, ErrJobNotFound
	}
	kind := ""
	if e.outcome != nil {
		kind = e.outcome.Kind
	}
	return JobView{
		JobID:   e.jobID,
		Status:  e.status,
		Stamp:   e.stamp,
		Digest:  e.digest,
		Kind:    kind,
		Outcome: e.outcome,
	}, nil
}

// Sweep evicts all terminal jobs whose result TTL has elapsed. The adapter calls
// it periodically; Submit/Get/RequestCancel also evict lazily so a caller never
// observes a stale result past its TTL. Returns the number evicted.
func (r *JobRegistry) Sweep() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.evictExpiredLocked()
}

// Len reports the number of tracked jobs (tests/observability).
func (r *JobRegistry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.entries)
}

// evictExpiredLocked removes terminal jobs past resultTTL. Caller holds mu. Only
// terminal jobs are eligible — a Queued/Running/CancelRequested job is never
// evicted by the result TTL (its liveness is bounded by the worker's wall budget,
// not the result-retention clock).
func (r *JobRegistry) evictExpiredLocked() int {
	if r.resultTTL <= 0 {
		return 0
	}
	now := r.clock()
	n := 0
	for jid, e := range r.entries {
		if e.status.IsTerminal() && now.Sub(e.terminalAt) > r.resultTTL {
			delete(r.entries, jid)
			delete(r.byRequest, e.requestID)
			n++
		}
	}
	return n
}
