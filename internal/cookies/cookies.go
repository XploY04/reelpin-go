// Package cookies manages the authenticated download slots. Cookie data is
// configuration, never content: it is read from the environment as base64
// Netscape files, written to a run's own directory with restrictive
// permissions, and never logged, stored or returned.
package cookies

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ErrNoSlots means no authenticated attempt is possible.
var ErrNoSlots = errors.New("no cookie slots are configured")

// Slot is one set of credentials, identified by position so health can be
// tracked without ever naming an account.
type Slot struct {
	Index int
	Name  string
	data  []byte
}

// Health is what an operator needs to know: which slot is working, without any
// cookie ever appearing.
type Health struct {
	Index       int       `json:"index"`
	Name        string    `json:"name"`
	Successes   int       `json:"successes"`
	Failures    int       `json:"failures"`
	LastSuccess time.Time `json:"last_success"`
	LastFailure time.Time `json:"last_failure"`
	Exhausted   bool      `json:"exhausted"`
}

// Jar holds the configured slots and their health.
type Jar struct {
	mu     sync.Mutex
	slots  []Slot
	health map[int]*Health
	// exhaustAfter is how many consecutive failures retire a slot until the
	// next success elsewhere.
	exhaustAfter int
}

// SlotNames are the standardized production slots, in the order they are tried.
// The deprecated single-slot variables are deliberately not read.
var SlotNames = []string{"active", "backup", "tertiary"}

// New builds a jar from base64 Netscape cookie data, one entry per slot name.
// Malformed data is skipped rather than failing startup: a bad backup slot must
// not stop the service.
func New(encoded map[string]string) *Jar {
	jar := &Jar{health: map[int]*Health{}, exhaustAfter: 3}

	for index, name := range SlotNames {
		raw := strings.TrimSpace(encoded[name])
		if raw == "" {
			continue
		}
		data, err := base64.StdEncoding.DecodeString(raw)
		if err != nil || len(data) == 0 {
			continue
		}
		if !looksLikeNetscapeCookies(data) {
			continue
		}
		jar.slots = append(jar.slots, Slot{Index: index, Name: name, data: data})
		jar.health[index] = &Health{Index: index, Name: name}
	}
	return jar
}

// Slots returns the usable slots in order, skipping exhausted ones.
func (j *Jar) Slots() []Slot {
	j.mu.Lock()
	defer j.mu.Unlock()

	usable := make([]Slot, 0, len(j.slots))
	for _, slot := range j.slots {
		if health := j.health[slot.Index]; health != nil && health.Exhausted {
			continue
		}
		usable = append(usable, slot)
	}
	return usable
}

// WriteFile materializes a slot inside the run's directory. The caller deletes
// the directory; the file never outlives the run.
func (s Slot) WriteFile(workDir string) (string, error) {
	path := filepath.Join(workDir, fmt.Sprintf("cookies-%d.txt", s.Index))
	if err := os.WriteFile(path, s.data, 0o600); err != nil {
		return "", fmt.Errorf("writing the cookie file: %w", err)
	}
	return path, nil
}

func (j *Jar) RecordSuccess(index int) {
	j.mu.Lock()
	defer j.mu.Unlock()

	health := j.health[index]
	if health == nil {
		return
	}
	health.Successes++
	health.LastSuccess = time.Now().UTC()
	health.Failures = 0
	health.Exhausted = false
}

// RecordFailure retires a slot after repeated refusals, so the pipeline stops
// spending attempts on credentials the platform has stopped accepting.
func (j *Jar) RecordFailure(index int) {
	j.mu.Lock()
	defer j.mu.Unlock()

	health := j.health[index]
	if health == nil {
		return
	}
	health.Failures++
	health.LastFailure = time.Now().UTC()
	if health.Failures >= j.exhaustAfter {
		health.Exhausted = true
	}
}

// Report is the health of every slot, safe to log and to serve.
func (j *Jar) Report() []Health {
	j.mu.Lock()
	defer j.mu.Unlock()

	report := make([]Health, 0, len(j.slots))
	for _, slot := range j.slots {
		if health := j.health[slot.Index]; health != nil {
			report = append(report, *health)
		}
	}
	return report
}

// AllExhausted is the state worth alerting on: authenticated downloads are no
// longer possible.
func (j *Jar) AllExhausted() bool {
	j.mu.Lock()
	defer j.mu.Unlock()

	if len(j.slots) == 0 {
		return false
	}
	for _, slot := range j.slots {
		if health := j.health[slot.Index]; health == nil || !health.Exhausted {
			return false
		}
	}
	return true
}

// looksLikeNetscapeCookies is a shape check, not a parse: enough to reject a
// pasted JSON blob or an empty file before it reaches a download.
func looksLikeNetscapeCookies(data []byte) bool {
	text := string(data)
	if strings.Contains(text, "# Netscape HTTP Cookie File") || strings.Contains(text, "# HTTP Cookie File") {
		return true
	}
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "#") || strings.TrimSpace(line) == "" {
			continue
		}
		if strings.Count(line, "\t") >= 6 {
			return true
		}
	}
	return false
}
