package jobs

import (
	"math"
	"strings"
	"time"
)

// StageBudget is one pipeline stage and the longest it may run. The pipeline
// owns the numbers; this package only reads them to estimate.
type StageBudget struct {
	Name   string
	Budget time.Duration
}

var stageLabels = map[string]string{
	"prepare":    "Reading the link",
	"download":   "Downloading",
	"transcribe": "Transcribing",
	"extract":    "Extracting details",
	"categorize": "Categorizing",
	"persist":    "Saving",
}

// legacySteps maps the step a Python job records while it is in it onto the
// pipeline stage doing the same work.
var legacySteps = map[string]string{
	"starting":       "prepare",
	"checking_cache": "prepare",
	"downloading":    "download",
	"transcribing":   "transcribe",
	"ocr":            "transcribe",
	"extracting":     "extract",
	"categorizing":   "categorize",
	"saving":         "persist",
	"embedding":      "persist",
}

// Stage names where a job is in words a person or an agent can repeat.
func Stage(record JobRecord, budgets []StageBudget) string {
	switch {
	case normalized(record.Status) == StatusCompleted:
		return "Done"
	case Terminal(record):
		return "Failed"
	case RetryScheduled(record):
		return "Waiting to retry"
	case normalized(record.Status) == StatusQueued:
		return "Queued"
	}
	index := runningStage(record, budgets)
	if index >= 0 && index < len(budgets) {
		if label, ok := stageLabels[budgets[index].Name]; ok {
			return label
		}
	}
	return "Processing"
}

// EstimatedSecondsRemaining is an upper bound: the budgets of the stages still
// to run, less the time the current one has already had, plus any wait before
// a scheduled retry. Nil means finished or unknowable.
func EstimatedSecondsRemaining(record JobRecord, budgets []StageBudget, now time.Time) *int {
	if Terminal(record) || len(budgets) == 0 {
		return nil
	}
	index := runningStage(record, budgets)
	if index < 0 {
		return nil
	}
	if index >= len(budgets) {
		return intPtr(0)
	}

	var remaining time.Duration
	for _, stage := range budgets[index:] {
		remaining += stage.Budget
	}
	switch {
	case RetryScheduled(record):
		if record.NextRetryAt != nil && record.NextRetryAt.After(now) {
			remaining += record.NextRetryAt.Sub(now)
		}
	case normalized(record.Status) == StatusProcessing && record.UpdatedAt != nil:
		// The running stage began when the previous one committed, which is
		// the job's last update.
		if elapsed := now.Sub(*record.UpdatedAt); elapsed > 0 {
			remaining -= min(elapsed, budgets[index].Budget)
		}
	}
	return intPtr(int(math.Ceil(remaining.Seconds())))
}

// runningStage is the index of the stage a live job is in, or -1 when its
// step means nothing here. The Go pipeline records the stage it last
// finished, so the one running is the next; a Python job records the step it
// is in.
func runningStage(record JobRecord, budgets []StageBudget) int {
	step := normalized(optionalString(record.CurrentStep))
	if step == "" || step == StatusQueued || step == "retry_scheduled" {
		return 0
	}
	for index, stage := range budgets {
		if stage.Name == step {
			return index + 1
		}
	}
	if name, ok := legacySteps[step]; ok {
		for index, stage := range budgets {
			if stage.Name == name {
				return index
			}
		}
	}
	return -1
}

// SummaryLine is the first line of a saved reel's summary, the one sentence
// an agent can repeat to say what was saved.
func SummaryLine(summary string) *string {
	for _, line := range strings.Split(summary, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			return &trimmed
		}
	}
	return nil
}
