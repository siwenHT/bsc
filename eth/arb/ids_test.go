package arb

import "testing"

func TestRandomID32ShapeIsWireID32(t *testing.T) {
	id := RandomID32()
	if !isID32(id) {
		t.Fatalf("RandomID32 %q is not a valid wire id32 (0x + 64 lowercase hex)", id)
	}
	if len(id) != 66 {
		t.Fatalf("id length=%d want 66", len(id))
	}
}

func TestRandomID32IsDistinct(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 1024; i++ {
		id := RandomID32()
		if seen[id] {
			t.Fatalf("RandomID32 produced a duplicate within 1024 draws: %q", id)
		}
		seen[id] = true
	}
}

func TestNewBootIDIsID32(t *testing.T) {
	if !isID32(NewBootID()) {
		t.Fatal("NewBootID must be a valid id32")
	}
}
