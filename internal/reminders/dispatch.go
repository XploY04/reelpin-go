package reminders

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/XploY04/reelpin-go/internal/notify"
	"github.com/google/uuid"
)

// Notifier is the push path. notify.Service implements it.
type Notifier interface {
	SendToUser(ctx context.Context, notification notify.Notification) (string, error)
}

// TickInterval is how often a worker asks Redis what is due. Asking costs one
// sorted-set range, so it can be frequent; PostgreSQL is read only for members
// that are already due.
const TickInterval = 10 * time.Second

// FireBatch bounds one tick. A backlog drains over several ticks rather than
// holding one tick open.
const FireBatch = 100

// rehydratePage is how many pending reminders one startup read takes at a time.
const rehydratePage = 1000

// Dispatcher is the worker half: it seeds the schedule from the record once at
// startup and then fires whatever comes due.
type Dispatcher struct {
	store    Store
	schedule Schedule
	notifier Notifier
	logger   *slog.Logger
	now      func() time.Time
}

func NewDispatcher(store Store, schedule Schedule, notifier Notifier, logger *slog.Logger, now func() time.Time) *Dispatcher {
	if now == nil {
		now = time.Now
	}
	return &Dispatcher{store: store, schedule: schedule, notifier: notifier, logger: logger, now: now}
}

// Run seeds the schedule and then ticks until the context ends.
func (d *Dispatcher) Run(ctx context.Context, interval time.Duration) error {
	if seeded, err := d.Rehydrate(ctx); err != nil {
		// The ticks still fire everything Redis already holds, so a failed
		// seed degrades to "reminders created without Redis wait for the next
		// start" rather than stopping the worker.
		d.logger.Error("seeding the reminder schedule failed", "error", err)
	} else if seeded > 0 {
		d.logger.Info("seeded the reminder schedule", "count", seeded)
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
		fired, err := d.Fire(ctx)
		if err != nil && !errors.Is(err, context.Canceled) {
			d.logger.Error("firing due reminders failed", "error", err)
			continue
		}
		if fired > 0 {
			d.logger.Info("fired due reminders", "count", fired)
		}
	}
}

// Rehydrate puts every pending reminder back into the schedule. It is the one
// full read of the table, and it happens once per worker start: it covers a
// Redis that lost its data and an API that ran without Redis. Adding a member
// that is already there only rewrites its score to the same value.
func (d *Dispatcher) Rehydrate(ctx context.Context) (int, error) {
	seeded, after := 0, ""
	for {
		page, err := d.store.Pending(ctx, after, rehydratePage)
		if err != nil {
			return seeded, err
		}
		for _, record := range page {
			if err := d.schedule.Add(ctx, record.ID, record.FireAt); err != nil {
				return seeded, err
			}
			seeded++
		}
		if len(page) < rehydratePage {
			return seeded, nil
		}
		after = page[len(page)-1].ID
	}
}

// Fire sends every reminder whose time has come and reports how many it sent.
func (d *Dispatcher) Fire(ctx context.Context) (int, error) {
	now := d.now().UTC()
	due, err := d.schedule.Due(ctx, now, FireBatch)
	if err != nil {
		return 0, err
	}

	fired := 0
	for _, id := range due {
		taken, err := d.schedule.Take(ctx, id)
		if err != nil {
			d.logger.Warn("taking a due reminder failed", "error", err)
			continue
		}
		if !taken {
			// Another worker took it between the range and the take.
			continue
		}
		if _, err := uuid.Parse(id); err != nil {
			d.logger.Warn("dropping a schedule entry that is not a reminder id")
			continue
		}

		record, err := d.store.Claim(ctx, id, now)
		if errors.Is(err, ErrNotFound) {
			// Deleted, or already fired by an earlier claim.
			continue
		}
		if err != nil {
			// The record is unreadable right now; put the entry back so the
			// next tick tries again rather than losing the reminder.
			if again := d.schedule.Add(ctx, id, now); again != nil {
				d.logger.Error("rescheduling an unclaimed reminder failed", "error", again)
			}
			d.logger.Error("claiming a due reminder failed", "error", err)
			continue
		}

		// The claim already marked it sent, as the Python worker did, so a
		// failed push is logged and not retried: the event key would refuse a
		// second send anyway.
		_, err = d.notifier.SendToUser(ctx, Notification(record))
		switch {
		case err == nil, errors.Is(err, notify.ErrNoDeviceTokens):
			fired++
		default:
			d.logger.Error("sending a reminder failed", "reminder_id", record.ID, "error", err)
		}
	}
	return fired, nil
}

// Notification is the push a reminder becomes. Target and data match what
// the Python worker sent, so the app's tap handler lands in the same place.
func Notification(record Record) notify.Notification {
	reelID := ""
	if record.SaveID != nil {
		reelID = *record.SaveID
	}
	target, targetID := "reminder", record.ID
	if reelID != "" {
		target, targetID = "reel", reelID
	}
	return notify.Notification{
		UserID:   record.UserID,
		Title:    record.Title,
		Body:     record.Body,
		Type:     "reminder",
		Target:   target,
		TargetID: targetID,
		EventKey: "reminder:" + record.ID,
		Data:     map[string]string{"reminder_id": record.ID, "reel_id": reelID},
	}
}
