package db

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

var (
	timerCall    = regexp.MustCompile(`time\.(NewTicker|Tick|After|NewTimer|AfterFunc)\(`)
	databaseCall = regexp.MustCompile(`\.(Query|QueryRow|Exec|Begin|SendBatch|CopyFrom)\(`)
)

// gatedTimers is every timer in a package that talks to PostgreSQL, and the
// gate that keeps it from querying an idle database. The production database
// suspends after five idle minutes and has a monthly budget of awake hours,
// so one ungated query on a timer keeps it awake around the clock.
//
// A new timer in such a package fails this test until it is listed here,
// with the identifier of its gate and how many timers the file holds.
var gatedTimers = map[string]struct {
	gate   string
	timers int
	why    string
}{
	"internal/outbox/outbox.go": {"d.signal.take(", 1,
		"asks Redis every tick; PostgreSQL only when a writer marked the outbox or a known row falls due"},
	"internal/lease/sweeper.go": {"s.inflight.active(", 1,
		"sweeps only while the in-flight marker says a lease was recently held"},
	"internal/metrics/sampler.go": {"runIfUsed(", 3,
		"database gauges are read only after the pool was used; the temp-disk sampler never touches it"},
	"internal/pipeline/pipeline.go": {"lease.Renew(", 1,
		"renews a lease this worker holds, and stops when the run does"},
	"cmd/worker/main.go": {"queue.DrainTimeout", 1,
		"a one-shot shutdown budget, not a loop"},
}

func TestNothingQueriesPostgresOnAnUngatedTimer(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}

	sources := map[string][]string{} // package directory -> source files
	for _, top := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			relative, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			directory := filepath.ToSlash(filepath.Dir(relative))
			sources[directory] = append(sources[directory], filepath.ToSlash(relative))
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	read := func(relative string) string {
		content, err := os.ReadFile(filepath.Join(root, relative))
		if err != nil {
			t.Fatal(err)
		}
		return string(content)
	}

	seen := map[string]bool{}
	problems := []string{}
	for _, files := range sources {
		queries := false
		for _, file := range files {
			if databaseCall.MatchString(read(file)) {
				queries = true
				break
			}
		}
		if !queries {
			continue
		}
		for _, file := range files {
			content := read(file)
			timers := len(timerCall.FindAllString(content, -1))
			if timers == 0 {
				continue
			}
			seen[file] = true
			gated, listed := gatedTimers[file]
			switch {
			case !listed:
				problems = append(problems, file+" starts a timer in a package that queries PostgreSQL, behind no listed gate")
			case timers != gated.timers:
				problems = append(problems, file+" now holds a timer that is not accounted for in gatedTimers")
			case !strings.Contains(content, gated.gate):
				problems = append(problems, file+" lost its gate "+gated.gate+": it "+gated.why)
			}
		}
	}

	// An entry for a file that no longer has a timer is an entry nobody checks.
	for file := range gatedTimers {
		if !seen[file] {
			problems = append(problems, file+" is listed in gatedTimers but has no timer in a querying package")
		}
	}

	sort.Strings(problems)
	for _, problem := range problems {
		t.Error(problem)
	}
}
