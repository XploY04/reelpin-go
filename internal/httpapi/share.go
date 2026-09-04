package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/XploY04/reelpin-go/internal/safehttp"
	"github.com/XploY04/reelpin-go/internal/sourceidentity"
)

// maxShareBodyBytes bounds a share payload before it reaches the parser. The
// per-field limit lives with the enqueue API.
const maxShareBodyBytes = 64 << 10

// ShareResolver answers "what link is this share about, and can we take it?".
type ShareResolver interface {
	ResolveSharePayload(ctx context.Context, payload string) sourceidentity.ShareResolveResponse
}

type shareResolveInput struct {
	RawPayloadText string         `json:"raw_payload_text"`
	Platform       *string        `json:"platform"`
	Metadata       map[string]any `json:"metadata"`
}

func (s *Server) handleResolveShare(w http.ResponseWriter, r *http.Request) {
	var input shareResolveInput
	if !decodeJSONBody(w, r, &input) {
		return
	}

	response := s.deps.Share.ResolveSharePayload(r.Context(), input.RawPayloadText)

	// The payload itself never reaches a log line. The hash is enough to
	// correlate one share across requests.
	fields := []any{"supported", response.Supported}
	if response.ExtractedURL != nil {
		fields = append(fields, "url_hash", safehttp.URLHash(*response.ExtractedURL))
	}
	if response.Provider != nil {
		fields = append(fields, "platform", *response.Provider)
	}
	s.deps.Logger.Info("share resolved", fields...)

	writeJSON(w, http.StatusOK, response)
}

func decodeJSONBody(w http.ResponseWriter, r *http.Request, target any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxShareBodyBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(target); err != nil {
		var maxBytes *http.MaxBytesError
		switch {
		case errors.As(err, &maxBytes):
			validationError(w, "the request body is too large")
		case errors.Is(err, io.EOF):
			validationError(w, "a JSON body is required")
		default:
			validationError(w, "the request body is not valid JSON")
		}
		return false
	}
	return true
}
