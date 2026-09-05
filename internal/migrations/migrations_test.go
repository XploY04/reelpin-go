package migrations

import (
	"regexp"
	"strings"
	"testing"
)

func TestMigrationsLoadInOrder(t *testing.T) {
	loaded, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded) != 3 {
		t.Fatalf("loaded %d migrations, want 3", len(loaded))
	}
	for i := 1; i < len(loaded); i++ {
		if loaded[i].Version <= loaded[i-1].Version {
			t.Fatalf("versions out of order: %d then %d", loaded[i-1].Version, loaded[i].Version)
		}
	}
	for _, migration := range loaded {
		if strings.TrimSpace(migration.Up) == "" {
			t.Errorf("%s has an empty up section", migration.Name)
		}
		if strings.TrimSpace(migration.Down) == "" {
			t.Errorf("%s has no down section for disposable databases", migration.Name)
		}
	}
}

// Up migrations are expand-only. A destructive statement in one would be
// applied to production by the deploy job with nobody reading it first.
func TestUpMigrationsAreNotDestructive(t *testing.T) {
	destructive := regexp.MustCompile(`(?i)\b(DROP\s+TABLE|DROP\s+COLUMN|DROP\s+SCHEMA|TRUNCATE|DELETE\s+FROM)\b`)

	loaded, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, migration := range loaded {
		if match := destructive.FindString(migration.Up); match != "" {
			t.Errorf("%s contains %q in its up section", migration.Name, match)
		}
	}
}

// No table carries an environment column: dev and production are separate
// infrastructure, per docs/decisions/0004.
func TestNoEnvironmentColumnInTheSQL(t *testing.T) {
	loaded, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, migration := range loaded {
		if regexp.MustCompile(`(?i)\benvironment\b`).MatchString(migration.Up) {
			t.Errorf("%s mentions an environment column", migration.Name)
		}
	}
}
