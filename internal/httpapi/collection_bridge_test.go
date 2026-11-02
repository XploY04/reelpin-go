package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/XploY04/reelpin-go/internal/collections"
	"github.com/XploY04/reelpin-go/internal/reels"
)

const testBridgeKey = "collection-bridge-test-credential-32-bytes"

type fakeCollectionBridge struct {
	*fakeCollections
	calls  int
	joined bool
}

func (f *fakeCollectionBridge) AcceptBridgeInvite(ctx context.Context, token, userID string) (collections.Collection, bool, error) {
	collection, err := f.AcceptInvite(ctx, token, userID)
	return collection, f.joined, err
}

func (f *fakeCollectionBridge) EnsureBridgeUser(context.Context, string) error { f.calls++; return nil }
func (f *fakeCollectionBridge) CreateImported(ctx context.Context, u, n, d string, ids []string, _ []collections.Snapshot) (collections.Collection, int, error) {
	f.calls++
	return f.Create(ctx, u, n, d, ids)
}
func (f *fakeCollectionBridge) AddImported(ctx context.Context, id, u string, ids []string, _ []collections.Snapshot) (collections.Collection, int, error) {
	f.calls++
	return f.collection, 1, nil
}
func (f *fakeCollectionBridge) FileImported(context.Context, string, []string, []string, []collections.Snapshot) ([]string, error) {
	f.calls++
	return []string{testCollectionID}, nil
}
func (f *fakeCollectionBridge) EditableTargets(context.Context, string, []string) ([]string, error) {
	f.calls++
	return []string{}, nil
}
func (f *fakeCollectionBridge) VisibleReel(context.Context, string, string) (reels.ReelRecord, error) {
	f.calls++
	return reels.ReelRecord{}, reels.ErrNotFound
}
func (f *fakeCollectionBridge) DeleteImported(context.Context, string, string) error {
	f.calls++
	return nil
}
func (f *fakeCollectionBridge) DeleteBridgeAccount(context.Context, string) error {
	f.calls++
	return nil
}

func bridgeDeps() (Deps, *fakeCollectionBridge) {
	fake := &fakeCollectionBridge{fakeCollections: newFakeCollections()}
	deps := collectionDeps(fake.fakeCollections)
	deps.CollectionBridge = fake
	deps.CollectionBridgeKey = testBridgeKey
	return deps, fake
}
func bridgeRequest(deps Deps, key, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/v2/internal/collections", strings.NewReader(body))
	req.Header.Set("X-ReelPin-Collection-Key", key)
	rec := httptest.NewRecorder()
	routes(deps).ServeHTTP(rec, req)
	return rec
}

func TestCollectionBridgeRequiresDedicatedCredential(t *testing.T) {
	for _, key := range []string{"", "Bearer good.token", "wrong-key"} {
		deps, fake := bridgeDeps()
		rec := bridgeRequest(deps, key, `{"operation":"list","user_id":"`+testUserID+`"}`)
		if rec.Code != http.StatusUnauthorized || fake.calls != 0 || fake.lastAction != "" {
			t.Fatalf("status=%d calls=%d action=%q", rec.Code, fake.calls, fake.lastAction)
		}
	}
	deps, _ := bridgeDeps()
	deps.CollectionBridgeKey = ""
	if rec := bridgeRequest(deps, testBridgeKey, `{}`); rec.Code != http.StatusNotFound {
		t.Fatalf("disabled bridge status=%d", rec.Code)
	}
	deps, _ = bridgeDeps()
	req := httptest.NewRequest(http.MethodGet, "/api/v2/reels", nil)
	req.Header.Set("X-ReelPin-Collection-Key", testBridgeKey)
	rec := httptest.NewRecorder()
	routes(deps).ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bridge key authorized ordinary API: %d", rec.Code)
	}
}

func TestCollectionBridgeRejectsMalformedPayloadBeforeCallingStore(t *testing.T) {
	for _, body := range []string{
		`{"operation":"list","user_id":"not-a-uuid"}`,
		`{"operation":"delete_reel","user_id":"` + testUserID + `","reel_id":"00000000-0000-0000-0000-000000000000"}`,
		`{"operation":"exec","user_id":"` + testUserID + `"}`,
		`{"operation":"detail","user_id":"` + testUserID + `"}`,
		`{"operation":"remove_item","user_id":"` + testUserID + `","collection_id":"` + testCollectionID + `"}`,
		`{"operation":"list","user_id":"` + testUserID + `","offset":10001}`,
		`{"operation":"list","user_id":"` + testUserID + `","url":"https://untrusted.example"}`,
		`{"operation":"list","user_id":"` + testUserID + `"} {}`,
		`{"operation":"create","user_id":"` + testUserID + `","name":"X","reel_ids":["` + testCollectionID + `"],"snapshots":[{"id":"` + testCollectionID + `","user_id":"foreign-user","url":"https://example.com"}]}`,
	} {
		deps, fake := bridgeDeps()
		rec := bridgeRequest(deps, testBridgeKey, body)
		if rec.Code != http.StatusUnprocessableEntity || fake.calls != 0 {
			t.Fatalf("payload=%s status=%d calls=%d body=%s", body, rec.Code, fake.calls, rec.Body.String())
		}
	}
	deps, fake := bridgeDeps()
	rec := bridgeRequest(deps, testBridgeKey, `{"operation":"list","user_id":"`+testUserID+`","token":"`+strings.Repeat("x", 4<<20)+`"}`)
	if rec.Code != http.StatusUnprocessableEntity || fake.calls != 0 {
		t.Fatalf("oversize status=%d calls=%d", rec.Code, fake.calls)
	}
}

func TestCollectionBridgePreservesActorAndReadCache(t *testing.T) {
	deps, fake := bridgeDeps()
	cache := &fakeCache{}
	deps.Cache = cache
	rec := bridgeRequest(deps, testBridgeKey, `{"operation":"list","user_id":"`+testUserID+`"}`)
	if rec.Code != http.StatusOK || fake.lastUserID != testUserID || len(cache.users) != 0 {
		t.Fatalf("status=%d user=%s cache=%v", rec.Code, fake.lastUserID, cache.users)
	}
	rec = bridgeRequest(deps, testBridgeKey, `{"operation":"create","user_id":"`+testUserID+`","name":"旅行"}`)
	var result collectionMutationResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK || result.Collection.Name != "旅行" || len(cache.users) != 1 {
		t.Fatalf("status=%d name=%s cache=%v", rec.Code, result.Collection.Name, cache.users)
	}
}

func TestCollectionBridgeJoinFlagDoesNotChangePublicInviteResponse(t *testing.T) {
	for _, joined := range []bool{false, true} {
		deps, fake := bridgeDeps()
		fake.joined = joined
		rec := bridgeRequest(deps, testBridgeKey, `{"operation":"accept_invite","user_id":"`+testUserID+`","token":"invite-token"}`)
		var result struct {
			Joined bool `json:"joined"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if rec.Code != http.StatusOK || result.Joined != joined {
			t.Fatalf("status=%d joined=%v want=%v", rec.Code, result.Joined, joined)
		}
		public := request(deps, http.MethodPost, "/api/v2/collection-invites/invite-token/accept", "", "Bearer good.token")
		var body map[string]json.RawMessage
		if err := json.Unmarshal(public.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if public.Code != http.StatusOK {
			t.Fatalf("public status=%d", public.Code)
		}
		if _, ok := body["joined"]; ok {
			t.Fatal("bridge-only join flag leaked into public response")
		}
	}
}
