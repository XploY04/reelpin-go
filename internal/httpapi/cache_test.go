package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/XploY04/reelpin-go/internal/lifecycle"
)

// fakeCache records whom it invalidated and how much of the response had been
// written at that moment.
type fakeCache struct {
	rec          *httptest.ResponseRecorder
	users        []string
	bodyWhenDone []int
	err          error
}

func (f *fakeCache) InvalidateUser(_ context.Context, userID string) error {
	f.users = append(f.users, userID)
	if f.rec != nil {
		f.bodyWhenDone = append(f.bodyWhenDone, f.rec.Body.Len())
	}
	return f.err
}

func serveCached(deps Deps, cache *fakeCache, method, target, body string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	rec := httptest.NewRecorder()
	cache.rec = rec
	deps.Cache = cache
	routes(deps).ServeHTTP(rec, req)
	return rec
}

var bearerHeader = map[string]string{"Authorization": "Bearer good.token"}

func TestASuccessfulWriteDropsTheCallersCacheBeforeAnswering(t *testing.T) {
	cache := &fakeCache{}
	rec := serveCached(collectionDeps(newFakeCollections()), cache, "POST", "/api/v2/collections",
		`{"name":"Trips"}`, bearerHeader)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if len(cache.users) != 1 || cache.users[0] != testUserID {
		t.Fatalf("invalidated %v, want exactly the token subject", cache.users)
	}
	// After the body is out, a client reading again could beat the bump and
	// be served the library from before its own write.
	if cache.bodyWhenDone[0] != 0 {
		t.Errorf("invalidated after %d body bytes were written, want before any", cache.bodyWhenDone[0])
	}
}

func TestAReadNeverDropsTheCache(t *testing.T) {
	cache := &fakeCache{}
	rec := serveCached(testDeps(&fakePinger{}), cache, "GET", "/api/v2/reels", "", bearerHeader)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if len(cache.users) != 0 {
		t.Errorf("a GET invalidated %v", cache.users)
	}
}

func TestAFailedWriteLeavesTheCacheAlone(t *testing.T) {
	cache := &fakeCache{}
	deps := lifecycleDeps(&fakeLifecycle{deleteErr: lifecycle.ErrNotFound})
	rec := serveCached(deps, cache, "DELETE", "/api/v2/reels/"+testReelID, "", bearerHeader)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if len(cache.users) != 0 {
		t.Errorf("a refused write invalidated %v", cache.users)
	}
}

func TestANativeShareDropsTheSharersCache(t *testing.T) {
	cache := &fakeCache{}
	rec := serveCached(submitDeps(&fakeSubmitter{}), cache, "POST", "/api/v2/native-shares/reels",
		`{"raw_payload_text":"look at this https://www.instagram.com/reel/C8abc123/"}`,
		map[string]string{"X-Share-Token": "device.token", "Idempotency-Key": validKey})

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (%s)", rec.Code, rec.Body.String())
	}
	if len(cache.users) != 1 || cache.users[0] != testUserID {
		t.Errorf("invalidated %v, want the token's user, so the job list shows the new job", cache.users)
	}
}

func TestACacheThatFailsDoesNotFailTheWrite(t *testing.T) {
	cache := &fakeCache{err: errFake}
	rec := serveCached(lifecycleDeps(&fakeLifecycle{}), cache, "DELETE", "/api/v2/reels/"+testReelID, "", bearerHeader)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204: the delete committed", rec.Code)
	}
}
