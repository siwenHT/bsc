// Budgeted state-read accounting core (NODE shared infra, design §379-381). This
// is the node-agnostic counter/cap/latch state machine that enforces max_state_reads
// and guarantees that a budget-exceeded read FAILS rather than returning a faked
// zero. It imports nothing from the parent eth package (no CGO). The concrete
// binding wraps vm.StateDB and the typed PoolReader around this core
// (eth/arb_budget.go), charging one logical read per GetState / GetCommittedState /
// GetCode / GetCodeHash / GetCodeSize / GetBalance / GetNonce / GetStorageRoot.
//
// Contract (design §379-381, honesty-critical):
//
//   - Logical reads are counted here; this is NOT disk IO (a cached read still
//     counts one logical read, a cold read still counts one). The design is explicit
//     that logical read count != disk IO count, and that a wrapper cannot abort an
//     already-blocked disk read — so this bounds work admitted, not latency.
//   - When the cap is reached, ChargeRead returns false and latches `exceeded`. The
//     adapter MUST then request EVM cancel and fail the job. It must NEVER hand back
//     a zero/empty value as if the read succeeded (§381 "不要返回伪造零值作为成功
//     读取"). Once latched, every subsequent ChargeRead also returns false.
//   - A wall-clock deadline is layered on top (design §291: server builds the
//     deadline from a local monotonic clock, taking min(client_budget, server_cap);
//     it never trusts a client-sent Instant/absolute deadline). DeadlineExceeded is
//     also a latched terminal condition.
package arb

import (
	"errors"
	"time"
)

// ReadKind labels the logical access, for per-kind observability. Every kind costs
// exactly one unit against max_state_reads; the label does not change the charge.
type ReadKind uint8

const (
	ReadStorage       ReadKind = iota // GetState
	ReadCommitted                     // GetCommittedState
	ReadCode                          // GetCode / GetCodeSize / GetCodeHash
	ReadBalance                       // GetBalance
	ReadNonce                         // GetNonce
	ReadStorageRoot                   // GetStorageRoot
	ReadOther                         // any other counted logical access
)

func (k ReadKind) String() string {
	switch k {
	case ReadStorage:
		return "storage"
	case ReadCommitted:
		return "committed"
	case ReadCode:
		return "code"
	case ReadBalance:
		return "balance"
	case ReadNonce:
		return "nonce"
	case ReadStorageRoot:
		return "storage_root"
	default:
		return "other"
	}
}

// Budget failure reasons. Both are terminal and latched.
var (
	// ErrReadBudgetExceeded: the max_state_reads cap was reached. The job must fail;
	// callers must not fabricate a value.
	ErrReadBudgetExceeded = errors.New("arb: state read budget exceeded")
	// ErrDeadlineExceeded: the wall-clock budget elapsed. Terminal, latched.
	ErrDeadlineExceeded = errors.New("arb: simulation deadline exceeded")
)

// ReadBudget tracks logical reads against a cap and a monotonic deadline. Not safe
// for concurrent use; a single EVM worker owns one budget for the life of a job.
type ReadBudget struct {
	clock    Clock
	maxReads uint64
	deadline time.Time // zero => no wall-clock bound

	reads    uint64
	perKind  [7]uint64
	exceeded bool // latched: read cap hit
	expired  bool // latched: deadline hit
}

// NewReadBudget builds a budget with a logical-read cap and an OPTIONAL wall-clock
// budget. wall <= 0 means no deadline. The deadline is computed from the injected
// clock at construction (local monotonic time), never from a caller-supplied
// absolute instant (design §291).
func NewReadBudget(clock Clock, maxReads uint64, wall time.Duration) *ReadBudget {
	b := &ReadBudget{clock: clock, maxReads: maxReads}
	if wall > 0 {
		b.deadline = clock().Add(wall)
	}
	return b
}

// ChargeRead accounts one logical read of the given kind. It returns nil if the
// read is admitted, or a terminal error if the budget is (or just became)
// exhausted. On any error the caller MUST fail the read — never return a value.
//
// Order of checks: an already-latched terminal state stays terminal; then the
// deadline (so a slow job cannot keep charging reads past its wall budget); then
// the read cap. The charge is only applied when the read is admitted, so the
// counter never overshoots the cap.
func (b *ReadBudget) ChargeRead(kind ReadKind) error {
	if b.expired {
		return ErrDeadlineExceeded
	}
	if b.exceeded {
		return ErrReadBudgetExceeded
	}
	if !b.deadline.IsZero() && !b.clock().Before(b.deadline) {
		b.expired = true
		return ErrDeadlineExceeded
	}
	if b.reads >= b.maxReads {
		b.exceeded = true
		return ErrReadBudgetExceeded
	}
	b.reads++
	if int(kind) < len(b.perKind) {
		b.perKind[kind]++
	}
	return nil
}

// CheckDeadline latches/reports the wall-clock budget without charging a read. The
// adapter's OnOpcode hook can call this at a bounded frequency (design §399) to set
// the cancel flag between reads. Returns ErrDeadlineExceeded once elapsed.
func (b *ReadBudget) CheckDeadline() error {
	if b.expired {
		return ErrDeadlineExceeded
	}
	if !b.deadline.IsZero() && !b.clock().Before(b.deadline) {
		b.expired = true
		return ErrDeadlineExceeded
	}
	return nil
}

// Failed reports whether any terminal budget condition has latched. When true, the
// job result MUST be a rejection; a latched budget can never be "recovered" into a
// successful read (design §381/§399: check budget error after return so an
// interrupt is never mistaken for success).
func (b *ReadBudget) Failed() bool { return b.exceeded || b.expired }

// Err returns the latched terminal error, or nil if the budget is still healthy.
func (b *ReadBudget) Err() error {
	if b.expired {
		return ErrDeadlineExceeded
	}
	if b.exceeded {
		return ErrReadBudgetExceeded
	}
	return nil
}

// Reads returns the total logical reads charged so far.
func (b *ReadBudget) Reads() uint64 { return b.reads }

// ReadsOfKind returns the count for one kind (observability / metering echo).
func (b *ReadBudget) ReadsOfKind(k ReadKind) uint64 {
	if int(k) < len(b.perKind) {
		return b.perKind[k]
	}
	return 0
}
