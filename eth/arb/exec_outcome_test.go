package arb

import "testing"

func TestClassifySuccess(t *testing.T) {
	c := Classify(ExecSignals{Applied: true})
	if c.Status != StatusSuccess || !c.ReceiptTrusted {
		t.Fatalf("want success+trusted, got %s trusted=%v", c.Status, c.ReceiptTrusted)
	}
}

func TestClassifyRevertedTrustsReceipt(t *testing.T) {
	c := Classify(ExecSignals{Applied: true, ReceiptFailed: true})
	if c.Status != StatusReverted {
		t.Fatalf("want reverted, got %s", c.Status)
	}
	if !c.ReceiptTrusted {
		t.Fatal("reverted must trust receipt gas fields")
	}
}

func TestClassifyCoreErrorIsInvalidNotReverted(t *testing.T) {
	// core error (nonce/balance/fee) => invalid, NOT reverted, receipt untrusted.
	c := Classify(ExecSignals{CoreError: true})
	if c.Status != StatusInvalid {
		t.Fatalf("want invalid, got %s", c.Status)
	}
	if c.ReceiptTrusted {
		t.Fatal("invalid must not trust receipt")
	}
}

// The honesty core: an interrupt/infra signal must WIN over a receipt, so a
// post-interrupt receipt is never mistaken for a real revert/success (§277).
func TestCancelWinsOverReceipt(t *testing.T) {
	// Even with Applied+ReceiptFailed set, a deadline makes it cancelled.
	c := Classify(ExecSignals{Applied: true, ReceiptFailed: true, DeadlineExceeded: true})
	if c.Status != StatusCancelled {
		t.Fatalf("deadline must win over receipt, got %s", c.Status)
	}
	if c.ReceiptTrusted {
		t.Fatal("cancelled receipt must not be trusted")
	}
}

func TestControlCancelWinsOverSuccess(t *testing.T) {
	c := Classify(ExecSignals{Applied: true, ControlCancelled: true})
	if c.Status != StatusCancelled || c.ReceiptTrusted {
		t.Fatalf("control cancel must override apparent success, got %s trusted=%v", c.Status, c.ReceiptTrusted)
	}
}

func TestReadBudgetExhaustedIsFailed(t *testing.T) {
	c := Classify(ExecSignals{Applied: true, ReadBudgetExhausted: true})
	if c.Status != StatusFailed || c.ReceiptTrusted {
		t.Fatalf("budget exhaustion must be failed+untrusted, got %s trusted=%v", c.Status, c.ReceiptTrusted)
	}
}

func TestBackendReadErrorIsFailed(t *testing.T) {
	c := Classify(ExecSignals{Applied: true, BackendReadError: true})
	if c.Status != StatusFailed || c.ReceiptTrusted {
		t.Fatalf("backend read error must be failed+untrusted, got %s", c.Status)
	}
}

func TestStateStale(t *testing.T) {
	c := Classify(ExecSignals{Applied: true, ReceiptFailed: true, StateStale: true})
	if c.Status != StatusStale || c.ReceiptTrusted {
		t.Fatalf("stale must win over receipt, got %s", c.Status)
	}
}

// Priority ordering: cancel beats budget beats infra beats stale beats core beats
// receipt. Set ALL and confirm cancelled wins.
func TestFullPriorityOrder(t *testing.T) {
	all := ExecSignals{
		DeadlineExceeded:    true,
		ReadBudgetExhausted: true,
		BackendReadError:    true,
		StateStale:          true,
		CoreError:           true,
		Applied:             true,
		ReceiptFailed:       true,
	}
	if got := Classify(all).Status; got != StatusCancelled {
		t.Fatalf("cancel must have top priority, got %s", got)
	}
	// Remove cancel: budget wins.
	all.DeadlineExceeded = false
	all.ControlCancelled = false
	if got := Classify(all).Status; got != StatusFailed {
		t.Fatalf("budget/infra must be next, got %s", got)
	}
	// Remove budget+infra: stale wins.
	all.ReadBudgetExhausted = false
	all.BackendReadError = false
	if got := Classify(all).Status; got != StatusStale {
		t.Fatalf("stale must be next, got %s", got)
	}
	// Remove stale: core error wins.
	all.StateStale = false
	if got := Classify(all).Status; got != StatusInvalid {
		t.Fatalf("core error must be next, got %s", got)
	}
}

func TestNotAppliedNoErrorIsFailedNotSuccess(t *testing.T) {
	// No signals at all: we must NOT invent success.
	c := Classify(ExecSignals{})
	if c.Status != StatusFailed {
		t.Fatalf("unexecuted with no signal must be failed, got %s", c.Status)
	}
	if c.ReceiptTrusted {
		t.Fatal("must not trust a receipt that never existed")
	}
}

func TestPrefixShouldStop(t *testing.T) {
	if PrefixShouldStop(Classify(ExecSignals{Applied: true})) {
		t.Fatal("success must NOT stop the prefix")
	}
	for _, sig := range []ExecSignals{
		{Applied: true, ReceiptFailed: true}, // reverted
		{CoreError: true},                    // invalid
		{DeadlineExceeded: true},             // cancelled
		{BackendReadError: true},             // failed
		{StateStale: true},                   // stale
	} {
		if !PrefixShouldStop(Classify(sig)) {
			t.Fatalf("non-success %+v must stop the prefix (no post handle)", sig)
		}
	}
}
