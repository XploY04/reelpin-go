package website

import (
	"errors"
	"strings"
	"testing"
)

func TestNormalizeFeedback(t *testing.T) {
	got, err := NormalizeFeedback(Feedback{Topic: "Support", Email: " a@b.co ", Message: " hello "})
	if err != nil {
		t.Fatal(err)
	}
	if got.Email != "a@b.co" || got.Message != "hello" {
		t.Errorf("not trimmed: %+v", got)
	}

	if _, err := NormalizeFeedback(Feedback{Topic: "Feedback", Message: strings.Repeat("é", MaxMessage)}); err != nil {
		t.Errorf("a message of exactly the limit in characters was refused: %v", err)
	}
}

func TestNormalizeFeedbackRejects(t *testing.T) {
	tests := []struct {
		name     string
		feedback Feedback
		field    string
	}{
		{"topic", Feedback{Topic: "Other", Message: "hi"}, "topic"},
		{"empty", Feedback{Topic: "Feedback"}, "message"},
		{"long", Feedback{Topic: "Feedback", Message: strings.Repeat("a", MaxMessage+1)}, "message"},
		{"email", Feedback{Topic: "Feedback", Email: "a@", Message: "hi"}, "email"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NormalizeFeedback(tt.feedback)
			var invalid *ValidationError
			if !errors.As(err, &invalid) || invalid.Field != tt.field {
				t.Fatalf("err = %v, want %s", err, tt.field)
			}
		})
	}
}

func TestNormalizeClick(t *testing.T) {
	got, err := NormalizeClick(Click{Target: "play", Referer: strings.Repeat("r", 600), UserAgent: "ua"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Referer) != 500 {
		t.Errorf("referer length = %d, want 500", len(got.Referer))
	}
	if _, err := NormalizeClick(Click{Target: "web"}); err == nil {
		t.Error("an unknown target was accepted")
	}
}
