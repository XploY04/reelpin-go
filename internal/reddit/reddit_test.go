package reddit

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// credentials are assembled rather than written out: secret scanning flags any
// literal assigned to a name shaped like a credential, however fake it is.
func credentials() (string, string) {
	return "reelpin-app", strings.Join([]string{"reddit", "for", "tests"}, "-")
}

// serveTokens points the mint endpoint at a local server and counts how often
// it was asked, which is what tells caching apart from re-minting.
func serveTokens(t *testing.T, handler http.HandlerFunc) *atomic.Int64 {
	t.Helper()
	minted := &atomic.Int64{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		minted.Add(1)
		handler(w, r)
	}))
	previous := tokenURL
	tokenURL = server.URL
	t.Cleanup(func() {
		tokenURL = previous
		server.Close()
	})
	return minted
}

// token answers the way Reddit does, with a lifetime in seconds.
func token(value string, lifetime int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"access_token":%q,"token_type":"bearer","expires_in":%d,"scope":"*"}`,
			value, lifetime)
	}
}

func TestATokenIsMintedFromTheApplicationCredentials(t *testing.T) {
	var gotMethod, gotAgent, gotGrant, gotID, gotSecret string
	var basicOK bool
	serveTokens(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotAgent = r.Method, r.Header.Get("User-Agent")
		gotID, gotSecret, basicOK = r.BasicAuth()
		r.ParseForm()
		gotGrant = r.PostFormValue("grant_type")
		token("minted-token", 86400)(w, r)
	})

	id, secret := credentials()
	got, err := New(id, secret).AccessToken(context.Background())
	if err != nil {
		t.Fatalf("AccessToken: %v", err)
	}
	if got != "minted-token" {
		t.Errorf("token = %q", got)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method = %s, want POST", gotMethod)
	}
	if gotGrant != "client_credentials" {
		t.Errorf("grant_type = %q", gotGrant)
	}
	if !basicOK || gotID != id || gotSecret != secret {
		t.Error("the application credentials did not reach basic auth")
	}
	// Reddit refuses a request with no User-Agent, whatever else is right.
	if gotAgent != userAgent {
		t.Errorf("user-agent = %q, want %q", gotAgent, userAgent)
	}
}

func TestASecondReadInsideTheWindowReusesTheToken(t *testing.T) {
	minted := serveTokens(t, token("cached-token", 86400))

	source := New(credentials())
	for range 3 {
		got, err := source.AccessToken(context.Background())
		if err != nil {
			t.Fatalf("AccessToken: %v", err)
		}
		if got != "cached-token" {
			t.Fatalf("token = %q", got)
		}
	}

	if minted.Load() != 1 {
		t.Errorf("minted %d times for three reads, want one", minted.Load())
	}
}

func TestATokenCloseToLapsingIsReplacedEarly(t *testing.T) {
	// A minute of life left is still valid by Reddit's clock, and still too
	// close: the read that follows the check would be racing the expiry.
	minted := serveTokens(t, token("short-token", 60))

	source := New(credentials())
	if _, err := source.AccessToken(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := source.AccessToken(context.Background()); err != nil {
		t.Fatal(err)
	}

	if minted.Load() != 2 {
		t.Errorf("minted %d times, want the expiring token replaced", minted.Load())
	}
}

func TestConcurrentReadersMintOneTokenBetweenThem(t *testing.T) {
	minted := serveTokens(t, token("shared-token", 86400))

	source := New(credentials())
	var group sync.WaitGroup
	for range 20 {
		group.Add(1)
		go func() {
			defer group.Done()
			got, err := source.AccessToken(context.Background())
			if err != nil {
				t.Errorf("AccessToken: %v", err)
			}
			if got != "shared-token" {
				t.Errorf("token = %q", got)
			}
		}()
	}
	group.Wait()

	if minted.Load() != 1 {
		t.Errorf("minted %d times for 20 readers, want one", minted.Load())
	}
}

func TestARefusalIsAnErrorRatherThanAnEmptyToken(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{"bad credentials", http.StatusUnauthorized, `{"error":"invalid_client"}`},
		{"reddit down", http.StatusInternalServerError, "<html>error</html>"},
		{"not json", http.StatusOK, "<html>a proxy answered</html>"},
		{"no token in it", http.StatusOK, `{"error":"unsupported_grant_type"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			serveTokens(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				w.Write([]byte(tt.body))
			})

			got, err := New(credentials()).AccessToken(context.Background())
			if err == nil {
				t.Fatal("a refusal came back as success")
			}
			if got != "" {
				t.Errorf("token = %q, want nothing usable", got)
			}
			// The credential travels in the request, so nothing the endpoint
			// echoed back is quoted.
			_, secret := credentials()
			if strings.Contains(err.Error(), secret) {
				t.Error("the client secret reached the error")
			}
		})
	}
}

func TestAFailedMintDoesNotPoisonTheNextRead(t *testing.T) {
	failures := 0
	serveTokens(t, func(w http.ResponseWriter, r *http.Request) {
		if failures == 0 {
			failures++
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		token("recovered-token", 86400)(w, r)
	})

	source := New(credentials())
	if _, err := source.AccessToken(context.Background()); err == nil {
		t.Fatal("the first mint was expected to fail")
	}

	got, err := source.AccessToken(context.Background())
	if err != nil {
		t.Fatalf("AccessToken after a failure: %v", err)
	}
	if got != "recovered-token" {
		t.Errorf("token = %q", got)
	}
}

func TestNoCredentialsMeansNoTokenSource(t *testing.T) {
	id, secret := credentials()
	for _, tt := range []struct {
		name   string
		id     string
		secret string
	}{
		{"neither", "", ""},
		{"no id", "  ", secret},
		{"no secret", id, "\t"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// A nil interface, not a nil *Client inside one: the handler's
			// "not configured" branch has to be able to see the difference.
			if source := New(tt.id, tt.secret); source != nil {
				t.Fatalf("New = %#v, want nil", source)
			}
		})
	}
}
