package httpapi

import (
	"context"
	"net/http"

	"github.com/XploY04/reelpin-go/internal/cache"
	"github.com/XploY04/reelpin-go/internal/reels"
)

// Free plan limits, unchanged from the Python API.
var freeLimits = Limits{
	SavedReels:       intPointer(25),
	PinnedLocations:  intPointer(10),
	SearchesPerMonth: intPointer(50),
}

var proLimits = Limits{}

var planFeatures = map[string][]string{
	"free": {
		"Save up to 25 reels",
		"Search saved reels",
		"View up to 10 pinned locations",
	},
	"pro": {
		"Unlimited saved reels",
		"Unlimited search",
		"Unlimited pinned locations",
		"Full history and map access",
	},
}

var pricing = map[string]string{
	"free_monthly": "$0",
	"pro_monthly":  "$9.99",
	"pro_yearly":   "$79.99",
}

type Limits struct {
	SavedReels          *int `json:"saved_reels"`
	PinnedLocations     *int `json:"pinned_locations"`
	SearchesPerMonth    *int `json:"searches_per_month"`
	ProcessingJobsPerHr *int `json:"processing_jobs_per_hour"`
}

type Usage struct {
	SavedReels      int `json:"saved_reels"`
	PinnedLocations int `json:"pinned_locations"`
	Tags            int `json:"tags"`
	Categories      int `json:"categories"`
	Subcategories   int `json:"subcategories"`
}

type CurrentEntitlement struct {
	Plan               string  `json:"plan"`
	BillingInterval    string  `json:"billing_interval"`
	SubscriptionStatus string  `json:"subscription_status"`
	IsPremium          bool    `json:"is_premium"`
	Restricted         bool    `json:"restricted"`
	ErrorMessage       *string `json:"error_message"`
}

type PlanCard struct {
	Plan            string   `json:"plan"`
	Name            string   `json:"name"`
	BillingInterval string   `json:"billing_interval"`
	Price           string   `json:"price"`
	Features        []string `json:"features"`
	Limits          Limits   `json:"limits"`
	CTALabel        string   `json:"cta_label"`
}

type EntitlementsResponse struct {
	UserID             string             `json:"user_id"`
	CurrentEntitlement CurrentEntitlement `json:"current_entitlement"`
	Usage              Usage              `json:"usage"`
	Limits             Limits             `json:"limits"`
	Features           map[string]bool    `json:"features"`
	Pricing            map[string]string  `json:"pricing"`
	PlanCards          []PlanCard         `json:"plan_cards"`
	PaywallMessages    map[string]string  `json:"paywall_messages"`
	Plan               string             `json:"plan"`
	IsPremium          bool               `json:"is_premium"`
}

func (s *Server) handleLibraryStats(w http.ResponseWriter, r *http.Request) {
	stats, err := s.deps.Reels.Stats(r.Context(), requestUserID(r))
	if err != nil {
		s.deps.Logger.Error("library stats failed", "error", err)
		internalError(w, "library_stats_failed", "Could not load library stats right now.")
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

func (s *Server) handleEntitlements(w http.ResponseWriter, r *http.Request) {
	userID := requestUserID(r)

	stats, err := cache.GetOrLoad(r.Context(), s.deps.Cache, userID, "library_stats", "",
		filterCacheTTL,
		func(ctx context.Context) (reels.LibraryStats, error) {
			return s.deps.Reels.Stats(ctx, userID)
		})
	if err != nil {
		// The app must still render a plan screen, so a lookup failure answers
		// 200 with everything switched off, exactly as the Python API does.
		s.deps.Logger.Error("account entitlement lookup failed", "error", err)
		writeJSON(w, http.StatusOK, restrictedEntitlements(userID, "Plan access could not be verified right now."))
		return
	}
	writeJSON(w, http.StatusOK, buildEntitlements(userID, stats))
}

func buildEntitlements(userID string, stats reels.LibraryStats) EntitlementsResponse {
	const (
		plan            = "free"
		billingInterval = "monthly"
		isPremium       = false
	)

	return EntitlementsResponse{
		UserID: userID,
		CurrentEntitlement: CurrentEntitlement{
			Plan:               plan,
			BillingInterval:    billingInterval,
			SubscriptionStatus: "active",
			IsPremium:          isPremium,
		},
		Usage: Usage{
			SavedReels:      stats.TotalReels,
			PinnedLocations: stats.TotalPinnedLocations,
			Tags:            stats.TotalTags,
			Categories:      stats.TotalCategories,
			Subcategories:   stats.TotalSubcategories,
		},
		Limits: freeLimits,
		Features: map[string]bool{
			"save_reels":         true,
			"search":             true,
			"map":                true,
			"discover":           true,
			"unlimited_saves":    isPremium,
			"unlimited_map_pins": isPremium,
		},
		Pricing: pricing,
		PlanCards: []PlanCard{
			{
				Plan:            "free",
				Name:            "Free",
				BillingInterval: "monthly",
				Price:           "$0",
				Features:        planFeatures["free"],
				Limits:          freeLimits,
				CTALabel:        "Current plan",
			},
			{
				Plan:            "pro",
				Name:            "Pro",
				BillingInterval: billingInterval,
				Price:           "$9.99/mo",
				Features:        planFeatures["pro"],
				Limits:          proLimits,
				CTALabel:        "Upgrade",
			},
		},
		PaywallMessages: map[string]string{
			"account":    "Upgrade to Pro for unlimited saves, search, and map pins.",
			"save_limit": "You have reached the Free save limit.",
			"history":    "Upgrade to Pro to keep your full reel history.",
		},
		Plan:      plan,
		IsPremium: isPremium,
	}
}

func restrictedEntitlements(userID, message string) EntitlementsResponse {
	return EntitlementsResponse{
		UserID: userID,
		CurrentEntitlement: CurrentEntitlement{
			Plan:               "restricted",
			BillingInterval:    "monthly",
			SubscriptionStatus: "unavailable",
			Restricted:         true,
			ErrorMessage:       &message,
		},
		Limits: freeLimits,
		Features: map[string]bool{
			"save_reels":         false,
			"search":             false,
			"map":                false,
			"discover":           false,
			"unlimited_saves":    false,
			"unlimited_map_pins": false,
		},
		Pricing:   pricing,
		PlanCards: []PlanCard{},
		PaywallMessages: map[string]string{
			"account":    message,
			"save_limit": message,
			"history":    message,
		},
		Plan: "restricted",
	}
}

func intPointer(value int) *int { return &value }
