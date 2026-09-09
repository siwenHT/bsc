package eth

import "testing"

// TestValidPoolReadEnforcesNoGuess proves the §389 no-guess rule on a v2 pool_reads
// entry: the node holds no registry, so a read spec must fully declare how to read.
// A v2 read MUST carry both token addresses; a v3 read must carry none; bad locator or
// unknown kind is rejected. The node never substitutes a guessed kind or token.
func TestValidPoolReadEnforcesNoGuess(t *testing.T) {
	const a = "0x000000000000000000000000000000000000dead"
	const b = "0x000000000000000000000000000000000000beef"
	const c = "0x000000000000000000000000000000000000cafe"

	cases := []struct {
		name string
		pr   PoolReadSpec
		want bool
	}{
		{"v2 with both tokens", PoolReadSpec{Locator: a, Kind: "v2", Token0: b, Token1: c}, true},
		{"v3 without tokens", PoolReadSpec{Locator: a, Kind: "v3"}, true},
		{"v2 missing token1", PoolReadSpec{Locator: a, Kind: "v2", Token0: b}, false},
		{"v2 missing both tokens", PoolReadSpec{Locator: a, Kind: "v2"}, false},
		{"v2 malformed token", PoolReadSpec{Locator: a, Kind: "v2", Token0: "0xnothex", Token1: c}, false},
		{"v3 with stray tokens", PoolReadSpec{Locator: a, Kind: "v3", Token0: b, Token1: c}, false},
		{"bad locator", PoolReadSpec{Locator: "0xshort", Kind: "v3"}, false},
		{"unknown kind", PoolReadSpec{Locator: a, Kind: "v1"}, false},
		{"empty kind", PoolReadSpec{Locator: a, Kind: ""}, false},
	}
	for _, tc := range cases {
		if got := validPoolRead(&tc.pr); got != tc.want {
			t.Errorf("%s: validPoolRead = %v, want %v", tc.name, got, tc.want)
		}
	}
}
