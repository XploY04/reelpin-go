package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"time"

	"github.com/XploY04/reelpin-go/internal/enqueue"
	"github.com/XploY04/reelpin-go/internal/jobs"
	"github.com/XploY04/reelpin-go/internal/ratelimit"
	"github.com/XploY04/reelpin-go/internal/sourceidentity"
)

// Submitter is the submission use case, faked in handler tests.
type Submitter interface {
	Submit(ctx context.Context, request enqueue.Request) (enqueue.Result, error)
}

// ShareResolver previews shared text without processing anything.
type ShareResolver interface {
	ResolvePayload(ctx context.Context, payload string) (sourceidentity.SourceIdentity, error)
}

// RateLimiter is what the costed endpoints consult. Nil means unconfigured,
// which fails closed for them and touches reads not at all.
type RateLimiter interface {
	Allow(ctx context.Context, policy ratelimit.Policy, subject string) (ratelimit.Decision, error)
}

const maxSubmissionBody = 16 << 10 // identifiers and one URL; anything bigger is not a submission

type submitRequest struct {
	URL           string   `json:"url"`
	CollectionIDs []string `json:"collection_ids"`
}

type nativeShareRequest struct {
	RawPayloadText string   `json:"raw_payload_text"`
	CollectionIDs  []string `json:"collection_ids"`
}

// allowSubmission applies the user and IP windows and fails closed: a
// submission costs a provider call, and without a working limiter the safe
// answer is the stable 503, not an unmetered spend.
func (s *Server) allowSubmission(w http.ResponseWriter, r *http.Request, userID string) bool {
	if s.deps.Limiter == nil {
		writeError(w, http.StatusServiceUnavailable, errorBody{
			Code:      "processing_unavailable",
			Message:   "Submissions are unavailable right now.",
			Retryable: true,
		})
		return false
	}

	for _, check := range []struct {
		policy  ratelimit.Policy
		subject string
	}{
		{ratelimit.Submission, userID},
		{ratelimit.SubmissionIP, s.limitSubject(r)},
	} {
		decision, err := s.deps.Limiter.Allow(r.Context(), check.policy, check.subject)
		if errors.Is(err, ratelimit.ErrUnavailable) {
			writeError(w, http.StatusServiceUnavailable, errorBody{
				Code:      "processing_unavailable",
				Message:   "Submissions are unavailable right now.",
				Retryable: true,
			})
			return false
		}
		if err != nil {
			s.deps.Logger.Error("rate limit check failed", "policy", check.policy.Name, "error", err)
			internalError(w, "internal_error", "The server could not finish this request.")
			return false
		}
		if !decision.Allowed {
			w.Header().Set("Retry-After", formatSeconds(decision.RetryAfter))
			writeError(w, http.StatusTooManyRequests, errorBody{
				Code:      "rate_limited",
				Message:   "Too many submissions. Try again later.",
				Retryable: true,
				Details:   map[string]any{"retry_after_seconds": int(decision.RetryAfter.Seconds())},
			})
			return false
		}
	}
	return true
}

// limitSubject is what a per-IP policy counts against.
//
// The socket peer is the honest answer for a request from a phone. It is the
// wrong answer once Next.js is the caller, because every web visitor arrives
// from one socket and the whole web collapses into one bucket. So the boundary
// that really sees the client forwards an opaque bucket it has signed, and this
// side believes it only if it verifies. A forged or absent header costs nothing
// and changes nothing: it falls through to the peer.
func (s *Server) limitSubject(r *http.Request) string {
	if bucket := ratelimit.VerifyIPBucket(
		r.Header.Get(ratelimit.IPBucketHeader), s.deps.IPBucketSecret, s.deps.Now(),
	); bucket != "" {
		return bucket
	}
	return clientIP(r)
}

// clientIP is the socket peer. A forwarding header is never read here: anyone
// can send one, and the signed bucket above is how a real one arrives.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func formatSeconds(d time.Duration) string {
	seconds := int(d.Seconds())
	if seconds < 1 {
		seconds = 1
	}
	return time.Duration(seconds * int(time.Second)).String()
}

// idempotencyKey requires the header the contract documents. Retrying without
// one cannot be made safe, so it is a validation error, not a default.
func idempotencyKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		validationError(w, "Idempotency-Key", "the header is required")
		return "", false
	}
	return key, true
}

func decodeBody(w http.ResponseWriter, r *http.Request, into any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxSubmissionBody))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		validationError(w, "body", "must be a JSON object matching the contract")
		return false
	}
	return true
}

func (s *Server) handleSubmitReel(w http.ResponseWriter, r *http.Request) {
	userID := requestUserID(r)
	key, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	var body submitRequest
	if !decodeBody(w, r, &body) {
		return
	}
	if body.URL == "" {
		validationError(w, "url", "is required")
		return
	}
	if !s.allowSubmission(w, r, userID) {
		return
	}

	result, err := s.deps.Enqueue.Submit(r.Context(), enqueue.Request{
		UserID:         userID,
		URL:            body.URL,
		CollectionIDs:  body.CollectionIDs,
		IdempotencyKey: key,
		Endpoint:       "processing-jobs/reels",
	})
	s.respondToSubmission(w, result, err)
}

func (s *Server) handleNativeShare(w http.ResponseWriter, r *http.Request) {
	userID := requestUserID(r)
	key, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	var body nativeShareRequest
	if !decodeBody(w, r, &body) {
		return
	}
	if body.RawPayloadText == "" {
		validationError(w, "raw_payload_text", "is required")
		return
	}
	if !s.allowSubmission(w, r, userID) {
		return
	}

	result, err := s.deps.Enqueue.Submit(r.Context(), enqueue.Request{
		UserID:         userID,
		RawPayloadText: body.RawPayloadText,
		CollectionIDs:  body.CollectionIDs,
		IdempotencyKey: key,
		Endpoint:       "native-shares/reels",
	})
	s.respondToSubmission(w, result, err)
}

// respondToSubmission maps the use case's outcomes onto the contract's shapes.
func (s *Server) respondToSubmission(w http.ResponseWriter, result enqueue.Result, err error) {
	switch {
	case err == nil:
	case errors.Is(err, enqueue.ErrActiveJobLimit):
		writeError(w, http.StatusTooManyRequests, errorBody{
			Code:      "active_job_limit",
			Message:   "Two submissions are already processing. Wait for one to finish.",
			Retryable: true,
		})
		return
	case errors.Is(err, enqueue.ErrIdempotencyMismatch):
		writeError(w, http.StatusConflict, errorBody{
			Code:    "idempotency_conflict",
			Message: "This Idempotency-Key was already used with a different request.",
		})
		return
	case errors.Is(err, enqueue.ErrCollectionUnreachable):
		validationError(w, "collection_ids", "must all be collections you can add to")
		return
	case errors.Is(err, enqueue.ErrUnsupported):
		validationError(w, "url", "is not a link this service can ingest")
		return
	default:
		s.deps.Logger.Error("submission failed", "error", err)
		internalError(w, "submission_failed", "Could not accept the submission right now.")
		return
	}

	if result.Kind == enqueue.AlreadySaved {
		writeJSON(w, http.StatusOK, buildReelResponse(*result.Reel))
		return
	}
	writeJSON(w, http.StatusAccepted, presentJob(*result.Job))
}

// presentJob keeps the wire shape identical to the read side's job.
func presentJob(job enqueue.Job) map[string]any {
	return map[string]any{
		"id":               job.ID,
		"status":           job.Status,
		"url":              job.URL,
		"source_platform":  job.SourcePlatform,
		"current_step":     job.CurrentStep,
		"progress_percent": job.ProgressPercent,
		"status_message": jobs.StatusMessage(jobs.JobRecord{
			Status: job.Status, CurrentStep: job.CurrentStep,
		}),
		"failure_code":   job.FailureCode,
		"result_reel_id": job.ResultReelID,
		"collection_ids": job.CollectionIDs,
		"created_at":     job.CreatedAt,
		"updated_at":     job.UpdatedAt,
	}
}

func (s *Server) handleResolveShare(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RawPayloadText string `json:"raw_payload_text"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if body.RawPayloadText == "" {
		validationError(w, "raw_payload_text", "is required")
		return
	}

	identity, err := s.deps.Resolver.ResolvePayload(r.Context(), body.RawPayloadText)
	if err != nil {
		// Unsupported is an answer, not an error: the preview says so.
		writeJSON(w, http.StatusOK, map[string]any{
			"supported":           false,
			"url":                 nil,
			"source_platform":     nil,
			"source_content_type": nil,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"supported":           true,
		"url":                 identity.NormalizedURL,
		"source_platform":     identity.Platform,
		"source_content_type": identity.ContentType,
	})
}

func (s *Server) handleMintShareToken(w http.ResponseWriter, r *http.Request) {
	token, expiresAt, err := s.deps.ShareTokens.Mint(r.Context(), requestUserID(r))
	if err != nil {
		s.deps.Logger.Error("minting a share token failed", "error", err)
		internalError(w, "share_token_failed", "Could not issue a share token right now.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"token":      token,
		"expires_at": expiresAt.UTC(),
	})
}

func (s *Server) handleRevokeShareTokens(w http.ResponseWriter, r *http.Request) {
	revoked, err := s.deps.ShareTokens.RevokeAll(r.Context(), requestUserID(r))
	if err != nil {
		s.deps.Logger.Error("revoking share tokens failed", "error", err)
		internalError(w, "share_token_failed", "Could not revoke share tokens right now.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"revoked": revoked})
}
