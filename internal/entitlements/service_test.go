package entitlements

import (
	"context"
	"errors"
	"testing"

	"github.com/XploY04/reelpin-go/internal/reels"
)

type stubUsage struct {
	stats reels.LibraryStats
	err   error
}

func (s stubUsage) Stats(context.Context, string) (reels.LibraryStats, error) { return s.stats, s.err }

type stubPro bool

func (s stubPro) IsPro(context.Context, string) bool { return bool(s) }

func TestCurrentMeasuresUsageAgainstThePlan(t *testing.T) {
	service := NewService(stubUsage{stats: reels.LibraryStats{TotalReels: 7, TotalPinnedLocations: 3}}, stubPro(true))

	ent, err := service.Current(context.Background(), "user-1")
	if err != nil {
		t.Fatal(err)
	}
	if !ent.IsPremium || ent.Usage.SavedReels != 7 || ent.Usage.PinnedLocations != 3 {
		t.Errorf("entitlements = %+v", ent)
	}
}

// Without usage the plan cannot be shown honestly, so the restricted body
// comes back with the error for the caller to serve anyway.
func TestCurrentWithoutUsageIsRestricted(t *testing.T) {
	failure := errors.New("database asleep")
	service := NewService(stubUsage{err: failure}, stubPro(true))

	ent, err := service.Current(context.Background(), "user-1")
	if !errors.Is(err, failure) {
		t.Fatalf("error = %v, want the usage failure", err)
	}
	if ent.CurrentEntitlement.Plan != planRestricted {
		t.Errorf("plan = %q, want restricted", ent.CurrentEntitlement.Plan)
	}
}
