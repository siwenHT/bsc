package arb

import (
	"bytes"
	"math/big"
	"testing"

	"golang.org/x/crypto/sha3"
)

// selector computes keccak256(sig)[:4] with pure-Go legacy keccak (no CGO), so the
// hardcoded selector constants are verified, not trusted from memory.
func selector(sig string) [4]byte {
	h := sha3.NewLegacyKeccak256()
	h.Write([]byte(sig))
	sum := h.Sum(nil)
	var s [4]byte
	copy(s[:], sum[:4])
	return s
}

func TestSelectorsMatchKeccak(t *testing.T) {
	cases := []struct {
		sig  string
		want [4]byte
	}{
		{"getReserves()", SelGetReserves},
		{"balanceOf(address)", SelBalanceOf},
		{"slot0()", SelSlot0},
		{"liquidity()", SelLiquidity},
		{"token0()", SelToken0},
		{"token1()", SelToken1},
	}
	for _, c := range cases {
		if got := selector(c.sig); got != c.want {
			t.Fatalf("%s: selector %x != keccak %x", c.sig, c.want, got)
		}
	}
}

func TestCallBalanceOfLayout(t *testing.T) {
	var addr [20]byte
	for i := range addr {
		addr[i] = byte(i + 1)
	}
	cd := CallBalanceOf(addr)
	if len(cd) != 36 {
		t.Fatalf("calldata len want 36, got %d", len(cd))
	}
	if !bytes.Equal(cd[0:4], SelBalanceOf[:]) {
		t.Fatal("selector prefix wrong")
	}
	// address must be right-aligned: first 12 bytes of the word are zero.
	for i := 4; i < 4+12; i++ {
		if cd[i] != 0 {
			t.Fatalf("address not left-padded at %d", i)
		}
	}
	if !bytes.Equal(cd[16:36], addr[:]) {
		t.Fatal("address bytes misplaced")
	}
}

// word builds a 32-byte big-endian word from a big.Int.
func word(v *big.Int) []byte {
	b := v.Bytes()
	out := make([]byte, 32)
	copy(out[32-len(b):], b)
	return out
}

func TestDecodeGetReserves(t *testing.T) {
	r0 := big.NewInt(1_000_000)
	r1 := big.NewInt(2_500_000)
	ret := append(append(word(r0), word(r1)...), word(big.NewInt(1700000000))...)
	got, err := DecodeGetReserves(ret)
	if err != nil {
		t.Fatal(err)
	}
	if got.Reserve0.Cmp(r0) != 0 || got.Reserve1.Cmp(r1) != 0 {
		t.Fatalf("reserves wrong: %v %v", got.Reserve0, got.Reserve1)
	}
	if got.BlockTimestampLast != 1700000000 {
		t.Fatalf("ts wrong: %d", got.BlockTimestampLast)
	}
}

func TestDecodeGetReservesShortFails(t *testing.T) {
	// only 2 words -> too short, must error, not fabricate.
	ret := append(word(big.NewInt(1)), word(big.NewInt(2))...)
	if _, err := DecodeGetReserves(ret); err != ErrReturnTooShort {
		t.Fatalf("want too-short error, got %v", err)
	}
}

func TestDecodeGetReservesOutOfRange(t *testing.T) {
	// reserve0 = 2^112 -> exceeds uint112 -> reject.
	big112 := new(big.Int).Lsh(big.NewInt(1), 112)
	ret := append(append(word(big112), word(big.NewInt(1))...), word(big.NewInt(0))...)
	if _, err := DecodeGetReserves(ret); err != ErrValueOutOfRange {
		t.Fatalf("want out-of-range, got %v", err)
	}
}

func TestDecodeUint128Range(t *testing.T) {
	ok := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 128), big.NewInt(1)) // 2^128-1
	if _, err := DecodeUint128(word(ok)); err != nil {
		t.Fatalf("2^128-1 should decode, got %v", err)
	}
	over := new(big.Int).Lsh(big.NewInt(1), 128) // 2^128
	if _, err := DecodeUint128(word(over)); err != ErrValueOutOfRange {
		t.Fatalf("want out-of-range for 2^128, got %v", err)
	}
}

func TestDecodeSlot0(t *testing.T) {
	sqrt := new(big.Int).Lsh(big.NewInt(1), 96) // 2^96, in-range for uint160
	// tick = -100 as int24 two's complement in a 256-bit word.
	tickWord := make([]byte, 32)
	for i := 0; i < 29; i++ {
		tickWord[i] = 0xff
	}
	// -100 = 0xFFFF9C in 24-bit
	tickWord[29] = 0xff
	tickWord[30] = 0xff
	tickWord[31] = 0x9c
	ret := append(word(sqrt), tickWord...)
	got, err := DecodeSlot0(ret)
	if err != nil {
		t.Fatal(err)
	}
	if got.SqrtPriceX96.Cmp(sqrt) != 0 {
		t.Fatalf("sqrt wrong: %v", got.SqrtPriceX96)
	}
	if got.Tick != -100 {
		t.Fatalf("tick want -100, got %d", got.Tick)
	}
}

func TestDecodeSlot0PositiveTick(t *testing.T) {
	sqrt := big.NewInt(12345)
	tickWord := make([]byte, 32)
	// tick = +200 = 0x0000C8
	tickWord[31] = 0xc8
	ret := append(word(sqrt), tickWord...)
	got, err := DecodeSlot0(ret)
	if err != nil {
		t.Fatal(err)
	}
	if got.Tick != 200 {
		t.Fatalf("tick want 200, got %d", got.Tick)
	}
}

func TestDecodeSlot0SqrtOutOfRange(t *testing.T) {
	over := new(big.Int).Lsh(big.NewInt(1), 160) // 2^160 -> exceeds uint160
	tickWord := make([]byte, 32)
	ret := append(word(over), tickWord...)
	if _, err := DecodeSlot0(ret); err != ErrValueOutOfRange {
		t.Fatalf("want out-of-range sqrt, got %v", err)
	}
}

func TestDecodeInt24BadSignExtension(t *testing.T) {
	// high bytes inconsistent with sign -> malformed, must error.
	w := make([]byte, 32)
	w[0] = 0x01 // stray high byte, but low 3 bytes look positive
	w[31] = 0x05
	if _, err := decodeInt24(w); err != ErrValueOutOfRange {
		t.Fatalf("want out-of-range for bad sign extension, got %v", err)
	}
}

func TestDecodeAddress(t *testing.T) {
	var addr [20]byte
	for i := range addr {
		addr[i] = byte(0xa0 + i)
	}
	w := make([]byte, 32)
	copy(w[12:], addr[:])
	got, err := DecodeAddress(w)
	if err != nil {
		t.Fatal(err)
	}
	if got != addr {
		t.Fatalf("address decode mismatch")
	}
	// dirty high bytes -> reject.
	w[5] = 0x01
	if _, err := DecodeAddress(w); err != ErrValueOutOfRange {
		t.Fatalf("want out-of-range for dirty high bytes, got %v", err)
	}
}

func TestDecodeUint256Short(t *testing.T) {
	if _, err := DecodeUint256(make([]byte, 31)); err != ErrReturnTooShort {
		t.Fatalf("want too-short, got %v", err)
	}
}
