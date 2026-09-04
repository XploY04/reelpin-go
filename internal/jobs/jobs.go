// Package jobs holds the processing-job read port and the status presentation
// the app polls against.
package jobs

import (
	"context"
	"errors"
	"time"

	"github.com/XploY04/reelpin-go/internal/uuid"
)

// ErrNotFound covers a missing job and a job owned by another user.
var ErrNotFound = errors.New("processing job not found")

const (
	StatusQueued       = "queued"
	StatusProcessing   = "processing"
	StatusCompleted    = "completed"
	StatusFailed       = "failed"
	StatusDeadLettered = "dead_lettered"
)

type JobRecord struct {
	ID                string
	UserID            string
	URL               string
	NormalizedURL     *string
	SourcePlatform    *string
	SourceContentType *string
	SourceContentID   *string
	ProcessingVersion *string
	IngestionMethod   *string
	TranscriptSource  *string
	Status            string
	CurrentStep       *string
	ProgressPercent   int
	FailureCode       *string
	ErrorMessage      *string
	ResultReelID      *string
	AttemptCount      int
	MaxAttempts       int
	NextRetryAt       *time.Time
	StepDurations     map[string]float64
	CollectionIDs     []string
	CreatedAt         *time.Time
	UpdatedAt         *time.Time
	StartedAt         *time.Time
	CompletedAt       *time.Time
}

type JobReader interface {
	List(context.Context, string, bool, int) ([]JobRecord, error)
	Get(context.Context, string, uuid.UUID) (JobRecord, error)
}
