package ratelimit

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"time"
)

// The trusted web boundary is the only hop that sees a browser's real address.
// Once Next.js is the caller, every visitor reaches this service from one
// socket, so limiting on the socket peer puts the whole web in one bucket.
//
// The answer is not to start believing X-Forwarded-For, which anyone can send.
// It is for the hop that really knows to name an opaque bucket and sign it, and
// for this side to fall back to the socket peer whenever the signature does not
// check out. An unsigned or forged header is therefore worth nothing: it cannot
// raise a limit, and it cannot claim somebody else's bucket.
//
// The web signs with the same scheme in src/lib/security/ip-bucket.ts. Both
// sides have to agree on the version string, the two message layouts and the
// skew, so all three live here as constants rather than as literals.

const (
	// IPBucketHeader carries the claim. Absent is not an error: it is what dev
	// and preview send, and it means "use the socket peer".
	IPBucketHeader = "X-ReelPin-IP-Bucket"

	ipBucketVersion = "v1"

	// MaxIPBucketSkew is how far the two clocks may drift. It exists so a value
	// scraped from a log cannot be replayed as somebody's bucket forever, not
	// to defend against a browser, which cannot sign at all.
	MaxIPBucketSkew = 5 * time.Minute
)

// VerifyIPBucket returns the bucket a well-signed header claims, or "" for
// anything else. Every failure is the same answer on purpose: the caller falls
// back to the socket peer and never learns which check failed.
func VerifyIPBucket(value, secret string, now time.Time) string {
	if value == "" || secret == "" {
		return ""
	}

	parts := strings.Split(value, ".")
	if len(parts) != 4 {
		return ""
	}
	version, issuedAt, bucket, signature := parts[0], parts[1], parts[2], parts[3]

	if version != ipBucketVersion || len(bucket) != 32 || !isLowerHex(bucket) {
		return ""
	}
	seconds, err := strconv.ParseInt(issuedAt, 10, 64)
	if err != nil {
		return ""
	}

	given, err := hex.DecodeString(signature)
	if err != nil {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(version + "." + issuedAt + "." + bucket))
	if !hmac.Equal(given, mac.Sum(nil)) {
		return ""
	}

	skew := now.Sub(time.Unix(seconds, 0))
	if skew < 0 {
		skew = -skew
	}
	if skew > MaxIPBucketSkew {
		return ""
	}
	return bucket
}

func isLowerHex(s string) bool {
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
