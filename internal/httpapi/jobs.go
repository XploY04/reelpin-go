package httpapi

import (
	"errors"
	"net/http"

	"github.com/XploY04/reelpin-go/internal/jobs"
	"github.com/XploY04/reelpin-go/internal/reels"
	"github.com/XploY04/reelpin-go/internal/uuid"
)

func (s *Server) handleListJobs(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()

	activeOnly, ok := boolParam(w, query, "active_only", false)
	if !ok {
		return
	}
	limit, ok := intParam(w, query, "limit", 20, 1, 100)
	if !ok {
		return
	}

	userID := requestUserID(r)
	records, err := s.deps.Jobs.List(r.Context(), userID, activeOnly, limit)
	if err != nil {
		s.deps.Logger.Error("list processing jobs failed", "error", err)
		internalError(w, "processing_job_list_failed", "Could not load processing jobs right now.")
		return
	}

	responses := make([]jobs.Response, 0, len(records))
	for _, record := range records {
		responses = append(responses, jobs.BuildResponse(record, s.jobResultReel(r, userID, record), s.now()))
	}
	writeJSON(w, http.StatusOK, responses)
}

func (s *Server) handleGetJob(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "job_id", "processing_job_not_found")
	if !ok {
		return
	}

	userID := requestUserID(r)
	record, err := s.deps.Jobs.Get(r.Context(), userID, id)
	if errors.Is(err, jobs.ErrNotFound) {
		notFoundError(w, "processing_job_not_found")
		return
	}
	if err != nil {
		s.deps.Logger.Error("get processing job failed", "error", err)
		internalError(w, "processing_job_lookup_failed", "Could not load the processing job right now.")
		return
	}

	writeJSON(w, http.StatusOK, jobs.BuildResponse(record, s.jobResultReel(r, userID, record), s.now()))
}

// jobResultReel attaches the saved reel a finished job produced. A reel that
// cannot be loaded is left off the response rather than failing the request.
func (s *Server) jobResultReel(r *http.Request, userID string, record jobs.JobRecord) *reels.DisplayReel {
	if record.ResultReelID == nil || *record.ResultReelID == "" {
		return nil
	}
	id, err := uuid.Parse(*record.ResultReelID)
	if err != nil {
		return nil
	}
	reel, err := s.deps.Reels.Get(r.Context(), userID, id)
	if err != nil {
		return nil
	}
	display := reels.BuildDisplayReel(reel, s.now())
	return &display
}
