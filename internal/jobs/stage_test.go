package jobs

import (
	"testing"
	"time"

	"github.com/XploY04/reelpin-go/internal/reels"
)

// budgets mirror the pipeline's shape with round numbers, so the arithmetic in
// each case can be read off the test.
var budgets = []StageBudget{
	{Name: "prepare", Budget: 100 * time.Second},
	{Name: "download", Budget: 100 * time.Second},
	{Name: "transcribe", Budget: 100 * time.Second},
	{Name: "extract", Budget: 50 * time.Second},
	{Name: "categorize", Budget: 30 * time.Second},
	{Name: "persist", Budget: 20 * time.Second},
}

func TestStageAndEstimate(t *testing.T) {
	updated := now.Add(-40 * time.Second)

	tests := []struct {
		name      string
		record    JobRecord
		wantStage string
		wantETA   *int
	}{
		{name: "queued has every stage ahead", record: record(StatusQueued, "queued"),
			wantStage: "Queued", wantETA: ptr(400)},
		{name: "claimed but nothing finished is preparing",
			record:    JobRecord{Status: StatusProcessing, CurrentStep: ptr("queued")},
			wantStage: "Reading the link", wantETA: ptr(400)},
		{name: "the stage after the last finished one is running, less its elapsed time",
			record:    JobRecord{Status: StatusProcessing, CurrentStep: ptr("download"), UpdatedAt: &updated},
			wantStage: "Transcribing", wantETA: ptr(60 + 50 + 30 + 20)},
		{name: "a stage past its budget counts as spent, not negative",
			record:    JobRecord{Status: StatusProcessing, CurrentStep: ptr("categorize"), UpdatedAt: ptr(now.Add(-time.Hour))},
			wantStage: "Saving", wantETA: ptr(0)},
		{name: "a legacy step is the stage it names",
			record:    JobRecord{Status: StatusProcessing, CurrentStep: ptr("extracting")},
			wantStage: "Extracting details", wantETA: ptr(50 + 30 + 20)},
		{name: "a retry adds its wait",
			record: JobRecord{Status: StatusQueued, CurrentStep: ptr("retry_scheduled"),
				NextRetryAt: ptr(now.Add(30 * time.Second))},
			wantStage: "Waiting to retry", wantETA: ptr(430)},
		{name: "an unknown step cannot be estimated",
			record:    JobRecord{Status: StatusProcessing, CurrentStep: ptr("teleporting")},
			wantStage: "Processing", wantETA: nil},
		{name: "completed", record: JobRecord{Status: StatusCompleted, ResultReelID: ptr("reel")},
			wantStage: "Done", wantETA: nil},
		{name: "failed", record: record(StatusFailed, "failed"), wantStage: "Failed", wantETA: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Stage(tt.record, budgets); got != tt.wantStage {
				t.Errorf("stage = %q, want %q", got, tt.wantStage)
			}
			got := EstimatedSecondsRemaining(tt.record, budgets, now)
			switch {
			case tt.wantETA == nil && got != nil:
				t.Errorf("estimate = %d, want null", *got)
			case tt.wantETA != nil && (got == nil || *got != *tt.wantETA):
				t.Errorf("estimate = %v, want %d", got, *tt.wantETA)
			}
		})
	}
}

func TestNoBudgetsMeansNoEstimate(t *testing.T) {
	if got := EstimatedSecondsRemaining(record(StatusQueued, "queued"), nil, now); got != nil {
		t.Errorf("estimate = %d without budgets, want null", *got)
	}
}

func TestACompletedJobSummarisesItsReel(t *testing.T) {
	job := JobRecord{Status: StatusCompleted, ResultReelID: ptr("reel")}
	reel := &reels.DisplayReel{Summary: "\n  Three cafes worth the ride.  \nOpen early."}

	response := BuildResponse(job, reel, budgets, now)
	if response.Summary == nil || *response.Summary != "Three cafes worth the ride." {
		t.Errorf("summary = %v, want the first line of the reel's summary", response.Summary)
	}

	running := BuildResponse(record(StatusProcessing, "prepare"), reel, budgets, now)
	if running.Summary != nil {
		t.Errorf("a running job has summary %q, want null", *running.Summary)
	}
}
