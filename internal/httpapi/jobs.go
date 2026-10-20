package httpapi

import (
	"errors"
	"net/http"

	"github.com/XploY04/reelpin-go/internal/jobs"
)

type processingJobListResponse struct {
	Jobs []jobs.Response `json:"jobs"`
}

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

	list, err := s.jobs.List(r.Context(), requestUserID(r), jobs.ListParams{ActiveOnly: activeOnly, Limit: limit})
	if err != nil {
		s.deps.Logger.Error("list processing jobs failed", "error", err)
		internalError(w, "processing_job_list_failed", "Could not load processing jobs right now.")
		return
	}
	writeJSON(w, http.StatusOK, processingJobListResponse{Jobs: list})
}

func (s *Server) handleGetJob(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "job_id", "processing_job_not_found")
	if !ok {
		return
	}

	job, err := s.jobs.Get(r.Context(), requestUserID(r), id)
	if errors.Is(err, jobs.ErrNotFound) {
		notFoundError(w, "processing_job_not_found")
		return
	}
	if err != nil {
		s.deps.Logger.Error("get processing job failed", "error", err)
		internalError(w, "processing_job_lookup_failed", "Could not load the processing job right now.")
		return
	}
	writeJSON(w, http.StatusOK, job)
}
