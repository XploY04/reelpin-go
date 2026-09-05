package auth

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/lestrrat-go/httprc/v3"
	"github.com/lestrrat-go/jwx/v3/jwa"
	"github.com/lestrrat-go/jwx/v3/jwk"
	"github.com/lestrrat-go/jwx/v3/jws"
	"github.com/lestrrat-go/jwx/v3/jwt"
)

const (
	jwksFetchTimeout = 5 * time.Second
	jwksMaxCacheAge  = 10 * time.Minute
	clockSkew        = 30 * time.Second
	requiredRole     = "authenticated"
)

// Verifier validates Supabase ES256 access tokens locally, using the project's
// published JWKS. It never calls Supabase on the request path.
type Verifier struct {
	cache    *jwk.Cache
	jwksURL  string
	issuer   string
	audience string
}

func NewVerifier(ctx context.Context, supabaseURL, audience string) (*Verifier, error) {
	base := strings.TrimSuffix(strings.TrimSpace(supabaseURL), "/")
	if base == "" {
		return nil, fmt.Errorf("supabase url is required")
	}

	cache, err := jwk.NewCache(ctx, httprc.NewClient())
	if err != nil {
		return nil, fmt.Errorf("creating jwks cache: %w", err)
	}

	jwksURL := base + "/auth/v1/.well-known/jwks.json"
	fetchCtx, cancel := context.WithTimeout(ctx, jwksFetchTimeout)
	defer cancel()
	if err := cache.Register(fetchCtx, jwksURL, jwk.WithMaxInterval(jwksMaxCacheAge)); err != nil {
		_ = cache.Shutdown(context.Background())
		return nil, fmt.Errorf("fetching jwks: %w", err)
	}

	return &Verifier{
		cache:    cache,
		jwksURL:  jwksURL,
		issuer:   base + "/auth/v1",
		audience: audience,
	}, nil
}

// Shutdown stops the background JWKS refresh.
func (v *Verifier) Shutdown(ctx context.Context) error {
	return v.cache.Shutdown(ctx)
}

func (v *Verifier) Authenticate(ctx context.Context, raw string) (string, error) {
	keyID, err := es256KeyID(raw)
	if err != nil {
		return "", err
	}

	set, err := v.cache.Lookup(ctx, v.jwksURL)
	if err != nil {
		return "", fmt.Errorf("%w: jwks unavailable: %v", ErrUnauthenticated, err)
	}
	if _, found := set.LookupKeyID(keyID); !found {
		// A rotated key is the one case worth a live fetch.
		refreshed, err := v.cache.Refresh(ctx, v.jwksURL)
		if err != nil {
			return "", fmt.Errorf("%w: jwks refresh failed: %v", ErrUnauthenticated, err)
		}
		set = refreshed
	}

	token, err := jwt.Parse([]byte(raw),
		jwt.WithKeySet(set),
		jwt.WithValidate(true),
		jwt.WithIssuer(v.issuer),
		jwt.WithAudience(v.audience),
		jwt.WithAcceptableSkew(clockSkew),
	)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrUnauthenticated, err)
	}

	var role string
	if err := token.Get("role", &role); err != nil || role != requiredRole {
		return "", fmt.Errorf("%w: role is not %s", ErrUnauthenticated, requiredRole)
	}

	subject, ok := token.Subject()
	if !ok {
		return "", fmt.Errorf("%w: token has no subject", ErrUnauthenticated)
	}
	if _, err := uuid.Parse(subject); err != nil {
		return "", fmt.Errorf("%w: subject is not a uuid", ErrUnauthenticated)
	}
	return subject, nil
}

// es256KeyID reads the protected header, rejecting anything but a single
// ES256 signature so an unexpected algorithm never reaches verification.
func es256KeyID(raw string) (string, error) {
	message, err := jws.Parse([]byte(raw))
	if err != nil {
		return "", fmt.Errorf("%w: malformed token", ErrUnauthenticated)
	}
	signatures := message.Signatures()
	if len(signatures) != 1 {
		return "", fmt.Errorf("%w: expected exactly one signature", ErrUnauthenticated)
	}

	headers := signatures[0].ProtectedHeaders()
	algorithm, ok := headers.Algorithm()
	if !ok || algorithm != jwa.ES256() {
		return "", fmt.Errorf("%w: algorithm is not ES256", ErrUnauthenticated)
	}
	keyID, ok := headers.KeyID()
	if !ok || keyID == "" {
		return "", fmt.Errorf("%w: token has no kid", ErrUnauthenticated)
	}
	return keyID, nil
}
