package readcache

import (
	"context"

	"github.com/XploY04/reelpin-go/internal/jobs"
	"github.com/XploY04/reelpin-go/internal/reels"
)

// Reels caches the library reads an app open makes: the list, the filter
// facets and the library stats. A single reel is read through, because it is
// opened from a list that was just served and carries the transcript.
type Reels struct {
	reels.ReelReader
	cache *Cache
}

// CachedReels wraps a reader. Without a cache it returns the reader itself.
func CachedReels(reader reels.ReelReader, cache *Cache) reels.ReelReader {
	if cache == nil {
		return reader
	}
	return Reels{ReelReader: reader, cache: cache}
}

func (r Reels) List(ctx context.Context, userID string, options reels.ListOptions) ([]reels.ReelRecord, error) {
	return read(ctx, r.cache, userID, "reels.list", options, func() ([]reels.ReelRecord, error) {
		return r.ReelReader.List(ctx, userID, options)
	})
}

func (r Reels) Facets(ctx context.Context, userID string) ([]reels.FacetRow, error) {
	return read(ctx, r.cache, userID, "reels.facets", nil, func() ([]reels.FacetRow, error) {
		return r.ReelReader.Facets(ctx, userID)
	})
}

func (r Reels) Stats(ctx context.Context, userID string) (reels.LibraryStats, error) {
	return read(ctx, r.cache, userID, "reels.stats", nil, func() (reels.LibraryStats, error) {
		return r.ReelReader.Stats(ctx, userID)
	})
}

// Jobs caches the job list. A single job is what the app polls while a run is
// in progress, so it is always read through.
type Jobs struct {
	jobs.JobReader
	cache *Cache
}

// CachedJobs wraps a reader. Without a cache it returns the reader itself.
func CachedJobs(reader jobs.JobReader, cache *Cache) jobs.JobReader {
	if cache == nil {
		return reader
	}
	return Jobs{JobReader: reader, cache: cache}
}

type jobListParams struct {
	ActiveOnly bool `json:"active_only"`
	Limit      int  `json:"limit"`
}

func (j Jobs) List(ctx context.Context, userID string, activeOnly bool, limit int) ([]jobs.JobRecord, error) {
	params := jobListParams{ActiveOnly: activeOnly, Limit: limit}
	return read(ctx, j.cache, userID, "jobs.list", params, func() ([]jobs.JobRecord, error) {
		return j.JobReader.List(ctx, userID, activeOnly, limit)
	})
}
