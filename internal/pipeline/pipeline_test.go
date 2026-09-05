package pipeline

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestClassify(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		wantClass FailureClass
		wantCode  string
	}{
		{name: "nothing", err: nil},
		{name: "cancellation", err: context.Canceled, wantClass: Transient, wantCode: "internal_error"},
		{name: "deadline", err: context.DeadlineExceeded, wantClass: Transient, wantCode: "provider_timeout"},
		{name: "rate limit", err: errors.New("provider returned 429 Too Many Requests"),
			wantClass: ProviderExhausted, wantCode: "rate_limit"},
		{name: "quota", err: errors.New("quota exceeded for this project"),
			wantClass: ProviderExhausted, wantCode: "rate_limit"},
		{name: "timeout text", err: errors.New("read timeout after 30s"),
			wantClass: Transient, wantCode: "provider_timeout"},
		{name: "anything else", err: errors.New("nil pointer somewhere"),
			wantClass: Internal, wantCode: "internal_error"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Classify(tt.err)
			if tt.err == nil {
				if got != nil {
					t.Fatalf("Classify(nil) = %+v", got)
				}
				return
			}
			if got.Class != tt.wantClass || got.Code != tt.wantCode {
				t.Fatalf("Classify = %v/%s, want %v/%s", got.Class, got.Code, tt.wantClass, tt.wantCode)
			}
			if got.Message == "" {
				t.Error("a failure with no user-facing message")
			}
			if strings.Contains(got.Message, tt.err.Error()) {
				t.Error("the provider's words leaked into the user-facing message")
			}
		})
	}
}

func TestDeliberateFailuresAreTerminalOrNot(t *testing.T) {
	terminal := []*Failure{
		PostNotFound(nil), ProtectedOrUnavailable(nil), UnsupportedPostType(nil),
		EmptyPostContent(nil), NoAudio(nil), TranscriptUnavailable(nil),
	}
	for _, failure := range terminal {
		if !failure.Terminal() {
			t.Errorf("%s should be terminal: retrying will not change the content", failure.Code)
		}
	}

	for _, failure := range []*Failure{RateLimited(nil), AuthFailure(nil)} {
		if failure.Terminal() {
			t.Errorf("%s should not be terminal: the provider may recover", failure.Code)
		}
		if failure.Class != ProviderExhausted {
			t.Errorf("%s should put the platform on cooldown", failure.Code)
		}
	}

	// A wrapped failure keeps its classification through errors.As.
	wrapped := Classify(NoAudio(errors.New("no audio stream")))
	if wrapped.Code != "no_audio" {
		t.Errorf("a deliberate failure was reclassified as %s", wrapped.Code)
	}
}

func TestProgressOnlyMovesForward(t *testing.T) {
	previous := 0
	for _, stage := range Stages {
		percent := progressFor(stage)
		if percent < previous {
			t.Fatalf("%s reports %d after %d", stage, percent, previous)
		}
		if percent < 5 || percent > 100 {
			t.Fatalf("%s reports %d, want it inside 5..100", stage, percent)
		}
		previous = percent
	}
	if progressFor(StageComplete) != 100 {
		t.Error("a finished run does not report 100")
	}
	// Only the very last stage may claim completion.
	for _, stage := range Stages[:len(Stages)-1] {
		if progressFor(stage) == 100 {
			t.Errorf("%s claims 100 before the reel exists", stage)
		}
	}
}

func TestInputHashDistinguishesInputs(t *testing.T) {
	base := InputHash("a", "b")
	if base == InputHash("a", "c") {
		t.Error("different inputs produced one hash")
	}
	if base != InputHash("a", "b") {
		t.Error("the same input produced two hashes")
	}
	// Concatenation must not be confusable: ("ab","") and ("a","b") differ.
	if base == InputHash("ab", "") {
		t.Error("the field separator is not doing its job")
	}
}

func TestBackoffClimbsAndIsJittered(t *testing.T) {
	first := backoff(1)
	if first < 30*time.Second || first > 40*time.Second {
		t.Fatalf("first backoff = %s, want about 30 seconds", first)
	}
	if backoff(2) < 5*time.Minute {
		t.Fatalf("second backoff = %s, want at least five minutes", backoff(2))
	}
	if backoff(9) < 30*time.Minute {
		t.Fatalf("late backoff = %s, want the longest stage", backoff(9))
	}

	// Jitter: two calls should not always agree.
	same := 0
	for i := 0; i < 20; i++ {
		if backoff(1) == first {
			same++
		}
	}
	if same == 20 {
		t.Error("backoff is not jittered, so a provider gets every run back at once")
	}
}

func TestChunkKeepsWholeShortTranscripts(t *testing.T) {
	if got := Chunk(""); got != nil {
		t.Fatalf("empty transcript produced %v", got)
	}
	if got := Chunk("  a short line  "); len(got) != 1 || got[0] != "a short line" {
		t.Fatalf("chunks = %v, want the trimmed line", got)
	}
}

func TestChunkOverlapsAndIsDeterministic(t *testing.T) {
	sentence := "This is a sentence about a cafe in Goa. "
	transcript := strings.Repeat(sentence, 200)

	chunks := Chunk(transcript)
	if len(chunks) < 2 {
		t.Fatalf("a long transcript produced %d chunks", len(chunks))
	}
	for i, chunk := range chunks {
		if len([]rune(chunk)) > MaxChunkRunes {
			t.Fatalf("chunk %d is %d runes, over the %d limit", i, len([]rune(chunk)), MaxChunkRunes)
		}
	}

	again := Chunk(transcript)
	if len(again) != len(chunks) {
		t.Fatal("chunking is not deterministic")
	}
	for i := range chunks {
		if again[i] != chunks[i] {
			t.Fatal("chunking is not deterministic")
		}
	}

	// Consecutive chunks share text, so a sentence split across a boundary is
	// still searchable.
	if len(chunks) > 1 {
		tail := chunks[0][len(chunks[0])-40:]
		if !strings.Contains(chunks[1], strings.TrimSpace(tail[:20])) {
			t.Error("consecutive chunks do not overlap")
		}
	}
}

func TestChunkAlwaysTerminates(t *testing.T) {
	// A transcript with no spaces or punctuation has no boundary to prefer,
	// which is exactly where a naive walk stands still.
	transcript := strings.Repeat("x", MaxChunkRunes*3)
	done := make(chan []string, 1)
	go func() { done <- Chunk(transcript) }()

	select {
	case chunks := <-done:
		if len(chunks) < 2 {
			t.Fatalf("chunks = %d", len(chunks))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("chunking did not terminate")
	}
}

func TestSweepRemovesOnlyOldRunDirectories(t *testing.T) {
	root := t.TempDir()
	now := time.Now()

	old := filepath.Join(root, "run-oldrun-1")
	fresh := filepath.Join(root, "run-newrun-2")
	other := filepath.Join(root, "not-a-run")
	for _, dir := range []string{old, fresh, other} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chtimes(old, now.Add(-4*time.Hour), now.Add(-4*time.Hour)); err != nil {
		t.Fatal(err)
	}

	removed, err := SweepTempDirectories(root, time.Hour, now)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed %d directories, want 1", removed)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("the abandoned directory survived")
	}
	for _, dir := range []string{fresh, other} {
		if _, err := os.Stat(dir); err != nil {
			t.Errorf("%s was removed, and it should not have been", dir)
		}
	}

	// A missing root is not an error: nothing has run yet.
	if _, err := SweepTempDirectories(filepath.Join(root, "missing"), time.Hour, now); err != nil {
		t.Errorf("sweeping a missing root: %v", err)
	}
}

func TestDeterministicEventIDIsStableAndUUIDShaped(t *testing.T) {
	first := deterministicEventID("run", "job", "save")
	if first != deterministicEventID("run", "job", "save") {
		t.Fatal("the same inputs produced two event ids, so redelivery would duplicate")
	}
	if first == deterministicEventID("run", "job", "index") {
		t.Fatal("different events share one id")
	}
	if len(first) != 36 || strings.Count(first, "-") != 4 {
		t.Fatalf("event id = %q, want a uuid shape", first)
	}
}
