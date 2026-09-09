// Crypto-random id32 generation, shared by every arb component that needs an
// opaque unique id: the per-boot node id (arb_subscribe/getJob boot scoping), job
// ids (JobRegistry.newID), and state handle ids (HandleStore.newID). The FORMAT is
// the wire id32 — 0x + 64 lowercase hex chars — so the value round-trips through
// isID32/isHash32 without a second definition of "what an id looks like".
//
// This lives in package arb and is node-agnostic: the randomness source is
// crypto/rand (never math/rand — ids must be unguessable so a client cannot forge
// a boot id to read another session's jobs, §7.1). The generation is pure enough
// to unit-test its shape and distinctness; the eth/ adapter simply calls it once
// at boot for the node id and passes RandomID32 as the id generator to the
// registry and handle store.

package arb

import (
	"crypto/rand"
	"encoding/hex"
)

// RandomID32 returns a fresh cryptographically-random id32: "0x" + 64 lowercase
// hex chars (32 bytes). It panics only if the OS entropy source fails, which for
// crypto/rand.Read is a fatal, unrecoverable condition (the node cannot safely
// mint unguessable ids) — callers at boot treat that as a startup failure.
func RandomID32() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand.Read never returns a partial read without an error, and an
		// error here means the entropy source is unavailable — there is no safe
		// fallback to a weaker source, so fail loudly.
		panic("arb: crypto/rand entropy source failed: " + err.Error())
	}
	return "0x" + hex.EncodeToString(b[:])
}

// NewBootID mints the node's per-boot id. It is a distinct name (not just
// RandomID32) so the call site reads as intent — this id is stamped into every
// feed frame and job so a client can detect a node restart and refuse to continue
// a stale stream or read a job across the boot boundary.
func NewBootID() string { return RandomID32() }
