package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/XploY04/reelpin-go/internal/pricetracks"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const priceTrackColumns = `id::text, user_id::text, save_id::text, url, title, target_price::float8,
	currency, last_price::float8, active, last_checked_at, notified_at, created_at`

// PriceTracks is the pgx implementation of pricetracks.Store. Due and
// RecordCheck belong to the daily run and are scoped by track id; everything
// else is scoped by the token's user id.
type PriceTracks struct {
	pool *pgxpool.Pool
}

func NewPriceTracks(pool *pgxpool.Pool) *PriceTracks {
	return &PriceTracks{pool: pool}
}

func (p *PriceTracks) List(ctx context.Context, userID string, limit int) ([]pricetracks.Record, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()

	rows, err := p.pool.Query(ctx, `
		SELECT `+priceTrackColumns+`
		FROM reelpin.price_tracks
		WHERE user_id = $1
		ORDER BY created_at DESC, id DESC
		LIMIT $2`, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("listing price tracks: %w", err)
	}
	return collectPriceTracks(rows)
}

// Create inserts only when the reel, if one is named, is this user's own save.
func (p *PriceTracks) Create(ctx context.Context, userID string, track pricetracks.New) (pricetracks.Record, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()

	rows, err := p.pool.Query(ctx, `
		INSERT INTO reelpin.price_tracks (user_id, save_id, url, title, target_price)
		SELECT $1::uuid, $2::uuid, $3::text, $4::text, $5::numeric
		WHERE $2::uuid IS NULL OR EXISTS (
			SELECT 1 FROM reelpin.user_saves WHERE id = $2::uuid AND user_id = $1::uuid)
		RETURNING `+priceTrackColumns,
		userID, track.SaveID, track.URL, track.Title, track.TargetPrice)
	if err != nil {
		return pricetracks.Record{}, fmt.Errorf("creating a price track: %w", err)
	}
	created, err := collectPriceTracks(rows)
	if err != nil {
		return pricetracks.Record{}, err
	}
	if len(created) == 0 {
		return pricetracks.Record{}, pricetracks.ErrUnknownSave
	}
	return created[0], nil
}

func (p *PriceTracks) Delete(ctx context.Context, userID, trackID string) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()

	tag, err := p.pool.Exec(ctx,
		`DELETE FROM reelpin.price_tracks WHERE id = $1 AND user_id = $2`, trackID, userID)
	if err != nil {
		return fmt.Errorf("deleting a price track: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return pricetracks.ErrNotFound
	}
	return nil
}

func (p *PriceTracks) Due(ctx context.Context, checkedBefore time.Time, afterID string, limit int) ([]pricetracks.Record, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()

	rows, err := p.pool.Query(ctx, `
		SELECT `+priceTrackColumns+`
		FROM reelpin.price_tracks
		WHERE active
		  AND (last_checked_at IS NULL OR last_checked_at <= $1)
		  AND ($2 = '' OR id > $2::uuid)
		ORDER BY id
		LIMIT $3`, checkedBefore, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("reading due price tracks: %w", err)
	}
	return collectPriceTracks(rows)
}

// RecordCheck writes what one read found. notified_at is only ever set, never
// cleared, so a second run cannot notify the same drop again.
func (p *PriceTracks) RecordCheck(ctx context.Context, trackID string, check pricetracks.Check) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()

	_, err := p.pool.Exec(ctx, `
		UPDATE reelpin.price_tracks
		SET last_checked_at = $2,
		    last_price = $3,
		    notified_at = CASE WHEN $4 THEN $2 ELSE notified_at END
		WHERE id = $1`, trackID, check.CheckedAt, check.LastPrice, check.Notified)
	if err != nil {
		return fmt.Errorf("recording a price check: %w", err)
	}
	return nil
}

func collectPriceTracks(rows pgx.Rows) ([]pricetracks.Record, error) {
	defer rows.Close()
	list := []pricetracks.Record{}
	for rows.Next() {
		var record pricetracks.Record
		if err := rows.Scan(&record.ID, &record.UserID, &record.SaveID, &record.URL, &record.Title,
			&record.TargetPrice, &record.Currency, &record.LastPrice, &record.Active,
			&record.LastCheckedAt, &record.NotifiedAt, &record.CreatedAt); err != nil {
			return nil, fmt.Errorf("reading a price track: %w", err)
		}
		list = append(list, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading price tracks: %w", err)
	}
	return list, nil
}
