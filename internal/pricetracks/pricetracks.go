// Package pricetracks owns price watches on product pages: what a user asked
// to watch, and the daily check that tells them once when the price reaches
// their target.
//
// The check is a scheduled run, not a sweep: the worker fleet parks one
// delayed broker message per day, and the table is read only when it arrives.
package pricetracks

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/XploY04/reelpin-go/internal/sourceidentity"
	"github.com/google/uuid"
)

// ErrNotFound is a track that does not exist, or is not this user's.
var ErrNotFound = errors.New("price track not found")

// ErrUnknownSave is a reel id that is not one of this user's saves.
var ErrUnknownSave = errors.New("the reel is not one of this user's saves")

// MaxListed bounds one list, as the Python API did.
const MaxListed = 200

// ValidationError names the field a caller can fix.
type ValidationError struct {
	Field  string
	Reason string
}

func (e *ValidationError) Error() string { return e.Field + " " + e.Reason }

// Record is one stored track.
type Record struct {
	ID            string
	UserID        string
	SaveID        *string
	URL           string
	Title         string
	TargetPrice   float64
	Currency      string
	LastPrice     *float64
	Active        bool
	LastCheckedAt *time.Time
	NotifiedAt    *time.Time
	CreatedAt     time.Time
}

// New is what Create stores.
type New struct {
	URL         string
	Title       string
	TargetPrice float64
	SaveID      *string
}

// PriceTrack is the wire shape, field for field what the Python API answered.
type PriceTrack struct {
	ID            string   `json:"id"`
	URL           string   `json:"url"`
	Title         string   `json:"title"`
	TargetPrice   float64  `json:"target_price"`
	Currency      string   `json:"currency"`
	LastPrice     *float64 `json:"last_price"`
	Active        bool     `json:"active"`
	LastCheckedAt *string  `json:"last_checked_at"`
	NotifiedAt    *string  `json:"notified_at"`
	ReelID        *string  `json:"reel_id"`
	CreatedAt     *string  `json:"created_at"`
}

// CreateParams is a request as the client sent it. TargetPrice is a pointer
// so a missing price is told apart from a zero one.
type CreateParams struct {
	URL         string
	TargetPrice *float64
	Title       string
	ReelID      *string
}

// Store is the persistence a track needs. postgres implements it.
type Store interface {
	List(ctx context.Context, userID string, limit int) ([]Record, error)
	Create(ctx context.Context, userID string, track New) (Record, error)
	Delete(ctx context.Context, userID, trackID string) error
	// Due pages active tracks not checked since the cutoff, by id.
	Due(ctx context.Context, checkedBefore time.Time, afterID string, limit int) ([]Record, error)
	RecordCheck(ctx context.Context, trackID string, check Check) error
}

// Check is the outcome of reading one page.
type Check struct {
	CheckedAt time.Time
	LastPrice *float64
	Notified  bool
}

// Service is the user-facing half: list, create, delete.
type Service struct {
	store Store
}

func NewService(store Store) *Service {
	return &Service{store: store}
}

// List returns this user's tracks, newest first.
func (s *Service) List(ctx context.Context, userID string) ([]PriceTrack, error) {
	records, err := s.store.List(ctx, userID, MaxListed)
	if err != nil {
		return nil, err
	}
	list := make([]PriceTrack, 0, len(records))
	for _, record := range records {
		list = append(list, Present(record))
	}
	return list, nil
}

// Create starts watching a page. The URL must be one the service would fetch;
// it is not fetched now, so creating a track never costs a request.
func (s *Service) Create(ctx context.Context, userID string, params CreateParams) (PriceTrack, error) {
	url := strings.TrimSpace(params.URL)
	if url == "" {
		return PriceTrack{}, &ValidationError{Field: "url", Reason: "a product URL is required"}
	}
	if _, err := sourceidentity.Resolve(url); err != nil {
		return PriceTrack{}, &ValidationError{Field: "url", Reason: "that does not look like a valid URL"}
	}
	if params.TargetPrice == nil || !(*params.TargetPrice > 0) {
		return PriceTrack{}, &ValidationError{Field: "target_price", Reason: "must be greater than 0"}
	}
	saveID, err := optionalSaveID(params.ReelID)
	if err != nil {
		return PriceTrack{}, err
	}

	record, err := s.store.Create(ctx, userID, New{
		URL:         url,
		Title:       strings.TrimSpace(params.Title),
		TargetPrice: *params.TargetPrice,
		SaveID:      saveID,
	})
	if err != nil {
		return PriceTrack{}, err
	}
	return Present(record), nil
}

// Delete stops watching one of this user's tracks.
func (s *Service) Delete(ctx context.Context, userID, trackID string) error {
	return s.store.Delete(ctx, userID, trackID)
}

func optionalSaveID(reelID *string) (*string, error) {
	if reelID == nil || strings.TrimSpace(*reelID) == "" {
		return nil, nil
	}
	parsed, err := uuid.Parse(strings.TrimSpace(*reelID))
	if err != nil {
		return nil, &ValidationError{Field: "reel_id", Reason: "must be one of your saves"}
	}
	id := parsed.String()
	return &id, nil
}

// Present is the wire shape of a record.
func Present(record Record) PriceTrack {
	return PriceTrack{
		ID:            record.ID,
		URL:           record.URL,
		Title:         record.Title,
		TargetPrice:   record.TargetPrice,
		Currency:      record.Currency,
		LastPrice:     record.LastPrice,
		Active:        record.Active,
		LastCheckedAt: isoTime(record.LastCheckedAt),
		NotifiedAt:    isoTime(record.NotifiedAt),
		ReelID:        record.SaveID,
		CreatedAt:     isoTime(&record.CreatedAt),
	}
}

func isoTime(value *time.Time) *string {
	if value == nil {
		return nil
	}
	formatted := value.UTC().Format(time.RFC3339Nano)
	return &formatted
}
