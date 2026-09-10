// Pool view-call ABI encode/decode core (NODE-02.2, design §9.3/§9.5). Node-agnostic
// and pure-byte: it builds calldata for the read-only pool accessors and decodes
// their return words, failing honestly on short/malformed returns rather than
// fabricating a zero (design §381). The real EVM Call execution against a fixed
// parent state lives in the parent package adapter (eth/arb_pool_reader.go), which
// feeds the returned bytes here.
//
// No CGO: this file computes nothing that needs the node; selectors are the
// well-known 4-byte function selectors (keccak256(signature)[:4]), verified against
// a pure-Go keccak in the test (pool_abi_test.go) so they are not trusted from
// memory. Amounts are returned as big.Int (never float); the caller maps them onto
// the Rust wire (decimal strings / BigUint).
package arb

import (
	"errors"
	"math/big"
)

// Well-known function selectors, keccak256(sig)[:4]. Verified in tests.
var (
	// getReserves() -> (uint112 reserve0, uint112 reserve1, uint32 blockTimestampLast)
	SelGetReserves = [4]byte{0x09, 0x02, 0xf1, 0xac}
	// balanceOf(address) -> uint256
	SelBalanceOf = [4]byte{0x70, 0xa0, 0x82, 0x31}
	// slot0() -> (uint160 sqrtPriceX96, int24 tick, uint16, uint16, uint16, uint8, bool)
	SelSlot0 = [4]byte{0x38, 0x50, 0xc7, 0xbd}
	// liquidity() -> uint128
	SelLiquidity = [4]byte{0x1a, 0x68, 0x65, 0x02}
	// token0() -> address
	SelToken0 = [4]byte{0x0d, 0xfe, 0x16, 0x81}
	// token1() -> address
	SelToken1 = [4]byte{0xd2, 0x12, 0x20, 0xa7}
)

// Errors surfaced by decoding. A short or malformed return is a hard error — the
// caller must fail the read, never treat a missing word as zero (design §381).
var (
	ErrReturnTooShort  = errors.New("arb: view-call return shorter than expected")
	ErrValueOutOfRange = errors.New("arb: decoded value out of declared range")
)

const wordLen = 32

// CallGetReserves / CallSlot0 / CallLiquidity / CallToken0 / CallToken1 build
// zero-argument calldata (just the selector).
func CallGetReserves() []byte { return SelGetReserves[:] }
func CallSlot0() []byte       { return SelSlot0[:] }
func CallLiquidity() []byte   { return SelLiquidity[:] }
func CallToken0() []byte      { return SelToken0[:] }
func CallToken1() []byte      { return SelToken1[:] }

// CallBalanceOf builds balanceOf(address) calldata: selector + 32-byte left-padded
// address. addr is the 20 raw address bytes.
func CallBalanceOf(addr [20]byte) []byte {
	out := make([]byte, 4+wordLen)
	copy(out[0:4], SelBalanceOf[:])
	// address right-aligned in the 32-byte word: bytes [16:36) of out hold it.
	copy(out[4+wordLen-20:], addr[:])
	return out
}

// V2Reserves is the decoded getReserves() result.
type V2Reserves struct {
	Reserve0         *big.Int
	Reserve1         *big.Int
	BlockTimestampLast uint32
}

// DecodeGetReserves decodes 3 ABI words. reserve0/reserve1 are uint112 (validated
// < 2^112); blockTimestampLast is uint32. A return shorter than 3 words fails.
func DecodeGetReserves(ret []byte) (*V2Reserves, error) {
	if len(ret) < 3*wordLen {
		return nil, ErrReturnTooShort
	}
	r0 := new(big.Int).SetBytes(ret[0:wordLen])
	r1 := new(big.Int).SetBytes(ret[wordLen : 2*wordLen])
	// uint112 range check: high 144 bits must be zero.
	if r0.BitLen() > 112 || r1.BitLen() > 112 {
		return nil, ErrValueOutOfRange
	}
	tsWord := ret[2*wordLen : 3*wordLen]
	ts := new(big.Int).SetBytes(tsWord)
	if ts.BitLen() > 32 {
		return nil, ErrValueOutOfRange
	}
	return &V2Reserves{Reserve0: r0, Reserve1: r1, BlockTimestampLast: uint32(ts.Uint64())}, nil
}

// DecodeUint256 decodes a single uint256 word (balanceOf).
func DecodeUint256(ret []byte) (*big.Int, error) {
	if len(ret) < wordLen {
		return nil, ErrReturnTooShort
	}
	return new(big.Int).SetBytes(ret[0:wordLen]), nil
}

// DecodeUint128 decodes a single word validated < 2^128 (liquidity()).
func DecodeUint128(ret []byte) (*big.Int, error) {
	if len(ret) < wordLen {
		return nil, ErrReturnTooShort
	}
	v := new(big.Int).SetBytes(ret[0:wordLen])
	if v.BitLen() > 128 {
		return nil, ErrValueOutOfRange
	}
	return v, nil
}

// V3Slot0 is the decoded slot0() head (only the fields we consume).
type V3Slot0 struct {
	SqrtPriceX96 *big.Int // uint160, validated < 2^160
	Tick         int32    // int24, sign-extended
}

// DecodeSlot0 decodes the first two ABI words of slot0(): sqrtPriceX96 (uint160)
// and tick (int24). Remaining words (observationIndex, cardinality, feeProtocol,
// unlocked) are not consumed here. A return shorter than 2 words fails.
func DecodeSlot0(ret []byte) (*V3Slot0, error) {
	if len(ret) < 2*wordLen {
		return nil, ErrReturnTooShort
	}
	sqrt := new(big.Int).SetBytes(ret[0:wordLen])
	if sqrt.BitLen() > 160 {
		return nil, ErrValueOutOfRange
	}
	tick, err := decodeInt24(ret[wordLen : 2*wordLen])
	if err != nil {
		return nil, err
	}
	return &V3Slot0{SqrtPriceX96: sqrt, Tick: tick}, nil
}

// DecodeAddress decodes a 32-byte word into a 20-byte address (token0/token1),
// checking the high 12 bytes are zero (a well-formed ABI address word).
func DecodeAddress(ret []byte) ([20]byte, error) {
	var out [20]byte
	if len(ret) < wordLen {
		return out, ErrReturnTooShort
	}
	for i := 0; i < wordLen-20; i++ {
		if ret[i] != 0 {
			return out, ErrValueOutOfRange
		}
	}
	copy(out[:], ret[wordLen-20:wordLen])
	return out, nil
}

// decodeInt24 interprets a 32-byte ABI word as a two's-complement int24. The value
// occupies the low 3 bytes with sign extension across the high bytes; we validate
// the ABI sign extension is consistent and return the signed value.
func decodeInt24(word []byte) (int32, error) {
	// The signed value is encoded as a 256-bit two's-complement, so all high bytes
	// equal 0x00 (non-negative) or 0xff (negative) down to the sign byte.
	neg := word[wordLen-3]&0x80 != 0
	var fill byte
	if neg {
		fill = 0xff
	}
	for i := 0; i < wordLen-3; i++ {
		if word[i] != fill {
			return 0, ErrValueOutOfRange
		}
	}
	v := int32(word[wordLen-3])<<16 | int32(word[wordLen-2])<<8 | int32(word[wordLen-1])
	if neg {
		v -= 1 << 24 // sign-extend 24-bit to int32
	}
	return v, nil
}
