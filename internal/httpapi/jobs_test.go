package httpapi

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/XploY04/reelpin-go/internal/jobs"
	"github.com/XploY04/reelpin-go/internal/reels"
)

func sampleJob(id, userID, status string) jobs.JobRecord {
	step := status
	return jobs.JobRecord{
		ID:              id,
		UserID:          userID,
		URL:             "https://www.instagram.com/reel/abc/",
		Status:          status,
		CurrentStep:     &step,
		ProgressPercent: 40,
		MaxAttempts:     3,
		CreatedAt:       timePtr(testNow),
	}
}

func TestListJobsParameters(t *testing.T) {
	tests := []struct {
		name           string
		query          string
		wantStatus     int
		wantActiveOnly bool
		wantLimit      int
		wantCode       string
	}{
		{name: "defaults", wantStatus: http.StatusOK, wantLimit: 20},
		{name: "active only", query: "?active_only=true", wantStatus: http.StatusOK, wantActiveOnly: true, wantLimit: 20},
		{name: "active only as 1", query: "?active_only=1", wantStatus: http.StatusOK, wantActiveOnly: true, wantLimit: 20},
		{name: "custom limit", query: "?limit=100", wantStatus: http.StatusOK, wantLimit: 100},
		{name: "limit too high", query: "?limit=101", wantStatus: http.StatusUnprocessableEntity, wantCode: "validation_error"},
		{name: "bad active_only", query: "?active_only=maybe", wantStatus: http.StatusUnprocessableEntity, wantCode: "validation_error"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader := &fakeJobs{records: []jobs.JobRecord{sampleJob(testJobID, testUserID, jobs.StatusQueued)}}
			deps := testDeps(&fakePinger{})
			deps.Jobs = reader

			rec := serve(deps, "GET", "/api/v2/processing-jobs"+tt.query, "Bearer good.token")
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if tt.wantCode != "" {
				if code := decodeError(t, rec).Error.Code; code != tt.wantCode {
					t.Errorf("error_code = %q, want %q", code, tt.wantCode)
				}
				return
			}
			if reader.lastUserID != testUserID {
				t.Errorf("queried user %q, want %q", reader.lastUserID, testUserID)
			}
			if reader.lastActiveOnly != tt.wantActiveOnly {
				t.Errorf("active_only = %v, want %v", reader.lastActiveOnly, tt.wantActiveOnly)
			}
			if reader.lastLimit != tt.wantLimit {
				t.Errorf("limit = %d, want %d", reader.lastLimit, tt.wantLimit)
			}
		})
	}
}

func TestListJobsIsAnArray(t *testing.T) {
	deps := testDeps(&fakePinger{})
	deps.Jobs = &fakeJobs{}

	rec := serve(deps, "GET", "/api/v2/processing-jobs", "Bearer good.token")
	if body := rec.Body.String(); body != "[]\n" {
		t.Fatalf("empty list body = %q, want []", body)
	}
}

func TestGetJob(t *testing.T) {
	reader := &fakeJobs{records: []jobs.JobRecord{sampleJob(testJobID, testUserID, jobs.StatusProcessing)}}
	deps := testDeps(&fakePinger{})
	deps.Jobs = reader

	tests := []struct {
		name       string
		path       string
		wantStatus int
	}{
		{name: "own job", path: "/api/v2/processing-jobs/" + testJobID, wantStatus: http.StatusOK},
		{name: "malformed id", path: "/api/v2/processing-jobs/nope", wantStatus: http.StatusNotFound},
		{name: "missing id", path: "/api/v2/processing-jobs/55555555-5555-4555-8555-555555555555", wantStatus: http.StatusNotFound},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := serve(deps, "GET", tt.path, "Bearer good.token")
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if tt.wantStatus == http.StatusNotFound {
				if code := decodeError(t, rec).Error.Code; code != "processing_job_not_found" {
					t.Errorf("error_code = %q, want processing_job_not_found", code)
				}
			}
		})
	}
}

func TestGetJobOwnedByAnotherUserIs404(t *testing.T) {
	deps := testDeps(&fakePinger{})
	deps.Jobs = &fakeJobs{records: []jobs.JobRecord{sampleJob(testJobID, otherUserID, jobs.StatusQueued)}}

	rec := serve(deps, "GET", "/api/v2/processing-jobs/"+testJobID, "Bearer good.token")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestCompletedJobCarriesItsReel(t *testing.T) {
	record := sampleJob(testJobID, testUserID, jobs.StatusCompleted)
	record.ResultReelID = stringPtr(testReelID)

	deps := testDeps(&fakePinger{})
	deps.Jobs = &fakeJobs{records: []jobs.JobRecord{record}}
	deps.Reels = &fakeReels{byID: map[string]reels.ReelRecord{
		testReelID: sampleReel(testReelID, testUserID),
	}}

	rec := serve(deps, "GET", "/api/v2/processing-jobs/"+testJobID, "Bearer good.token")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}

	var body jobs.Response
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not a job: %v", err)
	}
	if body.Reel == nil || body.Reel.ID != testReelID {
		t.Fatalf("reel = %v, want the result reel", body.Reel)
	}
	if body.Status != jobs.StatusCompleted || body.ProgressPercent != 100 {
		t.Errorf("status/progress = %s/%d", body.Status, body.ProgressPercent)
	}
	if body.RecommendedPollAfterSeconds != nil {
		t.Errorf("recommended_poll_after_seconds = %v, want null on a terminal job", *body.RecommendedPollAfterSeconds)
	}
}

func TestJobReelFromAnotherUserIsLeftOff(t *testing.T) {
	record := sampleJob(testJobID, testUserID, jobs.StatusCompleted)
	record.ResultReelID = stringPtr(testReelID)

	deps := testDeps(&fakePinger{})
	deps.Jobs = &fakeJobs{records: []jobs.JobRecord{record}}
	deps.Reels = &fakeReels{byID: map[string]reels.ReelRecord{
		testReelID: sampleReel(testReelID, otherUserID),
	}}

	rec := serve(deps, "GET", "/api/v2/processing-jobs/"+testJobID, "Bearer good.token")
	var body jobs.Response
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not a job: %v", err)
	}
	if body.Reel != nil {
		t.Fatal("a job must never carry another user's reel")
	}
}

func TestJobFailuresAreOpaque(t *testing.T) {
	deps := testDeps(&fakePinger{})
	deps.Jobs = &fakeJobs{err: errFake}

	for path, wantCode := range map[string]string{
		"/api/v2/processing-jobs":              "processing_job_list_failed",
		"/api/v2/processing-jobs/" + testJobID: "processing_job_lookup_failed",
	} {
		rec := serve(deps, "GET", path, "Bearer good.token")
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("%s status = %d, want 500", path, rec.Code)
		}
		if code := decodeError(t, rec).Error.Code; code != wantCode {
			t.Errorf("%s error_code = %q, want %q", path, code, wantCode)
		}
	}
}
