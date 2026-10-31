package pipeline

import (
	"testing"
	"time"

	"github.com/XploY04/reelpin-go/internal/ai"
	"github.com/XploY04/reelpin-go/internal/apify"
	"github.com/XploY04/reelpin-go/internal/googleplaces"
)

// Prepare calls an Apify actor, and the stage context is the shorter of the
// two deadlines. A prepare budget under the actor timeout expires first, so
// every Apify-backed platform fails with a timeout it can never win.
func TestPrepareOutlastsApifyTimeout(t *testing.T) {
	if got := stageTimeouts[stagePrepare]; got <= apify.DefaultTimeout {
		t.Fatalf("prepare budget %s does not outlast the apify timeout %s", got, apify.DefaultTimeout)
	}
}

func TestCategorizeAllowsPlaceLookupsAndModelCall(t *testing.T) {
	minimum := time.Duration(ai.MaxLocations)*googleplaces.DefaultTimeout + 45*time.Second
	if stageTimeouts[stageCategorize] < minimum {
		t.Fatalf("categorize budget %s cannot fit %d place lookups and the model call (%s)",
			stageTimeouts[stageCategorize], ai.MaxLocations, minimum)
	}
	var total time.Duration
	for _, budget := range stageTimeouts {
		total += budget
	}
	if total >= runLifetime {
		t.Fatalf("stage budgets %s exceed the run lifetime %s", total, runLifetime)
	}
}

// The job estimate is only as good as the budgets it reads, so every budgeted
// stage has to reach it, once, with its own number.
func TestStageBudgetsCoverEveryStage(t *testing.T) {
	budgets := StageBudgets()
	if len(budgets) != len(stageTimeouts) {
		t.Fatalf("%d stage budgets exported, %d stages budgeted", len(budgets), len(stageTimeouts))
	}
	for _, budget := range budgets {
		if want, ok := stageTimeouts[budget.Name]; !ok || budget.Budget != want {
			t.Errorf("stage %q exports %s, budgeted %s", budget.Name, budget.Budget, want)
		}
	}
	if budgets[0].Name != stagePrepare || budgets[len(budgets)-1].Name != stagePersist {
		t.Errorf("stages are out of order: %v", budgets)
	}
}
