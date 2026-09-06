// Package enqueue is the submission use case: one shared link becomes at most
// one global processing run, however many users share it, and every submission
// answers with either the reel the user already has or the job to poll.
package enqueue

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/XploY04/reelpin-go/internal/queue"
	"github.com/XploY04/reelpin-go/internal/reels"
	"github.com/XploY04/reelpin-go/internal/sourceidentity"
	"github.com/XploY04/reelpin-go/internal/spend"
	"github.com/google/uuid"
)

// ProcessorVersion names the pipeline this enqueue feeds. A version bump means
// old completed content is reprocessed on next demand rather than reused.
const ProcessorVersion = "go-v1"

// MaxActiveJobs is enforced inside the transaction under the user's advisory
// lock, so two concurrent submissions cannot both be the second job.
const MaxActiveJobs = 2

// IdempotencyLifetime is how long a stored response answers a retried key.
const IdempotencyLifetime = 24 * time.Hour

var (
	// ErrActiveJobLimit maps to 429 active_job_limit.
	ErrActiveJobLimit = errors.New("active job limit reached")
	// ErrIdempotencyMismatch maps to 409: the key was used with another body.
	ErrIdempotencyMismatch = errors.New("idempotency key was used with a different request")
	// ErrUnsupported maps to 422: the URL or text resolves to nothing we ingest.
	ErrUnsupported = errors.New("unsupported source")
	// ErrCollectionUnreachable maps to 422 naming collection_ids: one of the
	// ids is not a collection this user may file into. It is deliberately not
	// a 403, which would tell a stranger the collection exists, and never a
	// silent drop, which would promise a filing that never happens.
	ErrCollectionUnreachable = errors.New("collection cannot be filed into")
)

// Request is one submission attempt. Exactly one of URL or RawPayloadText is
// set; the idempotency key identifies the attempt, not the content.
type Request struct {
	UserID         string
	URL            string
	RawPayloadText string
	CollectionIDs  []string
	IdempotencyKey string
	// Endpoint scopes the idempotency key, so the same key on another endpoint
	// is a different attempt rather than a replay.
	Endpoint string
}

// OutcomeKind says which of the contract's two success shapes to answer with.
type OutcomeKind int

const (
	// AlreadySaved answers 200 with the reel: the user has this content.
	AlreadySaved OutcomeKind = iota
	// Accepted answers 202 with the job to poll.
	Accepted
)

type Result struct {
	Kind OutcomeKind
	Reel *reels.ReelRecord
	Job  *Job
	// Replayed is true when a stored idempotent response answered.
	Replayed bool
}

// Job is the private, user-visible half of a run, in the wire's terms.
type Job struct {
	ID              string
	Status          string
	URL             string
	SourcePlatform  *string
	CurrentStep     *string
	ProgressPercent int
	FailureCode     *string
	ResultReelID    *string
	CollectionIDs   []string
	CreatedAt       *time.Time
	UpdatedAt       *time.Time
}

// Store is the one transaction this package needs. Implemented by
// internal/postgres; faked in handler tests.
type Store interface {
	Submit(ctx context.Context, submission Submission) (Result, error)
}

// Submission is the resolved, validated form the transaction runs on.
type Submission struct {
	Request
	Identity  sourceidentity.SourceIdentity
	ScopeHash string
	// RoutingKey and EventType are decided here, outside the transaction:
	// routing is deterministic from platform metadata, never from a model.
	RoutingKey string
	EventType  string
	EventID    string
}

// Resolver turns raw input into an identity. Satisfied by
// *sourceidentity.Resolver.
type Resolver interface {
	Resolve(ctx context.Context, rawURL string) (sourceidentity.SourceIdentity, error)
	ResolvePayload(ctx context.Context, payload string) (sourceidentity.SourceIdentity, error)
}

// Gate decides whether new provider-costing work may still be accepted. It is
// consulted after the link resolves, because what a submission will cost
// depends on where it came from, and before anything is committed, because a
// refused submission must leave no run behind. Nil means no cost gate is
// configured and nothing is refused on spending grounds.
type Gate interface {
	Allow(ctx context.Context, work spend.Work) error
}

type Service struct {
	store    Store
	resolver Resolver
	gate     Gate
}

func New(store Store, resolver Resolver, gate Gate) *Service {
	return &Service{store: store, resolver: resolver, gate: gate}
}

// mediaPlatforms need a download before anything can be read from them.
// Everything else starts as light inspection, which may escalate later.
var mediaPlatforms = map[string]bool{
	"instagram": true,
	"youtube":   true,
	"tiktok":    true,
}

// Submit validates, resolves and runs the transaction. Everything that can
// touch the network happens before the transaction starts.
func (s *Service) Submit(ctx context.Context, request Request) (Result, error) {
	if request.UserID == "" {
		return Result{}, errors.New("submission has no user")
	}
	if _, err := uuid.Parse(strings.TrimSpace(request.IdempotencyKey)); err != nil {
		return Result{}, fmt.Errorf("%w: the idempotency key must be a UUID", ErrUnsupported)
	}
	// Shape is checked here; reach is checked inside the transaction, where a
	// collection cannot be deleted between the check and the filing.
	for _, collectionID := range request.CollectionIDs {
		if _, err := uuid.Parse(strings.TrimSpace(collectionID)); err != nil {
			return Result{}, fmt.Errorf("%w: a collection id must be a UUID", ErrCollectionUnreachable)
		}
	}

	var identity sourceidentity.SourceIdentity
	var err error
	switch {
	case request.URL != "":
		identity, err = s.resolver.Resolve(ctx, request.URL)
	case request.RawPayloadText != "":
		identity, err = s.resolver.ResolvePayload(ctx, request.RawPayloadText)
	default:
		return Result{}, fmt.Errorf("%w: no url and no shared text", ErrUnsupported)
	}
	if err != nil {
		return Result{}, fmt.Errorf("%w: %v", ErrUnsupported, err)
	}

	scopeHash, err := identity.Scope.ForUser(request.UserID).Hash()
	if err != nil {
		return Result{}, fmt.Errorf("qualifying the access scope: %w", err)
	}

	eventType := queue.EventProcessLight
	routingKey := queue.QueueLight
	class := spend.ClassLight
	if mediaPlatforms[identity.Platform] {
		eventType = queue.EventProcessMedia
		routingKey = queue.QueueMedia
		class = spend.ClassMedia
	}

	if s.gate != nil {
		if err := s.gate.Allow(ctx, spend.Work{Platform: identity.Platform, Class: class}); err != nil {
			return Result{}, err
		}
	}

	return s.store.Submit(ctx, Submission{
		Request:    request,
		Identity:   identity,
		ScopeHash:  scopeHash,
		RoutingKey: routingKey,
		EventType:  eventType,
		// One submission, one event: the id is minted here so a database retry
		// inside the store cannot mint a second.
		EventID: uuid.NewString(),
	})
}
