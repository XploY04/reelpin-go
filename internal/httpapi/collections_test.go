package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/XploY04/reelpin-go/internal/collections"
	"github.com/XploY04/reelpin-go/internal/reels"
)

func collectionDeps(fake *fakeCollections) Deps {
	deps := testDeps(&fakePinger{})
	deps.Collections = fake
	return deps
}

// request is serve with a body, which the collection mutations need.
func request(deps Deps, method, target, body, authorization string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	rec := httptest.NewRecorder()
	routes(deps).ServeHTTP(rec, req)
	return rec
}

// TestCollectionRoutesActAsTheTokenSubject is the rule that matters most here:
// no collection route may take its user from anywhere but the credential.
func TestCollectionRoutesActAsTheTokenSubject(t *testing.T) {
	tests := []struct {
		name   string
		method string
		target string
		body   string
		action string
	}{
		{name: "list", method: "GET", target: "/api/v2/collections", action: "list"},
		{name: "create", method: "POST", target: "/api/v2/collections", body: `{"name":"Trips"}`, action: "create"},
		{name: "detail", method: "GET", target: "/api/v2/collections/" + testCollectionID, action: "detail"},
		{name: "update", method: "PATCH", target: "/api/v2/collections/" + testCollectionID, body: `{"name":"Trips"}`, action: "update"},
		{name: "delete", method: "DELETE", target: "/api/v2/collections/" + testCollectionID, action: "delete"},
		{
			name: "add items", method: "POST", target: "/api/v2/collections/" + testCollectionID + "/items",
			body: `{"reel_ids":["` + testReelID + `"]}`, action: "add_items",
		},
		{
			name: "remove item", method: "DELETE",
			target: "/api/v2/collections/" + testCollectionID + "/items/" + testReelID, action: "remove_item",
		},
		{name: "members", method: "GET", target: "/api/v2/collections/" + testCollectionID + "/members", action: "members"},
		{
			name: "remove member", method: "DELETE",
			target: "/api/v2/collections/" + testCollectionID + "/members/" + otherUserID, action: "remove_member",
		},
		{name: "leave", method: "POST", target: "/api/v2/collections/" + testCollectionID + "/leave", action: "leave"},
		{name: "enable link", method: "POST", target: "/api/v2/collections/" + testCollectionID + "/link", action: "enable_link"},
		{name: "disable link", method: "DELETE", target: "/api/v2/collections/" + testCollectionID + "/link", action: "disable_link"},
		{
			name: "create invite", method: "POST", target: "/api/v2/collections/" + testCollectionID + "/invites",
			body: `{"role":"viewer"}`, action: "create_invite",
		},
		{name: "accept invite", method: "POST", target: "/api/v2/collection-invites/sometoken/accept", action: "accept_invite"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := newFakeCollections()
			rec := request(collectionDeps(fake), tt.method, tt.target, tt.body, "Bearer good.token")

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
			}
			if fake.lastAction != tt.action {
				t.Errorf("action = %q, want %q", fake.lastAction, tt.action)
			}
			if fake.lastUserID != testUserID {
				t.Errorf("acted as %q, want the token subject", fake.lastUserID)
			}
		})
	}
}

func TestCollectionRoutesRequireASession(t *testing.T) {
	targets := []string{
		"/api/v2/collections",
		"/api/v2/collections/" + testCollectionID,
		"/api/v2/collections/" + testCollectionID + "/members",
	}
	for _, target := range targets {
		rec := request(collectionDeps(newFakeCollections()), "GET", target, "", "")
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s status = %d, want 401", target, rec.Code)
		}
	}
}

// TestASharedCollectionNeedsNoSession is the one public-share route: the
// unguessable token in the path is the whole authorization.
func TestASharedCollectionNeedsNoSession(t *testing.T) {
	fake := newFakeCollections()
	rec := request(collectionDeps(fake), "GET", "/api/v2/shared-collections/sometoken", "", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if fake.lastToken != "sometoken" {
		t.Errorf("token = %q", fake.lastToken)
	}
	if strings.Contains(rec.Body.String(), "owner_id") {
		t.Error("the anonymous view carries an owner id")
	}
}

// TestAnUnknownShareTokenIsIndistinguishableFromAPrivateOne keeps a token
// guesser from learning which tokens exist.
func TestAnUnknownShareTokenIsIndistinguishableFromAPrivateOne(t *testing.T) {
	fake := newFakeCollections()
	fake.err = collections.ErrNotFound

	rec := request(collectionDeps(fake), "GET", "/api/v2/shared-collections/guessed", "", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	body := decodeError(t, rec)
	if body.Error.Code != "collection_not_found" {
		t.Errorf("code = %q", body.Error.Code)
	}
	if strings.Contains(strings.ToLower(rec.Body.String()), "expire") ||
		strings.Contains(strings.ToLower(rec.Body.String()), "revok") {
		t.Error("the response says why the token failed, which is what a guesser wants")
	}
}

// TestAnotherUsersCollectionIs404 proves access and existence are the same
// answer: a 403 would confirm the collection exists.
func TestAnotherUsersCollectionIs404(t *testing.T) {
	fake := newFakeCollections()
	fake.err = collections.ErrNotFound

	rec := request(collectionDeps(fake), "GET", "/api/v2/collections/"+testCollectionID, "", "Bearer good.token")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if code := decodeError(t, rec).Error.Code; code != "collection_not_found" {
		t.Errorf("code = %q", code)
	}
}

// TestFilingAnotherUsersSaveIs404 answers what getReel answers for that id, so
// filing cannot probe whether someone else's save exists.
func TestFilingAnotherUsersSaveIs404(t *testing.T) {
	fake := newFakeCollections()
	fake.err = reels.ErrNotFound

	rec := request(collectionDeps(fake), "POST", "/api/v2/collections/"+testCollectionID+"/items",
		`{"reel_ids":["`+testReelID+`"]}`, "Bearer good.token")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if code := decodeError(t, rec).Error.Code; code != "reel_not_found" {
		t.Errorf("code = %q", code)
	}
}

func TestAViewerCannotChangeACollection(t *testing.T) {
	fake := newFakeCollections()
	fake.err = collections.ErrForbidden

	rec := request(collectionDeps(fake), "POST", "/api/v2/collections/"+testCollectionID+"/items",
		`{"reel_ids":["`+testReelID+`"]}`, "Bearer good.token")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if code := decodeError(t, rec).Error.Code; code != "collection_forbidden" {
		t.Errorf("code = %q", code)
	}
}

func TestAnInvalidInviteIsOneError(t *testing.T) {
	fake := newFakeCollections()
	fake.err = collections.ErrInviteInvalid

	rec := request(collectionDeps(fake), "POST", "/api/v2/collection-invites/stale/accept", "", "Bearer good.token")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if code := decodeError(t, rec).Error.Code; code != "collection_invite_invalid" {
		t.Errorf("code = %q", code)
	}
}

func TestCreatingACollectionNeedsAName(t *testing.T) {
	for _, body := range []string{`{}`, `{"name":"   "}`} {
		rec := request(collectionDeps(newFakeCollections()), "POST", "/api/v2/collections", body, "Bearer good.token")
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("body %s status = %d, want 422", body, rec.Code)
		}
	}
}

func TestAddingItemsNeedsAtLeastOne(t *testing.T) {
	rec := request(collectionDeps(newFakeCollections()), "POST",
		"/api/v2/collections/"+testCollectionID+"/items", `{"reel_ids":[]}`, "Bearer good.token")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
}

func TestAnUnknownBodyFieldIsRejected(t *testing.T) {
	// The request schemas declare additionalProperties false; silently
	// dropping a misspelled field hides client bugs.
	rec := request(collectionDeps(newFakeCollections()), "POST", "/api/v2/collections",
		`{"name":"Trips","colour":"red"}`, "Bearer good.token")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
}

func TestAMalformedCollectionIDIs404(t *testing.T) {
	rec := request(collectionDeps(newFakeCollections()), "GET", "/api/v2/collections/not-a-uuid", "", "Bearer good.token")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestItemPagesUseOpaqueCursors(t *testing.T) {
	fake := newFakeCollections()
	cursor := reels.Cursor{SavedAt: testNow, ID: testReelID}

	rec := request(collectionDeps(fake), "GET",
		"/api/v2/collections/"+testCollectionID+"?limit=10&cursor="+cursor.Encode(), "", "Bearer good.token")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	if fake.lastQuery.Limit != 10 {
		t.Errorf("limit = %d", fake.lastQuery.Limit)
	}
	if fake.lastQuery.After == nil || fake.lastQuery.After.ID != testReelID {
		t.Fatalf("cursor = %+v, want the decoded position", fake.lastQuery.After)
	}

	// Anything this API did not issue is a validation error, never a guessed
	// position.
	rec = request(collectionDeps(fake), "GET",
		"/api/v2/collections/"+testCollectionID+"?cursor=25", "", "Bearer good.token")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("an offset-shaped cursor status = %d, want 422", rec.Code)
	}
}

func TestAServiceFailureIsA500WithoutDetail(t *testing.T) {
	fake := newFakeCollections()
	fake.err = errFake

	rec := request(collectionDeps(fake), "GET", "/api/v2/collections", "", "Bearer good.token")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), errFake.Error()) {
		t.Error("the driver error leaked into the response body")
	}
}

func TestCollectionsLinkTheirPublicPages(t *testing.T) {
	fake := newFakeCollections()
	fake.items = []collections.ItemRecord{{Reel: sampleReel(testReelID, otherUserID)}}

	rec := request(collectionDeps(fake), "GET", "/api/v2/collections/"+testCollectionID, "", "Bearer good.token")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	var body collections.Detail
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if want := testWebBaseURL + "/collections/" + testCollectionID; body.Collection.WebURL != want {
		t.Errorf("collection web_url = %q, want %q", body.Collection.WebURL, want)
	}
	if len(body.Items) != 1 || body.Items[0].WebURL != testWebBaseURL+"/r/"+testReelID {
		t.Errorf("items = %+v, want the item's web_url", body.Items)
	}

	list := request(collectionDeps(newFakeCollections()), "GET", "/api/v2/collections", "", "Bearer good.token")
	if !strings.Contains(list.Body.String(), `"web_url":"`+testWebBaseURL+"/collections/"+testCollectionID+`"`) {
		t.Errorf("the list does not link the collection: %s", list.Body.String())
	}
}

// Adding items answers the collection as it now stands, from a read after the
// write, so the count a client shows includes what it just added.
func TestAddingItemsAnswersTheCollectionAfterTheChange(t *testing.T) {
	fake := newFakeCollections()
	fake.added = 1
	rec := request(collectionDeps(fake), "POST", "/api/v2/collections/"+testCollectionID+"/items",
		`{"reel_ids":["`+testReelID+`"]}`, "Bearer good.token")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	var body struct {
		Collection collections.Collection `json:"collection"`
		AddedCount int                    `json:"added_count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.AddedCount != 1 || body.Collection.ID != testCollectionID || body.Collection.WebURL == "" {
		t.Errorf("body = %+v", body)
	}
}

// TestCollectionItemsAreFullReels covers the reason items changed shape: a
// collaborator or a link viewer cannot call getReel for a save that is not
// theirs, so the item itself carries the detailed reel, and concise trims it
// the same way it trims the reel list.
func TestCollectionItemsAreFullReels(t *testing.T) {
	addedAt := testNow.Add(-time.Hour)
	for _, target := range []string{
		"/api/v2/collections/" + testCollectionID,
		"/api/v2/shared-collections/sometoken",
	} {
		t.Run(target, func(t *testing.T) {
			fake := newFakeCollections()
			fake.items = []collections.ItemRecord{{
				Reel:    sampleReel(testReelID, otherUserID),
				AddedAt: &addedAt,
				AddedBy: stringPtr(otherUserID),
			}}

			rec := request(collectionDeps(fake), "GET", target, "", "Bearer good.token")
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
			}
			if !fake.lastQuery.Transcripts {
				t.Error("the detailed view did not ask storage for transcripts")
			}
			item := firstItem(t, rec)
			for key, want := range map[string]any{
				"reel_id":            testReelID,
				"id":                 testReelID,
				"transcript":         "spoken words",
				"sub_category":       "cafes",
				"sub_category_label": "Cafes",
				"category_label":     "Food And Drink",
				"relative_date":      "2 days ago",
				"map_location_count": float64(1),
				"source_platform":    "instagram",
				"added_by":           otherUserID,
			} {
				if item[key] != want {
					t.Errorf("%s = %#v, want %#v", key, item[key], want)
				}
			}
			for _, key := range []string{"key_facts", "people_mentioned", "actionable_items", "events", "locations", "mappable_locations", "added_at", "saved_at"} {
				if _, present := item[key]; !present {
					t.Errorf("the detailed item has no %s", key)
				}
			}
			if locations, _ := item["locations"].([]any); len(locations) != 2 {
				t.Errorf("locations = %v, want both, coordinates or not", item["locations"])
			}
			// The anonymous link view must not name the save owner's account.
			userID, present := item["user_id"]
			if strings.HasPrefix(target, "/api/v2/shared-collections/") {
				if present {
					t.Errorf("the shared item carries user_id %v", userID)
				}
			} else if userID != otherUserID {
				t.Errorf("user_id = %#v, want the save owner %s", userID, otherUserID)
			}

			concise := request(collectionDeps(fake), "GET", target+"?view=concise", "", "Bearer good.token")
			if concise.Code != http.StatusOK {
				t.Fatalf("concise status = %d (%s)", concise.Code, concise.Body.String())
			}
			if fake.lastQuery.Transcripts {
				t.Error("the concise view asked storage for transcripts it does not send")
			}
			item = firstItem(t, concise)
			for _, key := range []string{"transcript", "key_facts", "people_mentioned", "actionable_items", "events"} {
				if _, present := item[key]; present {
					t.Errorf("the concise item carries %s", key)
				}
			}
			if item["sub_category"] != "cafes" || item["added_at"] == nil {
				t.Errorf("the concise item lost a display or item field: %v", item)
			}
		})
	}
}

func TestAnUnknownItemViewIsRejected(t *testing.T) {
	for _, target := range []string{
		"/api/v2/collections/" + testCollectionID + "?view=verbose",
		"/api/v2/shared-collections/sometoken?view=verbose",
	} {
		rec := request(collectionDeps(newFakeCollections()), "GET", target, "", "Bearer good.token")
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s status = %d, want 422", target, rec.Code)
		}
	}
}

func TestCanEditFollowsTheRole(t *testing.T) {
	for role, want := range map[string]bool{
		collections.RoleOwner: true, collections.RoleEditor: true, collections.RoleViewer: false,
	} {
		fake := newFakeCollections()
		fake.collection.Role = role
		rec := request(collectionDeps(fake), "GET", "/api/v2/collections/"+testCollectionID, "", "Bearer good.token")
		var body collections.Detail
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body.CanEdit != want {
			t.Errorf("role %s can_edit = %v, want %v", role, body.CanEdit, want)
		}
	}
}

func firstItem(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(body.Items))
	}
	return body.Items[0]
}
