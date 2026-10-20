package entitlements

import (
	"context"

	"github.com/XploY04/reelpin-go/internal/reels"
)

// UsageReader reads the library totals a plan is measured against.
type UsageReader interface {
	Stats(ctx context.Context, userID string) (reels.LibraryStats, error)
}

// ProStatus reports whether a user is on the paid plan. The implementation
// caches the active grants, so a call is in-memory on the request path.
type ProStatus interface {
	IsPro(ctx context.Context, userID string) bool
}

// Service answers a user's plan and how much of it they use.
type Service struct {
	usage UsageReader
	pro   ProStatus
}

func NewService(usage UsageReader, pro ProStatus) *Service {
	return &Service{usage: usage, pro: pro}
}

// Current is the user's entitlements. Usage is the cumulative library totals,
// the same numbers library-stats reports. If they cannot be read the plan
// cannot be shown honestly, so the restricted body comes back together with
// the error: the caller logs the error and still answers with the body, and
// the client degrades and retries.
func (s *Service) Current(ctx context.Context, userID string) (Entitlements, error) {
	stats, err := s.usage.Stats(ctx, userID)
	if err != nil {
		return Restricted(userID, "Plan access could not be verified right now."), err
	}
	usage := Usage{
		SavedReels:      stats.TotalReels,
		PinnedLocations: stats.TotalPinnedLocations,
		Tags:            stats.TotalTags,
		Categories:      stats.TotalCategories,
		Subcategories:   stats.TotalSubcategories,
	}
	return Build(userID, usage, s.pro.IsPro(ctx, userID)), nil
}
