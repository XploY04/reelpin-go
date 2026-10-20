package collections

import (
	"context"
	"strings"
	"time"

	"github.com/XploY04/reelpin-go/internal/reels"
)

// Store is what the collection use cases need from storage. Implemented by
// internal/postgres; faked in handler tests.
type Store interface {
	List(ctx context.Context, userID string) ([]Collection, error)
	Create(ctx context.Context, userID, name, description string, saveIDs []string) (Collection, int, error)
	Get(ctx context.Context, collectionID, userID string) (Collection, error)
	Update(ctx context.Context, collectionID, userID string, name, description, coverReelID *string) (Collection, error)
	Delete(ctx context.Context, collectionID, userID string) error
	Detail(ctx context.Context, collectionID, userID string, after *reels.Cursor, limit int) (Detail, error)
	SharedByToken(ctx context.Context, token string, after *reels.Cursor, limit int) (Shared, error)
	AddItems(ctx context.Context, collectionID, userID string, saveIDs []string) (int, error)
	RemoveItem(ctx context.Context, collectionID, userID, saveID string) error
	EnableLink(ctx context.Context, collectionID, userID string) (string, string, time.Time, error)
	DisableLink(ctx context.Context, collectionID, userID string) error
	Members(ctx context.Context, collectionID, userID string) (string, []Member, error)
	RemoveMember(ctx context.Context, collectionID, userID, memberUserID string) error
	Leave(ctx context.Context, collectionID, userID string) error
	CreateInvite(ctx context.Context, collectionID, userID, role string) (string, string, string, time.Time, error)
	AcceptInvite(ctx context.Context, token, userID string) (Collection, error)
}

// Service is the collection use cases every client shares. It adds what
// storage does not know, the public page each collection and item lives on.
type Service struct {
	store      Store
	webBaseURL string
}

func NewService(store Store, webBaseURL string) *Service {
	return &Service{store: store, webBaseURL: webBaseURL}
}

// WebURL is a collection's public page.
func WebURL(baseURL, collectionID string) string {
	return strings.TrimSuffix(baseURL, "/") + "/collections/" + collectionID
}

// Link is a minted share link or invite. The raw token exists in this value
// and nowhere else.
type Link struct {
	URL       string
	Token     string
	Role      string
	ExpiresAt time.Time
}

func (s *Service) List(ctx context.Context, userID string) ([]Collection, error) {
	list, err := s.store.List(ctx, userID)
	if err != nil {
		return nil, err
	}
	for index := range list {
		list[index] = s.collection(list[index])
	}
	return list, nil
}

func (s *Service) Create(ctx context.Context, userID, name, description string, saveIDs []string) (Collection, int, error) {
	collection, added, err := s.store.Create(ctx, userID, name, description, saveIDs)
	if err != nil {
		return Collection{}, 0, err
	}
	return s.collection(collection), added, nil
}

func (s *Service) Update(ctx context.Context, collectionID, userID string, name, description, coverReelID *string) (Collection, error) {
	collection, err := s.store.Update(ctx, collectionID, userID, name, description, coverReelID)
	if err != nil {
		return Collection{}, err
	}
	return s.collection(collection), nil
}

func (s *Service) Delete(ctx context.Context, collectionID, userID string) error {
	return s.store.Delete(ctx, collectionID, userID)
}

func (s *Service) Detail(ctx context.Context, collectionID, userID string, after *reels.Cursor, limit int) (Detail, error) {
	detail, err := s.store.Detail(ctx, collectionID, userID, after, limit)
	if err != nil {
		return Detail{}, err
	}
	detail.Collection = s.collection(detail.Collection)
	detail.Items = s.items(detail.Items)
	return detail, nil
}

func (s *Service) SharedByToken(ctx context.Context, token string, after *reels.Cursor, limit int) (Shared, error) {
	shared, err := s.store.SharedByToken(ctx, token, after, limit)
	if err != nil {
		return Shared{}, err
	}
	shared.Collection = s.collection(shared.Collection)
	shared.Items = s.items(shared.Items)
	return shared, nil
}

// AddItems files saves and answers the collection as it now stands, so the
// item count a client shows is the one after the change.
func (s *Service) AddItems(ctx context.Context, collectionID, userID string, saveIDs []string) (Collection, int, error) {
	added, err := s.store.AddItems(ctx, collectionID, userID, saveIDs)
	if err != nil {
		return Collection{}, 0, err
	}
	collection, err := s.store.Get(ctx, collectionID, userID)
	if err != nil {
		return Collection{}, 0, err
	}
	return s.collection(collection), added, nil
}

func (s *Service) RemoveItem(ctx context.Context, collectionID, userID, saveID string) error {
	return s.store.RemoveItem(ctx, collectionID, userID, saveID)
}

func (s *Service) EnableLink(ctx context.Context, collectionID, userID string) (Link, error) {
	url, token, expiresAt, err := s.store.EnableLink(ctx, collectionID, userID)
	if err != nil {
		return Link{}, err
	}
	return Link{URL: url, Token: token, ExpiresAt: expiresAt}, nil
}

func (s *Service) DisableLink(ctx context.Context, collectionID, userID string) error {
	return s.store.DisableLink(ctx, collectionID, userID)
}

func (s *Service) Members(ctx context.Context, collectionID, userID string) (string, []Member, error) {
	return s.store.Members(ctx, collectionID, userID)
}

func (s *Service) RemoveMember(ctx context.Context, collectionID, userID, memberUserID string) error {
	return s.store.RemoveMember(ctx, collectionID, userID, memberUserID)
}

func (s *Service) Leave(ctx context.Context, collectionID, userID string) error {
	return s.store.Leave(ctx, collectionID, userID)
}

func (s *Service) CreateInvite(ctx context.Context, collectionID, userID, role string) (Link, error) {
	url, token, grantedRole, expiresAt, err := s.store.CreateInvite(ctx, collectionID, userID, role)
	if err != nil {
		return Link{}, err
	}
	return Link{URL: url, Token: token, Role: grantedRole, ExpiresAt: expiresAt}, nil
}

func (s *Service) AcceptInvite(ctx context.Context, token, userID string) (Collection, error) {
	collection, err := s.store.AcceptInvite(ctx, token, userID)
	if err != nil {
		return Collection{}, err
	}
	return s.collection(collection), nil
}

func (s *Service) collection(collection Collection) Collection {
	collection.WebURL = WebURL(s.webBaseURL, collection.ID)
	return collection
}

func (s *Service) items(items []Item) []Item {
	for index := range items {
		items[index].WebURL = reels.WebURL(s.webBaseURL, items[index].ReelID)
	}
	return items
}
