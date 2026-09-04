package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lestrrat-go/jwx/v3/jwa"
	"github.com/lestrrat-go/jwx/v3/jwk"
	"github.com/lestrrat-go/jwx/v3/jws"
	"github.com/lestrrat-go/jwx/v3/jwt"
)

const (
	testSubject  = "22222222-2222-4222-8222-222222222222"
	testAudience = "authenticated"
)

type signingKey struct {
	keyID   string
	private *ecdsa.PrivateKey
	public  jwk.Key
}

func newSigningKey(t *testing.T, keyID string) signingKey {
	t.Helper()
	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	public, err := jwk.Import(private.Public())
	if err != nil {
		t.Fatalf("importing key: %v", err)
	}
	if err := public.Set(jwk.KeyIDKey, keyID); err != nil {
		t.Fatalf("setting kid: %v", err)
	}
	if err := public.Set(jwk.AlgorithmKey, jwa.ES256()); err != nil {
		t.Fatalf("setting alg: %v", err)
	}
	return signingKey{keyID: keyID, private: private, public: public}
}

// jwksServer publishes a key set and counts how often it is fetched.
type jwksServer struct {
	*httptest.Server
	mu      sync.Mutex
	keys    []signingKey
	fetches atomic.Int64
}

func newJWKSServer(t *testing.T, keys ...signingKey) *jwksServer {
	t.Helper()
	server := &jwksServer{keys: keys}
	server.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/auth/v1/.well-known/jwks.json" {
			http.NotFound(w, r)
			return
		}
		server.fetches.Add(1)

		set := jwk.NewSet()
		server.mu.Lock()
		for _, key := range server.keys {
			if err := set.AddKey(key.public); err != nil {
				t.Errorf("adding key: %v", err)
			}
		}
		server.mu.Unlock()

		// No caching headers, so the cache honours its own interval instead.
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(set); err != nil {
			t.Errorf("encoding jwks: %v", err)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func (s *jwksServer) rotate(keys ...signingKey) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys = keys
}

type claims struct {
	issuer    string
	audience  string
	subject   string
	role      string
	expiresAt time.Time
	notBefore *time.Time
}

func (c claims) sign(t *testing.T, key signingKey, algorithm jwa.SignatureAlgorithm, raw any) string {
	t.Helper()
	builder := jwt.NewBuilder().
		Issuer(c.issuer).
		Audience([]string{c.audience}).
		Subject(c.subject).
		IssuedAt(time.Now()).
		Expiration(c.expiresAt)
	if c.role != "" {
		builder = builder.Claim("role", c.role)
	}
	if c.notBefore != nil {
		builder = builder.NotBefore(*c.notBefore)
	}

	token, err := builder.Build()
	if err != nil {
		t.Fatalf("building token: %v", err)
	}
	signed, err := jwt.Sign(token, jwt.WithKey(algorithm, raw, jws.WithProtectedHeaders(protectedKeyID(t, key.keyID))))
	if err != nil {
		t.Fatalf("signing token: %v", err)
	}
	return string(signed)
}

func protectedKeyID(t *testing.T, keyID string) jws.Headers {
	t.Helper()
	headers := jws.NewHeaders()
	if err := headers.Set(jws.KeyIDKey, keyID); err != nil {
		t.Fatalf("setting protected kid: %v", err)
	}
	return headers
}

func newVerifier(t *testing.T, server *jwksServer) *Verifier {
	t.Helper()
	verifier, err := NewVerifier(context.Background(), server.URL, testAudience)
	if err != nil {
		t.Fatalf("creating verifier: %v", err)
	}
	t.Cleanup(func() { _ = verifier.Shutdown(context.Background()) })
	return verifier
}

func validClaims(server *jwksServer) claims {
	return claims{
		issuer:    server.URL + "/auth/v1",
		audience:  testAudience,
		subject:   testSubject,
		role:      "authenticated",
		expiresAt: time.Now().Add(time.Hour),
	}
}

func TestAuthenticateValidToken(t *testing.T) {
	key := newSigningKey(t, "key-1")
	server := newJWKSServer(t, key)
	verifier := newVerifier(t, server)

	token := validClaims(server).sign(t, key, jwa.ES256(), key.private)
	subject, err := verifier.Authenticate(context.Background(), token)
	if err != nil {
		t.Fatalf("Authenticate() error: %v", err)
	}
	if subject != testSubject {
		t.Errorf("subject = %q, want %q", subject, testSubject)
	}
}

func TestAuthenticateRejects(t *testing.T) {
	key := newSigningKey(t, "key-1")
	server := newJWKSServer(t, key)
	verifier := newVerifier(t, server)
	future := time.Now().Add(time.Hour)

	tests := []struct {
		name   string
		mutate func(c *claims)
	}{
		{name: "wrong issuer", mutate: func(c *claims) { c.issuer = "https://evil.example.com/auth/v1" }},
		{name: "wrong audience", mutate: func(c *claims) { c.audience = "anon" }},
		{name: "expired", mutate: func(c *claims) { c.expiresAt = time.Now().Add(-time.Hour) }},
		{name: "not yet valid", mutate: func(c *claims) { c.notBefore = &future }},
		{name: "wrong role", mutate: func(c *claims) { c.role = "service_role" }},
		{name: "no role", mutate: func(c *claims) { c.role = "" }},
		{name: "subject is not a uuid", mutate: func(c *claims) { c.subject = "user-42" }},
		{name: "no subject", mutate: func(c *claims) { c.subject = "" }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := validClaims(server)
			tt.mutate(&c)
			token := c.sign(t, key, jwa.ES256(), key.private)

			if _, err := verifier.Authenticate(context.Background(), token); err == nil {
				t.Fatal("Authenticate() accepted a token it should have rejected")
			}
		})
	}
}

func TestAuthenticateRejectsMalformedAndForeignSignatures(t *testing.T) {
	key := newSigningKey(t, "key-1")
	server := newJWKSServer(t, key)
	verifier := newVerifier(t, server)

	t.Run("garbage", func(t *testing.T) {
		if _, err := verifier.Authenticate(context.Background(), "not-a-token"); err == nil {
			t.Fatal("Authenticate() accepted garbage")
		}
	})

	t.Run("another key", func(t *testing.T) {
		other := newSigningKey(t, "key-1") // same kid, different key
		token := validClaims(server).sign(t, other, jwa.ES256(), other.private)
		if _, err := verifier.Authenticate(context.Background(), token); err == nil {
			t.Fatal("Authenticate() accepted a foreign signature")
		}
	})

	t.Run("rs256 is refused", func(t *testing.T) {
		rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatalf("generating rsa key: %v", err)
		}
		token := validClaims(server).sign(t, key, jwa.RS256(), rsaKey)
		if _, err := verifier.Authenticate(context.Background(), token); err == nil {
			t.Fatal("Authenticate() accepted RS256")
		}
	})
}

func TestJWKSIsCached(t *testing.T) {
	key := newSigningKey(t, "key-1")
	server := newJWKSServer(t, key)
	verifier := newVerifier(t, server)

	after := server.fetches.Load()
	if after == 0 {
		t.Fatal("the key set was not fetched during startup")
	}

	for i := 0; i < 5; i++ {
		token := validClaims(server).sign(t, key, jwa.ES256(), key.private)
		if _, err := verifier.Authenticate(context.Background(), token); err != nil {
			t.Fatalf("Authenticate() error: %v", err)
		}
	}
	if server.fetches.Load() != after {
		t.Errorf("fetches = %d, want %d: verification must not refetch", server.fetches.Load(), after)
	}
}

func TestUnknownKeyIDRefreshesOnce(t *testing.T) {
	first := newSigningKey(t, "key-1")
	server := newJWKSServer(t, first)
	verifier := newVerifier(t, server)

	rotated := newSigningKey(t, "key-2")
	server.rotate(rotated)

	before := server.fetches.Load()
	token := validClaims(server).sign(t, rotated, jwa.ES256(), rotated.private)
	subject, err := verifier.Authenticate(context.Background(), token)
	if err != nil {
		t.Fatalf("Authenticate() after rotation: %v", err)
	}
	if subject != testSubject {
		t.Errorf("subject = %q, want %q", subject, testSubject)
	}
	if got := server.fetches.Load() - before; got != 1 {
		t.Errorf("fetches during rotation = %d, want 1", got)
	}
}
