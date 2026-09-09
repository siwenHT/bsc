package arb

import (
	"encoding/hex"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/crypto"
)

func hexStr(h [32]byte) string { return "0x" + hex.EncodeToString(h[:]) }

// The empty-input Keccak-256 known answer proves this uses Ethereum Keccak, NOT
// SHA3-256 (they differ). Same anchor as the Rust module's keccak_empty_kat.
func TestKeccakEmptyKAT(t *testing.T) {
	got := "0x" + hex.EncodeToString(crypto.Keccak256(nil))
	want := "0xc5d2460186f7233c927e7db2dcc703c0e500b653ca82273b7bfad8045d85a470"
	if got != want {
		t.Fatalf("keccak256(\"\")=%s want %s (must be Ethereum Keccak, not SHA3)", got, want)
	}
}

// Cross-language parity: these two vectors were produced by the Rust arb-types
// DigestBuilder (crates/arb-types/src/digest.rs) and hardcoded here. If the Go
// port drifts one byte from the frozen §2.1 encoding, these fail — which is
// exactly the guard that keeps the node from growing a second digest spec.
func TestDigestCrossLanguageParityVec1(t *testing.T) {
	got := hexStr(NewDigest("arb.req.v1").
		FieldU64(1).
		FieldBytes([]byte("hello")).
		FieldU32(0xdeadbeef).
		FieldBool(true).
		Finalize())
	want := "0x374555ae9796371f5866d271555bad3b25c71b36944cf687e0e8f4e3c3dced60"
	if got != want {
		t.Fatalf("vec1=%s want %s (Go DigestBuilder diverged from Rust arb-types)", got, want)
	}
}

func TestDigestCrossLanguageParityVec2(t *testing.T) {
	got := hexStr(NewDigest("arb.req.v1").
		FieldU256(big.NewInt(1)).
		FieldOptionalBytes(nil, false).
		FieldOptionalBytes([]byte("x"), true).
		Finalize())
	want := "0x1ecf84c6693af1a76b8bb44a55680af230efca44e360a6ca4a712cb5f57b0d17"
	if got != want {
		t.Fatalf("vec2=%s want %s (Go DigestBuilder diverged from Rust arb-types)", got, want)
	}
}

// FieldU256(1) must equal FieldBytes(32-byte BE 1) — same invariant the Rust
// u256_left_padded_fixed_width test locks.
func TestFieldU256LeftPadded(t *testing.T) {
	d1 := hexStr(NewDigest("t").FieldU256(big.NewInt(1)).Finalize())
	var thirtyTwo [32]byte
	thirtyTwo[31] = 1
	d2 := hexStr(NewDigest("t").FieldBytes(thirtyTwo[:]).Finalize())
	if d1 != d2 {
		t.Fatalf("FieldU256(1)=%s != FieldBytes(32BE 1)=%s", d1, d2)
	}
}

// optional absent (0x00) must differ from optional present-but-empty (0x01) —
// the tag byte carries the distinction, same as the Rust test.
func TestOptionalAbsentDiffersFromEmpty(t *testing.T) {
	absent := hexStr(NewDigest("t").FieldOptionalBytes(nil, false).Finalize())
	empty := hexStr(NewDigest("t").FieldOptionalBytes([]byte{}, true).Finalize())
	if absent == empty {
		t.Fatal("optional absent must differ from present-empty (0x00 vs 0x01 tag)")
	}
}

// Field order matters: swapping two fields changes the digest (LP framing makes
// the byte stream order-sensitive).
func TestDigestOrderSensitive(t *testing.T) {
	a := hexStr(NewDigest("t").FieldU32(1).FieldU32(2).Finalize())
	b := hexStr(NewDigest("t").FieldU32(2).FieldU32(1).Finalize())
	if a == b {
		t.Fatal("digest must be order-sensitive")
	}
}
