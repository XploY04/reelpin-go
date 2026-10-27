package ratelimit

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"testing"
	"time"
)

const testSecret = "shared-with-the-web-boundary"

// sign builds a header the way src/lib/security/ip-bucket.ts does, so a change
// to either side's message layout fails here rather than in production.
func sign(t *testing.T, secret, bucket string, issuedAt time.Time) string {
	t.Helper()
	seconds := strconv.FormatInt(issuedAt.Unix(), 10)
	payload := ipBucketVersion + "." + seconds + "." + bucket
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(payload))
	return payload + "." + hex.EncodeToString(mac.Sum(nil))
}

// bucketFor mirrors the web's derivation: the first 16 bytes of an HMAC over
// the address, hex encoded. This side never needs it, but a test that invents
// its own bucket shape would not notice the web changing.
func bucketFor(secret, ip string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ipBucketVersion + ":ip:" + ip))
	return hex.EncodeToString(mac.Sum(nil)[:16])
}

func TestAWellSignedBucketIsAccepted(t *testing.T) {
	now := time.Unix(1_780_000_000, 0)
	want := bucketFor(testSecret, "203.0.113.7")

	got := VerifyIPBucket(sign(t, testSecret, want, now), testSecret, now)
	if got != want {
		t.Fatalf("bucket = %q, want %q", got, want)
	}
}

func TestOnlyAValidSignatureIsBelieved(t *testing.T) {
	now := time.Unix(1_780_000_000, 0)
	bucket := bucketFor(testSecret, "203.0.113.7")
	valid := sign(t, testSecret, bucket, now)

	// Every one of these must answer "", so the handler falls back to the
	// socket peer. None may answer a bucket, and none may answer differently
	// from the others: a caller that could tell them apart could probe.
	cases := map[string]string{
		"no header":         "",
		"not four parts":    ipBucketVersion + "." + bucket,
		"wrong version":     "v2" + valid[2:],
		"bucket edited":     sign(t, testSecret, "ffffffffffffffffffffffffffffffff", now)[:len(valid)-64] + valid[len(valid)-64:],
		"signed elsewhere":  sign(t, "a-different-secret", bucket, now),
		"signature dropped": ipBucketVersion + "." + strconv.FormatInt(now.Unix(), 10) + "." + bucket + ".",
		"signature garbage": valid[:len(valid)-64] + "not-hex-at-all-not-hex-at-all-not-hex-at-all-not-hex-at-all-nnnn",
		"bucket not hex":    sign(t, testSecret, "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz", now),
		"bucket too short":  sign(t, testSecret, "abcdef", now),
		"timestamp words":   sign(t, testSecret, bucket, now)[:3] + "later" + valid[3:],
	}

	for name, header := range cases {
		t.Run(name, func(t *testing.T) {
			if got := VerifyIPBucket(header, testSecret, now); got != "" {
				t.Fatalf("accepted %s: %q", name, got)
			}
		})
	}
}

// A bucket lifted from a log must stop working, or it is a permanent identity
// for whoever found it.
func TestABucketExpiresOutsideTheSkewWindow(t *testing.T) {
	issued := time.Unix(1_780_000_000, 0)
	header := sign(t, testSecret, bucketFor(testSecret, "203.0.113.7"), issued)

	for name, at := range map[string]time.Time{
		"just inside, late":  issued.Add(MaxIPBucketSkew - time.Second),
		"just inside, early": issued.Add(-MaxIPBucketSkew + time.Second),
	} {
		if VerifyIPBucket(header, testSecret, at) == "" {
			t.Fatalf("%s was rejected", name)
		}
	}

	for name, at := range map[string]time.Time{
		"too late":  issued.Add(MaxIPBucketSkew + time.Second),
		"too early": issued.Add(-MaxIPBucketSkew - time.Second),
		"next week": issued.Add(7 * 24 * time.Hour),
	} {
		if got := VerifyIPBucket(header, testSecret, at); got != "" {
			t.Fatalf("%s was accepted: %q", name, got)
		}
	}
}

// Without a configured secret this service has no way to check anything, so it
// must believe nothing rather than believe everything.
func TestNoSecretMeansNoClaimIsBelieved(t *testing.T) {
	now := time.Unix(1_780_000_000, 0)
	header := sign(t, testSecret, bucketFor(testSecret, "203.0.113.7"), now)

	if got := VerifyIPBucket(header, "", now); got != "" {
		t.Fatalf("believed a claim with no secret: %q", got)
	}
}

// Two addresses must not share a bucket, or the limit is one bucket again.
func TestDifferentAddressesGetDifferentBuckets(t *testing.T) {
	if bucketFor(testSecret, "203.0.113.7") == bucketFor(testSecret, "203.0.113.8") {
		t.Fatal("two addresses produced one bucket")
	}
}
