package pipeline

import (
	"testing"

	"github.com/XploY04/reelpin-go/internal/apify"
)

// Prepare calls an Apify actor, and the stage context is the shorter of the
// two deadlines. A prepare budget under the actor timeout expires first, so
// every Apify-backed platform fails with a timeout it can never win.
func TestPrepareOutlastsApifyTimeout(t *testing.T) {
	if got := stageTimeouts[stagePrepare]; got <= apify.DefaultTimeout {
		t.Fatalf("prepare budget %s does not outlast the apify timeout %s", got, apify.DefaultTimeout)
	}
}
