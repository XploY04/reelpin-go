package reminders

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/XploY04/reelpin-go/internal/notify"
)

var clock = time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)

func scheduled(t *testing.T, store *fakeStore, schedule *fakeSchedule, fireAt time.Time) Record {
	t.Helper()
	service := NewService(store, schedule, discard())
	reminder, err := service.Create(context.Background(), userID, CreateParams{
		Title: "Concert", Body: "Doors at 8", FireAt: fireAt.Format(time.RFC3339),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return store.rows[reminder.ID]
}

func dispatcher(store *fakeStore, schedule *fakeSchedule, notifier *fakeNotifier) *Dispatcher {
	return NewDispatcher(store, schedule, notifier, discard(), func() time.Time { return clock })
}

func TestOnlyDueRemindersFire(t *testing.T) {
	store, schedule, notifier := newFakeStore(), newFakeSchedule(), &fakeNotifier{}
	due := scheduled(t, store, schedule, clock.Add(-time.Minute))
	later := scheduled(t, store, schedule, clock.Add(time.Hour))

	fired, err := dispatcher(store, schedule, notifier).Fire(context.Background())
	if err != nil {
		t.Fatalf("Fire: %v", err)
	}
	if fired != 1 || notifier.count() != 1 {
		t.Fatalf("fired %d, sent %d, want exactly the due one", fired, notifier.count())
	}
	if store.rows[due.ID].Status != StatusSent || store.rows[due.ID].SentAt == nil {
		t.Errorf("the fired reminder was not marked sent: %+v", store.rows[due.ID])
	}
	if store.rows[later.ID].Status != StatusPending || !schedule.has(later.ID) {
		t.Error("a reminder that is not due yet was touched")
	}
	if store.claims != 1 {
		t.Errorf("%d claims, want PostgreSQL read only for the due member", store.claims)
	}
}

func TestANothingDueTickNeverReadsPostgres(t *testing.T) {
	store, schedule := newFakeStore(), newFakeSchedule()
	scheduled(t, store, schedule, clock.Add(time.Hour))

	if _, err := dispatcher(store, schedule, &fakeNotifier{}).Fire(context.Background()); err != nil {
		t.Fatal(err)
	}
	if store.claims != 0 || store.pending != 0 {
		t.Fatalf("an idle tick read the table (%d claims, %d pending reads)", store.claims, store.pending)
	}
}

func TestTwoWorkersFireAReminderOnce(t *testing.T) {
	store, schedule, notifier := newFakeStore(), newFakeSchedule(), &fakeNotifier{}
	for range 20 {
		scheduled(t, store, schedule, clock.Add(-time.Second))
	}

	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := dispatcher(store, schedule, notifier).Fire(context.Background()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()

	if notifier.count() != 20 {
		t.Fatalf("sent %d pushes for 20 reminders", notifier.count())
	}
}

func TestADeletedReminderNeverFires(t *testing.T) {
	store, schedule, notifier := newFakeStore(), newFakeSchedule(), &fakeNotifier{}
	reminder := scheduled(t, store, schedule, clock.Add(-time.Second))
	// The row goes but the schedule entry stays, as when Redis was briefly
	// unreachable during the delete.
	delete(store.rows, reminder.ID)

	if _, err := dispatcher(store, schedule, notifier).Fire(context.Background()); err != nil {
		t.Fatal(err)
	}
	if notifier.count() != 0 {
		t.Fatal("a deleted reminder fired")
	}
	if schedule.has(reminder.ID) {
		t.Error("the stale entry was not dropped")
	}
}

func TestAnUnreadableRecordIsRetriedNextTick(t *testing.T) {
	store, schedule, notifier := newFakeStore(), newFakeSchedule(), &fakeNotifier{}
	reminder := scheduled(t, store, schedule, clock.Add(-time.Second))
	store.claimErr = errFake

	if _, err := dispatcher(store, schedule, notifier).Fire(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !schedule.has(reminder.ID) {
		t.Fatal("a reminder whose claim failed was dropped from the schedule")
	}

	store.claimErr = nil
	if fired, _ := dispatcher(store, schedule, notifier).Fire(context.Background()); fired != 1 {
		t.Fatalf("fired %d on the retry, want 1", fired)
	}
}

func TestAFailedPushIsNotRetried(t *testing.T) {
	// The claim already marked it sent, and the notification's event key would
	// refuse a second send anyway: a retry loop here would only spin.
	store, schedule := newFakeStore(), newFakeSchedule()
	reminder := scheduled(t, store, schedule, clock.Add(-time.Second))
	notifier := &fakeNotifier{err: notify.ErrRetryable}

	if _, err := dispatcher(store, schedule, notifier).Fire(context.Background()); err != nil {
		t.Fatal(err)
	}
	if schedule.has(reminder.ID) || store.rows[reminder.ID].Status != StatusSent {
		t.Fatal("a failed push was put back")
	}
}

func TestRehydrateSeedsEveryPendingReminder(t *testing.T) {
	// Reminders created while the API had no Redis exist only in PostgreSQL.
	store := newFakeStore()
	service := NewService(store, nil, discard())
	for range 3 {
		if _, err := service.Create(context.Background(), userID, CreateParams{Title: "t", FireAt: clock.Format(time.RFC3339)}); err != nil {
			t.Fatal(err)
		}
	}
	for id, row := range store.rows {
		row.Status = StatusSent
		store.rows[id] = row
		break
	}

	schedule := newFakeSchedule()
	seeded, err := dispatcher(store, schedule, &fakeNotifier{}).Rehydrate(context.Background())
	if err != nil {
		t.Fatalf("Rehydrate: %v", err)
	}
	if seeded != 2 {
		t.Fatalf("seeded %d, want the two still pending", seeded)
	}
	for id, row := range store.rows {
		if schedule.has(id) != (row.Status == StatusPending) {
			t.Errorf("reminder %s (%s) scheduled = %v", id, row.Status, schedule.has(id))
		}
	}
}

func TestRunStopsWithItsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- dispatcher(newFakeStore(), newFakeSchedule(), &fakeNotifier{}).Run(ctx, time.Millisecond)
	}()
	time.Sleep(5 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not stop")
	}
}

func TestAReminderPushRoutesLikePython(t *testing.T) {
	onReel := Notification(Record{ID: "r1", UserID: userID, SaveID: ptr(saveID), Title: "Concert", Body: "Doors"})
	if onReel.Target != "reel" || onReel.TargetID != saveID || onReel.Type != "reminder" {
		t.Errorf("on a save: %+v", onReel)
	}
	if onReel.EventKey != "reminder:r1" || onReel.Data["reminder_id"] != "r1" || onReel.Data["reel_id"] != saveID {
		t.Errorf("on a save: %+v", onReel)
	}

	alone := Notification(Record{ID: "r2", UserID: userID, Title: "Call mum"})
	if alone.Target != "reminder" || alone.TargetID != "r2" || alone.Data["reel_id"] != "" {
		t.Errorf("on nothing: %+v", alone)
	}
}

func TestNoDevicesStillCountsAsFired(t *testing.T) {
	store, schedule := newFakeStore(), newFakeSchedule()
	scheduled(t, store, schedule, clock.Add(-time.Second))
	fired, err := dispatcher(store, schedule, &fakeNotifier{err: notify.ErrNoDeviceTokens}).Fire(context.Background())
	if err != nil || fired != 1 {
		t.Fatalf("fired %d, err %v", fired, err)
	}
}
