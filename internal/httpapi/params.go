package httpapi

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/XploY04/reelpin-go/internal/auth"
	"github.com/XploY04/reelpin-go/internal/reels"
	"github.com/google/uuid"
)

func requestUserID(r *http.Request) string {
	userID, _ := auth.UserID(r.Context())
	return userID
}

// validationError carries the offending field in details when the caller can
// act on it. The message is what a person reads; details is what a client can
// point at.
func validationError(w http.ResponseWriter, field, detail string) {
	details := map[string]any{}
	if field != "" {
		details["field"] = field
	}
	if detail != "" {
		details["reason"] = detail
	}
	writeError(w, http.StatusUnprocessableEntity, errorBody{
		Code:    "validation_error",
		Message: "The request is missing required fields or contains invalid values.",
		Details: details,
	})
}

func notFoundError(w http.ResponseWriter, code string) {
	writeError(w, http.StatusNotFound, errorBody{
		Code:    code,
		Message: "The requested record was not found.",
	})
}

func internalError(w http.ResponseWriter, code, message string) {
	writeError(w, http.StatusInternalServerError, errorBody{
		Code:      code,
		Message:   message,
		Retryable: true,
	})
}

// parsePlatforms writes the 400 the Python API returns for an unknown value.
func parsePlatforms(w http.ResponseWriter, query url.Values) ([]string, bool) {
	platforms, err := reels.ParsePlatformFilter(query.Get("platform"))
	if err == nil {
		return platforms, true
	}

	var invalid *reels.InvalidPlatformError
	if !errors.As(err, &invalid) {
		internalError(w, "reel_list_failed", "Could not load reels right now.")
		return nil, false
	}
	writeError(w, http.StatusBadRequest, errorBody{
		Code:    "invalid_platform",
		Message: "'" + invalid.Value + "' is not a supported platform filter.",
		Details: map[string]any{"field": "platform", "allowed": invalid.Allowed},
	})
	return nil, false
}

func intParam(w http.ResponseWriter, query url.Values, name string, fallback, minimum, maximum int) (int, bool) {
	raw := strings.TrimSpace(query.Get(name))
	if raw == "" {
		return fallback, true
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		validationError(w, name, "must be an integer")
		return 0, false
	}
	if value < minimum || value > maximum {
		validationError(w, name, "must be between "+strconv.Itoa(minimum)+" and "+strconv.Itoa(maximum))
		return 0, false
	}
	return value, true
}

func boolParam(w http.ResponseWriter, query url.Values, name string, fallback bool) (bool, bool) {
	raw := strings.TrimSpace(query.Get(name))
	if raw == "" {
		return fallback, true
	}
	switch strings.ToLower(raw) {
	case "true", "1", "yes", "on":
		return true, true
	case "false", "0", "no", "off":
		return false, true
	}
	validationError(w, name, "must be a boolean")
	return false, false
}

// savedDateParam keeps a malformed date out of the SQL the Python API would
// have let Supabase reject.
func savedDateParam(w http.ResponseWriter, query url.Values) (string, bool) {
	raw := strings.TrimSpace(query.Get("saved_date"))
	if raw == "" {
		return "", true
	}
	if _, err := time.Parse("2006-01-02", raw); err != nil {
		validationError(w, "saved_date", "must be YYYY-MM-DD")
		return "", false
	}
	return raw, true
}

// pathUUID rejects a malformed id with the same 404 a missing record gets, so
// probing ids never leaks the difference.
func pathUUID(w http.ResponseWriter, r *http.Request, name, errorCode string) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue(name))
	if err != nil {
		notFoundError(w, errorCode)
		return uuid.Nil, false
	}
	return id, true
}
