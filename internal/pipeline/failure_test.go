package pipeline

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/XploY04/reelpin-go/internal/ai"
)

func TestClassifyCoversEveryClass(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		wantClass Class
		wantCode  string
		retryable bool
	}{
		{
			name:      "provider quota is exhausted",
			err:       &ai.ProviderError{StatusCode: http.StatusTooManyRequests, RetryAfter: time.Minute},
			wantClass: ProviderExhausted, wantCode: "provider_exhausted", retryable: true,
		},
		{
			name:      "provider outage is transient",
			err:       &ai.ProviderError{StatusCode: http.StatusBadGateway},
			wantClass: Transient, wantCode: "provider_unavailable", retryable: true,
		},
		{
			name:      "network failure is transient",
			err:       &ai.ProviderError{StatusCode: 0},
			wantClass: Transient, wantCode: "provider_unavailable", retryable: true,
		},
		{
			name:      "a 4xx from the provider is our bug",
			err:       &ai.ProviderError{StatusCode: http.StatusBadRequest},
			wantClass: Internal, wantCode: "internal_error", retryable: true,
		},
		{
			name:      "unusable model output",
			err:       ai.ErrInvalidExtraction,
			wantClass: Transient, wantCode: "extraction_invalid", retryable: true,
		},
		{
			name:      "a stage deadline",
			err:       context.DeadlineExceeded,
			wantClass: Transient, wantCode: "provider_timeout", retryable: true,
		},
		{
			name:      "anything else is internal",
			err:       errors.New("nil pointer somewhere"),
			wantClass: Internal, wantCode: "internal_error", retryable: true,
		},
		{
			name:      "a stage's own terminal failure passes through",
			err:       errUnsupportedPlatform,
			wantClass: ContentTerminal, wantCode: "unsupported_platform", retryable: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			failure := Classify(tt.err)
			if failure.Class != tt.wantClass {
				t.Errorf("class = %s, want %s", failure.Class, tt.wantClass)
			}
			if failure.Code != tt.wantCode {
				t.Errorf("code = %q, want %q", failure.Code, tt.wantCode)
			}
			if failure.Retryable() != tt.retryable {
				t.Errorf("retryable = %v, want %v", failure.Retryable(), tt.retryable)
			}
			if failure.Message == "" {
				t.Error("no message for a user to read")
			}
		})
	}
}

func TestClassesMatchTheSchemaConstraint(t *testing.T) {
	// The check constraint in 00002_processing.sql accepts exactly these four.
	for _, class := range []Class{Transient, ProviderExhausted, ContentTerminal, Internal} {
		switch class {
		case "transient", "provider_exhausted", "content_terminal", "internal":
		default:
			t.Errorf("class %q would violate stage_results_error_class_check", class)
		}
	}
}

func TestInputHashIsOrderSensitiveAndDelimited(t *testing.T) {
	if InputHash("ab", "c") == InputHash("a", "bc") {
		t.Fatal("concatenation without a delimiter lets different inputs collide")
	}
	if InputHash("a", "b") == InputHash("b", "a") {
		t.Fatal("input order must matter")
	}
	if InputHash("a") != InputHash("a") {
		t.Fatal("the hash is not deterministic")
	}
}
