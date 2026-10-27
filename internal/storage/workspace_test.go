package storage

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fullDisk fakes the statfs seam.
func fullDisk(t *testing.T, fraction float64) {
	t.Helper()
	previous := diskUsedFraction
	diskUsedFraction = func(string) (float64, error) { return fraction, nil }
	t.Cleanup(func() { diskUsedFraction = previous })
}

func TestAWorkspaceLivesAndDiesWithItsJob(t *testing.T) {
	fullDisk(t, 0.5)
	root := t.TempDir()

	workspace, err := NewWorkspace(root, "11111111-1111-4111-8111-111111111111")
	if err != nil {
		t.Fatalf("NewWorkspace: %v", err)
	}
	if _, err := os.Stat(workspace.Dir); err != nil {
		t.Fatalf("the directory does not exist: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspace.Dir, "source.mp4"), []byte("video"), 0o600); err != nil {
		t.Fatal(err)
	}

	workspace.Cleanup()
	if _, err := os.Stat(workspace.Dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cleanup left the directory behind")
	}
	// Twice is fine: cancellation paths and deferred cleanups overlap.
	workspace.Cleanup()
}

func TestAFullDiskRefusesMediaAdmission(t *testing.T) {
	fullDisk(t, 0.85)
	if _, err := NewWorkspace(t.TempDir(), "job"); !errors.Is(err, ErrDiskFull) {
		t.Fatalf("err = %v, want ErrDiskFull at 85%% used", err)
	}

	fullDisk(t, 0.79)
	workspace, err := NewWorkspace(t.TempDir(), "job")
	if err != nil {
		t.Fatalf("refused below the threshold: %v", err)
	}
	workspace.Cleanup()
}

func TestTheJobBudgetIsCounted(t *testing.T) {
	fullDisk(t, 0.1)
	workspace, err := NewWorkspace(t.TempDir(), "job")
	if err != nil {
		t.Fatal(err)
	}
	defer workspace.Cleanup()

	if err := os.WriteFile(filepath.Join(workspace.Dir, "a"), make([]byte, 1024), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace.Dir, "b"), make([]byte, 2048), 0o600); err != nil {
		t.Fatal(err)
	}
	used, err := workspace.Usage()
	if err != nil {
		t.Fatal(err)
	}
	if used != 3072 {
		t.Fatalf("usage = %d, want the sum of what was written", used)
	}
	if err := workspace.CheckBudget(); err != nil {
		t.Fatalf("a 3KB job failed its budget: %v", err)
	}

	// A sparse file has the logical size the budget cares about without
	// touching the disk for a real gigabyte.
	big, err := os.Create(filepath.Join(workspace.Dir, "huge.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	if err := big.Truncate(MaxJobBytes + 1); err != nil {
		t.Fatal(err)
	}
	big.Close()

	if err := workspace.CheckBudget(); !errors.Is(err, ErrJobBudget) {
		t.Fatalf("err = %v, want ErrJobBudget", err)
	}
}

func TestSweepRemovesOnlyAbandonedDirectories(t *testing.T) {
	fullDisk(t, 0.1)
	root := t.TempDir()

	stale, err := NewWorkspace(root, "stale")
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := NewWorkspace(root, "fresh")
	if err != nil {
		t.Fatal(err)
	}
	// Not a job directory; the sweep must never touch it.
	if err := os.MkdirAll(filepath.Join(root, "keep"), 0o700); err != nil {
		t.Fatal(err)
	}

	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(stale.Dir, old, old); err != nil {
		t.Fatal(err)
	}

	removed, err := Sweep(root, time.Hour, time.Now())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want only the stale directory", removed)
	}
	if _, err := os.Stat(stale.Dir); !errors.Is(err, os.ErrNotExist) {
		t.Error("the stale directory survived")
	}
	if _, err := os.Stat(fresh.Dir); err != nil {
		t.Error("the fresh directory was swept; it may belong to a live worker")
	}
	if _, err := os.Stat(filepath.Join(root, "keep")); err != nil {
		t.Error("a non-job directory was swept")
	}
}

func TestSweepOfAMissingRootIsNothing(t *testing.T) {
	removed, err := Sweep(filepath.Join(t.TempDir(), "never-created"), time.Hour, time.Now())
	if err != nil || removed != 0 {
		t.Fatalf("removed = %d, err = %v", removed, err)
	}
}

func TestJobIDsCannotShapeThePath(t *testing.T) {
	fullDisk(t, 0.1)
	root := t.TempDir()
	workspace, err := NewWorkspace(root, "../../escape")
	if err != nil {
		t.Fatal(err)
	}
	defer workspace.Cleanup()

	relative, err := filepath.Rel(root, workspace.Dir)
	if err != nil || relative == ".." || filepath.IsAbs(relative) || len(relative) > 0 && relative[0] == '.' {
		t.Fatalf("the workspace escaped its root: %s", workspace.Dir)
	}
}
