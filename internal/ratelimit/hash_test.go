package ratelimit

import (
	"strings"
	"testing"
	"time"
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
	if MapPin.Requests != 20 || MapPin.Window.Hours() != 1 {
		t.Errorf("map_pin = %+v, want 20 per hour", MapPin)
	}
	if MapPinIP.Requests != 60 || MapPinIP.Window.Hours() != 1 {
		t.Errorf("map_pin_ip = %+v, want 60 per hour", MapPinIP)
	}
}

func TestAPerClientPolicyAddsAClientBucketToTheUsers(t *testing.T) {
	app := Search.UserSubjects("user-1", "")
	agent := Search.UserSubjects("user-1", "mcp-client")
	extension := Search.UserSubjects("user-1", "extension")

	if len(app) != 1 || app[0] != "user-1" {
		t.Errorf("an app session keys %q, want the plain user bucket", app)
	}
	for _, subjects := range [][]string{agent, extension} {
		if len(subjects) != 2 || subjects[1] != "user-1" {
			t.Errorf("a client keys %q, want its own bucket and then the user's total", subjects)
		}
	}
	if agent[0] == "user-1" || agent[0] == extension[0] {
		t.Errorf("clients share a bucket: agent=%q extension=%q", agent, extension)
	}
	if other := Search.UserSubjects("user-2", "mcp-client"); other[0] == agent[0] {
		t.Error("one client shares a bucket across users")
	}

	perUser := Policy{Name: "per_user", Requests: 1, Window: time.Minute}
	if got := perUser.UserSubjects("user-1", "mcp-client"); len(got) != 1 || got[0] != "user-1" {
		t.Errorf("a per-user policy keyed %q, want the user alone", got)
	}
	for _, policy := range []Policy{Submission, Search, MapPin} {
		if !policy.PerClient {
			t.Errorf("%s is not per client", policy.Name)
		}
	}
}
