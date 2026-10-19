package postgres

import (
	"context"
	"time"
)

// Entitlements reads the hand-granted Pro table. It writes nothing: grants are
// made out of band today.
type Entitlements struct {
	db Querier
}

func NewEntitlements(db Querier) *Entitlements {
	return &Entitlements{db: db}
}

// ActivePro returns every user whose grant has not expired, mapped to its
// expiry. The caller caches the whole set and re-checks the expiry per request,
// so a grant that lapses between reloads is not served stale.
func (e *Entitlements) ActivePro(ctx context.Context) (map[string]time.Time, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()

	rows, err := e.db.Query(ctx,
		`SELECT user_id::text, pro_expires_at
		 FROM user_entitlements
		 WHERE pro_expires_at IS NOT NULL AND pro_expires_at > now()`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	active := map[string]time.Time{}
	for rows.Next() {
		var (
			userID  string
			expires time.Time
		)
		if err := rows.Scan(&userID, &expires); err != nil {
			return nil, err
		}
		active[userID] = expires
	}
	return active, rows.Err()
}
