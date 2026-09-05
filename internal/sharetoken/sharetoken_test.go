package sharetoken

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

// Tokens already on devices were hashed by the Python service, which does
// hashlib.sha256(raw.strip().encode("utf-8")).hexdigest(). Matching it exactly
// is what keeps existing installations working through the migration.
func TestHashMatchesThePythonImplementation(t *testing.T) {
	const raw = "device-token"
	expected := sha256.Sum256([]byte(raw))

	got := Hash(raw)
	if got != hex.EncodeToString(expected[:]) {
		t.Fatalf("Hash = %q, want the plain sha-256 hex digest", got)
	}
	if len(got) != 64 {
		t.Fatalf("Hash = %q, want 64 hex characters", got)
	}
	if got != Hash("  "+raw+"\n") {
		t.Fatal("surrounding whitespace changed the hash, so a copied token would stop working")
	}
}
