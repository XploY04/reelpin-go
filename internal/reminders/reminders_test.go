package reminders

import (
	"context"
	"errors"
	"testing"
	"time"
)

const (
	userID  = "11111111-1111-4111-8111-111111111111"
	otherID = "44444444-4444-4444-8444-444444444444"
	saveID  = "22222222-2222-4222-8222-222222222222"
)

func TestCreatingAReminderStoresAndSchedulesIt(t *testing.T) {
	store, schedule := newFakeStore(), newFakeSchedule()
	service := NewService(store, schedule, discard())

	reminder, err := service.Create(context.Background(), userID, CreateParams{
		Title: "  Concert  ", Body: " Doors at 8 ", FireAt: "2026-11-01T14:30:00+05:30",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if reminder.Title != "Concert" || reminder.Body != "Doors at 8" || reminder.Status != StatusPending {
		t.Errorf("reminder = %+v", reminder)
	}
	if reminder.FireAt != "2026-11-01T09:00:00Z" {
		t.Errorf("fire_at = %q, want the instant in UTC", reminder.FireAt)
	}
	if reminder.ReelID != nil || reminder.SentAt != nil {
		t.Errorf("reel_id and sent_at should be null: %+v", reminder)
	}
	if !schedule.has(reminder.ID) {
		t.Fatal("the reminder was stored but never scheduled, so it would never fire")
	}
}

func TestAReminderTheScheduleRefusesIsNotKept(t *testing.T) {
	// A stored reminder with no schedule entry would show in the list and
	// never fire. Better the request fails and the client retries.
	store, schedule := newFakeStore(), newFakeSchedule()
	schedule.addErr = errFake
	service := NewService(store, schedule, discard())

	_, err := service.Create(context.Background(), userID, CreateParams{Title: "t", FireAt: "2026-11-01T09:00:00Z"})
	if !errors.Is(err, errFake) {
		t.Fatalf("err = %v, want the schedule's failure", err)
	}
	if len(store.rows) != 0 {
		t.Fatalf("%d rows left behind with no schedule entry", len(store.rows))
	}
}

func TestWithoutRedisAReminderIsStillStored(t *testing.T) {
	store := newFakeStore()
	service := NewService(store, nil, discard())

	if _, err := service.Create(context.Background(), userID, CreateParams{Title: "t", FireAt: "2026-11-01T09:00:00Z"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len(store.rows) != 1 {
		t.Fatal("the reminder was not stored; a worker seeds it into Redis when it starts")
	}
}

func TestAReminderRequestIsValidated(t *testing.T) {
	tests := []struct {
		name   string
		params CreateParams
		field  string
	}{
		{"blank title", CreateParams{Title: "   ", FireAt: "2026-11-01T09:00:00Z"}, "title"},
		{"no fire_at", CreateParams{Title: "t"}, "fire_at"},
		{"fire_at without an offset", CreateParams{Title: "t", FireAt: "2026-11-01T09:00:00"}, "fire_at"},
		{"reel_id not an id", CreateParams{Title: "t", FireAt: "2026-11-01T09:00:00Z", ReelID: ptr("abc")}, "reel_id"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewService(newFakeStore(), newFakeSchedule(), discard()).Create(context.Background(), userID, tt.params)
			var invalid *ValidationError
			if !errors.As(err, &invalid) || invalid.Field != tt.field {
				t.Fatalf("err = %v, want a validation error on %s", err, tt.field)
			}
		})
	}
}

func TestAReminderCannotPointAtSomeoneElsesSave(t *testing.T) {
	store := newFakeStore()
	store.saves[saveID] = otherID
	_, err := NewService(store, newFakeSchedule(), discard()).Create(context.Background(), userID,
		CreateParams{Title: "t", FireAt: "2026-11-01T09:00:00Z", ReelID: ptr(saveID)})
	if !errors.Is(err, ErrUnknownSave) {
		t.Fatalf("err = %v, want ErrUnknownSave", err)
	}
}

func TestDeletingAReminderUnschedulesIt(t *testing.T) {
	store, schedule := newFakeStore(), newFakeSchedule()
	service := NewService(store, schedule, discard())
	reminder, err := service.Create(context.Background(), userID, CreateParams{Title: "t", FireAt: "2026-11-01T09:00:00Z"})
	if err != nil {
		t.Fatal(err)
	}

	if err := service.Delete(context.Background(), otherID, reminder.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another user's delete = %v, want ErrNotFound", err)
	}
	if !schedule.has(reminder.ID) {
		t.Fatal("another user's delete unscheduled the reminder")
	}

	if err := service.Delete(context.Background(), userID, reminder.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if schedule.has(reminder.ID) {
		t.Error("a deleted reminder is still scheduled")
	}
}

func TestPresentKeepsThePythonShape(t *testing.T) {
	sent := time.Date(2026, 11, 1, 9, 0, 5, 0, time.UTC)
	reminder := Present(Record{
		ID: "r", Title: "t", FireAt: time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC),
		Status: StatusSent, SaveID: ptr(saveID), CreatedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), SentAt: &sent,
	})
	if reminder.ReelID == nil || *reminder.ReelID != saveID {
		t.Errorf("reel_id = %v, want the save id", reminder.ReelID)
	}
	if reminder.SentAt == nil || *reminder.SentAt != "2026-11-01T09:00:05Z" {
		t.Errorf("sent_at = %v", reminder.SentAt)
	}
}

func ptr(value string) *string { return &value }
