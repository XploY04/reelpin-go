package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"testing"

	"github.com/XploY04/reelpin-go/internal/enqueue"
	"github.com/XploY04/reelpin-go/internal/jobs"
	"github.com/XploY04/reelpin-go/internal/metrics"
	"github.com/XploY04/reelpin-go/internal/reels"
	"github.com/XploY04/reelpin-go/internal/sourceidentity"
	"github.com/XploY04/reelpin-go/internal/spend"
)

// This is the plan's gate for Task 31, exercised end to end through the real
// submission use case and the real cost gate: synthetic usage moves the month's
// spend, and the response, the enqueue and the read path are all asserted at
// the exact configured amounts.
//
// The amounts here belong to the test. Nothing in the service carries one.
const (
	gateWarn  = "12.00"
	gateStop  = "20.00"
	gateOrder = "instagram, media, light, all"
)

// countingStore is the transaction the submission would commit. Committing one
// is what causes provider calls later, so its counter is the "did this cost
// money" assertion the plan asks for.
type countingStore struct {
	committed int
}

func (s *countingStore) Submit(_ context.Context, submission enqueue.Submission) (enqueue.Result, error) {
	s.committed++
	step := "queued"
	created := testNow
	return enqueue.Result{Kind: enqueue.Accepted, Job: &enqueue.Job{
		ID:            "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
		Status:        "queued",
		URL:           submission.Identity.NormalizedURL,
		CurrentStep:   &step,
		CollectionIDs: []string{},
		CreatedAt:     &created,
		UpdatedAt:     &created,
	}}, nil
}

// ledgerStore is the provider-usage ledger, in memory. The gate reads the same
// total the ledger writes, so the synthetic usage in these tests moves the gate
// exactly as a real provider call would.
type ledgerStore struct {
	total spend.Micros
}

func (s *ledgerStore) Insert(_ context.Context, entry spend.Entry) error {
	s.total += entry.CostMicros
	return nil
}

func (s *ledgerStore) MonthToDateMicros(context.Context) (spend.Micros, error) {
	return s.total, nil
}

// gatedDeps wires the real enqueue service and the real gate over the ledger.
func gatedDeps(t *testing.T, ledger *ledgerStore, store *countingStore, meters *metrics.Metrics) Deps {
	t.Helper()
	limits, err := spend.NewLimits(gateWarn, gateStop, gateOrder)
	if err != nil {
		t.Fatalf("NewLimits: %v", err)
	}

	deps := testDeps(&fakePinger{})
	deps.Limiter = allowAllLimiter{}
	deps.Enqueue = enqueue.New(store, &sourceidentity.Resolver{},
		spend.NewGate(limits, ledger, meters))
	deps.Metrics = meters
	// One job already committed before the gate tripped, and one reel already
	// saved: both must stay readable.
	deps.Jobs = &fakeJobs{records: []jobs.JobRecord{committedJob()}}
	saved := sampleReel(testReelID, testUserID)
	deps.Reels = &fakeReels{
		records: []reels.ReelRecord{saved},
		byID:    map[string]reels.ReelRecord{testReelID: saved},
	}
	return deps
}

func committedJob() jobs.JobRecord {
	step := "transcribe"
	return jobs.JobRecord{
		ID:              testJobID,
		UserID:          testUserID,
		URL:             "https://www.instagram.com/reel/committed/",
		Status:          jobs.StatusProcessing,
		CurrentStep:     &step,
		ProgressPercent: 60,
		MaxAttempts:     3,
		CreatedAt:       timePtr(testNow),
	}
}

// spendTo drives the month's total to an exact figure through the real ledger,
// priced from a real price list, rather than by writing the number in.
func spendTo(t *testing.T, ledger *ledgerStore, target spend.Micros) {
	t.Helper()
	prices, err := spend.ParsePrices("gemini:test-model:input_mtok=1.00")
	if err != nil {
		t.Fatalf("ParsePrices: %v", err)
	}
	remaining := target - ledger.total
	if remaining < 0 {
		t.Fatalf("the month's spend cannot go backwards: at %d, asked for %d", ledger.total, target)
	}
	// At $1.00 per million tokens one micro of spend is one token.
	spend.NewLedger(ledger, prices, nil, slog.New(slog.NewJSONHandler(io.Discard, nil))).
		Record(context.Background(), spend.Usage{
			Provider: "gemini", Model: "test-model", Operation: "extract",
			Calls: 1, InputTokens: int64(remaining), Measured: true,
		})
	if ledger.total != target {
		t.Fatalf("month-to-date is %d micros, want exactly %d", ledger.total, target)
	}
}

func submit(deps Deps, url string) *http.Response {
	return post(deps, "/api/v2/processing-jobs/reels", `{"url":"`+url+`"}`,
		map[string]string{"Authorization": "Bearer good.token", "Idempotency-Key": validKey}).Result()
}

func TestGroupsAreRefusedAtTheExactConfiguredAmounts(t *testing.T) {
	const (
		instagramURL = "https://www.instagram.com/reel/C8abc123/"
		youtubeURL   = "https://www.youtube.com/shorts/abc123XYZ01"
		blogURL      = "https://someblog.example/a-post"
	)

	// The ladder four groups imply between $12 and $20: instagram at $14,
	// the rest of media at $16, light at $18, everything at $20.
	tests := []struct {
		name          string
		spent         spend.Micros
		wantInstagram int
		wantYouTube   int
		wantBlog      int
	}{
		{"at the warning amount, nothing stops", 12_000_000, 202, 202, 202},
		{"one micro below the first step", 13_999_999, 202, 202, 202},
		{"at the first step, instagram stops", 14_000_000, 503, 202, 202},
		{"at the media step", 16_000_000, 503, 503, 202},
		{"at the light step", 18_000_000, 503, 503, 503},
		{"one micro below the hard stop", 19_999_999, 503, 503, 503},
		{"at the hard stop, everything stops", 20_000_000, 503, 503, 503},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ledger := &ledgerStore{}
			store := &countingStore{}
			deps := gatedDeps(t, ledger, store, metrics.New())
			spendTo(t, ledger, tt.spent)

			for _, attempt := range []struct {
				url  string
				want int
			}{
				{instagramURL, tt.wantInstagram},
				{youtubeURL, tt.wantYouTube},
				{blogURL, tt.wantBlog},
			} {
				response := submit(deps, attempt.url)
				if response.StatusCode != attempt.want {
					t.Errorf("%s: status = %d, want %d", attempt.url, response.StatusCode, attempt.want)
				}
			}
		})
	}
}

func TestTheHardStopCommitsNothingAndAnswersTheStableCode(t *testing.T) {
	ledger := &ledgerStore{}
	store := &countingStore{}
	deps := gatedDeps(t, ledger, store, metrics.New())
	spendTo(t, ledger, 20_000_000)

	recorder := post(deps, "/api/v2/processing-jobs/reels",
		`{"url":"https://www.instagram.com/reel/C8abc123/"}`,
		map[string]string{"Authorization": "Bearer good.token", "Idempotency-Key": validKey})

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (%s)", recorder.Code, recorder.Body.String())
	}
	body := decodeError(t, recorder)
	if body.Error.Code != "spend_limit_reached" {
		t.Errorf("code = %q, want spend_limit_reached", body.Error.Code)
	}
	if body.Error.Retryable {
		t.Error("retryable = true: an operator decision is not something to retry into")
	}
	// No run was committed, so nothing downstream will call a provider. This is
	// the whole point of the hard stop.
	if store.committed != 0 {
		t.Errorf("%d submissions were committed past the hard stop", store.committed)
	}
}

func TestTheHardStopLeavesReadsAndCommittedWorkAlone(t *testing.T) {
	ledger := &ledgerStore{}
	deps := gatedDeps(t, ledger, &countingStore{}, metrics.New())
	spendTo(t, ledger, 20_000_000)

	// A hard stop that breaks reads is an outage, not a cost control.
	for _, path := range []string{
		"/api/v2/reels",
		"/api/v2/reels/" + testReelID,
		"/api/v2/processing-jobs",
		"/api/v2/processing-jobs/" + testJobID,
		"/api/v2/account/library-stats",
	} {
		recorder := serve(deps, "GET", path, "Bearer good.token")
		if recorder.Code != http.StatusOK {
			t.Errorf("GET %s = %d past the hard stop, want 200 (%s)",
				path, recorder.Code, recorder.Body.String())
		}
	}

	// Readiness is what a load balancer asks. Spending is not its business.
	recorder := serve(deps, "GET", "/api/v2/health/ready", "")
	if recorder.Code != http.StatusOK {
		t.Errorf("readiness = %d past the hard stop", recorder.Code)
	}

	// The job committed before the stop is still processing, still readable,
	// and still on its way to a result: the stop order applies to new work.
	recorder = serve(deps, "GET", "/api/v2/processing-jobs/"+testJobID, "Bearer good.token")
	var job map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &job); err != nil {
		t.Fatal(err)
	}
	if job["status"] != string(jobs.StatusProcessing) {
		t.Errorf("committed job status = %v, want it still processing", job["status"])
	}
}

func TestWithNoGateConfiguredNothingIsRefused(t *testing.T) {
	// Unset amounts mean no gate at all, not a guessed one.
	store := &countingStore{}
	deps := testDeps(&fakePinger{})
	deps.Limiter = allowAllLimiter{}
	deps.Enqueue = enqueue.New(store, &sourceidentity.Resolver{}, nil)

	if response := submit(deps, "https://www.instagram.com/reel/C8abc123/"); response.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 with no gate configured", response.StatusCode)
	}
	if store.committed != 1 {
		t.Errorf("committed %d submissions, want 1", store.committed)
	}
}
