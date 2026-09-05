// Package enqueue turns a share into work. It is the one place that decides
// whether a shared link needs downloading at all: two users sharing the same
// public post produce one global processing run and two private jobs.
package enqueue

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/XploY04/reelpin-go/internal/jobs"
	"github.com/XploY04/reelpin-go/internal/outbox"
	"github.com/XploY04/reelpin-go/internal/sourceidentity"
	"github.com/XploY04/reelpin-go/internal/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// ProcessorVersion labels what produced a content version. It is part of
	// the dedup key, so changing it re-processes content on purpose.
	ProcessorVersion = "reelpin-v2"

	// MaxRawPayloadRunes bounds the share text a client may send.
	MaxRawPayloadRunes = 10_000
	// MaxCollectionIDs bounds the share-sheet targets.
	MaxCollectionIDs = 20

	defaultMaxAttempts = 3
)

// Limits are the per-user submission limits the Python service enforces today.
type Limits struct {
	SubmissionsPerHour int
	ActiveJobs         int
}

var DefaultLimits = Limits{SubmissionsPerHour: 20, ActiveJobs: 4}

// LimitError is a submission limit, not a rate limit: it is about how much work
// one user may have outstanding.
type LimitError struct {
	Code    string
	Message string
	Detail  string
}

func (e *LimitError) Error() string { return e.Detail }

// ErrNoURL means the share carried nothing that could be processed.
var ErrNoURL = errors.New("no link was found in the shared content")

type Request struct {
	UserID          string
	URL             string
	RawPayloadText  string
	CollectionIDs   []string
	IngestionMethod string
}

type Result struct {
	Job jobs.JobRecord
	// Reused is true when an existing job or save answered the share, so no
	// new work was created.
	Reused bool
}

type Service struct {
	pool     *pgxpool.Pool
	resolver *sourceidentity.Resolver
	limits   Limits
}

func New(pool *pgxpool.Pool, resolver *sourceidentity.Resolver, limits Limits) *Service {
	if limits.SubmissionsPerHour <= 0 {
		limits = DefaultLimits
	}
	return &Service{pool: pool, resolver: resolver, limits: limits}
}

// Enqueue is one transaction from the caller's point of view: either a job row
// exists with its outbox event, or nothing happened.
func (s *Service) Enqueue(ctx context.Context, request Request) (Result, error) {
	identity, err := s.resolve(ctx, request)
	if err != nil {
		return Result{}, err
	}

	collectionIDs, err := s.editableCollections(ctx, request.UserID, request.CollectionIDs)
	if err != nil {
		return Result{}, err
	}

	// An existing job for the same identity answers the share without creating
	// work. Re-sharing into new collections still merges the targets.
	if existing, ok, err := s.reuseJob(ctx, request.UserID, identity, collectionIDs); err != nil {
		return Result{}, err
	} else if ok {
		return Result{Job: existing, Reused: true}, nil
	}

	if err := s.checkSubmissionLimits(ctx, request.UserID); err != nil {
		return Result{}, err
	}

	transaction, err := s.pool.Begin(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("starting the enqueue transaction: %w", err)
	}
	defer transaction.Rollback(ctx)

	contentID, err := findOrCreateContent(ctx, transaction, identity)
	if err != nil {
		return Result{}, err
	}

	// Content that has already been processed needs personalizing, not
	// downloading: the expensive half is already done and shared.
	versionID, hasVersion, err := currentContentVersion(ctx, transaction, contentID)
	if err != nil {
		return Result{}, err
	}

	runID, routingQueue, err := findOrCreateRun(ctx, transaction, contentID, identity, hasVersion)
	if err != nil {
		return Result{}, err
	}

	job, err := createJob(ctx, transaction, request, identity, runID, collectionIDs)
	if err != nil {
		return Result{}, err
	}

	eventID, err := uuid.Parse(job.ID)
	if err != nil {
		return Result{}, fmt.Errorf("job id is not a uuid: %w", err)
	}
	if err := outbox.Insert(ctx, transaction, outbox.Event{
		// The job id doubles as the event id: one share produces one event, and
		// a retried enqueue cannot produce two.
		EventID:    eventID.String(),
		EventType:  eventType(hasVersion),
		RoutingKey: routingQueue,
		Payload: map[string]any{
			"run_id":             runID,
			"platform":           identity.Platform,
			"content_id":         contentID,
			"content_version_id": versionID,
			"job_id":             job.ID,
		},
	}); err != nil {
		return Result{}, err
	}

	if err := transaction.Commit(ctx); err != nil {
		return Result{}, fmt.Errorf("committing the enqueue: %w", err)
	}
	return Result{Job: job}, nil
}

func eventType(hasVersion bool) string {
	if hasVersion {
		return "content.personalize"
	}
	return "content.process"
}

// resolve turns the share into an identity, preferring an explicit URL and
// falling back to the first usable link in the raw payload.
func (s *Service) resolve(ctx context.Context, request Request) (sourceidentity.SourceIdentity, error) {
	url := strings.TrimSpace(request.URL)
	if url == "" {
		payload := request.RawPayloadText
		if len([]rune(payload)) > MaxRawPayloadRunes {
			payload = string([]rune(payload)[:MaxRawPayloadRunes])
		}
		url = sourceidentity.SelectPrimaryURL(sourceidentity.ExtractURLCandidates(payload))
	}
	if url == "" {
		return sourceidentity.SourceIdentity{}, ErrNoURL
	}

	identity, err := s.resolver.Resolve(ctx, url)
	if err != nil {
		return sourceidentity.SourceIdentity{}, err
	}
	return identity, nil
}

// editableCollections drops anything the user may not add to. A picker list is
// a cached snapshot on the device, so a target can legitimately disappear
// between choosing it and sharing: losing one target must never cost the save.
func (s *Service) editableCollections(ctx context.Context, userID string, requested []string) ([]string, error) {
	candidates := make([]string, 0, len(requested))
	seen := map[string]bool{}
	for _, raw := range requested {
		cleaned := strings.TrimSpace(raw)
		if cleaned == "" || seen[cleaned] {
			continue
		}
		if _, err := uuid.Parse(cleaned); err != nil {
			continue
		}
		seen[cleaned] = true
		candidates = append(candidates, cleaned)
		if len(candidates) >= MaxCollectionIDs {
			break
		}
	}
	if len(candidates) == 0 {
		return []string{}, nil
	}

	rows, err := s.pool.Query(ctx, `
		SELECT c.id::text
		FROM public.collections c
		LEFT JOIN public.collection_members m
		       ON m.collection_id = c.id AND m.user_id = $1
		WHERE c.id = ANY($2::uuid[])
		  AND (c.owner_id = $1 OR m.role = 'editor')`,
		userID, candidates,
	)
	if err != nil {
		return nil, fmt.Errorf("checking collection access: %w", err)
	}
	defer rows.Close()

	allowed := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("checking collection access: %w", err)
		}
		allowed[id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("checking collection access: %w", err)
	}

	// Keep the caller's order, so the first target stays the first target.
	editable := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		if allowed[candidate] {
			editable = append(editable, candidate)
		}
	}
	return editable, nil
}
