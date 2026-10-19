package entitlements

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestBuildFreePlan(t *testing.T) {
	usage := Usage{SavedReels: 7, PinnedLocations: 3, Tags: 5, Categories: 2, Subcategories: 4}
	ent := Build("user-1", usage, false)

	if ent.Plan != "free" || ent.IsPremium {
		t.Fatalf("plan = %q is_premium = %v, want free/false", ent.Plan, ent.IsPremium)
	}
	if ent.CurrentEntitlement.Plan != "free" || ent.CurrentEntitlement.IsPremium {
		t.Errorf("current_entitlement = %+v", ent.CurrentEntitlement)
	}
	if ent.Usage != usage {
		t.Errorf("usage = %+v, want %+v", ent.Usage, usage)
	}
	if ent.Limits.SavedReels == nil || *ent.Limits.SavedReels != 25 {
		t.Errorf("free saved_reels limit = %v, want 25", ent.Limits.SavedReels)
	}
	if ent.Features.UnlimitedSaves || ent.Features.UnlimitedMapPins {
		t.Errorf("free plan has unlimited features: %+v", ent.Features)
	}
	if !ent.Features.SaveReels || !ent.Features.Search || !ent.Features.Map || !ent.Features.Discover {
		t.Errorf("free plan missing base features: %+v", ent.Features)
	}
	if len(ent.PlanCards) != 2 {
		t.Errorf("free plan cards = %d, want 2", len(ent.PlanCards))
	}
	if ent.Pricing["pro_monthly"] == "" {
		t.Errorf("free plan has no pricing")
	}
	if len(ent.PaywallMessages) != 3 {
		t.Errorf("paywall messages = %d, want 3", len(ent.PaywallMessages))
	}

	wantPricing := map[string]string{"free_monthly": "$0", "pro_monthly": "$9.99", "pro_yearly": "$79.99"}
	if !reflect.DeepEqual(ent.Pricing, wantPricing) {
		t.Errorf("pricing = %+v, want %+v", ent.Pricing, wantPricing)
	}
	wantMessages := map[string]string{
		"account":    "Upgrade to Pro for unlimited saves, search, and map pins.",
		"save_limit": "You have reached the Free save limit.",
		"history":    "Upgrade to Pro to keep your full reel history.",
	}
	if !reflect.DeepEqual(ent.PaywallMessages, wantMessages) {
		t.Errorf("paywall messages = %+v, want %+v", ent.PaywallMessages, wantMessages)
	}
	wantFree := []string{"Save up to 25 reels", "Search saved reels", "View up to 10 pinned locations"}
	wantPro := []string{"Unlimited saved reels", "Unlimited search", "Unlimited pinned locations", "Full history and map access"}
	if !reflect.DeepEqual(ent.PlanCards[0].Features, wantFree) {
		t.Errorf("free features = %q, want %q", ent.PlanCards[0].Features, wantFree)
	}
	if !reflect.DeepEqual(ent.PlanCards[1].Features, wantPro) {
		t.Errorf("pro features = %q, want %q", ent.PlanCards[1].Features, wantPro)
	}
}

func TestBuildProPlan(t *testing.T) {
	ent := Build("user-1", Usage{SavedReels: 100}, true)

	if ent.Plan != "pro" || !ent.IsPremium {
		t.Fatalf("plan = %q is_premium = %v, want pro/true", ent.Plan, ent.IsPremium)
	}
	if ent.Limits.SavedReels != nil || ent.Limits.PinnedLocations != nil || ent.Limits.SearchesPerMonth != nil {
		t.Errorf("pro limits are not unlimited: %+v", ent.Limits)
	}
	if !ent.Features.UnlimitedSaves || !ent.Features.UnlimitedMapPins {
		t.Errorf("pro plan lacks unlimited features: %+v", ent.Features)
	}
	if len(ent.PlanCards) != 0 {
		t.Errorf("pro plan shows %d upgrade cards, want 0", len(ent.PlanCards))
	}
	if len(ent.Pricing) != 0 {
		t.Errorf("pro plan shows pricing: %+v", ent.Pricing)
	}
}

func TestRestricted(t *testing.T) {
	ent := Restricted("user-1", "Plan access could not be verified right now.")

	if ent.Plan != "restricted" || !ent.CurrentEntitlement.Restricted {
		t.Fatalf("restricted plan = %q restricted = %v", ent.Plan, ent.CurrentEntitlement.Restricted)
	}
	if ent.CurrentEntitlement.ErrorMessage == nil || *ent.CurrentEntitlement.ErrorMessage == "" {
		t.Errorf("restricted response has no error message")
	}
	if ent.Features != (Features{}) {
		t.Errorf("restricted response still has features on: %+v", ent.Features)
	}
	if len(ent.PlanCards) != 0 {
		t.Errorf("restricted response shows upgrade cards")
	}
	for _, message := range ent.PaywallMessages {
		if message != *ent.CurrentEntitlement.ErrorMessage {
			t.Errorf("paywall message = %q, want the error message", message)
		}
	}
}

type fakeReader struct {
	active map[string]time.Time
	err    error
	calls  int
}

func (f *fakeReader) ActivePro(context.Context) (map[string]time.Time, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.active, nil
}

func TestCacheReadsProFromTheActiveSet(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	reader := &fakeReader{active: map[string]time.Time{"pro-user": now.Add(24 * time.Hour)}}
	cache := NewCache(reader, time.Minute, func() time.Time { return now }, nil)

	if !cache.IsPro(context.Background(), "pro-user") {
		t.Error("a user with a future grant is not Pro")
	}
	if cache.IsPro(context.Background(), "free-user") {
		t.Error("a user absent from the set reads as Pro")
	}
}

func TestCacheTreatsALapsedGrantAsFree(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	reader := &fakeReader{active: map[string]time.Time{"pro-user": now.Add(-time.Second)}}
	cache := NewCache(reader, time.Minute, func() time.Time { return now }, nil)

	if cache.IsPro(context.Background(), "pro-user") {
		t.Error("a grant that expired is still read as Pro")
	}
}

func TestCacheReloadsOnlyAfterTTL(t *testing.T) {
	current := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	reader := &fakeReader{active: map[string]time.Time{"pro-user": current.Add(time.Hour)}}
	cache := NewCache(reader, time.Minute, func() time.Time { return current }, nil)

	cache.IsPro(context.Background(), "pro-user")
	cache.IsPro(context.Background(), "pro-user")
	if reader.calls != 1 {
		t.Fatalf("reads within the ttl reloaded %d times, want 1", reader.calls)
	}

	current = current.Add(2 * time.Minute)
	cache.IsPro(context.Background(), "pro-user")
	if reader.calls != 2 {
		t.Fatalf("a read past the ttl did not reload: calls = %d", reader.calls)
	}
}

func TestCacheKeepsLastGoodSetOnReloadFailure(t *testing.T) {
	current := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	reader := &fakeReader{active: map[string]time.Time{"pro-user": current.Add(time.Hour)}}
	cache := NewCache(reader, time.Minute, func() time.Time { return current }, nil)

	if !cache.IsPro(context.Background(), "pro-user") {
		t.Fatal("first load did not see the grant")
	}

	// The next window fails; the paying user must not be dropped.
	reader.err = errors.New("database down")
	current = current.Add(2 * time.Minute)
	if !cache.IsPro(context.Background(), "pro-user") {
		t.Error("a reload failure dropped a paying user instead of keeping the last good set")
	}
}

type gatedReader struct {
	active  map[string]time.Time
	calls   atomic.Int32
	release chan struct{}
}

func (g *gatedReader) ActivePro(context.Context) (map[string]time.Time, error) {
	g.calls.Add(1)
	<-g.release
	return g.active, nil
}

func TestCacheReloadsOncePerExpiryUnderConcurrency(t *testing.T) {
	current := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	reader := &gatedReader{
		active:  map[string]time.Time{"pro-user": current.Add(time.Hour)},
		release: make(chan struct{}),
	}
	cache := NewCache(reader, time.Minute, func() time.Time { return current }, nil)

	// Prime the cache, then let the window lapse.
	close(reader.release)
	cache.IsPro(context.Background(), "pro-user")
	reader.release = make(chan struct{})
	reader.calls.Store(0)
	current = current.Add(2 * time.Minute)

	const callers = 50
	var wg sync.WaitGroup
	returned := make(chan struct{}, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cache.IsPro(context.Background(), "pro-user")
			returned <- struct{}{}
		}()
	}

	// Every caller but the one holding the reload must return without waiting
	// on the database.
	for i := 0; i < callers-1; i++ {
		select {
		case <-returned:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d callers returned while one reload was in flight; the rest are querying too", i)
		}
	}
	close(reader.release)
	wg.Wait()

	if got := reader.calls.Load(); got != 1 {
		t.Fatalf("reloads at one expiry = %d, want 1", got)
	}
}
