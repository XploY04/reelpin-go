package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/XploY04/reelpin-go/internal/notify"
)

// Notifications is what the API needs from the notification service. Campaign
// administration is deliberately absent: it is an operator command, not part
// of the product API.
type Notifications interface {
	RegisterToken(ctx context.Context, userID, token, platform string) error
	DeleteToken(ctx context.Context, userID, token string) error
	MarkOpened(ctx context.Context, userID, notificationID string) error
}

type devicePushTokenInput struct {
	Token    string `json:"token"`
	Platform string `json:"platform"`
}

var devicePlatforms = map[string]bool{"ios": true, "android": true, "web": true}

func (s *Server) handleRegisterPushToken(w http.ResponseWriter, r *http.Request) {
	var input devicePushTokenInput
	if !decodeBody(w, r, &input) {
		return
	}
	if input.Token == "" {
		validationError(w, "token", "is required")
		return
	}
	if !devicePlatforms[input.Platform] {
		validationError(w, "platform", "must be ios, android or web")
		return
	}

	if err := s.deps.Notifications.RegisterToken(
		r.Context(), requestUserID(r), input.Token, input.Platform); err != nil {
		// The token is a credential, so neither it nor the driver error is
		// echoed back.
		s.deps.Logger.Error("registering a device token failed", "error", err)
		internalError(w, "device_token_failed", "Could not register this device right now.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"registered": true})
}

func (s *Server) handleDeletePushToken(w http.ResponseWriter, r *http.Request) {
	var input devicePushTokenInput
	if !decodeBody(w, r, &input) {
		return
	}
	if input.Token == "" {
		validationError(w, "token", "is required")
		return
	}

	err := s.deps.Notifications.DeleteToken(r.Context(), requestUserID(r), input.Token)
	if errors.Is(err, notify.ErrNotFound) {
		// Another user's token and a token that never existed answer the same.
		notFoundError(w, "device_token_not_found")
		return
	}
	if err != nil {
		s.deps.Logger.Error("deleting a device token failed", "error", err)
		internalError(w, "device_token_failed", "Could not remove this device right now.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

func (s *Server) handleNotificationOpened(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "notification_id", "notification_not_found")
	if !ok {
		return
	}

	err := s.deps.Notifications.MarkOpened(r.Context(), requestUserID(r), id.String())
	if errors.Is(err, notify.ErrNotFound) {
		notFoundError(w, "notification_not_found")
		return
	}
	if err != nil {
		s.deps.Logger.Error("marking a notification opened failed", "error", err)
		internalError(w, "notification_failed", "Could not record that right now.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"opened": true})
}
