// Package reminders owns the reminders a user sets and when each one fires.
//
// PostgreSQL is the record and Redis is the clock. Creating a reminder writes
// the row and then adds its id to a sorted set scored by fire_at; a worker
// reads only the members that are already due and touches PostgreSQL for
// those alone. Nothing here polls a table on a timer.
package reminders

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ErrNotFound is a reminder that does not exist, or is not this user's. The
// two are the same answer so ids cannot be probed.
var ErrNotFound = errors.New("reminder not found")

// ErrUnknownSave is a reel id that is not one of this user's saves.
var ErrUnknownSave = errors.New("the reel is not one of this user's saves")

// Statuses a reminder moves through, once.
const (
	StatusPending = "pending"
	StatusSent    = "sent"
)

// MaxListed bounds one list. A person sets tens of reminders, not thousands,
// and one bounded query serves the whole screen.
const MaxListed = 200

// ValidationError names the field a caller can fix.
type ValidationError struct {
	Field  string
	Reason string
}

func (e *ValidationError) Error() string { return e.Field + " " + e.Reason }

// Record is one stored reminder.
type Record struct {
	ID        string
	UserID    string
	SaveID    *string
	Title     string
	Body      string
	FireAt    time.Time
	Status    string
	CreatedAt time.Time
	SentAt    *time.Time
}

// New is what Create stores. The id is minted here so the row and its
// schedule entry carry the same one from the start.
type New struct {
	ID     string
	Title  string
	Body   string
	FireAt time.Time
	SaveID *string
}

// Reminder is the wire shape, field for field what the Python API answered.
type Reminder struct {
	ID        string  `json:"id"`
	Title     string  `json:"title"`
	Body      string  `json:"body"`
	FireAt    string  `json:"fire_at"`
	Status    string  `json:"status"`
	ReelID    *string `json:"reel_id"`
	CreatedAt *string `json:"created_at"`
	SentAt    *string `json:"sent_at"`
}

// CreateParams is a request as the client sent it.
type CreateParams struct {
	Title  string
	Body   string
	FireAt string
	ReelID *string
}

// Store is the persistence a reminder needs. postgres implements it.
type Store interface {
	List(ctx context.Context, userID string, limit int) ([]Record, error)
	Create(ctx context.Context, userID string, reminder New) (Record, error)
	Delete(ctx context.Context, userID, reminderID string) error
	// Claim moves a pending reminder to sent and returns it. A reminder that
	// is gone or already sent is ErrNotFound, so two claims fire it once.
	Claim(ctx context.Context, reminderID string, now time.Time) (Record, error)
	// Pending pages every pending reminder by id, for re-seeding the schedule.
	Pending(ctx context.Context, afterID string, limit int) ([]Record, error)
}

// Schedule is the due set. RedisSchedule implements it.
type Schedule interface {
	Add(ctx context.Context, reminderID string, fireAt time.Time) error
	Remove(ctx context.Context, reminderID string) error
	// Due lists members whose time has come, oldest first.
	Due(ctx context.Context, now time.Time, limit int) ([]string, error)
	// Take removes one member and reports whether this caller removed it. It
	// is the claim between workers: exactly one of them gets true.
	Take(ctx context.Context, reminderID string) (bool, error)
}

// Service is the user-facing half: list, create, delete.
type Service struct {
	store Store
	// schedule is nil where Redis is not configured. Reminders are still
	// stored, and a worker seeds them into its schedule when it starts.
	schedule Schedule
	logger   *slog.Logger
}

func NewService(store Store, schedule Schedule, logger *slog.Logger) *Service {
	return &Service{store: store, schedule: schedule, logger: logger}
}

// List returns this user's reminders, soonest first.
func (s *Service) List(ctx context.Context, userID string) ([]Reminder, error) {
	records, err := s.store.List(ctx, userID, MaxListed)
	if err != nil {
		return nil, err
	}
	list := make([]Reminder, 0, len(records))
	for _, record := range records {
		list = append(list, Present(record))
	}
	return list, nil
}

// Create stores a reminder and schedules it. The row is written first so a
// worker that sees the schedule entry always finds the row behind it; if the
// schedule cannot take it, the row is removed again and the caller retries,
// rather than holding a reminder that would never fire.
func (s *Service) Create(ctx context.Context, userID string, params CreateParams) (Reminder, error) {
	title := strings.TrimSpace(params.Title)
	if title == "" {
		return Reminder{}, &ValidationError{Field: "title", Reason: "a reminder title is required"}
	}
	fireAt, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(params.FireAt))
	if err != nil {
		return Reminder{}, &ValidationError{Field: "fire_at", Reason: "must be an RFC 3339 timestamp"}
	}
	saveID, err := optionalSaveID(params.ReelID)
	if err != nil {
		return Reminder{}, err
	}

	record, err := s.store.Create(ctx, userID, New{
		ID:     uuid.NewString(),
		Title:  title,
		Body:   strings.TrimSpace(params.Body),
		FireAt: fireAt.UTC(),
		SaveID: saveID,
	})
	if err != nil {
		return Reminder{}, err
	}

	if s.schedule != nil {
		if err := s.schedule.Add(ctx, record.ID, record.FireAt); err != nil {
			if undo := s.store.Delete(ctx, userID, record.ID); undo != nil {
				s.logger.Error("removing an unscheduled reminder failed", "error", undo)
			}
			return Reminder{}, err
		}
	}
	return Present(record), nil
}

// Delete removes one of this user's reminders and its schedule entry. A
// schedule entry left behind is harmless: the worker finds no row and drops it.
func (s *Service) Delete(ctx context.Context, userID, reminderID string) error {
	if err := s.store.Delete(ctx, userID, reminderID); err != nil {
		return err
	}
	if s.schedule != nil {
		if err := s.schedule.Remove(ctx, reminderID); err != nil {
			s.logger.Warn("removing a deleted reminder from the schedule failed", "error", err)
		}
	}
	return nil
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
func Present(record Record) Reminder {
	return Reminder{
		ID:        record.ID,
		Title:     record.Title,
		Body:      record.Body,
		FireAt:    record.FireAt.UTC().Format(time.RFC3339Nano),
		Status:    record.Status,
		ReelID:    record.SaveID,
		CreatedAt: isoTime(&record.CreatedAt),
		SentAt:    isoTime(record.SentAt),
	}
}

func isoTime(value *time.Time) *string {
	if value == nil {
		return nil
	}
	formatted := value.UTC().Format(time.RFC3339Nano)
	return &formatted
}
