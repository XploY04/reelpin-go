// Package entitlements answers what a user's plan allows. Pro is the only paid
// tier, and it is granted by hand today, so the plan is a single boolean over a
// grant table; the limits, pricing, and paywall copy are product constants that
// live here rather than in storage.
package entitlements

// Usage is the cumulative total of what a user has saved. It is not a monthly
// window: the numbers are lifetime counts the client shows next to the limits.
type Usage struct {
	SavedReels      int `json:"saved_reels"`
	PinnedLocations int `json:"pinned_locations"`
	Tags            int `json:"tags"`
	Categories      int `json:"categories"`
	Subcategories   int `json:"subcategories"`
}

// Limits is the ceiling for a plan. A nil field is "unlimited", which is how Pro
// reads on every count.
type Limits struct {
	SavedReels            *int `json:"saved_reels"`
	PinnedLocations       *int `json:"pinned_locations"`
	SearchesPerMonth      *int `json:"searches_per_month"`
	ProcessingJobsPerHour *int `json:"processing_jobs_per_hour"`
}

// Features are the capability flags the client gates its UI on.
type Features struct {
	SaveReels        bool `json:"save_reels"`
	Search           bool `json:"search"`
	Map              bool `json:"map"`
	Discover         bool `json:"discover"`
	UnlimitedSaves   bool `json:"unlimited_saves"`
	UnlimitedMapPins bool `json:"unlimited_map_pins"`
}

// CurrentEntitlement is the user's standing. billing_interval and
// subscription_status are fixed today; they exist so the shape does not change
// when real billing arrives.
type CurrentEntitlement struct {
	Plan               string  `json:"plan"`
	BillingInterval    string  `json:"billing_interval"`
	SubscriptionStatus string  `json:"subscription_status"`
	IsPremium          bool    `json:"is_premium"`
	Restricted         bool    `json:"restricted"`
	ErrorMessage       *string `json:"error_message"`
}

// PlanCard is one row of the upgrade comparison the paywall renders.
type PlanCard struct {
	Plan            string   `json:"plan"`
	Name            string   `json:"name"`
	BillingInterval string   `json:"billing_interval"`
	Price           string   `json:"price"`
	Features        []string `json:"features"`
	Limits          Limits   `json:"limits"`
	CTALabel        string   `json:"cta_label"`
}

// Entitlements is the whole response. plan and is_premium are duplicated at the
// top level because the client reads them in both places.
type Entitlements struct {
	UserID             string             `json:"user_id"`
	CurrentEntitlement CurrentEntitlement `json:"current_entitlement"`
	Usage              Usage              `json:"usage"`
	Limits             Limits             `json:"limits"`
	Features           Features           `json:"features"`
	Pricing            map[string]string  `json:"pricing"`
	PlanCards          []PlanCard         `json:"plan_cards"`
	PaywallMessages    map[string]string  `json:"paywall_messages"`
	Plan               string             `json:"plan"`
	IsPremium          bool               `json:"is_premium"`
}

const (
	planFree       = "free"
	planPro        = "pro"
	planRestricted = "restricted"
)

// freeLimits and proLimits are the ceilings each plan reads. ProcessingJobsPerHour
// is unset on both: it is declared so the shape is stable, not yet enforced.
var (
	freeLimits = Limits{
		SavedReels:       intPtr(25),
		PinnedLocations:  intPtr(10),
		SearchesPerMonth: intPtr(50),
	}
	proLimits = Limits{}
)

var (
	planFeaturesFree = []string{
		"Save up to 25 reels",
		"Search saved reels",
		"View up to 10 pinned locations",
	}
	planFeaturesPro = []string{
		"Unlimited saved reels",
		"Unlimited search",
		"Unlimited pinned locations",
		"Full history and map access",
	}
)

func pricing() map[string]string {
	return map[string]string{
		"free_monthly": "$0",
		"pro_monthly":  "$9.99",
		"pro_yearly":   "$79.99",
	}
}

func paywallMessages() map[string]string {
	return map[string]string{
		"account":    "Upgrade to Pro for unlimited saves, search, and map pins.",
		"save_limit": "You have reached the Free save limit.",
		"history":    "Upgrade to Pro to keep your full reel history.",
	}
}

// Build assembles the entitlements for a user whose plan is known. isPremium
// drives the limits, the features, and whether the upgrade cards are shown.
func Build(userID string, usage Usage, isPremium bool) Entitlements {
	plan := planFree
	limits := freeLimits
	if isPremium {
		plan = planPro
		limits = proLimits
	}

	ent := Entitlements{
		UserID: userID,
		CurrentEntitlement: CurrentEntitlement{
			Plan:               plan,
			BillingInterval:    "monthly",
			SubscriptionStatus: "active",
			IsPremium:          isPremium,
		},
		Usage:  usage,
		Limits: limits,
		Features: Features{
			SaveReels:        true,
			Search:           true,
			Map:              true,
			Discover:         true,
			UnlimitedSaves:   isPremium,
			UnlimitedMapPins: isPremium,
		},
		Pricing:         map[string]string{},
		PlanCards:       []PlanCard{},
		PaywallMessages: paywallMessages(),
		Plan:            plan,
		IsPremium:       isPremium,
	}

	// A Pro user is not shown prices or upgrade cards; there is nothing to sell.
	if !isPremium {
		ent.Pricing = pricing()
		ent.PlanCards = planCards()
	}
	return ent
}

// Restricted is the answer when the plan cannot be verified. It never raises:
// the client shows a degraded state and retries, rather than a 500. The plan is
// "restricted", every feature is off, and all three paywall messages carry the
// reason.
func Restricted(userID, message string) Entitlements {
	ent := Build(userID, Usage{}, false)
	ent.CurrentEntitlement.Plan = planRestricted
	ent.CurrentEntitlement.SubscriptionStatus = "unavailable"
	ent.CurrentEntitlement.Restricted = true
	ent.CurrentEntitlement.ErrorMessage = &message
	ent.Plan = planRestricted
	ent.Features = Features{}
	ent.PlanCards = []PlanCard{}
	ent.PaywallMessages = map[string]string{
		"account":    message,
		"save_limit": message,
		"history":    message,
	}
	return ent
}

func planCards() []PlanCard {
	return []PlanCard{
		{
			Plan:            planFree,
			Name:            "Free",
			BillingInterval: "monthly",
			Price:           "$0",
			Features:        planFeaturesFree,
			Limits:          freeLimits,
			CTALabel:        "Current plan",
		},
		{
			Plan:            planPro,
			Name:            "Pro",
			BillingInterval: "monthly",
			Price:           "$9.99/mo",
			Features:        planFeaturesPro,
			Limits:          proLimits,
			CTALabel:        "Upgrade",
		},
	}
}

func intPtr(value int) *int { return &value }
