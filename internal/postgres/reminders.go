package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/XploY04/reelpin-go/internal/reminders"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const reminderColumns = `id::text, user_id::text, save_id::text, title, body, fire_at, status, created_at, sent_at`

// Reminders is the pgx implementation of reminders.Store. Every user-facing
// statement is scoped by the token's user id; Claim and Pending are the
// worker's, and are scoped by the reminder id the schedule handed over.
type Reminders struct {
	pool *pgxpool.Pool
}

func NewReminders(pool *pgxpool.Pool) *Reminders {
	return &Reminders{pool: pool}
}

func (r *Reminders) List(ctx context.Context, userID string, limit int) ([]reminders.Record, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()

	rows, err := r.pool.Query(ctx, `
		SELECT `+reminderColumns+`
		FROM reelpin.reminders
		WHERE user_id = $1
		ORDER BY fire_at, id
		LIMIT $2`, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("listing reminders: %w", err)
	}
	return collectReminders(rows)
}

// Create inserts only when the reel, if one is named, is this user's own save,
// so a reminder can never point at someone else's.
func (r *Reminders) Create(ctx context.Context, userID string, reminder reminders.New) (reminders.Record, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()

	rows, err := r.pool.Query(ctx, `
		INSERT INTO reelpin.reminders (id, user_id, save_id, title, body, fire_at)
		SELECT $1::uuid, $2::uuid, $3::uuid, $4::text, $5::text, $6::timestamptz
		WHERE $3::uuid IS NULL OR EXISTS (
			SELECT 1 FROM reelpin.user_saves WHERE id = $3::uuid AND user_id = $2::uuid)
		RETURNING `+reminderColumns,
		reminder.ID, userID, reminder.SaveID, reminder.Title, reminder.Body, reminder.FireAt)
	if err != nil {
		return reminders.Record{}, fmt.Errorf("creating a reminder: %w", err)
	}
	created, err := collectReminders(rows)
	if err != nil {
		return reminders.Record{}, err
	}
	if len(created) == 0 {
		return reminders.Record{}, reminders.ErrUnknownSave
	}
	return created[0], nil
}

func (r *Reminders) Delete(ctx context.Context, userID, reminderID string) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()

	tag, err := r.pool.Exec(ctx,
		`DELETE FROM reelpin.reminders WHERE id = $1 AND user_id = $2`, reminderID, userID)
	if err != nil {
		return fmt.Errorf("deleting a reminder: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return reminders.ErrNotFound
	}
	return nil
}

// Claim is the pending-to-sent move. The status guard is what makes it the
// claim: a second caller updates nothing and gets ErrNotFound.
func (r *Reminders) Claim(ctx context.Context, reminderID string, now time.Time) (reminders.Record, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()

	rows, err := r.pool.Query(ctx, `
		UPDATE reelpin.reminders SET status = 'sent', sent_at = $2
		WHERE id = $1 AND status = 'pending'
		RETURNING `+reminderColumns, reminderID, now)
	if err != nil {
		return reminders.Record{}, fmt.Errorf("claiming a reminder: %w", err)
	}
	claimed, err := collectReminders(rows)
	if err != nil {
		return reminders.Record{}, err
	}
	if len(claimed) == 0 {
		return reminders.Record{}, reminders.ErrNotFound
	}
	return claimed[0], nil
}

func (r *Reminders) Pending(ctx context.Context, afterID string, limit int) ([]reminders.Record, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()

	rows, err := r.pool.Query(ctx, `
		SELECT `+reminderColumns+`
		FROM reelpin.reminders
		WHERE status = 'pending' AND ($1 = '' OR id > $1::uuid)
		ORDER BY id
		LIMIT $2`, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("reading pending reminders: %w", err)
	}
	return collectReminders(rows)
}

func collectReminders(rows pgx.Rows) ([]reminders.Record, error) {
	defer rows.Close()
	list := []reminders.Record{}
	for rows.Next() {
		var record reminders.Record
		if err := rows.Scan(&record.ID, &record.UserID, &record.SaveID, &record.Title, &record.Body,
			&record.FireAt, &record.Status, &record.CreatedAt, &record.SentAt); err != nil {
			return nil, fmt.Errorf("reading a reminder: %w", err)
		}
		list = append(list, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading reminders: %w", err)
	}
	return list, nil
}
