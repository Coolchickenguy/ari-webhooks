package ids

import (
	"regexp"
	"testing"
)

func TestShipIdShape(t *testing.T) {
	pattern := regexp.MustCompile(`^[a-z0-9]{8}$`)
	seen := map[string]bool{}
	for range 1000 {
		id := ShipId()
		if !pattern.MatchString(id) {
			t.Fatalf("ship id %q does not match ari's 8-char a-z0-9 shape", id)
		}
		seen[id] = true
	}
	if len(seen) < 990 {
		t.Fatalf("suspicious collision rate: %d unique of 1000", len(seen))
	}
}

func TestCuidShape(t *testing.T) {
	id := Cuid()
	if len(id) < 20 || len(id) > 32 {
		t.Fatalf("cuid %q has unexpected length %d", id, len(id))
	}
}
