package jobs

import (
	"fmt"
	"strings"
	"time"
)

var stepMessages = map[string]string{
	"queued":          "Queued for processing.",
	"starting":        "Starting processing.",
	"checking_cache":  "Checking for cached results.",
	"downloading":     "Downloading media.",
	"transcribing":    "Transcribing audio.",
	"ocr":             "Reading text from images.",
	"extracting":      "Extracting structured details.",
	"categorizing":    "Organizing the reel into your categories.",
	"saving":          "Saving the reel.",
	"embedding":       "Indexing the reel for search.",
	"completed":       "Processing completed.",
	"retry_scheduled": "Waiting before the next retry.",
	"dead_lettered":   "Processing stopped after a final failure.",
}

var failureMessages = map[string]string{
	"auth_failure":              "The source platform requires a fresh authenticated session.",
	"rate_limit":                "The source platform is rate limiting requests right now.",
	"no_audio":                  "This video does not include an audio track.",
	"transcript_unavailable":    "A transcript was not available for this media.",
	"unsupported_post_type":     "This shared post type is not supported yet.",
	"ocr_failure":               "Image text extraction failed for this post.",
	"provider_timeout":          "An upstream provider timed out while processing this request.",
	"x_oembed_timeout":          "X took too long to return this post.",
	"x_oembed_upstream_error":   "X could not return this post right now.",
	"invalid_url":               "This is not a supported X post URL.",
	"unsupported_x_url":         "This is not a supported X post URL.",
	"external_tco_destination":  "This t.co link does not point to an X post.",
	"post_not_found":            "This X post was not found.",
	"protected_or_unavailable":  "This X post is protected or unavailable.",
	"malformed_oembed_response": "X did not return usable public post content.",
	"post_id_mismatch":          "X did not return usable public post content.",
	"empty_post_content":        "X did not return usable public post content.",
	"unsafe_redirect":           "This shortened link could not be followed safely.",
	"request_too_large":         "The media payload was too large to process.",
	"internal_error":            "The server could not finish this request.",
}

var retryableFailureCodes = map[string]bool{
	"rate_limit":              true,
	"provider_timeout":        true,
	"x_oembed_timeout":        true,
	"x_oembed_upstream_error": true,
	"internal_error":          true,
}

// Normalize presents a completed job that never recorded a result reel as a
// failure. The Python API also writes that correction back; a GET here never
// mutates, so the row keeps whatever the worker left behind.
func Normalize(record JobRecord) JobRecord {
	if record.Status != StatusCompleted || optionalString(record.ResultReelID) != "" {
		return record
	}

	step := StatusFailed
	code := "internal_error"
	message := "Could not finish saving this reel. Please try again."

	record.Status = StatusFailed
	record.CurrentStep = &step
	record.ProgressPercent = 100
	record.FailureCode = &code
	record.ErrorMessage = &message
	if record.CompletedAt == nil {
		now := time.Now().UTC()
		record.CompletedAt = &now
	}
	return record
}

func FailureCode(value *string) *string {
	code := strings.TrimSpace(optionalString(value))
	if code == "" {
		return nil
	}
	if _, ok := failureMessages[code]; !ok {
		return nil
	}
	return &code
}

func StatusLabel(record JobRecord) string {
	label := strings.TrimSpace(strings.ReplaceAll(optionalString(record.CurrentStep), "_", " "))
	if label == "" {
		label = strings.TrimSpace(strings.ReplaceAll(record.Status, "_", " "))
	}
	if label == "" {
		label = "queued"
	}
	return titleCase(label)
}

func StatusMessage(record JobRecord) string {
	status := normalized(record.Status)
	step := normalized(optionalString(record.CurrentStep))
	code := FailureCode(record.FailureCode)

	switch {
	case status == StatusCompleted:
		return "Processing completed."
	case status == StatusDeadLettered:
		return failureMessage(code, "Processing stopped after a final failure.")
	case status == StatusFailed:
		return failureMessage(code, "Processing failed.")
	case status == StatusQueued && step == "retry_scheduled":
		if record.NextRetryAt != nil {
			return fmt.Sprintf("Retry scheduled for %s.", record.NextRetryAt.Local().Format("2006-01-02T15:04-07:00"))
		}
		return stepMessages["retry_scheduled"]
	}
	if message, ok := stepMessages[step]; ok {
		return message
	}
	switch status {
	case StatusProcessing:
		return "Processing is in progress."
	case StatusQueued:
		return "Queued for processing."
	}
	return "Processing state updated."
}

func Terminal(record JobRecord) bool {
	switch normalized(record.Status) {
	case StatusCompleted, StatusFailed, StatusDeadLettered:
		return true
	}
	return false
}

func RetryScheduled(record JobRecord) bool {
	return normalized(record.Status) == StatusQueued &&
		normalized(optionalString(record.CurrentStep)) == "retry_scheduled"
}

func Retryable(record JobRecord) bool {
	if RetryScheduled(record) {
		return true
	}
	switch normalized(record.Status) {
	case StatusProcessing, StatusQueued:
		return true
	}
	code := FailureCode(record.FailureCode)
	return code != nil && retryableFailureCodes[*code]
}

func RecommendedPollAfterSeconds(record JobRecord, now time.Time) *int {
	if Terminal(record) {
		return nil
	}

	if RetryScheduled(record) {
		if record.NextRetryAt == nil {
			return intPtr(10)
		}
		delta := int(record.NextRetryAt.Sub(now).Seconds())
		if delta <= 0 {
			return intPtr(2)
		}
		return intPtr(min(max(delta, 2), 30))
	}

	switch normalized(record.Status) {
	case StatusQueued:
		return intPtr(3)
	case StatusProcessing:
		return intPtr(2)
	}
	return nil
}

func ProgressPercent(record JobRecord) int {
	switch normalized(record.Status) {
	case StatusCompleted, StatusFailed, StatusDeadLettered:
		return 100
	case StatusProcessing:
		return max(record.ProgressPercent, 5)
	}
	return max(min(record.ProgressPercent, 99), 0)
}

func failureMessage(code *string, fallback string) string {
	if code == nil {
		return fallback
	}
	if message, ok := failureMessages[*code]; ok {
		return message
	}
	return fallback
}

func normalized(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func optionalString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func intPtr(value int) *int { return &value }

func titleCase(value string) string {
	var out strings.Builder
	newWord := true
	for _, r := range value {
		switch {
		case r == ' ':
			newWord = true
			out.WriteRune(r)
		case newWord:
			out.WriteString(strings.ToUpper(string(r)))
			newWord = false
		default:
			out.WriteString(strings.ToLower(string(r)))
		}
	}
	return out.String()
}
