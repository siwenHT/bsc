// Execution outcome classification core (NODE-03, design §8.2/§277/§333/§349). This
// is the node-agnostic, honesty-critical state machine that maps the raw signals of
// a single transaction execution onto the FROZEN wire status enum. It imports
// nothing from the parent eth package (no CGO); the real ApplyTransactionWithEVM
// execution lives in the parent-package adapter, which feeds its signals here.
//
// Wire status enum (design §318, authoritative):
//
//	success | invalid | reverted | cancelled | stale | failed
//
// The single most important rule (design §277): cancellation and backend read
// errors are marked as infrastructure failure / cancellation FIRST, BEFORE the
// receipt is even consulted. A cancelled or errored run's receipt is post-interrupt
// garbage; treating its Status as a real revert, or its absence of error as
// success, would fabricate a result. §333 reinforces: never return a fake canonical
// receipt, never fill an unexecuted tx's fields with zero. So this classifier also
// reports whether the receipt-derived fields (gas/logs) are TRUSTWORTHY; when they
// are not, the adapter must omit them, not zero-fill them.
//
// core error (nonce/balance/fee) vs receipt-failed (revert/OOG) are separate (§277):
// the former is `invalid` (the tx could not even be applied — protocol/validity);
// the latter is `reverted` (the tx executed and entered-block-eligible but failed,
// which policy rejects). We never collapse them into a single err==nil check.
package arb

// ExecStatus is the frozen wire status for a single executed tx.
type ExecStatus uint8

const (
	// StatusSuccess: applied with a successful receipt. Receipt fields trusted.
	StatusSuccess ExecStatus = iota
	// StatusInvalid: a core/validity error prevented application (nonce, balance,
	// fee, intrinsic gas, unsupported/invalid type). The tx never produced a real
	// receipt; receipt fields are NOT trusted.
	StatusInvalid
	// StatusReverted: applied but the receipt status is failed (contract revert or
	// out-of-gas at execution). Enters-block-eligible but policy-rejected. Receipt
	// gas is trusted; revert data is bounded by the adapter.
	StatusReverted
	// StatusCancelled: the run was cancelled by a wall-clock deadline or explicit
	// control cancel. Any receipt is post-interrupt; fields NOT trusted.
	StatusCancelled
	// StatusStale: the underlying state root went stale (flattened/pruned) mid-run;
	// the caller must re-acquire. Fields NOT trusted.
	StatusStale
	// StatusFailed: infrastructure failure — backend read error, or a resource cap
	// (read budget) exhausted. NOT a real revert/success. Fields NOT trusted.
	StatusFailed
)

func (s ExecStatus) String() string {
	switch s {
	case StatusSuccess:
		return "success"
	case StatusInvalid:
		return "invalid"
	case StatusReverted:
		return "reverted"
	case StatusCancelled:
		return "cancelled"
	case StatusStale:
		return "stale"
	case StatusFailed:
		return "failed"
	default:
		return "unknown"
	}
}

// ExecSignals is the set of raw signals the adapter collects after (attempting) a
// single ApplyTransactionWithEVM. The adapter classifies its Go errors into these
// booleans using errors.Is and explicit mapping (design §349: never parse English
// error strings). All fields are independent observations; Classify imposes the
// honest priority among them.
type ExecSignals struct {
	// Budget/cancel signals (checked FIRST). The adapter derives these from the
	// ReadBudget latch and evm.Cancelled().
	DeadlineExceeded  bool // wall-clock budget elapsed -> cancelled
	ControlCancelled  bool // explicit control cancel -> cancelled
	ReadBudgetExhausted bool // max_state_reads cap hit -> failed (resource)

	// Infrastructure/state signals.
	BackendReadError bool // state.Error() != nil for a non-stale reason -> failed
	StateStale       bool // root went stale (e.g. errSnapshotStale) -> stale

	// Application signals (only meaningful if none of the above fired).
	CoreError bool // ApplyMessage/ApplyTransactionWithEVM returned a core error
	//             (nonce/balance/fee/intrinsic/type) -> invalid
	Applied      bool // the tx was applied and a receipt was produced
	ReceiptFailed bool // receipt.Status == failed (revert/OOG) -> reverted
}

// Classification is the result of Classify.
type Classification struct {
	Status ExecStatus
	// ReceiptTrusted reports whether receipt-derived fields (gas_used, logs) reflect
	// a real execution and may be reported. When false the adapter MUST omit those
	// fields (design §333: never zero-fill an unexecuted tx). Only Success and
	// Reverted carry trusted receipt fields.
	ReceiptTrusted bool
}

// Classify applies the honest priority order:
//
//  1. deadline / control cancel   -> cancelled   (interrupt: receipt untrusted)
//  2. read-budget exhausted       -> failed      (resource cap: receipt untrusted)
//  3. backend read error          -> failed      (infra: receipt untrusted)
//  4. state stale                 -> stale       (re-acquire: receipt untrusted)
//  5. core error                  -> invalid     (never applied: receipt untrusted)
//  6. applied + receipt failed    -> reverted    (real revert: receipt trusted)
//  7. applied + receipt ok        -> success     (receipt trusted)
//
// Steps 1-4 win over any receipt because an interrupted/errored run's receipt is
// not a real result (design §277). If none of the terminal signals fired and the tx
// was not applied, we conservatively return `failed` rather than inventing success
// (never a fabricated result).
func Classify(sig ExecSignals) Classification {
	switch {
	case sig.DeadlineExceeded || sig.ControlCancelled:
		return Classification{Status: StatusCancelled, ReceiptTrusted: false}
	case sig.ReadBudgetExhausted:
		return Classification{Status: StatusFailed, ReceiptTrusted: false}
	case sig.BackendReadError:
		return Classification{Status: StatusFailed, ReceiptTrusted: false}
	case sig.StateStale:
		return Classification{Status: StatusStale, ReceiptTrusted: false}
	case sig.CoreError:
		return Classification{Status: StatusInvalid, ReceiptTrusted: false}
	case sig.Applied && sig.ReceiptFailed:
		return Classification{Status: StatusReverted, ReceiptTrusted: true}
	case sig.Applied:
		return Classification{Status: StatusSuccess, ReceiptTrusted: true}
	default:
		// Not applied, no error signalled: we do not know it succeeded, so we do
		// not claim success. Report infrastructure failure honestly.
		return Classification{Status: StatusFailed, ReceiptTrusted: false}
	}
}

// PrefixShouldStop reports whether, given a classification for the target tx, the
// signed-prefix simulation must stop and produce NO post-target handle (design
// §256/§260: "if receipt.Status != successful: stop prefix; no post handle"). Only
// a real success continues to a post-target state.
func PrefixShouldStop(c Classification) bool {
	return c.Status != StatusSuccess
}
