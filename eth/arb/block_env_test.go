package arb

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func validEnv() BlockEnv {
	return BlockEnv{
		Number:           "43000000",
		ParentHash:       hh(0xab),
		TimestampMs:      "1700000000500",
		TimestampSeconds: "1700000000",
		Author:           "0x" + strings.Repeat("cd", 20),
		GasLimit:         "140000000",
		Difficulty:       "2",
		MixDigest:        hh(0x01), // ms remainder lives here on BSC; shape-only check
		ExtraData:        "0xd883",
		ForkRulesDigest:  hh(0xfe),
	}
}

func TestValidBlockEnvPasses(t *testing.T) {
	if err := ValidateBlockEnv(validEnv()); err != nil {
		t.Fatalf("valid env rejected: %v", err)
	}
}

func TestValidBlockEnvWithOptionalsPasses(t *testing.T) {
	e := validEnv()
	bf := "1000000000"
	bg := "0"
	eb := "0"
	pbr := hh(0x07)
	e.BaseFee, e.BlobGasUsed, e.ExcessBlobGas, e.ParentBeaconRoot = &bf, &bg, &eb, &pbr
	if err := ValidateBlockEnv(e); err != nil {
		t.Fatalf("valid env with optionals rejected: %v", err)
	}
}

func TestBadFieldsRejected(t *testing.T) {
	cases := map[string]func(*BlockEnv){
		"number non-decimal":      func(e *BlockEnv) { e.Number = "0x1f" },
		"number leading zero":     func(e *BlockEnv) { e.Number = "007" },
		"gas_limit sign":          func(e *BlockEnv) { e.GasLimit = "+5" },
		"parent_hash short":       func(e *BlockEnv) { e.ParentHash = "0xabcd" },
		"mix_digest uppercase":    func(e *BlockEnv) { e.MixDigest = "0x" + strings.Repeat("A", 64) },
		"author short":            func(e *BlockEnv) { e.Author = "0x1234" },
		"extra_data odd":          func(e *BlockEnv) { e.ExtraData = "0xabc" },
		"fork_digest bad":         func(e *BlockEnv) { e.ForkRulesDigest = "nope" },
		"timestamp_ms non-uint":   func(e *BlockEnv) { e.TimestampMs = "abc" },
	}
	for name, mut := range cases {
		e := validEnv()
		mut(&e)
		err := ValidateBlockEnv(e)
		if !errors.Is(err, ErrBlockEnvBadField) {
			t.Fatalf("%s: want ErrBlockEnvBadField, got %v", name, err)
		}
	}
}

func TestZeroAuthorRejected(t *testing.T) {
	e := validEnv()
	e.Author = zeroAddress
	if err := ValidateBlockEnv(e); err != ErrZeroAuthor {
		t.Fatalf("want ErrZeroAuthor, got %v", err)
	}
}

func TestTimestampConsistency(t *testing.T) {
	e := validEnv()
	// ms/1000 = 1700000000 but set seconds to a different value
	e.TimestampSeconds = "1700000001"
	if err := ValidateBlockEnv(e); err != ErrTimestampMismatch {
		t.Fatalf("want ErrTimestampMismatch, got %v", err)
	}
	// exact multiple also fine (ms remainder 0)
	e2 := validEnv()
	e2.TimestampMs = "1700000000000"
	e2.TimestampSeconds = "1700000000"
	if err := ValidateBlockEnv(e2); err != nil {
		t.Fatalf("exact-second env rejected: %v", err)
	}
}

func TestZeroNumberAndGasLimitRejected(t *testing.T) {
	e := validEnv()
	e.Number = "0"
	if err := ValidateBlockEnv(e); err != ErrZeroNumber {
		t.Fatalf("want ErrZeroNumber, got %v", err)
	}
	e2 := validEnv()
	e2.GasLimit = "0"
	if err := ValidateBlockEnv(e2); err != ErrZeroGasLimit {
		t.Fatalf("want ErrZeroGasLimit, got %v", err)
	}
}

func TestBadOptionalRejected(t *testing.T) {
	e := validEnv()
	bad := "0x1f"
	e.BaseFee = &bad
	if !errors.Is(ValidateBlockEnv(e), ErrBlockEnvBadField) {
		t.Fatal("malformed base_fee must be rejected")
	}
	e2 := validEnv()
	badRoot := "short"
	e2.ParentBeaconRoot = &badRoot
	if !errors.Is(ValidateBlockEnv(e2), ErrBlockEnvBadField) {
		t.Fatal("malformed parent_beacon_root must be rejected")
	}
}

// Absent optionals must be OMITTED on the wire, never serialized as zero (§7.1).
func TestOptionalsOmittedNotZeroed(t *testing.T) {
	out, err := json.Marshal(validEnv())
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, f := range []string{"base_fee", "blob_gas_used", "excess_blob_gas", "parent_beacon_root"} {
		if strings.Contains(s, f) {
			t.Fatalf("absent optional %q must be omitted, got: %s", f, s)
		}
	}
	// required decimal string present as string
	if !strings.Contains(s, `"number":"43000000"`) {
		t.Fatalf("number must be decimal string: %s", s)
	}
}
