// Package migrations owns the forward-only SQL that shapes the database. The
// files are embedded so one binary applies them as a deployment step; no API
// replica ever migrates on startup.
package migrations

import (
	"context"
	"embed"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

//go:embed *.sql
var files embed.FS

// advisoryLockKey keeps two deployments from migrating at the same time.
const advisoryLockKey int64 = 774_120_001

// Migration is one embedded file. Version comes from the numeric filename
// prefix, so ordering is explicit and never depends on directory order.
type Migration struct {
	Version int64
	Name    string
	Up      string
	Down    string
}

// Load parses every embedded migration in version order.
func Load() ([]Migration, error) {
	entries, err := files.ReadDir(".")
	if err != nil {
		return nil, fmt.Errorf("reading migrations: %w", err)
	}

	loaded := make([]Migration, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".sql") {
			continue
		}
		prefix, _, found := strings.Cut(name, "_")
		if !found {
			return nil, fmt.Errorf("migration %q has no version prefix", name)
		}
		version, err := strconv.ParseInt(prefix, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("migration %q has an unparseable version: %w", name, err)
		}

		body, err := files.ReadFile(name)
		if err != nil {
			return nil, fmt.Errorf("reading %q: %w", name, err)
		}
		up, down, err := split(string(body))
		if err != nil {
			return nil, fmt.Errorf("migration %q: %w", name, err)
		}
		loaded = append(loaded, Migration{Version: version, Name: name, Up: up, Down: down})
	}

	sort.Slice(loaded, func(i, j int) bool { return loaded[i].Version < loaded[j].Version })
	for i := 1; i < len(loaded); i++ {
		if loaded[i].Version == loaded[i-1].Version {
			return nil, fmt.Errorf("two migrations share version %d", loaded[i].Version)
		}
	}
	return loaded, nil
}

// split cuts a file into its up and down halves at the -- migrate:down marker.
func split(body string) (string, string, error) {
	const upMarker = "-- migrate:up"
	const downMarker = "-- migrate:down"

	if !strings.Contains(body, upMarker) {
		return "", "", fmt.Errorf("missing %q", upMarker)
	}
	_, rest, _ := strings.Cut(body, upMarker)
	up, down, hasDown := strings.Cut(rest, downMarker)
	if !hasDown {
		return strings.TrimSpace(up), "", nil
	}
	return strings.TrimSpace(up), strings.TrimSpace(down), nil
}

// Up applies every migration the database has not recorded yet. Each one runs
// in its own transaction with its version, so a failure leaves the applied
// prefix intact and nothing half-written. Running it twice is a no-op.
func Up(ctx context.Context, databaseURL string) ([]string, error) {
	loaded, err := Load()
	if err != nil {
		return nil, err
	}

	conn, err := connect(ctx, databaseURL)
	if err != nil {
		return nil, err
	}
	defer conn.Close(ctx)

	unlock, err := lock(ctx, conn)
	if err != nil {
		return nil, err
	}
	defer unlock()

	if err := ensureVersionTable(ctx, conn); err != nil {
		return nil, err
	}
	applied, err := appliedVersions(ctx, conn)
	if err != nil {
		return nil, err
	}

	var ran []string
	for _, migration := range loaded {
		if applied[migration.Version] {
			continue
		}

		transaction, err := conn.Begin(ctx)
		if err != nil {
			return ran, fmt.Errorf("starting %s: %w", migration.Name, err)
		}
		if _, err := transaction.Exec(ctx, migration.Up); err != nil {
			_ = transaction.Rollback(ctx)
			return ran, fmt.Errorf("applying %s: %w", migration.Name, err)
		}
		if _, err := transaction.Exec(ctx,
			`INSERT INTO public.schema_migrations (version, name) VALUES ($1, $2)`,
			migration.Version, migration.Name,
		); err != nil {
			_ = transaction.Rollback(ctx)
			return ran, fmt.Errorf("recording %s: %w", migration.Name, err)
		}
		if err := transaction.Commit(ctx); err != nil {
			return ran, fmt.Errorf("committing %s: %w", migration.Name, err)
		}

		// A migration that ran without leaving its version behind would be
		// replayed forever, so the record is verified rather than assumed.
		var recorded int
		if err := conn.QueryRow(ctx,
			`SELECT count(*) FROM public.schema_migrations WHERE version = $1`, migration.Version,
		).Scan(&recorded); err != nil {
			return ran, fmt.Errorf("verifying %s: %w", migration.Name, err)
		}
		if recorded != 1 {
			return ran, fmt.Errorf("%s ran but its version was not recorded", migration.Name)
		}
		ran = append(ran, migration.Name)
	}
	return ran, nil
}

// Status reports every migration and whether the database has it.
func Status(ctx context.Context, databaseURL string) ([]string, error) {
	loaded, err := Load()
	if err != nil {
		return nil, err
	}

	conn, err := connect(ctx, databaseURL)
	if err != nil {
		return nil, err
	}
	defer conn.Close(ctx)

	if err := ensureVersionTable(ctx, conn); err != nil {
		return nil, err
	}
	applied, err := appliedVersions(ctx, conn)
	if err != nil {
		return nil, err
	}

	lines := make([]string, 0, len(loaded))
	for _, migration := range loaded {
		state := "pending"
		if applied[migration.Version] {
			state = "applied"
		}
		lines = append(lines, state+" "+migration.Name)
	}
	return lines, nil
}

// Down rolls back the most recently applied migration. Production never uses
// it: an expand-only migration is corrected by a later migration, not undone.
func Down(ctx context.Context, databaseURL string) (string, error) {
	loaded, err := Load()
	if err != nil {
		return "", err
	}

	conn, err := connect(ctx, databaseURL)
	if err != nil {
		return "", err
	}
	defer conn.Close(ctx)

	unlock, err := lock(ctx, conn)
	if err != nil {
		return "", err
	}
	defer unlock()

	if err := ensureVersionTable(ctx, conn); err != nil {
		return "", err
	}
	applied, err := appliedVersions(ctx, conn)
	if err != nil {
		return "", err
	}

	for i := len(loaded) - 1; i >= 0; i-- {
		migration := loaded[i]
		if !applied[migration.Version] {
			continue
		}
		if strings.TrimSpace(migration.Down) == "" {
			return "", fmt.Errorf("%s has no down section", migration.Name)
		}

		transaction, err := conn.Begin(ctx)
		if err != nil {
			return "", fmt.Errorf("starting rollback of %s: %w", migration.Name, err)
		}
		if _, err := transaction.Exec(ctx, migration.Down); err != nil {
			_ = transaction.Rollback(ctx)
			return "", fmt.Errorf("rolling back %s: %w", migration.Name, err)
		}
		if _, err := transaction.Exec(ctx,
			`DELETE FROM public.schema_migrations WHERE version = $1`, migration.Version,
		); err != nil {
			_ = transaction.Rollback(ctx)
			return "", fmt.Errorf("clearing the version of %s: %w", migration.Name, err)
		}
		if err := transaction.Commit(ctx); err != nil {
			return "", fmt.Errorf("committing the rollback of %s: %w", migration.Name, err)
		}
		return migration.Name, nil
	}
	return "", nil
}

// connect takes one dedicated connection: the advisory lock is session scoped,
// so it must not be handed back to a pool mid-migration.
func connect(ctx context.Context, databaseURL string) (*pgx.Conn, error) {
	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("connecting for migrations: %w", err)
	}
	return conn, nil
}

func lock(ctx context.Context, conn *pgx.Conn) (func(), error) {
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, advisoryLockKey); err != nil {
		return nil, fmt.Errorf("taking the migration lock: %w", err)
	}
	return func() {
		// The unlock runs even when the caller's context is already done.
		_, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, advisoryLockKey)
	}, nil
}

func ensureVersionTable(ctx context.Context, conn *pgx.Conn) error {
	// The table is schema-qualified on purpose. This migration creates a schema
	// named reelpin, which is also the database role, so an unqualified name
	// would resolve to the "$user" schema once it exists and quietly create a
	// second, empty version table.
	_, err := conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS public.schema_migrations (
			version    BIGINT PRIMARY KEY,
			name       TEXT NOT NULL,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`)
	if err != nil {
		return fmt.Errorf("creating public.schema_migrations: %w", err)
	}
	return nil
}

func appliedVersions(ctx context.Context, conn *pgx.Conn) (map[int64]bool, error) {
	rows, err := conn.Query(ctx, `SELECT version FROM public.schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("reading applied migrations: %w", err)
	}
	defer rows.Close()

	applied := map[int64]bool{}
	for rows.Next() {
		var version int64
		if err := rows.Scan(&version); err != nil {
			return nil, fmt.Errorf("reading applied migrations: %w", err)
		}
		applied[version] = true
	}
	return applied, rows.Err()
}
