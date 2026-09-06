package taxonomy

import (
	"strings"
	"testing"
)

func TestNormalizeAgreesWithItself(t *testing.T) {
	// Every one of these is the same category to a person, and the database's
	// uniqueness constraint depends on them being the same string here.
	same := []string{"Street Food", "street food", "  Street   Food  ", "Street-Food", "street_food!"}
	want := Normalize(same[0])
	for _, name := range same[1:] {
		if got := Normalize(name); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", name, got, want)
		}
	}
	if Normalize("street food") == Normalize("street foods") {
		t.Error("a plural collapsed into its singular; that is a merge decision, not a normalization")
	}
	if Normalize("   ") != "" {
		t.Error("blank did not normalize to empty")
	}
}

// policy runs applyPolicy with the given proposals and existing names.
func policy(t *testing.T, actions []Action, proposals []Proposal, existing map[string]string) []Action {
	t.Helper()
	if existing == nil {
		existing = map[string]string{}
	}
	return (&Curator{}).applyPolicy(actions, proposals, existing)
}

func proposal(name string, count int) Proposal {
	return Proposal{NormalizedName: name, ContentCount: count, IDs: []string{name + "-id"}}
}

func TestAdditionNeedsEveryThreshold(t *testing.T) {
	tests := []struct {
		name        string
		count       int
		confidence  float64
		wantApplied bool
		wantSkipped string
	}{
		{name: "enough evidence and confidence", count: 3, confidence: 0.95, wantApplied: true},
		{name: "exactly at both thresholds", count: MinContentCount, confidence: MinConfidence, wantApplied: true},
		{name: "too few proposals", count: 2, confidence: 0.99, wantSkipped: "distinct proposals"},
		{name: "not confident enough", count: 5, confidence: 0.89, wantSkipped: "confidence"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			decided := policy(t,
				[]Action{{NormalizedName: "street food", Verdict: VerdictAdd, Confidence: tt.confidence}},
				[]Proposal{proposal("street food", tt.count)}, nil)

			if decided[0].Applied != tt.wantApplied {
				t.Fatalf("applied = %v, want %v (skipped: %q)",
					decided[0].Applied, tt.wantApplied, decided[0].Skipped)
			}
			if tt.wantSkipped != "" && !strings.Contains(decided[0].Skipped, tt.wantSkipped) {
				t.Errorf("skipped = %q, want it to mention %q", decided[0].Skipped, tt.wantSkipped)
			}
		})
	}
}

func TestARunAddsAtMostFive(t *testing.T) {
	actions := []Action{}
	proposals := []Proposal{}
	for _, name := range []string{"one", "two", "three", "four", "five", "six", "seven"} {
		actions = append(actions, Action{NormalizedName: name, Verdict: VerdictAdd, Confidence: 1})
		proposals = append(proposals, proposal(name, 10))
	}

	decided := policy(t, actions, proposals, nil)
	applied := 0
	for _, action := range decided {
		if action.Applied {
			applied++
		}
	}
	if applied != MaxAdditionsPerRun {
		t.Fatalf("applied %d additions, want the cap of %d", applied, MaxAdditionsPerRun)
	}
	if !strings.Contains(decided[len(decided)-1].Skipped, "already added") {
		t.Errorf("the sixth was skipped for %q, want the cap", decided[len(decided)-1].Skipped)
	}
}

func TestADuplicateIsNeverAddedHoweverConfident(t *testing.T) {
	// The model may be certain and still wrong: the name is already taken, and
	// the database would reject it anyway.
	existing := map[string]string{"food": "food-id"}

	decided := policy(t,
		[]Action{{NormalizedName: "Food", Verdict: VerdictAdd, Confidence: 1.0}},
		[]Proposal{proposal("food", 100)}, existing)

	if decided[0].Applied {
		t.Fatal("a duplicate name was added")
	}
	if !strings.Contains(decided[0].Skipped, "already an active category") {
		t.Errorf("skipped = %q", decided[0].Skipped)
	}
}

func TestAnAliasNeedsARealTarget(t *testing.T) {
	existing := map[string]string{"food": "food-id"}
	proposals := []Proposal{proposal("street eats", 1), proposal("nonsense", 1)}

	decided := policy(t, []Action{
		// An alias is allowed below the addition thresholds: it adds nothing
		// to the tree and costs nothing to undo.
		{NormalizedName: "street eats", Verdict: VerdictAlias, AliasOf: "Food", Confidence: 0.4},
		{NormalizedName: "nonsense", Verdict: VerdictAlias, AliasOf: "Not A Category", Confidence: 0.99},
	}, proposals, existing)

	if !decided[0].Applied || decided[0].CategoryID != "food-id" {
		t.Fatalf("alias to a real category was not applied: %+v", decided[0])
	}
	if decided[1].Applied {
		t.Fatal("an alias to a category that does not exist was applied")
	}
}

func TestAnAnswerAboutSomethingNobodyProposedIsIgnored(t *testing.T) {
	decided := policy(t,
		[]Action{{NormalizedName: "invented", Verdict: VerdictAdd, Confidence: 1}},
		[]Proposal{proposal("street food", 5)}, nil)

	if decided[0].Applied {
		t.Fatal("the model added a category nobody asked for")
	}
	if !strings.Contains(decided[0].Skipped, "not a pending proposal") {
		t.Errorf("skipped = %q", decided[0].Skipped)
	}
}

func TestAnUnknownVerdictChangesNothing(t *testing.T) {
	decided := policy(t,
		[]Action{{NormalizedName: "street food", Verdict: "delete_everything", Confidence: 1}},
		[]Proposal{proposal("street food", 9)}, nil)

	if decided[0].Applied {
		t.Fatal("an unrecognised verdict was applied")
	}
}
