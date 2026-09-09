// Wire-shape validators for the arb RPC boundary. The pure package arb has its own
// isID32/isHash32 (unexported), so the eth-side adapter carries its own copies with
// the identical shape contract (0x + fixed-length lowercase hex). Keeping them here
// avoids exporting the pure ones just for the adapter, and keeps the frozen shapes
// (id32/hash32 = 32 bytes, address = 20 bytes) in one obvious place.

package eth

import "encoding/hex"

// isLowerHexOfLen reports whether s is "0x" + exactly n lowercase hex chars.
func isLowerHexOfLen(s string, n int) bool {
	if len(s) != 2+n || s[0] != '0' || s[1] != 'x' {
		return false
	}
	for i := 2; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// isID32Wire / isHash32Wire: 0x + 64 lowercase hex (32 bytes). Same shape for both.
func isID32Wire(s string) bool   { return isLowerHexOfLen(s, 64) }
func isHash32Wire(s string) bool { return isLowerHexOfLen(s, 64) }

// isAddressWire: 0x + 40 lowercase hex (20 bytes). Lowercase-only so a caller cannot
// smuggle a checksummed mixed-case address past the frozen wire shape.
func isAddressWire(s string) bool { return isLowerHexOfLen(s, 40) }

// hex32Str renders a 32-byte digest as 0x + 64 lowercase hex (wire hash32).
func hex32Str(h [32]byte) string { return "0x" + hex.EncodeToString(h[:]) }
