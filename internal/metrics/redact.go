package metrics

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Hash turns an identifier into something safe to log: stable enough to
// correlate two lines, useless to anyone reading the log. Never log a user id,
// a URL or a token directly.
func Hash(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(value))
	// 16 hex characters is 64 bits: enough that two users will not collide in
	// one log, short enough to read.
	return hex.EncodeToString(sum[:8])
}
