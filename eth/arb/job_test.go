package arb

import (
	"strings"
	"testing"
	"time"
)

// Test helpers. NOTE: package arb already defines counterIDs (handle_store_test.go,
// returns "h1" etc — NOT a valid id32) and fixedClock (state_capability_test.go,
// with .Clock() and a writable .now). We reuse fixedClock and add a job-specific
// id generator that produces real id32 job ids (jobs assert isID32 on job_id).

// hex32 builds a valid id32/hash32 (0x + 64 lowercase hex) from a single hex
// nibble repeated, so every test constant is unambiguously the right length.
func hex32(nibble byte) string { return "0x" + strings.Repeat(string(nibble), 64) }

// jobCounterIDs yields distinct valid id32 job ids deterministically.
func jobCounterIDs() func() string {
	n := 0
	return func() string {
		n++
		digits := "123456789abcdef"
		return "0x" + strings.Repeat("0", 63) + string(digits[(n-1)%len(digits)])
	}
}

var (
	boot   = hex32('a')
	stamp1 = hex32('b')
	dig1   = hex32('c')
	dig2   = hex32('d')
)

func newTestRegistry(t *testing.T) (*JobRegistry, *fixedClock) {
	t.Helper()
	clk := &fixedClock{now: time.Unix(1_700_000_000, 0)}
	r, err := NewJobRegistry(boot, clk.Clock(), jobCounterIDs(), 10*time.Second)
	if err != nil {
		t.Fatalf("NewJobRegistry: %v", err)
	}
	return r, clk
}

func TestSubmitMintsQueuedJob(t *testing.T) {
	r, _ := newTestRegistry(t)
	jid, status, existing, err := r.Submit(dig1, stamp1, dig1)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if existing {
		t.Fatal("fresh submit must not be existing")
	}
	if status != JobQueued {
		t.Fatalf("status=%v want Queued", status)
	}
	if !isID32(jid) {
		t.Fatalf("job_id %q not id32", jid)
	}
}

func TestIdempotentResubmitSameDigestReturnsExisting(t *testing.T) {
	r, _ := newTestRegistry(t)
	jid1, _, _, _ := r.Submit(dig1, stamp1, dig1)
	jid2, status, existing, err := r.Submit(dig1, stamp1, dig1)
	if err != nil {
		t.Fatalf("resubmit: %v", err)
	}
	if !existing {
		t.Fatal("same request_id+digest must be existing")
	}
	if jid1 != jid2 {
		t.Fatalf("idempotent resubmit gave a new job_id %q != %q", jid2, jid1)
	}
	if status != JobQueued {
		t.Fatalf("status=%v want Queued", status)
	}
	if r.Len() != 1 {
		t.Fatalf("idempotent resubmit must not create a second job; Len=%d", r.Len())
	}
}

func TestIdempotencyConflictOnDifferentDigest(t *testing.T) {
	r, _ := newTestRegistry(t)
	r.Submit(dig1, stamp1, dig1)
	_, _, _, err := r.Submit(dig1, stamp1, dig2) // same request_id, different digest
	if err != ErrIdempotencyConflict {
		t.Fatalf("err=%v want ErrIdempotencyConflict", err)
	}
}

func TestStartRejectsNonQueued(t *testing.T) {
	r, _ := newTestRegistry(t)
	jid, _, _, _ := r.Submit(dig1, stamp1, dig1)
	if err := r.Start(jid); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	if err := r.Start(jid); err != ErrJobNotQueued {
		t.Fatalf("second Start err=%v want ErrJobNotQueued", err)
	}
}

func TestCompleteSuccessFreezesOutcome(t *testing.T) {
	r, _ := newTestRegistry(t)
	jid, _, _, _ := r.Submit(dig1, stamp1, dig1)
	r.Start(jid)
	out := &JobOutcome{Kind: "target", Complete: true, PostHandle: dig2, Metering: Metering{GasUsed: 21000}}
	if err := r.Complete(jid, true, out); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	v, err := r.Get(jid, boot)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if v.Status != JobSucceeded {
		t.Fatalf("status=%v want Succeeded", v.Status)
	}
	if v.Outcome == nil || v.Outcome.Metering.GasUsed != 21000 || v.Outcome.PostHandle != dig2 {
		t.Fatalf("outcome not frozen faithfully: %+v", v.Outcome)
	}
	if v.Kind != "target" {
		t.Fatalf("kind=%q want target", v.Kind)
	}
}

func TestCompleteRejectsNonRunning(t *testing.T) {
	r, _ := newTestRegistry(t)
	jid, _, _, _ := r.Submit(dig1, stamp1, dig1)
	// still Queued
	if err := r.Complete(jid, true, &JobOutcome{}); err != ErrJobNotRunning {
		t.Fatalf("Complete on Queued err=%v want ErrJobNotRunning", err)
	}
}

// The core race: cancel arrives while Running -> externally still Running; the
// worker's later Complete must NOT produce success evidence, it becomes Cancelled.
func TestCancelWhileRunningDiscardsSuccessEvidence(t *testing.T) {
	r, _ := newTestRegistry(t)
	jid, _, _, _ := r.Submit(dig1, stamp1, dig1)
	r.Start(jid)
	req, err := r.RequestCancel(jid, boot)
	if err != nil || !req {
		t.Fatalf("RequestCancel returned (%v,%v)", req, err)
	}
	// Externally the job is still Running until the worker exits (§397).
	v, _ := r.Get(jid, boot)
	if v.Status.WireStatus() != "Running" {
		t.Fatalf("wire status=%q want Running while CancelRequested", v.Status.WireStatus())
	}
	// Worker exits and tries to report success — cancel won, evidence discarded.
	if err := r.Complete(jid, true, &JobOutcome{Kind: "target", Complete: true}); err != nil {
		t.Fatalf("Complete after cancel: %v", err)
	}
	v, _ = r.Get(jid, boot)
	if v.Status != JobCancelled {
		t.Fatalf("status=%v want Cancelled", v.Status)
	}
	if v.Outcome != nil {
		t.Fatalf("cancelled job must carry no success evidence, got %+v", v.Outcome)
	}
}

func TestCancelQueuedGoesStraightToCancelled(t *testing.T) {
	r, _ := newTestRegistry(t)
	jid, _, _, _ := r.Submit(dig1, stamp1, dig1)
	req, err := r.RequestCancel(jid, boot)
	if err != nil || !req {
		t.Fatalf("RequestCancel (%v,%v)", req, err)
	}
	v, _ := r.Get(jid, boot)
	if v.Status != JobCancelled {
		t.Fatalf("status=%v want Cancelled (queued cancel is terminal)", v.Status)
	}
	// A worker must never start a job cancelled while queued.
	if err := r.Start(jid); err != ErrJobNotQueued {
		t.Fatalf("Start on queued-cancelled err=%v want ErrJobNotQueued", err)
	}
}

func TestCancelTerminalIsNoop(t *testing.T) {
	r, _ := newTestRegistry(t)
	jid, _, _, _ := r.Submit(dig1, stamp1, dig1)
	r.Start(jid)
	r.Complete(jid, true, &JobOutcome{Kind: "target"})
	req, err := r.RequestCancel(jid, boot)
	if err != nil {
		t.Fatalf("cancel terminal: %v", err)
	}
	if req {
		t.Fatal("cancel of an already-terminal job must return cancel_requested=false")
	}
	v, _ := r.Get(jid, boot)
	if v.Status != JobSucceeded {
		t.Fatalf("status changed to %v; terminal must be preserved", v.Status)
	}
}

func TestCancelIsIdempotent(t *testing.T) {
	r, _ := newTestRegistry(t)
	jid, _, _, _ := r.Submit(dig1, stamp1, dig1)
	r.Start(jid)
	r.RequestCancel(jid, boot)
	req, err := r.RequestCancel(jid, boot) // second cancel while CancelRequested
	if err != nil || !req {
		t.Fatalf("idempotent cancel (%v,%v) want (true,nil)", req, err)
	}
}

func TestBootMismatchRejectedOnGetAndCancel(t *testing.T) {
	r, _ := newTestRegistry(t)
	jid, _, _, _ := r.Submit(dig1, stamp1, dig1)
	wrongBoot := hex32('f')
	if _, err := r.Get(jid, wrongBoot); err != ErrBootMismatch {
		t.Fatalf("Get wrong boot err=%v want ErrBootMismatch", err)
	}
	if _, err := r.RequestCancel(jid, wrongBoot); err != ErrBootMismatch {
		t.Fatalf("Cancel wrong boot err=%v want ErrBootMismatch", err)
	}
}

func TestResultTTLEvictionYieldsJobNotFound(t *testing.T) {
	r, clk := newTestRegistry(t)
	jid, _, _, _ := r.Submit(dig1, stamp1, dig1)
	r.Start(jid)
	r.Complete(jid, true, &JobOutcome{Kind: "target"})
	// Before TTL: retrievable.
	if _, err := r.Get(jid, boot); err != nil {
		t.Fatalf("Get before TTL: %v", err)
	}
	clk.now = clk.now.Add(10*time.Second + time.Millisecond) // past resultTTL
	if _, err := r.Get(jid, boot); err != ErrJobNotFound {
		t.Fatalf("Get after TTL err=%v want ErrJobNotFound (evicted, not rerun)", err)
	}
	if r.Len() != 0 {
		t.Fatalf("evicted job still tracked; Len=%d", r.Len())
	}
}

func TestNonTerminalJobNotEvictedByResultTTL(t *testing.T) {
	r, clk := newTestRegistry(t)
	jid, _, _, _ := r.Submit(dig1, stamp1, dig1)
	r.Start(jid) // Running, not terminal
	clk.now = clk.now.Add(time.Hour)
	if n := r.Sweep(); n != 0 {
		t.Fatalf("Sweep evicted %d non-terminal jobs; must be 0", n)
	}
	if _, err := r.Get(jid, boot); err != nil {
		t.Fatalf("running job wrongly evicted: %v", err)
	}
}

func TestEvictionFreesRequestIDForReuse(t *testing.T) {
	r, clk := newTestRegistry(t)
	jid1, _, _, _ := r.Submit(dig1, stamp1, dig1)
	r.Start(jid1)
	r.Complete(jid1, true, &JobOutcome{Kind: "target"})
	clk.now = clk.now.Add(11 * time.Second)
	r.Sweep()
	// After eviction, the same request_id is a brand-new job (not a conflict, not
	// a resurrected result).
	jid2, _, existing, err := r.Submit(dig1, stamp1, dig1)
	if err != nil {
		t.Fatalf("resubmit after eviction: %v", err)
	}
	if existing {
		t.Fatal("resubmit after eviction must be a fresh job, not existing")
	}
	if jid1 == jid2 {
		t.Fatalf("evicted+resubmitted job reused old job_id %q", jid2)
	}
}

func TestMalformedInputsRejected(t *testing.T) {
	r, _ := newTestRegistry(t)
	bad := "0xNOTHEX"
	if _, _, _, err := r.Submit(bad, stamp1, dig1); err != ErrBadRequestID {
		t.Fatalf("bad request_id err=%v", err)
	}
	if _, _, _, err := r.Submit(dig1, bad, dig1); err != ErrBadStamp {
		t.Fatalf("bad stamp err=%v", err)
	}
	if _, _, _, err := r.Submit(dig1, stamp1, bad); err != ErrBadDigest {
		t.Fatalf("bad digest err=%v", err)
	}
}

func TestRejectQueuedIsFailedNotCancelled(t *testing.T) {
	r, _ := newTestRegistry(t)
	jid, _, _, _ := r.Submit(dig1, stamp1, dig1)
	if err := r.Reject(jid, "queue_full"); err != nil {
		t.Fatalf("Reject: %v", err)
	}
	v, _ := r.Get(jid, boot)
	// Capacity rejection is Failed (retriable with a fresh request), NOT Cancelled
	// (the client never cancelled). §383.
	if v.Status != JobFailed {
		t.Fatalf("status=%v want Failed", v.Status)
	}
	if v.Outcome == nil || v.Outcome.ErrCode != "queue_full" {
		t.Fatalf("reject must record err code, got %+v", v.Outcome)
	}
	// A running/terminal job is never capacity-rejected.
	if err := r.Reject(jid, "queue_full"); err != ErrJobNotQueued {
		t.Fatalf("re-Reject err=%v want ErrJobNotQueued", err)
	}
}

func TestNewJobRegistryRejectsBadBoot(t *testing.T) {
	clk := &fixedClock{now: time.Now()}
	if _, err := NewJobRegistry("0xzz", clk.Clock(), jobCounterIDs(), time.Second); err != ErrBadBoot {
		t.Fatalf("bad boot err=%v want ErrBadBoot", err)
	}
}
