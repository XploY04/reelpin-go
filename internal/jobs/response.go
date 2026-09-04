package jobs

import (
	"time"

	"github.com/XploY04/reelpin-go/internal/reels"
)

type Response struct {
	ID                          string             `json:"id"`
	UserID                      string             `json:"user_id"`
	URL                         string             `json:"url"`
	NormalizedURL               *string            `json:"normalized_url"`
	SourcePlatform              *string            `json:"source_platform"`
	SourceContentType           *string            `json:"source_content_type"`
	SourceContentID             *string            `json:"source_content_id"`
	ProcessingVersion           *string            `json:"processing_version"`
	IngestionMethod             *string            `json:"ingestion_method"`
	TranscriptSource            *string            `json:"transcript_source"`
	Status                      string             `json:"status"`
	CurrentStep                 *string            `json:"current_step"`
	ProgressPercent             int                `json:"progress_percent"`
	FailureCode                 *string            `json:"failure_code"`
	ErrorMessage                *string            `json:"error_message"`
	AttemptCount                int                `json:"attempt_count"`
	MaxAttempts                 int                `json:"max_attempts"`
	NextRetryAt                 *string            `json:"next_retry_at"`
	Terminal                    bool               `json:"terminal"`
	RetryScheduled              bool               `json:"retry_scheduled"`
	Retryable                   bool               `json:"retryable"`
	StatusLabel                 string             `json:"status_label"`
	StatusMessage               string             `json:"status_message"`
	RecommendedPollAfterSeconds *int               `json:"recommended_poll_after_seconds"`
	ResultReelID                *string            `json:"result_reel_id"`
	CollectionIDs               []string           `json:"collection_ids"`
	StepDurations               map[string]float64 `json:"step_durations"`
	CreatedAt                   *string            `json:"created_at"`
	UpdatedAt                   *string            `json:"updated_at"`
	StartedAt                   *string            `json:"started_at"`
	CompletedAt                 *string            `json:"completed_at"`
	Reel                        *reels.DisplayReel `json:"reel"`
}

// BuildResponse presents one job. `reel` is the saved reel this job produced,
// when the caller could load it.
func BuildResponse(record JobRecord, reel *reels.DisplayReel, now time.Time) Response {
	record = Normalize(record)

	durations := record.StepDurations
	if durations == nil {
		durations = map[string]float64{}
	}
	// The app reads collection_ids on every job, so it is never null.
	collectionIDs := record.CollectionIDs
	if collectionIDs == nil {
		collectionIDs = []string{}
	}

	return Response{
		ID:                          record.ID,
		UserID:                      record.UserID,
		URL:                         record.URL,
		NormalizedURL:               record.NormalizedURL,
		SourcePlatform:              record.SourcePlatform,
		SourceContentType:           record.SourceContentType,
		SourceContentID:             record.SourceContentID,
		ProcessingVersion:           record.ProcessingVersion,
		IngestionMethod:             record.IngestionMethod,
		TranscriptSource:            record.TranscriptSource,
		Status:                      record.Status,
		CurrentStep:                 record.CurrentStep,
		ProgressPercent:             ProgressPercent(record),
		FailureCode:                 FailureCode(record.FailureCode),
		ErrorMessage:                record.ErrorMessage,
		AttemptCount:                record.AttemptCount,
		MaxAttempts:                 record.MaxAttempts,
		NextRetryAt:                 isoTimestamp(record.NextRetryAt),
		Terminal:                    Terminal(record),
		RetryScheduled:              RetryScheduled(record),
		Retryable:                   Retryable(record),
		StatusLabel:                 StatusLabel(record),
		StatusMessage:               StatusMessage(record),
		RecommendedPollAfterSeconds: RecommendedPollAfterSeconds(record, now),
		ResultReelID:                record.ResultReelID,
		CollectionIDs:               collectionIDs,
		StepDurations:               durations,
		CreatedAt:                   isoTimestamp(record.CreatedAt),
		UpdatedAt:                   isoTimestamp(record.UpdatedAt),
		StartedAt:                   isoTimestamp(record.StartedAt),
		CompletedAt:                 isoTimestamp(record.CompletedAt),
		Reel:                        reel,
	}
}

func isoTimestamp(value *time.Time) *string {
	if value == nil {
		return nil
	}
	formatted := value.UTC().Format(time.RFC3339Nano)
	return &formatted
}
