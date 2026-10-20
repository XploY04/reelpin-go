package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/XploY04/reelpin-go/internal/reminders"
)

// fakeReminders records what it was asked and answers with whatever the test
// set up.
type fakeReminders struct {
	list       []reminders.Reminder
	created    reminders.Reminder
	err        error
	lastUserID string
	lastParams reminders.CreateParams
	lastID     string
}

func (f *fakeReminders) List(_ context.Context, userID string) ([]reminders.Reminder, error) {
	f.lastUserID = userID
	if f.err != nil {
		return nil, f.err
	}
	if f.list == nil {
		return []reminders.Reminder{}, nil
	}
	return f.list, nil
}

func (f *fakeReminders) Create(_ context.Context, userID string, params reminders.CreateParams) (reminders.Reminder, error) {
	f.lastUserID, f.lastParams = userID, params
	return f.created, f.err
}

func (f *fakeReminders) Delete(_ context.Context, userID, reminderID string) error {
	f.lastUserID, f.lastID = userID, reminderID
	return f.err
}

func reminderDeps(fake *fakeReminders) Deps {
	deps := testDeps(&fakePinger{})
	deps.Reminders = fake
	return deps
}

func TestAnEmptyReminderListIsAListNotNull(t *testing.T) {
	fake := &fakeReminders{}
	rec := serve(reminderDeps(fake), "GET", "/api/v2/reminders", "Bearer good.token")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"reminders":[]`) {
		t.Errorf("body = %s, want an empty list", rec.Body.String())
	}
	if fake.lastUserID != testUserID {
		t.Errorf("listed for %q, want the token subject", fake.lastUserID)
	}
}

func TestCreatingAReminderPassesTheRequestThrough(t *testing.T) {
	fake := &fakeReminders{created: reminders.Reminder{
		ID: testJobID, Title: "Concert", FireAt: "2026-11-01T09:00:00Z", Status: "pending",
	}}
	rec := send(reminderDeps(fake), "POST", "/api/v2/reminders",
		`{"title":"Concert","fire_at":"2026-11-01T09:00:00Z","body":"Doors at 8","reel_id":"`+testReelID+`"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 as the Python API answered (%s)", rec.Code, rec.Body.String())
	}
	if fake.lastUserID != testUserID {
		t.Errorf("created for %q, want the token subject", fake.lastUserID)
	}
	if fake.lastParams.Title != "Concert" || fake.lastParams.FireAt != "2026-11-01T09:00:00Z" ||
		fake.lastParams.Body != "Doors at 8" || fake.lastParams.ReelID == nil || *fake.lastParams.ReelID != testReelID {
		t.Errorf("params = %+v", fake.lastParams)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["id"] != testJobID || body["status"] != "pending" {
		t.Errorf("body = %v", body)
	}
}

func TestAReminderRequestThatCannotBeStoredIsNamed(t *testing.T) {
	tests := []struct {
		name  string
		err   error
		body  string
		field string
	}{
		{"service refuses a field", &reminders.ValidationError{Field: "fire_at", Reason: "bad"},
			`{"title":"t","fire_at":"tomorrow"}`, "fire_at"},
		{"someone else's save", reminders.ErrUnknownSave,
			`{"title":"t","fire_at":"2026-11-01T09:00:00Z","reel_id":"` + testReelID + `"}`, "reel_id"},
		{"a user_id is smuggled in", nil, `{"title":"t","fire_at":"2026-11-01T09:00:00Z","user_id":"x"}`, "body"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := send(reminderDeps(&fakeReminders{err: tt.err}), "POST", "/api/v2/reminders", tt.body)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422 (%s)", rec.Code, rec.Body.String())
			}
			if field := decodeError(t, rec).Error.Details["field"]; field != tt.field {
				t.Errorf("field = %v, want %s", field, tt.field)
			}
		})
	}
}

func TestAFailedReminderWriteIsRetryable(t *testing.T) {
	rec := send(reminderDeps(&fakeReminders{err: errFake}), "POST", "/api/v2/reminders",
		`{"title":"t","fire_at":"2026-11-01T09:00:00Z"}`)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", rec.Code)
	}
	body := decodeError(t, rec)
	if body.Error.Code != "reminder_create_failed" || !body.Error.Retryable {
		t.Errorf("error = %+v", body.Error)
	}
}

func TestDeletingAReminderAnswersOK(t *testing.T) {
	fake := &fakeReminders{}
	rec := serve(reminderDeps(fake), "DELETE", "/api/v2/reminders/"+testReelID, "Bearer good.token")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	var body okResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || !body.OK {
		t.Fatalf("body = %s", rec.Body.String())
	}
	if fake.lastUserID != testUserID || fake.lastID != testReelID {
		t.Errorf("deleted %q for %q", fake.lastID, fake.lastUserID)
	}
}

func TestAMissingAndAMalformedReminderAnswerTheSame404(t *testing.T) {
	for name, target := range map[string]string{
		"missing":   "/api/v2/reminders/" + testReelID,
		"malformed": "/api/v2/reminders/not-a-uuid",
	} {
		t.Run(name, func(t *testing.T) {
			rec := serve(reminderDeps(&fakeReminders{err: reminders.ErrNotFound}), "DELETE", target, "Bearer good.token")
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404", rec.Code)
			}
			if code := decodeError(t, rec).Error.Code; code != "reminder_not_found" {
				t.Errorf("code = %q", code)
			}
		})
	}
}

func TestReminderRoutesRequireASession(t *testing.T) {
	for _, target := range []string{"/api/v2/reminders", "/api/v2/price-tracks"} {
		rec := serve(testDeps(&fakePinger{}), "GET", target, "")
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s status = %d, want 401", target, rec.Code)
		}
	}
}
