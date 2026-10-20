package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/XploY04/reelpin-go/internal/reminders"
)

// Reminders is what the API needs from the reminders service.
type Reminders interface {
	List(ctx context.Context, userID string) ([]reminders.Reminder, error)
	Create(ctx context.Context, userID string, params reminders.CreateParams) (reminders.Reminder, error)
	Delete(ctx context.Context, userID, reminderID string) error
}

type reminderListResponse struct {
	Reminders []reminders.Reminder `json:"reminders"`
}

type reminderCreateInput struct {
	Title  string  `json:"title"`
	FireAt string  `json:"fire_at"`
	Body   string  `json:"body"`
	ReelID *string `json:"reel_id"`
}

func (s *Server) handleListReminders(w http.ResponseWriter, r *http.Request) {
	list, err := s.deps.Reminders.List(r.Context(), requestUserID(r))
	if err != nil {
		s.deps.Logger.Error("listing reminders failed", "error", err)
		internalError(w, "reminder_list_failed", "Could not load reminders right now.")
		return
	}
	writeJSON(w, http.StatusOK, reminderListResponse{Reminders: list})
}

func (s *Server) handleCreateReminder(w http.ResponseWriter, r *http.Request) {
	var input reminderCreateInput
	if !decodeJSONBody(w, r, &input) {
		return
	}

	reminder, err := s.deps.Reminders.Create(r.Context(), requestUserID(r), reminders.CreateParams{
		Title: input.Title, FireAt: input.FireAt, Body: input.Body, ReelID: input.ReelID,
	})
	var invalid *reminders.ValidationError
	switch {
	case errors.As(err, &invalid):
		validationError(w, invalid.Field, invalid.Reason)
		return
	case errors.Is(err, reminders.ErrUnknownSave):
		validationError(w, "reel_id", "must be one of your saves")
		return
	case err != nil:
		s.deps.Logger.Error("creating a reminder failed", "error", err)
		internalError(w, "reminder_create_failed", "Could not create the reminder right now.")
		return
	}
	writeJSON(w, http.StatusOK, reminder)
}

func (s *Server) handleDeleteReminder(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "reminder_id", "reminder_not_found")
	if !ok {
		return
	}
	err := s.deps.Reminders.Delete(r.Context(), requestUserID(r), id.String())
	if errors.Is(err, reminders.ErrNotFound) {
		notFoundError(w, "reminder_not_found")
		return
	}
	if err != nil {
		s.deps.Logger.Error("deleting a reminder failed", "error", err)
		internalError(w, "reminder_delete_failed", "Could not delete the reminder right now.")
		return
	}
	writeJSON(w, http.StatusOK, okResponse{OK: true})
}
