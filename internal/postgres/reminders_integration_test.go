//go:build integration

package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/XploY04/reelpin-go/internal/pricetracks"
	"github.com/XploY04/reelpin-go/internal/reminders"
	"github.com/google/uuid"
)

func TestRemindersAreScopedClaimedOnceAndGoWithTheirSave(t *testing.T) {
	collections := collectionPool(t)
	store := NewReminders(collections.pool)
	ctx := context.Background()
	save := seedSave(t, collections, ownerID, "reminder1")
	fireAt := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)

	if _, err := store.Create(ctx, strangerID, reminders.New{
		ID: uuid.NewString(), Title: "t", FireAt: fireAt, SaveID: &save,
	}); !errors.Is(err, reminders.ErrUnknownSave) {
		t.Fatalf("a reminder on someone else's save = %v, want ErrUnknownSave", err)
	}

	created, err := store.Create(ctx, ownerID, reminders.New{
		ID: uuid.NewString(), Title: "Concert", Body: "Doors", FireAt: fireAt, SaveID: &save,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.Status != reminders.StatusPending || created.SaveID == nil || *created.SaveID != save {
		t.Fatalf("created = %+v", created)
	}
	loose, err := store.Create(ctx, ownerID, reminders.New{ID: uuid.NewString(), Title: "Call", FireAt: fireAt.Add(time.Hour)})
	if err != nil {
		t.Fatalf("Create without a save: %v", err)
	}

	if list, err := store.List(ctx, strangerID, 10); err != nil || len(list) != 0 {
		t.Fatalf("a stranger sees %d reminders (%v)", len(list), err)
	}
	if list, err := store.List(ctx, ownerID, 10); err != nil || len(list) != 2 || list[0].ID != created.ID {
		t.Fatalf("owner list = %+v (%v), want two, soonest first", list, err)
	}

	pending, err := store.Pending(ctx, "", 10)
	if err != nil || len(pending) != 2 {
		t.Fatalf("pending = %d (%v)", len(pending), err)
	}

	if _, err := store.Claim(ctx, created.ID, fireAt); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if _, err := store.Claim(ctx, created.ID, fireAt); !errors.Is(err, reminders.ErrNotFound) {
		t.Fatalf("second claim = %v, want ErrNotFound", err)
	}
	if pending, _ := store.Pending(ctx, "", 10); len(pending) != 1 {
		t.Fatalf("pending after a claim = %d, want 1", len(pending))
	}

	if err := store.Delete(ctx, strangerID, loose.ID); !errors.Is(err, reminders.ErrNotFound) {
		t.Fatalf("a stranger's delete = %v", err)
	}

	if _, err := collections.pool.Exec(ctx, `DELETE FROM reelpin.user_saves WHERE id = $1`, save); err != nil {
		t.Fatal(err)
	}
	if list, _ := store.List(ctx, ownerID, 10); len(list) != 1 || list[0].ID != loose.ID {
		t.Fatalf("after deleting the save, list = %+v", list)
	}
}

func TestPriceTracksRecordChecksAndNotifyOnce(t *testing.T) {
	collections := collectionPool(t)
	store := NewPriceTracks(collections.pool)
	ctx := context.Background()

	created, err := store.Create(ctx, ownerID, pricetracks.New{URL: "https://shop.example/p", Title: "Shoes", TargetPrice: 1299.5})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.TargetPrice != 1299.5 || !created.Active || created.LastPrice != nil || created.LastCheckedAt != nil {
		t.Fatalf("created = %+v", created)
	}
	if list, _ := store.List(ctx, strangerID, 10); len(list) != 0 {
		t.Fatal("a stranger sees the track")
	}

	now := time.Now().UTC()
	due, err := store.Due(ctx, now.Add(-20*time.Hour), "", 10)
	if err != nil || len(due) != 1 {
		t.Fatalf("due = %d (%v), want the unchecked track", len(due), err)
	}

	price := 999.0
	if err := store.RecordCheck(ctx, created.ID, pricetracks.Check{CheckedAt: now, LastPrice: &price, Notified: true}); err != nil {
		t.Fatalf("RecordCheck: %v", err)
	}
	later := now.Add(48 * time.Hour)
	if err := store.RecordCheck(ctx, created.ID, pricetracks.Check{CheckedAt: later}); err != nil {
		t.Fatalf("RecordCheck: %v", err)
	}

	list, err := store.List(ctx, ownerID, 10)
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %+v (%v)", list, err)
	}
	if list[0].NotifiedAt == nil || !list[0].NotifiedAt.Equal(now.Truncate(time.Microsecond)) {
		t.Errorf("notified_at = %v, want the first check's time kept", list[0].NotifiedAt)
	}
	if list[0].LastPrice != nil {
		t.Errorf("last_price = %v, want the latest check's null", *list[0].LastPrice)
	}

	if due, _ := store.Due(ctx, later.Add(-20*time.Hour), "", 10); len(due) != 0 {
		t.Fatalf("a freshly checked track is due again")
	}
	if err := store.Delete(ctx, strangerID, created.ID); !errors.Is(err, pricetracks.ErrNotFound) {
		t.Fatalf("a stranger's delete = %v", err)
	}
	if err := store.Delete(ctx, ownerID, created.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}
