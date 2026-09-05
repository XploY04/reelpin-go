package api

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"testing"
)

func TestSpecIsEmbeddedVerbatim(t *testing.T) {
	onDisk, err := os.ReadFile("openapi.yaml")
	if err != nil {
		t.Fatalf("reading the contract: %v", err)
	}
	if string(Spec) != string(onDisk) {
		t.Fatal("the embedded contract differs from the file; a generated client would not match what ships")
	}
	if len(Spec) == 0 {
		t.Fatal("the contract is empty")
	}
}

// TestSpecDigestIsReportable is what the release workflow relies on: the digest
// of the embedded bytes is the digest of the published artifact.
func TestSpecDigestIsReportable(t *testing.T) {
	sum := sha256.Sum256(Spec)
	digest := hex.EncodeToString(sum[:])
	if len(digest) != 64 {
		t.Fatalf("digest = %q", digest)
	}
	t.Logf("contract sha256: %s", digest)
}
