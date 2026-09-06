package httpapi

import (
	"context"
	"time"

	"github.com/XploY04/reelpin-go/internal/collections"
	"github.com/XploY04/reelpin-go/internal/reels"
)

const testCollectionID = "77777777-7777-4777-8777-777777777777"

// fakeCollections records what a handler asked for and answers with fixed
// values, so handler tests need no database.
type fakeCollections struct {
	err        error
	collection collections.Collection
	detail     collections.Detail
	shared     collections.Shared
	members    []collections.Member
	added      int

	lastUserID  string
	lastAction  string
	lastLimit   int
	lastCursor  *reels.Cursor
	lastSaveIDs []string
	lastToken   string
}

func newFakeCollections() *fakeCollections {
	collection := collections.Collection{
		ID: testCollectionID, Name: "Weekend cooking", Description: "Things to try",
		Visibility: "private", Role: collections.RoleOwner, ItemCount: 1,
	}
	return &fakeCollections{
		collection: collection,
		detail: collections.Detail{
			Collection: collection, Items: []collections.Item{},
			Page: collections.Page{Limit: 25}, CanEdit: true,
		},
		shared: collections.Shared{
			Collection: collection, Items: []collections.Item{},
			Page: collections.Page{Limit: 25},
		},
		members: []collections.Member{},
	}
}

func (f *fakeCollections) List(_ context.Context, userID string) ([]collections.Collection, error) {
	f.lastUserID, f.lastAction = userID, "list"
	if f.err != nil {
		return nil, f.err
	}
	return []collections.Collection{f.collection}, nil
}

func (f *fakeCollections) Create(_ context.Context, userID, name, _ string, saveIDs []string) (collections.Collection, int, error) {
	f.lastUserID, f.lastAction, f.lastSaveIDs = userID, "create", saveIDs
	if f.err != nil {
		return collections.Collection{}, 0, f.err
	}
	collection := f.collection
	collection.Name = name
	return collection, f.added, nil
}

func (f *fakeCollections) Get(_ context.Context, _, userID string) (collections.Collection, error) {
	f.lastUserID = userID
	return f.collection, f.err
}

func (f *fakeCollections) Update(_ context.Context, _, userID string, _, _, _ *string) (collections.Collection, error) {
	f.lastUserID, f.lastAction = userID, "update"
	return f.collection, f.err
}

func (f *fakeCollections) Delete(_ context.Context, _, userID string) error {
	f.lastUserID, f.lastAction = userID, "delete"
	return f.err
}

func (f *fakeCollections) Detail(_ context.Context, _, userID string, after *reels.Cursor, limit int) (collections.Detail, error) {
	f.lastUserID, f.lastAction, f.lastCursor, f.lastLimit = userID, "detail", after, limit
	return f.detail, f.err
}

func (f *fakeCollections) SharedByToken(_ context.Context, token string, after *reels.Cursor, limit int) (collections.Shared, error) {
	f.lastAction, f.lastToken, f.lastCursor, f.lastLimit = "shared", token, after, limit
	return f.shared, f.err
}

func (f *fakeCollections) AddItems(_ context.Context, _, userID string, saveIDs []string) (int, error) {
	f.lastUserID, f.lastAction, f.lastSaveIDs = userID, "add_items", saveIDs
	return f.added, f.err
}

func (f *fakeCollections) RemoveItem(_ context.Context, _, userID, _ string) error {
	f.lastUserID, f.lastAction = userID, "remove_item"
	return f.err
}

func (f *fakeCollections) EnableLink(_ context.Context, _, userID string) (string, string, time.Time, error) {
	f.lastUserID, f.lastAction = userID, "enable_link"
	if f.err != nil {
		return "", "", time.Time{}, f.err
	}
	return "https://reelpin.in/c/token", "token", testNow.Add(time.Hour), nil
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
	return testUserID, f.members, nil
}

func (f *fakeCollections) RemoveMember(_ context.Context, _, userID, _ string) error {
	f.lastUserID, f.lastAction = userID, "remove_member"
	return f.err
}

func (f *fakeCollections) Leave(_ context.Context, _, userID string) error {
	f.lastUserID, f.lastAction = userID, "leave"
	return f.err
}

func (f *fakeCollections) CreateInvite(_ context.Context, _, userID, role string) (string, string, string, time.Time, error) {
	f.lastUserID, f.lastAction = userID, "create_invite"
	if f.err != nil {
		return "", "", "", time.Time{}, f.err
	}
	if role != collections.RoleEditor {
		role = collections.RoleViewer
	}
	return "https://reelpin.in/c/invite/token", "token", role, testNow.Add(time.Hour), nil
}

func (f *fakeCollections) AcceptInvite(_ context.Context, token, userID string) (collections.Collection, error) {
	f.lastUserID, f.lastAction, f.lastToken = userID, "accept_invite", token
	return f.collection, f.err
}
