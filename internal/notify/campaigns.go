package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Campaign statuses. A campaign moves forward only: draft or scheduled can be
// cancelled, sending cannot be un-sent.
const (
	StatusDraft     = "draft"
	StatusScheduled = "scheduled"
	StatusSending   = "sending"
	StatusCompleted = "completed"
	StatusCancelled = "cancelled"
	StatusFailed    = "failed"
)

// AudienceFilters narrow who a campaign reaches.
type AudienceFilters struct {
	UserIDs        []string `json:"user_ids"`
	ExcludeUserIDs []string `json:"exclude_user_ids"`
	Platforms      []string `json:"platforms"`
	Locales        []string `json:"locales"`
	Timezones      []string `json:"timezones"`
}

type Campaign struct {
	CampaignID               string          `json:"campaign_id"`
	Title                    string          `json:"title"`
	Body                     string          `json:"body"`
	Target                   string          `json:"target"`
	AnnouncementID           *string         `json:"announcement_id"`
	MinimumSupportedAppBuild *int            `json:"minimum_supported_app_build"`
	ScheduledAt              *string         `json:"scheduled_at"`
	Status                   string          `json:"status"`
	AudienceFilters          AudienceFilters `json:"audience_filters"`
	RecipientCount           int             `json:"recipient_count"`
	SentCount                int             `json:"sent_count"`
	FailedCount              int             `json:"failed_count"`
	SkippedCount             int             `json:"skipped_count"`
	FailureReason            *string         `json:"failure_reason"`
	CreatedAt                *string         `json:"created_at"`
	UpdatedAt                *string         `json:"updated_at"`
	StartedAt                *string         `json:"started_at"`
	CompletedAt              *string         `json:"completed_at"`
}

func (s *Service) CreateCampaign(ctx context.Context, campaign Campaign, sendNow bool) (Campaign, error) {
	if strings.TrimSpace(campaign.Title) == "" || strings.TrimSpace(campaign.Body) == "" {
		return Campaign{}, fmt.Errorf("a campaign needs a title and a body")
	}

	filters, err := json.Marshal(campaign.AudienceFilters)
	if err != nil {
		return Campaign{}, fmt.Errorf("encoding audience filters: %w", err)
	}

	status := StatusDraft
	if campaign.ScheduledAt != nil {
		status = StatusScheduled
	}

	var campaignID string
	if err := s.pool.QueryRow(ctx, `
		INSERT INTO public.notification_campaigns
			(title, body, target, announcement_id, minimum_supported_app_build,
			 scheduled_at, status, audience_filters)
		VALUES ($1, $2, 'announcement', COALESCE($3, ''), $4, $5::timestamptz, $6, $7)
		RETURNING campaign_id::text`,
		campaign.Title, campaign.Body, campaign.AnnouncementID,
		campaign.MinimumSupportedAppBuild, campaign.ScheduledAt, status, filters,
	).Scan(&campaignID); err != nil {
		return Campaign{}, fmt.Errorf("creating the campaign: %w", err)
	}

	if sendNow {
		if _, err := s.SendCampaign(ctx, campaignID); err != nil {
			return Campaign{}, err
		}
	}
	return s.GetCampaign(ctx, campaignID)
}

func (s *Service) ListCampaigns(ctx context.Context, limit int) ([]Campaign, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, campaignQuery+` ORDER BY c.created_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("listing campaigns: %w", err)
	}
	defer rows.Close()

	campaigns := []Campaign{}
	for rows.Next() {
		campaign, err := scanCampaign(rows)
		if err != nil {
			return nil, err
		}
		campaigns = append(campaigns, campaign)
	}
	return campaigns, rows.Err()
}

func (s *Service) GetCampaign(ctx context.Context, campaignID string) (Campaign, error) {
	rows, err := s.pool.Query(ctx, campaignQuery+` WHERE c.campaign_id = $1`, campaignID)
	if err != nil {
		return Campaign{}, fmt.Errorf("reading the campaign: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		return Campaign{}, ErrNotFound
	}
	return scanCampaign(rows)
}

// CancelCampaign stops a campaign that has not started. One that is already
// sending cannot be un-sent, and saying otherwise would be a lie.
func (s *Service) CancelCampaign(ctx context.Context, campaignID string) (Campaign, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE public.notification_campaigns
		SET status = 'cancelled', updated_at = now()
		WHERE campaign_id = $1 AND status IN ('draft', 'scheduled')`, campaignID)
	if err != nil {
		return Campaign{}, fmt.Errorf("cancelling the campaign: %w", err)
	}
	if tag.RowsAffected() == 0 {
		campaign, err := s.GetCampaign(ctx, campaignID)
		if err != nil {
			return Campaign{}, err
		}
		return campaign, fmt.Errorf("a %s campaign cannot be cancelled", campaign.Status)
	}
	return s.GetCampaign(ctx, campaignID)
}

// SendCampaign resolves the audience, records one target row per recipient, and
// sends. It is resumable: re-running it picks up the pending targets rather
// than notifying everyone again.
func (s *Service) SendCampaign(ctx context.Context, campaignID string) (Campaign, error) {
	campaign, err := s.GetCampaign(ctx, campaignID)
	if err != nil {
		return Campaign{}, err
	}
	switch campaign.Status {
	case StatusCancelled, StatusCompleted:
		return campaign, fmt.Errorf("a %s campaign cannot be sent", campaign.Status)
	}

	recipients, err := s.audience(ctx, campaign)
	if err != nil {
		return Campaign{}, err
	}
	for _, userID := range recipients {
		if _, err := s.pool.Exec(ctx, `
			INSERT INTO reelpin.campaign_targets (campaign_id, user_id)
			VALUES ($1, $2) ON CONFLICT DO NOTHING`, campaignID, userID); err != nil {
			return Campaign{}, fmt.Errorf("recording a campaign target: %w", err)
		}
	}

	if _, err := s.pool.Exec(ctx, `
		UPDATE public.notification_campaigns
		SET status = 'sending', started_at = COALESCE(started_at, now()), updated_at = now()
		WHERE campaign_id = $1`, campaignID); err != nil {
		return Campaign{}, fmt.Errorf("marking the campaign sending: %w", err)
	}

	pending, err := s.pendingTargets(ctx, campaignID)
	if err != nil {
		return Campaign{}, err
	}

	for _, userID := range pending {
		notification := Notification{
			UserID:   userID,
			Title:    campaign.Title,
			Body:     campaign.Body,
			Type:     "announcement",
			Target:   "announcement",
			TargetID: campaignID,
			// One announcement per user per campaign, however often this runs.
			EventKey: EventKey("announcement", userID, campaignID),
			Data: map[string]string{
				"schema_version": "1",
				"type":           "announcement",
				"target":         "announcement",
				"campaign_id":    campaignID,
			},
		}

		notificationID, err := s.SendToUser(ctx, notification)
		switch {
		case err == nil:
			s.markTarget(ctx, campaignID, userID, "sent", notificationID, "")
		case errors.Is(err, ErrNoDeviceTokens):
			// Not a failure: they have no device registered.
			s.markTarget(ctx, campaignID, userID, "skipped", notificationID, "no device tokens")
		default:
			s.markTarget(ctx, campaignID, userID, "failed", notificationID, "delivery failed")
		}
	}

	if _, err := s.pool.Exec(ctx, `
		UPDATE public.notification_campaigns
		SET status = 'completed', completed_at = now(), updated_at = now()
		WHERE campaign_id = $1 AND status = 'sending'`, campaignID); err != nil {
		return Campaign{}, fmt.Errorf("completing the campaign: %w", err)
	}
	return s.GetCampaign(ctx, campaignID)
}

func (s *Service) markTarget(ctx context.Context, campaignID, userID, status, notificationID, reason string) {
	var id any
	if notificationID != "" {
		id = notificationID
	}
	if _, err := s.pool.Exec(ctx, `
		UPDATE reelpin.campaign_targets
		SET status = $3, notification_id = $4::uuid, failure_reason = NULLIF($5, ''), updated_at = now()
		WHERE campaign_id = $1 AND user_id = $2`,
		campaignID, userID, status, id, reason,
	); err != nil {
		s.logger.Error("recording a campaign target failed", "campaign_id", campaignID, "error", err)
	}
}

func (s *Service) pendingTargets(ctx context.Context, campaignID string) ([]string, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT user_id FROM reelpin.campaign_targets WHERE campaign_id = $1 AND status = 'pending'`,
		campaignID)
	if err != nil {
		return nil, fmt.Errorf("reading pending targets: %w", err)
	}
	defer rows.Close()

	pending := []string{}
	for rows.Next() {
		var userID string
		if err := rows.Scan(&userID); err != nil {
			return nil, fmt.Errorf("reading pending targets: %w", err)
		}
		pending = append(pending, userID)
	}
	return pending, rows.Err()
}

// audience resolves who gets this campaign. An explicit user list wins over
// every other filter: it is a deliberate, narrow send.
func (s *Service) audience(ctx context.Context, campaign Campaign) ([]string, error) {
	filters := campaign.AudienceFilters

	query := `SELECT DISTINCT user_id FROM public.device_push_tokens WHERE revoked = false`
	args := []any{}

	if len(filters.UserIDs) > 0 {
		args = append(args, filters.UserIDs)
		query += fmt.Sprintf(" AND user_id = ANY($%d)", len(args))
	}
	if len(filters.ExcludeUserIDs) > 0 {
		args = append(args, filters.ExcludeUserIDs)
		query += fmt.Sprintf(" AND user_id <> ALL($%d)", len(args))
	}
	if len(filters.Platforms) > 0 {
		args = append(args, filters.Platforms)
		query += fmt.Sprintf(" AND platform = ANY($%d)", len(args))
	}

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("resolving the audience: %w", err)
	}
	defer rows.Close()

	recipients := []string{}
	for rows.Next() {
		var userID string
		if err := rows.Scan(&userID); err != nil {
			return nil, fmt.Errorf("resolving the audience: %w", err)
		}
		recipients = append(recipients, userID)
	}
	return recipients, rows.Err()
}

const campaignQuery = `
	SELECT c.campaign_id::text, c.title, c.body, c.target, NULLIF(c.announcement_id, ''),
	       c.minimum_supported_app_build, c.scheduled_at, c.status, c.audience_filters,
	       c.created_at, c.updated_at, c.started_at, c.completed_at,
	       (SELECT count(*) FROM reelpin.campaign_targets t WHERE t.campaign_id = c.campaign_id),
	       (SELECT count(*) FROM reelpin.campaign_targets t WHERE t.campaign_id = c.campaign_id AND t.status = 'sent'),
	       (SELECT count(*) FROM reelpin.campaign_targets t WHERE t.campaign_id = c.campaign_id AND t.status = 'failed'),
	       (SELECT count(*) FROM reelpin.campaign_targets t WHERE t.campaign_id = c.campaign_id AND t.status = 'skipped')
	FROM public.notification_campaigns c`

func scanCampaign(rows pgx.Rows) (Campaign, error) {
	var (
		campaign  Campaign
		filters   []byte
		scheduled *time.Time
		created   *time.Time
		updated   *time.Time
		started   *time.Time
		completed *time.Time
	)
	if err := rows.Scan(&campaign.CampaignID, &campaign.Title, &campaign.Body, &campaign.Target,
		&campaign.AnnouncementID, &campaign.MinimumSupportedAppBuild, &scheduled, &campaign.Status,
		&filters, &created, &updated, &started, &completed,
		&campaign.RecipientCount, &campaign.SentCount, &campaign.FailedCount, &campaign.SkippedCount,
	); err != nil {
		return Campaign{}, fmt.Errorf("reading a campaign: %w", err)
	}

	_ = json.Unmarshal(filters, &campaign.AudienceFilters)
	campaign.ScheduledAt = isoTime(scheduled)
	campaign.CreatedAt = isoTime(created)
	campaign.UpdatedAt = isoTime(updated)
	campaign.StartedAt = isoTime(started)
	campaign.CompletedAt = isoTime(completed)
	return campaign, nil
}

func isoTime(value *time.Time) *string {
	if value == nil {
		return nil
	}
	formatted := value.UTC().Format(time.RFC3339Nano)
	return &formatted
}
