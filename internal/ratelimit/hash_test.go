package ratelimit

import (
	"strings"
	"testing"
)

func TestHashHidesTheSubjectAndStaysStable(t *testing.T) {
	hasher := NewHasher("salt-one")
	user := "11111111-1111-4111-8111-111111111111"

	hashed := hasher.Hash(user)
	if strings.Contains(hashed, user) || len(hashed) != 16 {
		t.Fatalf("hash = %q", hashed)
	}
	if hasher.Hash(user) != hashed {
		t.Error("the same subject hashed differently twice")
	}
	if hasher.Hash("203.0.113.7") == hashed {
		t.Error("two subjects collided")
	}
}

func TestRotatingTheSaltResetsEveryWindow(t *testing.T) {
	user := "11111111-1111-4111-8111-111111111111"
	if NewHasher("salt-one").Hash(user) == NewHasher("salt-two").Hash(user) {
		t.Fatal("a rotated salt produced the same key, so rotation would not reset windows")
	}
}

func TestAnEmptyPolicyAllowsEverything(t *testing.T) {
	limiter := New(nil, "reelpin:test:none", NewHasher("s"))
	decision, err := limiter.Allow(t.Context(), Policy{}, "anyone")
	if err != nil || !decision.Allowed {
		t.Fatalf("decision = %+v, err = %v; an unset policy must be a no-op", decision, err)
	}
}

func TestPlanPolicies(t *testing.T) {
	// Changing one is a product decision, not a refactor, which is why they are
	// pinned here rather than merely defined once.
	if Submission.Requests != 30 || Submission.Window.Hours() != 1 {
		t.Errorf("submission = %+v, want 30 per hour", Submission)
	}
	// Twice the per-user number. Any lower and this becomes the binding limit
	// for one person, and the per-user figure stops meaning anything.
	if SubmissionIP.Requests != 60 || SubmissionIP.Window.Hours() != 1 {
		t.Errorf("submission_ip = %+v, want 60 per hour", SubmissionIP)
	}
	if Search.Requests != 30 || Search.Window.Minutes() != 1 {
		t.Errorf("search = %+v, want 30 per minute", Search)
	}
	if SearchIP.Requests != 90 || SearchIP.Window.Minutes() != 1 {
		t.Errorf("search_ip = %+v, want 90 per minute", SearchIP)
	}
}
