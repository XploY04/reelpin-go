//go:build integration

package postgres

import (
	"context"
	"testing"
	"time"
)

func TestEntitlementsActiveProOnlyReturnsGrantsInForce(t *testing.T) {
	pool := rawPool(t)
	ctx := context.Background()

	const (
		future   = "aaaaaaaa-1111-4111-8111-000000000001"
		lapsed   = "aaaaaaaa-1111-4111-8111-000000000002"
		noExpiry = "aaaaaaaa-1111-4111-8111-000000000003"
	)
	grantEnds := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Microsecond)
	for _, grant := range []struct {
		userID  string
		expires any
	}{
		{future, grantEnds},
		{lapsed, time.Now().Add(-time.Hour)},
		{noExpiry, nil},
	} {
		if _, err := pool.Exec(ctx, `INSERT INTO auth.users (id) VALUES ($1)`, grant.userID); err != nil {
			t.Fatalf("seeding user %s: %v", grant.userID, err)
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO reelpin.user_entitlements (user_id, pro_expires_at) VALUES ($1, $2)`,
			grant.userID, grant.expires,
		); err != nil {
			t.Fatalf("seeding %s: %v", grant.userID, err)
		}
	}

	active, err := NewEntitlements(pool).ActivePro(ctx)
	if err != nil {
		t.Fatalf("ActivePro: %v", err)
	}

	if got, ok := active[future]; !ok || !got.Equal(grantEnds) {
		t.Errorf("future grant = %v (present %v), want %v", got, ok, grantEnds)
	}
	if _, ok := active[lapsed]; ok {
		t.Errorf("a lapsed grant is reported as active")
	}
	if _, ok := active[noExpiry]; ok {
		t.Errorf("a grant with no expiry is reported as active")
	}
	if len(active) != 1 {
		t.Errorf("active = %v, want only the future grant", active)
	}
}
