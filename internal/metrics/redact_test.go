package metrics

import (
	"strings"
	"testing"
)

func TestHashHidesTheValueButStaysStable(t *testing.T) {
	user := "11111111-1111-4111-8111-111111111111"
	hashed := Hash(user)

	if strings.Contains(hashed, user) || len(hashed) != 16 {
		t.Fatalf("hash = %q", hashed)
	}
	if Hash(user) != hashed {
		t.Error("the same value hashed differently twice")
	}
	if Hash("another user") == hashed {
		t.Error("two values collided")
	}
	if Hash("  ") != "" {
		t.Error("an empty value should hash to nothing, not to a constant")
	}
}
