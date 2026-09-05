package httpapi

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"

	"github.com/XploY04/reelpin-go/internal/notify"
	"github.com/XploY04/reelpin-go/internal/ratelimit"
)

// Notifications is what the API needs from the notification service.
type Notifications interface {
	RegisterToken(ctx context.Context, userID, token, platform, appVersion string, appBuild *int) error
	DeleteToken(ctx context.Context, userID, token string) error
	MarkOpened(ctx context.Context, userID, notificationID string) error
	SendToUser(ctx context.Context, notification notify.Notification) (string, error)
	CreateCampaign(ctx context.Context, campaign notify.Campaign, sendNow bool) (notify.Campaign, error)
	ListCampaigns(ctx context.Context, limit int) ([]notify.Campaign, error)
	GetCampaign(ctx context.Context, campaignID string) (notify.Campaign, error)
	SendCampaign(ctx context.Context, campaignID string) (notify.Campaign, error)
	CancelCampaign(ctx context.Context, campaignID string) (notify.Campaign, error)
}

type devicePushTokenInput struct {
	Token      string  `json:"token"`
	Platform   string  `json:"platform"`
	AppVersion *string `json:"app_version"`
	AppBuild   *int    `json:"app_build"`
	Timezone   *string `json:"timezone"`
	Locale     *string `json:"locale"`
}

type devicePushTokenDeleteInput struct {
	Token string `json:"token"`
}

type proactiveRecallInput struct {
	UserID string            `json:"user_id"`
	Title  string            `json:"title"`
	Body   string            `json:"body"`
	Data   map[string]string `json:"data"`
}

type campaignCreateInput struct {
	Title                    string                 `json:"title"`
	Body                     string                 `json:"body"`
	Target                   string                 `json:"target"`
	AnnouncementID           *string                `json:"announcement_id"`
	MinimumSupportedAppBuild *int                   `json:"minimum_supported_app_build"`
	ScheduledAt              *string                `json:"scheduled_at"`
	SendNow                  bool                   `json:"send_now"`
	AudienceFilters          notify.AudienceFilters `json:"audience_filters"`
}

type campaignListResponse struct {
	Campaigns []notify.Campaign `json:"campaigns"`
}

func (s *Server) handleRegisterPushToken(w http.ResponseWriter, r *http.Request) {
	var input devicePushTokenInput
	if !decodeJSONBody(w, r, &input) {
		return
	}
	if strings.TrimSpace(input.Token) == "" {
		validationError(w, "token is required")
		return
	}

	version := ""
	if input.AppVersion != nil {
		version = *input.AppVersion
	}
	if err := s.deps.Notifications.RegisterToken(r.Context(), requestUserID(r),
		input.Token, input.Platform, version, input.AppBuild); err != nil {
		// The token itself never reaches the log.
		s.deps.Logger.Error("registering a device token failed", "error", err)
		internalError(w, "device_token_registration_failed", "Could not register this device right now.")
		return
	}
	writeJSON(w, http.StatusOK, genericSuccessResponse{Success: true, Message: "registered"})
}

func (s *Server) handleDeletePushToken(w http.ResponseWriter, r *http.Request) {
	var input devicePushTokenDeleteInput
	if !decodeJSONBody(w, r, &input) {
		return
	}
	if strings.TrimSpace(input.Token) == "" {
		validationError(w, "token is required")
		return
	}

	if err := s.deps.Notifications.DeleteToken(r.Context(), requestUserID(r), input.Token); err != nil {
		s.deps.Logger.Error("removing a device token failed", "error", err)
		internalError(w, "device_token_removal_failed", "Could not remove this device right now.")
		return
	}
	writeJSON(w, http.StatusOK, genericSuccessResponse{Success: true, Message: "removed"})
}

func (s *Server) handleNotificationOpened(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "notification_id", "notification_not_found")
	if !ok {
		return
	}

	err := s.deps.Notifications.MarkOpened(r.Context(), requestUserID(r), id.String())
	if errors.Is(err, notify.ErrNotFound) {
		// Another user's notification is not this user's to mark.
		notFoundError(w, "notification_not_found")
		return
	}
	if err != nil {
		s.deps.Logger.Error("recording a notification open failed", "error", err)
		internalError(w, "notification_open_failed", "Could not record that right now.")
		return
	}
	writeJSON(w, http.StatusOK, genericSuccessResponse{Success: true, Message: "recorded"})
}

// handleProactiveRecall is an operator tool, guarded by the admin key rather
// than a user session.
func (s *Server) handleProactiveRecall(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminKey(w, r) {
		return
	}

	var input proactiveRecallInput
	if !decodeJSONBody(w, r, &input) {
		return
	}
	if strings.TrimSpace(input.UserID) == "" || strings.TrimSpace(input.Title) == "" {
		validationError(w, "user_id and title are required")
		return
	}

	data := map[string]string{"schema_version": "1", "type": "proactive_recall", "target": "home"}
	for key, value := range input.Data {
		data[key] = value
	}

	if _, err := s.deps.Notifications.SendToUser(r.Context(), notify.Notification{
		UserID: input.UserID, Title: input.Title, Body: input.Body,
		Type: "proactive_recall", Target: "home", TargetID: input.UserID,
		Data: data,
	}); err != nil {
		if errors.Is(err, notify.ErrNoDeviceTokens) {
			writeJSON(w, http.StatusOK, genericSuccessResponse{
				Success: false, Message: "the user has no registered device",
			})
			return
		}
		s.deps.Logger.Error("proactive recall failed", "error", err)
		internalError(w, "notification_send_failed", "Could not send that notification right now.")
		return
	}
	writeJSON(w, http.StatusOK, genericSuccessResponse{Success: true, Message: "sent"})
}

func (s *Server) handleCreateCampaign(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminKey(w, r) {
		return
	}
	var input campaignCreateInput
	if !decodeJSONBody(w, r, &input) {
		return
	}

	campaign, err := s.deps.Notifications.CreateCampaign(r.Context(), notify.Campaign{
		Title: input.Title, Body: input.Body, Target: "announcement",
		AnnouncementID:           input.AnnouncementID,
		MinimumSupportedAppBuild: input.MinimumSupportedAppBuild,
		ScheduledAt:              input.ScheduledAt,
		AudienceFilters:          input.AudienceFilters,
	}, input.SendNow)
	if err != nil {
		s.writeCampaignError(w, r, err, "campaign_create_failed", "Could not create the campaign right now.")
		return
	}
	writeJSON(w, http.StatusOK, campaign)
}

func (s *Server) handleListCampaigns(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminKey(w, r) {
		return
	}
	limit, ok := intParam(w, r.URL.Query(), "limit", 50, 1, 200)
	if !ok {
		return
	}

	campaigns, err := s.deps.Notifications.ListCampaigns(r.Context(), limit)
	if err != nil {
		s.writeCampaignError(w, r, err, "campaign_list_failed", "Could not load campaigns right now.")
		return
	}
	writeJSON(w, http.StatusOK, campaignListResponse{Campaigns: campaigns})
}

func (s *Server) handleCampaignDetail(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminKey(w, r) {
		return
	}
	id, ok := pathUUID(w, r, "campaign_id", "campaign_not_found")
	if !ok {
		return
	}

	campaign, err := s.deps.Notifications.GetCampaign(r.Context(), id.String())
	if err != nil {
		s.writeCampaignError(w, r, err, "campaign_detail_failed", "Could not load the campaign right now.")
		return
	}
	writeJSON(w, http.StatusOK, campaign)
}

func (s *Server) handleSendCampaign(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminKey(w, r) {
		return
	}
	id, ok := pathUUID(w, r, "campaign_id", "campaign_not_found")
	if !ok {
		return
	}

	campaign, err := s.deps.Notifications.SendCampaign(r.Context(), id.String())
	if err != nil {
		s.writeCampaignError(w, r, err, "campaign_send_failed", "Could not send the campaign right now.")
		return
	}
	writeJSON(w, http.StatusOK, campaign)
}

func (s *Server) handleCancelCampaign(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminKey(w, r) {
		return
	}
	id, ok := pathUUID(w, r, "campaign_id", "campaign_not_found")
	if !ok {
		return
	}

	campaign, err := s.deps.Notifications.CancelCampaign(r.Context(), id.String())
	if err != nil {
		s.writeCampaignError(w, r, err, "campaign_cancel_failed", "Could not cancel the campaign right now.")
		return
	}
	writeJSON(w, http.StatusOK, campaign)
}

func (s *Server) writeCampaignError(w http.ResponseWriter, r *http.Request, err error, code, message string) {
	if errors.Is(err, notify.ErrNotFound) {
		notFoundError(w, "campaign_not_found")
		return
	}
	// A refused state change is the caller's mistake, not a server fault.
	if strings.Contains(err.Error(), "cannot be") || strings.Contains(err.Error(), "needs a") {
		writeError(w, http.StatusBadRequest, errorResponse{
			ErrorCode: "campaign_state_invalid",
			Message:   "That campaign cannot do this in its current state.",
			Detail:    err.Error(),
		})
		return
	}
	s.deps.Logger.Error(code, "path", r.URL.Path, "error", err)
	internalError(w, code, message)
}

// requireAdminKey guards the operator routes. The comparison is constant time,
// so a wrong key cannot be found by timing it.
func (s *Server) requireAdminKey(w http.ResponseWriter, r *http.Request) bool {
	configured := strings.TrimSpace(s.deps.AdminKey)
	if configured == "" {
		writeError(w, http.StatusServiceUnavailable, errorResponse{
			ErrorCode: "dashboard_not_configured",
			Message:   "The admin dashboard is not configured yet.",
			Detail:    "No admin key is set on the backend",
		})
		return false
	}

	candidate := r.Header.Get("X-Admin-Key")
	if subtle.ConstantTimeCompare([]byte(candidate), []byte(configured)) != 1 {
		writeError(w, http.StatusUnauthorized, errorResponse{
			ErrorCode: "unauthorized_dashboard_access",
			Message:   "Admin dashboard authentication failed.",
			Detail:    "The X-Admin-Key header is missing or invalid",
		})
		return false
	}
	return true
}

// adminLimit is the shared policy for the operator routes.
var adminLimit = routeLimit{IP: &ratelimit.AdminActionIP}
