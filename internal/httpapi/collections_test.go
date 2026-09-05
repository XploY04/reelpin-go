package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/XploY04/reelpin-go/internal/collections"
)

// fakeCollections records what the handlers asked for and returns fixed shapes.
type fakeCollections struct {
	err          error
	collection   collections.Collection
	detail       collections.Detail
	shared       collections.Shared
	members      []collections.Member
	added        int
	lastUserID   string
	lastAction   string
	lastArgument string
}

func (f *fakeCollections) List(_ context.Context, userID string) ([]collections.Collection, error) {
	f.lastUserID, f.lastAction = userID, "list"
	if f.err != nil {
		return nil, f.err
	}
	return []collections.Collection{f.collection}, nil
}

func (f *fakeCollections) Create(_ context.Context, userID, name, _ string, reelIDs []string) (collections.Collection, int, error) {
	f.lastUserID, f.lastAction, f.lastArgument = userID, "create", name
	if f.err != nil {
		return collections.Collection{}, 0, f.err
	}
	return f.collection, len(reelIDs), nil
}

func (f *fakeCollections) Get(_ context.Context, collectionID, userID string) (collections.Collection, error) {
	f.lastUserID = userID
	if f.err != nil {
		return collections.Collection{}, f.err
	}
	return f.collection, nil
}

func (f *fakeCollections) Update(_ context.Context, _, userID string, name, _, _ *string) (collections.Collection, error) {
	f.lastUserID, f.lastAction = userID, "update"
	if name != nil {
		f.lastArgument = *name
	}
	if f.err != nil {
		return collections.Collection{}, f.err
	}
	return f.collection, nil
}

func (f *fakeCollections) Delete(_ context.Context, _, userID string) error {
	f.lastUserID, f.lastAction = userID, "delete"
	return f.err
}

func (f *fakeCollections) Detail(_ context.Context, _, userID string, offset, limit int, _ time.Time) (collections.Detail, error) {
	f.lastUserID, f.lastAction = userID, "detail"
	if f.err != nil {
		return collections.Detail{}, f.err
	}
	detail := f.detail
	detail.Pagination.Offset, detail.Pagination.Limit = offset, limit
	return detail, nil
}

func (f *fakeCollections) SharedByToken(_ context.Context, token string, _, _ int, _ time.Time) (collections.Shared, error) {
	f.lastAction, f.lastArgument = "shared", token
	if f.err != nil {
		return collections.Shared{}, f.err
	}
	return f.shared, nil
}

func (f *fakeCollections) AddItems(_ context.Context, _, userID string, reelIDs []string) (int, error) {
	f.lastUserID, f.lastAction = userID, "add_items"
	if f.err != nil {
		return 0, f.err
	}
	return len(reelIDs), nil
}

func (f *fakeCollections) RemoveItem(_ context.Context, _, userID, reelID string) error {
	f.lastUserID, f.lastAction, f.lastArgument = userID, "remove_item", reelID
	return f.err
}

func (f *fakeCollections) EnableLink(_ context.Context, _, userID string) (string, string, error) {
	f.lastUserID, f.lastAction = userID, "enable_link"
	if f.err != nil {
		return "", "", f.err
	}
	return "https://reelpin.in/c/raw-token", "raw-token", nil
}

func (f *fakeCollections) DisableLink(_ context.Context, _, userID string) error {
	f.lastUserID, f.lastAction = userID, "disable_link"
	return f.err
}

func (f *fakeCollections) Members(_ context.Context, _, userID string) (string, []collections.Member, error) {
	f.lastUserID, f.lastAction = userID, "members"
	if f.err != nil {
		return "", nil, f.err
	}
	return otherUserID, f.members, nil
}

func (f *fakeCollections) RemoveMember(_ context.Context, _, userID, memberUserID string) error {
	f.lastUserID, f.lastAction, f.lastArgument = userID, "remove_member", memberUserID
	return f.err
}

func (f *fakeCollections) Leave(_ context.Context, _, userID string) error {
	f.lastUserID, f.lastAction = userID, "leave"
	return f.err
}

func (f *fakeCollections) CreateInvite(_ context.Context, _, userID, role string) (string, string, string, time.Time, error) {
	f.lastUserID, f.lastAction, f.lastArgument = userID, "invite", role
	if f.err != nil {
		return "", "", "", time.Time{}, f.err
	}
	if role != collections.RoleEditor {
		role = collections.RoleViewer
	}
	return "https://reelpin.in/c/invite/raw", "raw", role, testNow.Add(collections.InviteExpiry), nil
}

func (f *fakeCollections) AcceptInvite(_ context.Context, token, userID string) (collections.Collection, error) {
	f.lastUserID, f.lastAction, f.lastArgument = userID, "accept", token
	if f.err != nil {
		return collections.Collection{}, f.err
	}
	return f.collection, nil
}

const testCollectionID = "77777777-7777-4777-8777-777777777777"

func collectionDeps(fake *fakeCollections) Deps {
	deps := testDeps(&fakePinger{})
	deps.Collections = fake
	return deps
}

func newFakeCollections() *fakeCollections {
	return &fakeCollections{
		collection: collections.Collection{
			ID: testCollectionID, Name: "Goa", Role: collections.RoleOwner, ItemCount: 2, MemberCount: 1,
		},
		members: []collections.Member{{UserID: otherUserID, Role: collections.RoleEditor}},
	}
}

func request(deps Deps, method, target, body, authorization string) *httptest.ResponseRecorder {
	var reader *strings.Reader
	if body != "" {
		reader = strings.NewReader(body)
	} else {
		reader = strings.NewReader("")
	}
	req := httptest.NewRequest(method, target, reader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	rec := httptest.NewRecorder()
	New(deps).Routes().ServeHTTP(rec, req)
	return rec
}

func TestEveryCollectionRouteIsReachable(t *testing.T) {
	tests := []struct {
		name   string
		method string
		target string
		body   string
		action string
	}{
		{name: "list", method: "GET", target: "/api/v1/collections", action: "list"},
		{name: "create", method: "POST", target: "/api/v1/collections", body: `{"name":"Goa"}`, action: "create"},
		{name: "detail", method: "GET", target: "/api/v1/collections/" + testCollectionID, action: "detail"},
		{name: "update", method: "PATCH", target: "/api/v1/collections/" + testCollectionID, body: `{"name":"Goa trips"}`, action: "update"},
		{name: "delete", method: "DELETE", target: "/api/v1/collections/" + testCollectionID, action: "delete"},
		{name: "add items", method: "POST", target: "/api/v1/collections/" + testCollectionID + "/items", body: `{"reel_ids":["` + testReelID + `"]}`, action: "add_items"},
		{name: "remove item", method: "DELETE", target: "/api/v1/collections/" + testCollectionID + "/items/" + testReelID, action: "remove_item"},
		{name: "enable link", method: "POST", target: "/api/v1/collections/" + testCollectionID + "/link", body: `{}`, action: "enable_link"},
		{name: "disable link", method: "DELETE", target: "/api/v1/collections/" + testCollectionID + "/link", action: "disable_link"},
		{name: "members", method: "GET", target: "/api/v1/collections/" + testCollectionID + "/members", action: "members"},
		{name: "remove member", method: "DELETE", target: "/api/v1/collections/" + testCollectionID + "/members/" + otherUserID, action: "remove_member"},
		{name: "leave", method: "POST", target: "/api/v1/collections/" + testCollectionID + "/leave", body: `{}`, action: "leave"},
		{name: "invite", method: "POST", target: "/api/v1/collections/" + testCollectionID + "/invites", body: `{"role":"editor"}`, action: "invite"},
		{name: "accept invite", method: "POST", target: "/api/v1/collections/invites/raw-token/accept", body: `{}`, action: "accept"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := newFakeCollections()
			rec := request(collectionDeps(fake), tt.method, tt.target, tt.body, "Bearer good.token")

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
			}
			if fake.lastAction != tt.action {
				t.Errorf("service action = %q, want %q", fake.lastAction, tt.action)
			}
			if fake.lastUserID != testUserID {
				t.Errorf("acted as %q, want the token subject", fake.lastUserID)
			}
		})
	}
}

func TestCollectionRoutesRequireASession(t *testing.T) {
	for _, target := range []string{
		"/api/v1/collections",
		"/api/v1/collections/" + testCollectionID,
		"/api/v1/collections/" + testCollectionID + "/members",
	} {
		rec := request(collectionDeps(newFakeCollections()), "GET", target, "", "")
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s status = %d, want 401", target, rec.Code)
		}
	}
}

func TestSharedCollectionNeedsNoSessionAndHidesTheOwner(t *testing.T) {
	fake := newFakeCollections()
	fake.shared = collections.Shared{
		Collection: collections.Collection{ID: testCollectionID, Name: "Goa", ItemCount: 2},
	}

	rec := request(collectionDeps(fake), "GET", "/api/v1/collections/shared/raw-token", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if fake.lastArgument != "raw-token" {
		t.Errorf("token = %q", fake.lastArgument)
	}

	body := rec.Body.String()
	if strings.Contains(body, "owner_id") {
		t.Error("an anonymous reader was shown the owner id")
	}
	if strings.Contains(body, `"member_count":1`) {
		t.Error("an anonymous reader was shown the member count")
	}
}

func TestPrivateAndMissingCollectionsAreIndistinguishable(t *testing.T) {
	fake := newFakeCollections()
	fake.err = collections.ErrNotFound

	rec := request(collectionDeps(fake), "GET", "/api/v1/collections/"+testCollectionID, "", "Bearer good.token")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if code := decodeError(t, rec).ErrorCode; code != "collection_not_found" {
		t.Errorf("error_code = %q", code)
	}
}

func TestAViewerCannotChangeACollection(t *testing.T) {
	fake := newFakeCollections()
	fake.err = collections.ErrForbidden

	rec := request(collectionDeps(fake), "POST", "/api/v1/collections/"+testCollectionID+"/items",
		`{"reel_ids":["`+testReelID+`"]}`, "Bearer good.token")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if code := decodeError(t, rec).ErrorCode; code != "collection_forbidden" {
		t.Errorf("error_code = %q", code)
	}
}

func TestAnExpiredInviteIsRejected(t *testing.T) {
	fake := newFakeCollections()
	fake.err = collections.ErrInviteInvalid

	rec := request(collectionDeps(fake), "POST", "/api/v1/collections/invites/old/accept", `{}`, "Bearer good.token")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if code := decodeError(t, rec).ErrorCode; code != "collection_invite_invalid" {
		t.Errorf("error_code = %q", code)
	}
}

func TestInviteResponseCarriesTheRawTokenOnce(t *testing.T) {
	rec := request(collectionDeps(newFakeCollections()), "POST",
		"/api/v1/collections/"+testCollectionID+"/invites", `{"role":"editor"}`, "Bearer good.token")

	var body collectionInviteResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body: %v", err)
	}
	if body.Token == "" || body.URL == "" {
		t.Fatalf("invite = %+v", body)
	}
	if body.Role != collections.RoleEditor {
		t.Errorf("role = %q", body.Role)
	}
	if body.ExpiresAt == nil {
		t.Error("an invite with no expiry never stops working")
	}
}

func TestCreateRequiresAName(t *testing.T) {
	rec := request(collectionDeps(newFakeCollections()), "POST", "/api/v1/collections", `{"name":"  "}`, "Bearer good.token")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (%s)", rec.Code, rec.Body.String())
	}
}

func TestAnUnknownSubresourceIs404(t *testing.T) {
	rec := request(collectionDeps(newFakeCollections()), "GET",
		"/api/v1/collections/"+testCollectionID+"/nonsense", "", "Bearer good.token")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}
