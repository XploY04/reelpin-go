package storage

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// MaxJobBytes is the temporary disk one job may hold. yt-dlp's own per-file
// cap comes first; this is the backstop that counts everything the job wrote.
const MaxJobBytes = 1 << 30

// AdmissionThreshold stops new media work when the disk is this full. Light
// work keeps flowing: an article costs kilobytes, a video costs the disk.
const AdmissionThreshold = 0.80

// ErrDiskFull means media admission is paused until the disk drains.
var ErrDiskFull = errors.New("the disk is too full to admit media work")

// ErrJobBudget means one job tried to hold more temporary bytes than allowed.
var ErrJobBudget = errors.New("the job exceeded its temporary disk budget")

// diskUsedFraction is a seam so tests do not need a full disk.
var diskUsedFraction = func(path string) (float64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, fmt.Errorf("reading disk usage: %w", err)
	}
	total := stat.Blocks * uint64(stat.Bsize)
	if total == 0 {
		return 0, fmt.Errorf("the filesystem reports zero size")
	}
	free := stat.Bavail * uint64(stat.Bsize)
	return 1 - float64(free)/float64(total), nil
}

// Workspace is one job's scratch directory. It is created under a root the
// worker owns and removed however the job ends; the sweep catches what a
// crashed process could not remove itself.
type Workspace struct {
	Dir string
}

// NewWorkspace admits one job's media work and gives it a directory. Admission
// is refused above the disk threshold, so a full disk degrades to "try later"
// instead of failed downloads with confusing errors.
func NewWorkspace(root, jobID string) (*Workspace, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("creating the workspace root: %w", err)
	}

	used, err := diskUsedFraction(root)
	if err != nil {
		return nil, err
	}
	if used >= AdmissionThreshold {
		return nil, fmt.Errorf("%w: %.0f%% used", ErrDiskFull, used*100)
	}

	dir, err := os.MkdirTemp(root, "job-"+sanitize(jobID)+"-")
	if err != nil {
		return nil, fmt.Errorf("creating the job directory: %w", err)
	}
	return &Workspace{Dir: dir}, nil
}

// Usage walks the job's directory. It is called after each artifact lands, not
// per write, which is exact enough: the per-file caps bound any single write.
func (w *Workspace) Usage() (int64, error) {
	var total int64
	err := filepath.WalkDir(w.Dir, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil // removed mid-walk is normal here
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return nil
		}
		total += info.Size()
		return nil
	})
	return total, err
}

// CheckBudget fails the job when its directory outgrew the cap.
func (w *Workspace) CheckBudget() error {
	used, err := w.Usage()
	if err != nil {
		return err
	}
	if used > MaxJobBytes {
		return fmt.Errorf("%w: %d bytes against %d", ErrJobBudget, used, MaxJobBytes)
	}
	return nil
}

// Cleanup removes everything. Called on success, terminal failure and
// cancellation alike; it is safe to call twice.
func (w *Workspace) Cleanup() {
	_ = os.RemoveAll(w.Dir)
}

// Sweep removes job directories older than the cutoff: the leftovers of
// processes that could not clean up after themselves. Fresh directories are
// left alone, they may belong to a live worker.
func Sweep(root string, olderThan time.Duration, now time.Time) (int, error) {
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("reading the workspace root: %w", err)
	}

	removed := 0
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "job-") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if now.Sub(info.ModTime()) < olderThan {
			continue
		}
		if err := os.RemoveAll(filepath.Join(root, entry.Name())); err == nil {
			removed++
		}
	}
	return removed, nil
}

// sanitize keeps a job id from smuggling path elements into a directory name.
func sanitize(value string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-':
			return r
		}
		return '_'
	}, value)
}
