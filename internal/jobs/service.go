package jobs

import (
	"context"
	"time"

	"github.com/XploY04/reelpin-go/internal/reels"
	"github.com/google/uuid"
)

// ResultReels loads the reel a finished job produced.
type ResultReels interface {
	Card(ctx context.Context, userID string, id uuid.UUID, view reels.View) (reels.DisplayReel, error)
}

// Service is the processing-job use cases every client polls through.
type Service struct {
	reader  JobReader
	results ResultReels
	budgets []StageBudget
	now     func() time.Time
}

func NewService(reader JobReader, results ResultReels, budgets []StageBudget, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{reader: reader, results: results, budgets: budgets, now: now}
}

// ListParams bound the list. It is bounded rather than paged: a user has a
// few jobs in flight and a short tail of finished ones.
type ListParams struct {
	ActiveOnly bool
	Limit      int
}

func (s *Service) List(ctx context.Context, userID string, params ListParams) ([]Response, error) {
	records, err := s.reader.List(ctx, userID, params.ActiveOnly, params.Limit)
	if err != nil {
		return nil, err
	}
	responses := make([]Response, 0, len(records))
	for _, record := range records {
		responses = append(responses, s.present(ctx, userID, record))
	}
	return responses, nil
}

// Get answers one job. ErrNotFound covers a job that belongs to someone else.
func (s *Service) Get(ctx context.Context, userID string, id uuid.UUID) (Response, error) {
	record, err := s.reader.Get(ctx, userID, id)
	if err != nil {
		return Response{}, err
	}
	return s.present(ctx, userID, record), nil
}

func (s *Service) present(ctx context.Context, userID string, record JobRecord) Response {
	now := s.now().UTC()
	return BuildResponse(record, s.resultReel(ctx, userID, record), s.budgets, now)
}

// resultReel attaches the saved reel a finished job produced. A reel that
// cannot be loaded is left off the response rather than failing the request.
func (s *Service) resultReel(ctx context.Context, userID string, record JobRecord) *reels.DisplayReel {
	if record.ResultReelID == nil || *record.ResultReelID == "" {
		return nil
	}
	id, err := uuid.Parse(*record.ResultReelID)
	if err != nil {
		return nil
	}
	reel, err := s.results.Card(ctx, userID, id, reels.ViewDetailed)
	if err != nil {
		return nil
	}
	return &reel
}
