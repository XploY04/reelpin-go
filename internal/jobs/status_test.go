package jobs

import (
	"testing"
	"time"
)

var now = time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)

func ptr[T any](value T) *T { return &value }

func record(status, step string) JobRecord {
	return JobRecord{ID: "job", Status: status, CurrentStep: ptr(step), ProgressPercent: 40}
}

func TestStatusPresentation(t *testing.T) {
	tests := []struct {
		name        string
		record      JobRecord
		wantLabel   string
		wantMessage string
		wantPercent int
		terminal    bool
		retryable   bool
		wantPoll    *int
	}{
		{
			name: "queued", record: record(StatusQueued, "queued"),
			wantLabel: "Queued", wantMessage: "Queued for processing.", wantPercent: 40,
			retryable: true, wantPoll: ptr(3),
		},
		{
			name: "processing a known step", record: record(StatusProcessing, "transcribing"),
			wantLabel: "Transcribing", wantMessage: "Transcribing audio.", wantPercent: 40,
			retryable: true, wantPoll: ptr(2),
		},
		{
			name: "processing with no progress yet", record: JobRecord{Status: StatusProcessing, CurrentStep: ptr("")},
			wantLabel: "Processing", wantMessage: "Processing is in progress.", wantPercent: 5,
			retryable: true, wantPoll: ptr(2),
		},
		{
			name: "queued above the cap", record: JobRecord{Status: StatusQueued, CurrentStep: ptr("queued"), ProgressPercent: 140},
			wantLabel: "Queued", wantMessage: "Queued for processing.", wantPercent: 99,
			retryable: true, wantPoll: ptr(3),
		},
		{
			name: "completed", record: JobRecord{Status: StatusCompleted, CurrentStep: ptr("completed"), ResultReelID: ptr("reel")},
			wantLabel: "Completed", wantMessage: "Processing completed.", wantPercent: 100,
			terminal: true,
		},
		{
			name:      "failed with a retryable code",
			record:    JobRecord{Status: StatusFailed, CurrentStep: ptr("failed"), FailureCode: ptr("rate_limit")},
			wantLabel: "Failed", wantMessage: "The source platform is rate limiting requests right now.",
			wantPercent: 100, terminal: true, retryable: true,
		},
		{
			name:      "failed with a terminal code",
			record:    JobRecord{Status: StatusFailed, CurrentStep: ptr("failed"), FailureCode: ptr("no_audio")},
			wantLabel: "Failed", wantMessage: "This video does not include an audio track.",
			wantPercent: 100, terminal: true,
		},
		{
			name:      "failed with an unknown code",
			record:    JobRecord{Status: StatusFailed, CurrentStep: ptr("failed"), FailureCode: ptr("who_knows")},
			wantLabel: "Failed", wantMessage: "Processing failed.", wantPercent: 100, terminal: true,
		},
		{
			name:      "dead lettered",
			record:    JobRecord{Status: StatusDeadLettered, CurrentStep: ptr("dead_lettered")},
			wantLabel: "Dead Lettered", wantMessage: "Processing stopped after a final failure.",
			wantPercent: 100, terminal: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := StatusLabel(tt.record); got != tt.wantLabel {
				t.Errorf("status_label = %q, want %q", got, tt.wantLabel)
			}
			if got := StatusMessage(tt.record); got != tt.wantMessage {
				t.Errorf("status_message = %q, want %q", got, tt.wantMessage)
			}
			if got := ProgressPercent(tt.record); got != tt.wantPercent {
				t.Errorf("progress_percent = %d, want %d", got, tt.wantPercent)
			}
			if got := Terminal(tt.record); got != tt.terminal {
				t.Errorf("terminal = %v, want %v", got, tt.terminal)
			}
			if got := Retryable(tt.record); got != tt.retryable {
				t.Errorf("retryable = %v, want %v", got, tt.retryable)
			}

			got := RecommendedPollAfterSeconds(tt.record, now)
			switch {
			case tt.wantPoll == nil && got != nil:
				t.Errorf("recommended_poll_after_seconds = %d, want null", *got)
			case tt.wantPoll != nil && (got == nil || *got != *tt.wantPoll):
				t.Errorf("recommended_poll_after_seconds = %v, want %d", got, *tt.wantPoll)
			}
		})
	}
}

func TestRetryScheduledPolling(t *testing.T) {
	tests := []struct {
		name     string
		retryAt  *time.Time
		wantPoll int
	}{
		{name: "no retry time", retryAt: nil, wantPoll: 10},
		{name: "already due", retryAt: ptr(now.Add(-time.Minute)), wantPoll: 2},
		{name: "below the floor", retryAt: ptr(now.Add(time.Second)), wantPoll: 2},
		{name: "inside the window", retryAt: ptr(now.Add(15 * time.Second)), wantPoll: 15},
		{name: "above the ceiling", retryAt: ptr(now.Add(5 * time.Minute)), wantPoll: 30},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := JobRecord{Status: StatusQueued, CurrentStep: ptr("retry_scheduled"), NextRetryAt: tt.retryAt}
			if !RetryScheduled(job) || !Retryable(job) {
				t.Fatal("a retry-scheduled job must read as retryable")
			}
			got := RecommendedPollAfterSeconds(job, now)
			if got == nil || *got != tt.wantPoll {
				t.Fatalf("recommended_poll_after_seconds = %v, want %d", got, tt.wantPoll)
			}
			if tt.retryAt == nil && StatusMessage(job) != "Waiting before the next retry." {
				t.Errorf("status_message = %q", StatusMessage(job))
			}
		})
	}
}

func TestUnknownFailureCodeIsDropped(t *testing.T) {
	if code := FailureCode(ptr("not_a_code")); code != nil {
		t.Errorf("failure_code = %q, want null", *code)
	}
	if code := FailureCode(ptr("rate_limit")); code == nil || *code != "rate_limit" {
		t.Errorf("failure_code = %v, want rate_limit", code)
	}
}

func TestCompletedWithoutResultReelReadsAsFailed(t *testing.T) {
	job := JobRecord{ID: "job", Status: StatusCompleted, CurrentStep: ptr("completed")}
	normalized := Normalize(job)

	if normalized.Status != StatusFailed {
		t.Errorf("status = %q, want failed", normalized.Status)
	}
	if normalized.FailureCode == nil || *normalized.FailureCode != "internal_error" {
		t.Errorf("failure_code = %v, want internal_error", normalized.FailureCode)
	}
	if normalized.ProgressPercent != 100 {
		t.Errorf("progress_percent = %d, want 100", normalized.ProgressPercent)
	}
	// The stored record is untouched: a GET never writes.
	if job.Status != StatusCompleted || job.FailureCode != nil {
		t.Errorf("the input record was mutated: %+v", job)
	}
}

func TestBuildResponseFillsEmptyStepDurations(t *testing.T) {
	response := BuildResponse(record(StatusQueued, "queued"), nil, now)
	if response.StepDurations == nil {
		t.Fatal("step_durations must serialize as an object, not null")
	}
	if response.Reel != nil {
		t.Error("reel must stay null when none was attached")
	}
}

func TestCollectionIDsAreNeverNull(t *testing.T) {
	response := BuildResponse(record(StatusQueued, "queued"), nil, now)
	if response.CollectionIDs == nil {
		t.Fatal("collection_ids must serialize as an array, not null")
	}

	withIDs := record(StatusQueued, "queued")
	withIDs.CollectionIDs = []string{"collection-1"}
	if got := BuildResponse(withIDs, nil, now).CollectionIDs; len(got) != 1 || got[0] != "collection-1" {
		t.Errorf("collection_ids = %v, want [collection-1]", got)
	}
}
