package reminders

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/XploY04/reelpin-go/internal/notify"
)

var errFake = errors.New("fake failure")

func discard() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

// fakeStore is the table in memory. Claim keeps the status guard, which is
// the property the dispatcher's exactly-once relies on.
type fakeStore struct {
	mu       sync.Mutex
	rows     map[string]Record
	saves    map[string]string // save id -> owner
	claimErr error
	claims   int
	pending  int
}

func newFakeStore() *fakeStore {
	return &fakeStore{rows: map[string]Record{}, saves: map[string]string{}}
}

func (f *fakeStore) List(_ context.Context, userID string, limit int) ([]Record, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	list := []Record{}
	for _, row := range f.rows {
		if row.UserID == userID {
			list = append(list, row)
		}
	}
	sort.Slice(list, func(i, j int) bool { return list[i].FireAt.Before(list[j].FireAt) })
	if len(list) > limit {
		list = list[:limit]
	}
	return list, nil
}

func (f *fakeStore) Create(_ context.Context, userID string, reminder New) (Record, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if reminder.SaveID != nil && f.saves[*reminder.SaveID] != userID {
		return Record{}, ErrUnknownSave
	}
	row := Record{
		ID: reminder.ID, UserID: userID, SaveID: reminder.SaveID, Title: reminder.Title,
		Body: reminder.Body, FireAt: reminder.FireAt, Status: StatusPending, CreatedAt: time.Unix(0, 0),
	}
	f.rows[row.ID] = row
	return row, nil
}

func (f *fakeStore) Delete(_ context.Context, userID, reminderID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[reminderID]
	if !ok || row.UserID != userID {
		return ErrNotFound
	}
	delete(f.rows, reminderID)
	return nil
}

func (f *fakeStore) Claim(_ context.Context, reminderID string, now time.Time) (Record, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.claims++
	if f.claimErr != nil {
		return Record{}, f.claimErr
	}
	row, ok := f.rows[reminderID]
	if !ok || row.Status != StatusPending {
		return Record{}, ErrNotFound
	}
	row.Status, row.SentAt = StatusSent, &now
	f.rows[reminderID] = row
	return row, nil
}

func (f *fakeStore) Pending(_ context.Context, afterID string, limit int) ([]Record, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pending++
	list := []Record{}
	for _, row := range f.rows {
		if row.Status == StatusPending && row.ID > afterID {
			list = append(list, row)
		}
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
	if len(list) > limit {
		list = list[:limit]
	}
	return list, nil
}

// fakeSchedule is a sorted set in memory, with the same Take semantics as
// ZREM: only the caller that removed the member gets true.
type fakeSchedule struct {
	mu     sync.Mutex
	scores map[string]time.Time
	addErr error
}

func newFakeSchedule() *fakeSchedule { return &fakeSchedule{scores: map[string]time.Time{}} }

func (f *fakeSchedule) Add(_ context.Context, reminderID string, fireAt time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.addErr != nil {
		return f.addErr
	}
	f.scores[reminderID] = fireAt
	return nil
}

func (f *fakeSchedule) Remove(_ context.Context, reminderID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.scores, reminderID)
	return nil
}

func (f *fakeSchedule) Due(_ context.Context, now time.Time, limit int) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ids := []string{}
	for id, at := range f.scores {
		if !at.After(now) {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return f.scores[ids[i]].Before(f.scores[ids[j]]) })
	if len(ids) > limit {
		ids = ids[:limit]
	}
	return ids, nil
}

func (f *fakeSchedule) Take(_ context.Context, reminderID string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.scores[reminderID]; !ok {
		return false, nil
	}
	delete(f.scores, reminderID)
	return true, nil
}

func (f *fakeSchedule) has(reminderID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.scores[reminderID]
	return ok
}

type fakeNotifier struct {
	mu   sync.Mutex
	sent []notify.Notification
	err  error
}

func (f *fakeNotifier) SendToUser(_ context.Context, notification notify.Notification) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, notification)
	return "notification-id", f.err
}

func (f *fakeNotifier) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}
