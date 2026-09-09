// A byte-for-byte port of arb-types' DigestBuilder (crates/arb-types/src/digest.rs,
// frozen convention §2.1) into the node. It exists so the node can recompute a
// request_digest (the idempotency key behind arb_getJob, §225 "请求 hash 由服务端
// 重算") and any env/config stamp under the SAME encoding the Rust engine uses —
// one frozen digest spec across both languages, never a second one.
//
// §2.1 encoding (identical to the Rust module, verified in digest_test.go against
// its known-answer vectors):
//   - Ethereum Keccak-256 (NOT SHA3-256; geth's crypto.Keccak256).
//   - LP(x) = uint32_be(len(x)) || x   (length checked to fit u32).
//   - Digest(tag, fields) = keccak( LP(utf8(tag)) || LP(f0) || ... || LP(fN) ).
//   - integers: unsigned fixed-width big-endian (u16/u32/u64); U256 = 32-byte BE.
//   - optional: absent => 0x00, present => 0x01 || value (LP-wrapped as one field).
//   - never hash JSON text; the caller feeds fields in a fixed order.

package arb

import (
	"encoding/binary"
	"math/big"

	"github.com/ethereum/go-ethereum/crypto"
)

// DigestBuilder is the incremental §2.1 digest constructor. Start with NewDigest,
// append fields in a fixed order, then Finalize. It mirrors the Rust API 1:1.
type DigestBuilder struct {
	buf []byte
}

// lpInto appends LP(x) = uint32_be(len(x)) || x. Go slices cannot exceed the u32
// range in any realistic request, but if len(x) overflows uint32 we panic rather
// than silently truncate the length prefix (which would corrupt the digest) — the
// Rust side returns LengthOverflow for the same guard.
func lpInto(buf []byte, x []byte) []byte {
	n := len(x)
	if uint64(n) > 0xffffffff {
		panic("arb: digest field length exceeds u32")
	}
	var lp [4]byte
	binary.BigEndian.PutUint32(lp[:], uint32(n))
	buf = append(buf, lp[:]...)
	buf = append(buf, x...)
	return buf
}

// NewDigest begins a digest with a domain tag (written LP-wrapped as UTF-8).
func NewDigest(tag string) *DigestBuilder {
	b := &DigestBuilder{buf: make([]byte, 0, 64)}
	b.buf = lpInto(b.buf, []byte(tag))
	return b
}

// FieldBytes appends a raw byte field (LP-wrapped).
func (b *DigestBuilder) FieldBytes(x []byte) *DigestBuilder {
	b.buf = lpInto(b.buf, x)
	return b
}

// FieldU16/U32/U64 append an unsigned fixed-width big-endian integer field.
func (b *DigestBuilder) FieldU16(v uint16) *DigestBuilder {
	var x [2]byte
	binary.BigEndian.PutUint16(x[:], v)
	return b.FieldBytes(x[:])
}

func (b *DigestBuilder) FieldU32(v uint32) *DigestBuilder {
	var x [4]byte
	binary.BigEndian.PutUint32(x[:], v)
	return b.FieldBytes(x[:])
}

func (b *DigestBuilder) FieldU64(v uint64) *DigestBuilder {
	var x [8]byte
	binary.BigEndian.PutUint64(x[:], v)
	return b.FieldBytes(x[:])
}

// FieldU256 appends a value as a fixed 32-byte big-endian field (left zero-padded).
// A nil or negative big.Int is treated as its absolute-value BE bytes right-aligned
// into 32 bytes; callers must only pass non-negative values (amounts are unsigned),
// matching the Rust field_u256 contract. Values wider than 256 bits are truncated
// to the low 32 bytes, same as the Rust debug_assert path in release.
func (b *DigestBuilder) FieldU256(v *big.Int) *DigestBuilder {
	var fixed [32]byte
	if v != nil {
		be := v.Bytes() // big-endian, no sign, minimal length
		if len(be) > 32 {
			be = be[len(be)-32:]
		}
		copy(fixed[32-len(be):], be)
	}
	return b.FieldBytes(fixed[:])
}

// FieldB256 appends a 32-byte hash field.
func (b *DigestBuilder) FieldB256(v [32]byte) *DigestBuilder {
	return b.FieldBytes(v[:])
}

// FieldOptionalBytes appends absent as 0x00, present as 0x01 || value, the whole
// thing LP-wrapped once (identical to Rust field_optional_bytes).
func (b *DigestBuilder) FieldOptionalBytes(v []byte, present bool) *DigestBuilder {
	if !present {
		return b.FieldBytes([]byte{0x00})
	}
	framed := make([]byte, 0, 1+len(v))
	framed = append(framed, 0x01)
	framed = append(framed, v...)
	return b.FieldBytes(framed)
}

// FieldBool appends a boolean as a single byte 0x00/0x01.
func (b *DigestBuilder) FieldBool(v bool) *DigestBuilder {
	if v {
		return b.FieldBytes([]byte{0x01})
	}
	return b.FieldBytes([]byte{0x00})
}

// Finalize returns the Ethereum Keccak-256 digest of the accumulated buffer.
func (b *DigestBuilder) Finalize() [32]byte {
	var out [32]byte
	copy(out[:], crypto.Keccak256(b.buf))
	return out
}
