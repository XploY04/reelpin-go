package httpapi

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/XploY04/reelpin-go/internal/auth"
	"github.com/XploY04/reelpin-go/internal/collections"
	"github.com/XploY04/reelpin-go/internal/enqueue"
	"github.com/XploY04/reelpin-go/internal/jobs"
	"github.com/XploY04/reelpin-go/internal/ratelimit"
	"github.com/XploY04/reelpin-go/internal/reels"
	"github.com/XploY04/reelpin-go/internal/search"
	"github.com/XploY04/reelpin-go/internal/spend"
	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type mcpListReelsInput struct {
	Platform    string `json:"platform,omitempty" jsonschema:"Only reels from this platform: instagram, youtube, x, tiktok, pinterest, reddit, linkedin or other. Omit for every platform."`
	Category    string `json:"category,omitempty" jsonschema:"Only reels in this category, as the category field on a reel spells it."`
	Subcategory string `json:"subcategory,omitempty" jsonschema:"Only reels in this subcategory, as the sub_category field on a reel spells it."`
	SavedDate   string `json:"saved_date,omitempty" jsonschema:"Only reels saved on this day, as YYYY-MM-DD."`
	Cursor      string `json:"cursor,omitempty" jsonschema:"The next_cursor from the previous page. Omit to start at the newest reel."`
	Limit       int    `json:"limit,omitempty" jsonschema:"Reels per page, 1 to 100. Default 25."`
}

type mcpGetReelInput struct {
	ReelID string `json:"reel_id" jsonschema:"The reel's id, as list_reels, search_reels or a collection returned it."`
}

type mcpSearchInput struct {
	Query       string `json:"query" jsonschema:"What to look for, in plain words. Matches meaning, exact words and misspelt titles."`
	Platform    string `json:"platform,omitempty" jsonschema:"Only reels from this platform: instagram, youtube, x, tiktok, pinterest, reddit, linkedin or other. Omit for every platform."`
	Category    string `json:"category,omitempty" jsonschema:"Only reels in this category, as the category field on a reel spells it."`
	Subcategory string `json:"subcategory,omitempty" jsonschema:"Only reels in this subcategory, as the sub_category field on a reel spells it."`
	SavedDate   string `json:"saved_date,omitempty" jsonschema:"Only reels saved on this day, as YYYY-MM-DD."`
	Limit       int    `json:"limit,omitempty" jsonschema:"Results to return, 1 to 20. Default 5."`
}

type mcpSaveLinkInput struct {
	URL            string   `json:"url" jsonschema:"The link to save: an Instagram reel or post, a YouTube video, a TikTok, an X post, a Reddit post, a LinkedIn post, a Pinterest pin, a Google Maps place or any web page."`
	CollectionIDs  []string `json:"collection_ids,omitempty" jsonschema:"Collections to file the reel into once it is saved. Each must be one the user can add to."`
	IdempotencyKey string   `json:"idempotency_key,omitempty" jsonschema:"A UUID naming this attempt. Send the same one when retrying the same save so it is not counted twice. Omit to start a fresh attempt."`
}

type mcpNoInput struct{}

type mcpGetCollectionInput struct {
	CollectionID string `json:"collection_id" jsonschema:"The collection's id, as list_collections returned it."`
	Cursor       string `json:"cursor,omitempty" jsonschema:"The next_cursor from the previous page. Omit to start at the newest reel."`
	Limit        int    `json:"limit,omitempty" jsonschema:"Reels per page, 1 to 100. Default 25."`
}

type mcpAddToCollectionInput struct {
	CollectionID string   `json:"collection_id" jsonschema:"The collection's id, as list_collections returned it."`
	ReelIDs      []string `json:"reel_ids" jsonschema:"Ids of the user's own reels to file, up to 100."`
}

// mcpReelCard is the compact reel every list, search hit and collection item
// carries: enough to recognise a reel and link to it, never the transcript.
type mcpReelCard struct {
	ID           string  `json:"id"`
	Title        string  `json:"title"`
	Summary      string  `json:"summary"`
	Platform     string  `json:"platform"`
	Category     string  `json:"category"`
	SubCategory  string  `json:"sub_category"`
	URL          string  `json:"url"`
	WebURL       string  `json:"web_url"`
	ThumbnailURL *string `json:"thumbnail_url"`
	SavedAt      *string `json:"saved_at"`
	Location     *string `json:"location"`
}

type mcpReelDetail struct {
	mcpReelCard
	ContentType     string        `json:"content_type"`
	Transcript      string        `json:"transcript"`
	KeyFacts        []string      `json:"key_facts"`
	Tags            []string      `json:"tags"`
	PeopleMentioned []string      `json:"people_mentioned"`
	ActionableItems []string      `json:"actionable_items"`
	Events          []reels.Event `json:"events"`
	Locations       []mcpLocation `json:"locations"`
}

type mcpLocation struct {
	Name          string   `json:"name"`
	Address       string   `json:"address"`
	Latitude      *float64 `json:"latitude"`
	Longitude     *float64 `json:"longitude"`
	GoogleMapsURL *string  `json:"google_maps_url"`
}

type mcpReelPage struct {
	Reels      []mcpReelCard `json:"reels"`
	NextCursor *string       `json:"next_cursor"`
	HasMore    bool          `json:"has_more"`
}

type mcpSearchHit struct {
	Reel             mcpReelCard `json:"reel"`
	RelevancePercent int         `json:"relevance_percent"`
	MatchLabel       string      `json:"match_label"`
}

type mcpSearchPage struct {
	Query      string         `json:"query"`
	SearchMode string         `json:"search_mode"`
	Results    []mcpSearchHit `json:"results"`
}

type mcpJob struct {
	ID              string  `json:"id"`
	Status          string  `json:"status"`
	CurrentStep     *string `json:"current_step"`
	ProgressPercent int     `json:"progress_percent"`
	StatusMessage   string  `json:"status_message"`
	ResultReelID    *string `json:"result_reel_id"`
}

type mcpSaveResult struct {
	// Status is "saved" when the reel is ready, "processing" while a job runs
	// for it, and the job's own final status when a retried key replays a job
	// that has since finished.
	Status       string       `json:"status"`
	AlreadySaved bool         `json:"already_saved"`
	Reel         *mcpReelCard `json:"reel,omitempty"`
	Job          *mcpJob      `json:"job,omitempty"`
}

type mcpCollection struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	WebURL      string `json:"web_url"`
	ItemCount   int    `json:"item_count"`
	Role        string `json:"role"`
	Visibility  string `json:"visibility"`
}

type mcpCollectionList struct {
	Collections []mcpCollection `json:"collections"`
}

type mcpCollectionItem struct {
	mcpReelCard
	AddedAt *string `json:"added_at"`
}

type mcpCollectionPage struct {
	Collection mcpCollection       `json:"collection"`
	Items      []mcpCollectionItem `json:"items"`
	NextCursor *string             `json:"next_cursor"`
	HasMore    bool                `json:"has_more"`
	CanEdit    bool                `json:"can_edit"`
}

type mcpCollectionChange struct {
	Collection mcpCollection `json:"collection"`
	AddedCount int           `json:"added_count"`
}

func (s *Server) addMCPTools(server *mcp.Server) {
	hint := func(b bool) *bool { return &b }
	readOnly := &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: hint(false)}

	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_reels",
		Description: "List the signed-in user's saved reels, newest first, one page at a time. Filter by platform, category, subcategory or the day they were saved. Use get_reel for one reel in full and search_reels to find reels by meaning.",
		Annotations: readOnly,
	}, s.mcpListReels)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_reel",
		Description: "Get one of the signed-in user's saved reels in full: summary, transcript, key facts, people, action items, events and the places ReelPin extracted, plus its web_url.",
		Annotations: readOnly,
	}, s.mcpGetReel)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "search_reels",
		Description: "Search the signed-in user's saved reels by meaning, exact words or a misspelt title, ranked by relevance. Narrow with the same filters as list_reels. Every search costs a provider call and is rate limited, so use list_reels to browse.",
		Annotations: readOnly,
	}, s.mcpSearchReels)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "save_link",
		Description: "Save a link to the signed-in user's ReelPin library. A link the user already has answers already_saved with the reel or the job still processing it. A new link starts processing and answers a job; processing takes up to a few minutes, after which the reel appears in list_reels and search_reels. Retrying with the same idempotency_key answers that job as it stands now, so status can be completed or failed.",
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:    false,
			DestructiveHint: hint(false),
			IdempotentHint:  true,
			OpenWorldHint:   hint(true),
		},
	}, s.mcpSaveLink)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_collections",
		Description: "List the signed-in user's collections: the ones they own and the ones shared with them. Use get_collection to see what is in one.",
		Annotations: readOnly,
	}, s.mcpListCollections)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_collection",
		Description: "Get one collection and a page of the reels in it, most recently added first. can_edit says whether add_to_collection would work for this user.",
		Annotations: readOnly,
	}, s.mcpGetCollection)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "add_to_collection",
		Description: "Add reels the signed-in user owns to a collection they own or can edit. Reels already in it are skipped; added_count is how many were new.",
		Annotations: &mcp.ToolAnnotations{
			DestructiveHint: hint(false),
			IdempotentHint:  true,
			OpenWorldHint:   hint(false),
		},
	}, s.mcpAddToCollection)
}

func (s *Server) mcpListReels(ctx context.Context, _ *mcp.CallToolRequest, in mcpListReelsInput) (*mcp.CallToolResult, mcpReelPage, error) {
	userID, err := mcpUser(ctx)
	if err != nil {
		return nil, mcpReelPage{}, err
	}
	platforms, err := mcpPlatforms(in.Platform)
	if err != nil {
		return nil, mcpReelPage{}, err
	}
	savedDate, err := mcpSavedDate(in.SavedDate)
	if err != nil {
		return nil, mcpReelPage{}, err
	}
	after, err := mcpCursor(in.Cursor)
	if err != nil {
		return nil, mcpReelPage{}, err
	}
	limit, err := mcpLimit(in.Limit, reels.DefaultPageSize, 1, reels.MaxPageSize)
	if err != nil {
		return nil, mcpReelPage{}, err
	}

	page, err := s.reels.List(ctx, userID, reels.ListParams{
		Platforms:   platforms,
		Category:    in.Category,
		Subcategory: in.Subcategory,
		SavedDate:   savedDate,
		After:       after,
		Limit:       limit,
		View:        reels.ViewConcise,
	})
	if err != nil {
		return nil, mcpReelPage{}, s.mcpInternal(ctx, "list_reels", err, "could not load reels right now")
	}

	cards := make([]mcpReelCard, 0, len(page.Reels))
	for _, reel := range page.Reels {
		cards = append(cards, mcpCard(reel))
	}
	return nil, mcpReelPage{Reels: cards, NextCursor: page.NextCursor, HasMore: page.HasMore}, nil
}

func (s *Server) mcpGetReel(ctx context.Context, _ *mcp.CallToolRequest, in mcpGetReelInput) (*mcp.CallToolResult, mcpReelDetail, error) {
	userID, err := mcpUser(ctx)
	if err != nil {
		return nil, mcpReelDetail{}, err
	}
	// A malformed id answers exactly as a missing one, so ids cannot be probed.
	id, err := uuid.Parse(in.ReelID)
	if err != nil {
		return nil, mcpReelDetail{}, errors.New("reel not found")
	}

	reel, err := s.reels.Get(ctx, userID, id)
	if errors.Is(err, reels.ErrNotFound) {
		return nil, mcpReelDetail{}, errors.New("reel not found")
	}
	if err != nil {
		return nil, mcpReelDetail{}, s.mcpInternal(ctx, "get_reel", err, "could not load the reel right now")
	}
	return nil, mcpDetail(reel), nil
}

func (s *Server) mcpSearchReels(ctx context.Context, _ *mcp.CallToolRequest, in mcpSearchInput) (*mcp.CallToolResult, mcpSearchPage, error) {
	userID, err := mcpUser(ctx)
	if err != nil {
		return nil, mcpSearchPage{}, err
	}
	if strings.TrimSpace(in.Query) == "" {
		return nil, mcpSearchPage{}, errors.New("query is required")
	}
	// Zero passes through: the search service applies its own default.
	limit, err := mcpLimit(in.Limit, 0, 1, search.MaxLimit)
	if err != nil {
		return nil, mcpSearchPage{}, err
	}
	platforms, err := mcpPlatforms(in.Platform)
	if err != nil {
		return nil, mcpSearchPage{}, err
	}
	savedDate, err := mcpSavedDate(in.SavedDate)
	if err != nil {
		return nil, mcpSearchPage{}, err
	}
	if err := s.allowMCPMetered(ctx, userID, ratelimit.Search, ratelimit.SearchIP,
		"search is unavailable right now", "too many searches"); err != nil {
		return nil, mcpSearchPage{}, err
	}

	response, err := s.deps.Search.Search(ctx, userID, search.Params{
		Query: in.Query,
		Filters: search.Filters{
			Platforms:   platforms,
			Category:    in.Category,
			Subcategory: in.Subcategory,
			SavedDate:   savedDate,
		},
		Limit: limit,
		View:  reels.ViewConcise,
	})
	if err != nil {
		return nil, mcpSearchPage{}, s.mcpInternal(ctx, "search_reels", err, "could not search right now")
	}

	hits := make([]mcpSearchHit, 0, len(response.Results))
	for _, result := range response.Results {
		hits = append(hits, mcpSearchHit{
			Reel:             mcpCard(result.Reel),
			RelevancePercent: result.RelevancePercent,
			MatchLabel:       result.DisplayScoreLabel,
		})
	}
	return nil, mcpSearchPage{Query: response.Query, SearchMode: response.SearchMode, Results: hits}, nil
}

func (s *Server) mcpSaveLink(ctx context.Context, _ *mcp.CallToolRequest, in mcpSaveLinkInput) (*mcp.CallToolResult, mcpSaveResult, error) {
	userID, err := mcpUser(ctx)
	if err != nil {
		return nil, mcpSaveResult{}, err
	}
	if strings.TrimSpace(in.URL) == "" {
		return nil, mcpSaveResult{}, errors.New("url is required")
	}
	key := in.IdempotencyKey
	if key == "" {
		key = uuid.NewString()
	} else if _, err := uuid.Parse(key); err != nil {
		return nil, mcpSaveResult{}, errors.New("idempotency_key must be a UUID")
	}
	if err := s.allowMCPMetered(ctx, userID, ratelimit.Submission, ratelimit.SubmissionIP,
		"saving is unavailable right now", "too many saves"); err != nil {
		return nil, mcpSaveResult{}, err
	}

	// The endpoint scopes the key: the same key on a REST route is a different attempt.
	result, err := s.deps.Enqueue.Submit(ctx, enqueue.Request{
		UserID:         userID,
		URL:            in.URL,
		CollectionIDs:  in.CollectionIDs,
		IdempotencyKey: key,
		Endpoint:       "mcp/save_link",
	})
	if err != nil {
		return nil, mcpSaveResult{}, s.mcpSubmissionError(ctx, err)
	}
	s.invalidateUserCache(ctx, userID)

	saved := mcpSaveResult{AlreadySaved: result.WasAlreadySaved()}
	switch result.Kind {
	case enqueue.AlreadySaved, enqueue.Reused:
		card := mcpCardFromReel(s.reels.Present(*result.Reel))
		saved.Status, saved.Reel = "saved", &card
	default:
		job := mcpJobFrom(*result.Job)
		saved.Status, saved.Job = "processing", &job
		if jobs.Terminal(jobs.JobRecord{Status: job.Status}) {
			saved.Status = job.Status
		}
	}
	return nil, saved, nil
}

func (s *Server) mcpSubmissionError(ctx context.Context, err error) error {
	switch {
	case errors.Is(err, enqueue.ErrActiveJobLimit):
		return errors.New("five links are already processing; wait for one to finish")
	case errors.Is(err, enqueue.ErrIdempotencyMismatch):
		return errors.New("this idempotency_key was already used for a different save")
	case errors.Is(err, enqueue.ErrCollectionUnreachable):
		return errors.New("collection_ids must all be collections you can add to")
	case errors.Is(err, spend.ErrLimitReached):
		return errors.New("saving is paused until the monthly provider budget is reviewed; everything already saved is still available")
	case errors.Is(err, enqueue.ErrUnsupported):
		return errors.New("this is not a link ReelPin can save")
	}
	return s.mcpInternal(ctx, "save_link", err, "could not save the link right now")
}

func (s *Server) mcpListCollections(ctx context.Context, _ *mcp.CallToolRequest, _ mcpNoInput) (*mcp.CallToolResult, mcpCollectionList, error) {
	userID, err := mcpUser(ctx)
	if err != nil {
		return nil, mcpCollectionList{}, err
	}

	list, err := s.collections.List(ctx, userID)
	if err != nil {
		return nil, mcpCollectionList{}, s.mcpInternal(ctx, "list_collections", err, "could not load collections right now")
	}

	out := make([]mcpCollection, 0, len(list))
	for _, collection := range list {
		out = append(out, mcpCollectionFrom(collection))
	}
	return nil, mcpCollectionList{Collections: out}, nil
}

func (s *Server) mcpGetCollection(ctx context.Context, _ *mcp.CallToolRequest, in mcpGetCollectionInput) (*mcp.CallToolResult, mcpCollectionPage, error) {
	userID, err := mcpUser(ctx)
	if err != nil {
		return nil, mcpCollectionPage{}, err
	}
	id, err := uuid.Parse(in.CollectionID)
	if err != nil {
		return nil, mcpCollectionPage{}, errors.New("collection not found")
	}
	after, err := mcpCursor(in.Cursor)
	if err != nil {
		return nil, mcpCollectionPage{}, err
	}
	limit, err := mcpLimit(in.Limit, reels.DefaultPageSize, 1, reels.MaxPageSize)
	if err != nil {
		return nil, mcpCollectionPage{}, err
	}

	detail, err := s.collections.Detail(ctx, id.String(), userID, collections.ItemParams{
		After: after, Limit: limit, View: reels.ViewConcise,
	})
	switch {
	case errors.Is(err, collections.ErrNotFound):
		return nil, mcpCollectionPage{}, errors.New("collection not found")
	case errors.Is(err, reels.ErrNotFound):
		return nil, mcpCollectionPage{}, errors.New("reel not found")
	case err != nil:
		return nil, mcpCollectionPage{}, s.mcpInternal(ctx, "get_collection", err, "could not load the collection right now")
	}

	items := make([]mcpCollectionItem, 0, len(detail.Items))
	for _, item := range detail.Items {
		items = append(items, mcpCollectionItem{mcpReelCard: mcpCardFromReel(item.Reel), AddedAt: item.AddedAt})
	}
	return nil, mcpCollectionPage{
		Collection: mcpCollectionFrom(detail.Collection),
		Items:      items,
		NextCursor: detail.Page.NextCursor,
		HasMore:    detail.Page.HasMore,
		CanEdit:    detail.CanEdit,
	}, nil
}

func (s *Server) mcpAddToCollection(ctx context.Context, _ *mcp.CallToolRequest, in mcpAddToCollectionInput) (*mcp.CallToolResult, mcpCollectionChange, error) {
	userID, err := mcpUser(ctx)
	if err != nil {
		return nil, mcpCollectionChange{}, err
	}
	if len(in.ReelIDs) == 0 {
		return nil, mcpCollectionChange{}, errors.New("reel_ids must name at least one reel")
	}
	if len(in.ReelIDs) > collections.MaxSaveIDsPerRequest {
		return nil, mcpCollectionChange{}, fmt.Errorf("reel_ids can name at most %d reels", collections.MaxSaveIDsPerRequest)
	}
	id, err := uuid.Parse(in.CollectionID)
	if err != nil {
		return nil, mcpCollectionChange{}, errors.New("collection not found")
	}

	collection, added, err := s.collections.AddItems(ctx, id.String(), userID, in.ReelIDs)
	switch {
	case errors.Is(err, collections.ErrNotFound):
		return nil, mcpCollectionChange{}, errors.New("collection not found")
	case errors.Is(err, collections.ErrForbidden):
		return nil, mcpCollectionChange{}, errors.New("this collection is read-only for you")
	case errors.Is(err, reels.ErrNotFound):
		return nil, mcpCollectionChange{}, errors.New("reel not found: every id must be one of your own reels")
	case err != nil:
		return nil, mcpCollectionChange{}, s.mcpInternal(ctx, "add_to_collection", err, "could not update the collection right now")
	}
	s.invalidateUserCache(ctx, userID)
	return nil, mcpCollectionChange{Collection: mcpCollectionFrom(collection), AddedCount: added}, nil
}

func mcpCard(reel reels.DisplayReel) mcpReelCard {
	return mcpReelCard{
		ID:           reel.ID,
		Title:        reel.Title,
		Summary:      reel.Summary,
		Platform:     reels.RecordPlatform(reel.SourcePlatform),
		Category:     reel.Category,
		SubCategory:  reel.SubCategory,
		URL:          reel.URL,
		WebURL:       reel.WebURL,
		ThumbnailURL: reel.ThumbnailURL,
		SavedAt:      reel.CreatedAt,
		Location:     reel.PrimaryLocationLabel,
	}
}

func mcpCardFromReel(reel reels.Reel) mcpReelCard {
	return mcpReelCard{
		ID:           reel.ID,
		Title:        reel.Title,
		Summary:      reel.Summary,
		Platform:     reels.RecordPlatform(reel.SourcePlatform),
		Category:     reel.Category,
		SubCategory:  reel.SubCategory,
		URL:          reel.URL,
		WebURL:       reel.WebURL,
		ThumbnailURL: reel.ThumbnailURL,
		SavedAt:      reel.CreatedAt,
		Location:     reel.PrimaryLocationLabel,
	}
}

func mcpDetail(reel reels.Reel) mcpReelDetail {
	locations := make([]mcpLocation, 0, len(reel.MappableLocations))
	for _, location := range reel.MappableLocations {
		locations = append(locations, mcpLocation{
			Name:          location.LocationName,
			Address:       location.LocationAddress,
			Latitude:      location.Latitude,
			Longitude:     location.Longitude,
			GoogleMapsURL: location.GoogleMapsURL,
		})
	}
	return mcpReelDetail{
		mcpReelCard:     mcpCardFromReel(reel),
		ContentType:     reel.ContentType,
		Transcript:      reel.Transcript,
		KeyFacts:        reel.KeyFacts,
		Tags:            reel.SecondaryCategories,
		PeopleMentioned: reel.PeopleMentioned,
		ActionableItems: reel.ActionableItems,
		Events:          reel.Events,
		Locations:       locations,
	}
}

func mcpJobFrom(job enqueue.Job) mcpJob {
	return mcpJob{
		ID:              job.ID,
		Status:          job.Status,
		CurrentStep:     job.CurrentStep,
		ProgressPercent: job.ProgressPercent,
		StatusMessage: jobs.StatusMessage(jobs.JobRecord{
			Status: job.Status, CurrentStep: job.CurrentStep, FailureCode: job.FailureCode,
		}),
		ResultReelID: job.ResultReelID,
	}
}

func mcpCollectionFrom(collection collections.Collection) mcpCollection {
	return mcpCollection{
		ID:          collection.ID,
		Name:        collection.Name,
		Description: collection.Description,
		WebURL:      collection.WebURL,
		ItemCount:   collection.ItemCount,
		Role:        collection.Role,
		Visibility:  collection.Visibility,
	}
}

// mcpUser is the loud failure if the middleware ever stops guaranteeing a user.
func mcpUser(ctx context.Context) (string, error) {
	userID, ok := auth.UserID(ctx)
	if !ok {
		return "", errors.New("sign in is required")
	}
	return userID, nil
}

// allowMCPMetered is allowMetered for a tool: the same buckets in order, failing closed.
func (s *Server) allowMCPMetered(ctx context.Context, userID string, user, ip ratelimit.Policy, unavailable, tooMany string) error {
	if s.deps.Limiter == nil {
		return errors.New(unavailable)
	}

	for _, check := range meteredChecks(userID, auth.ClientID(ctx), mcpCallFromContext(ctx).limitSubject, user, ip) {
		decision, err := s.deps.Limiter.Allow(ctx, check.policy, check.subject)
		if errors.Is(err, ratelimit.ErrUnavailable) {
			return errors.New(unavailable)
		}
		if err != nil {
			s.deps.Logger.Error("rate limit check failed", "policy", check.policy.Name, "error", err)
			return errors.New("the server could not finish this request")
		}
		if !decision.Allowed {
			seconds := max(int(decision.RetryAfter.Seconds()), 1)
			return fmt.Errorf("%s; try again in %d seconds", tooMany, seconds)
		}
	}
	return nil
}

func (s *Server) invalidateUserCache(ctx context.Context, userID string) {
	if s.deps.Cache == nil {
		return
	}
	if err := s.deps.Cache.InvalidateUser(ctx, userID); err != nil {
		// The read cache expires on its own; a stale screen is the cost.
		s.deps.Logger.Warn("invalidating the user's cache failed",
			"request_id", mcpCallFromContext(ctx).requestID, "error", err)
	}
}

// mcpInternal keeps the driver error in the log and hands the model a sentence.
func (s *Server) mcpInternal(ctx context.Context, tool string, err error, message string) error {
	s.deps.Logger.Error("mcp tool failed",
		"tool", tool, "request_id", mcpCallFromContext(ctx).requestID, "error", err)
	return errors.New(message)
}

func mcpPlatforms(value string) ([]string, error) {
	platforms, err := reels.ParsePlatformFilter(value)
	var invalid *reels.InvalidPlatformError
	if errors.As(err, &invalid) {
		return nil, fmt.Errorf("platform %q is not supported; use one of %s",
			invalid.Value, strings.Join(invalid.Allowed, ", "))
	}
	return platforms, err
}

func mcpSavedDate(value string) (string, error) {
	raw := strings.TrimSpace(value)
	if raw == "" {
		return "", nil
	}
	if _, err := time.Parse("2006-01-02", raw); err != nil {
		return "", errors.New("saved_date must be YYYY-MM-DD")
	}
	return raw, nil
}

func mcpCursor(value string) (*reels.Cursor, error) {
	raw := strings.TrimSpace(value)
	if raw == "" {
		return nil, nil
	}
	cursor, err := reels.DecodeCursor(raw)
	if err != nil {
		return nil, errors.New("cursor is not one this server issued; use next_cursor from the previous page")
	}
	return &cursor, nil
}

// mcpLimit reads zero as absent, which is how an omitted optional field decodes.
func mcpLimit(value, fallback, minimum, maximum int) (int, error) {
	if value == 0 {
		return fallback, nil
	}
	if value < minimum || value > maximum {
		return 0, fmt.Errorf("limit must be between %d and %d", minimum, maximum)
	}
	return value, nil
}
