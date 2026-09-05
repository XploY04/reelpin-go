package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/XploY04/reelpin-go/internal/metrics"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Service struct {
	pool   *pgxpool.Pool
	sender Sender
	logger *slog.Logger
	now    func() time.Time
	// Metrics is optional. Nil means delivery is not counted.
	Metrics *metrics.Metrics
}

func (s *Service) count(outcome string) {
	if s.Metrics != nil {
		s.Metrics.PushDelivery.WithLabelValues(outcome).Inc()
	}
}

func NewService(pool *pgxpool.Pool, sender Sender, logger *slog.Logger, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{pool: pool, sender: sender, logger: logger, now: now}
}

// RegisterToken records a device. The same token arriving again moves it to
// this user, because a shared phone is a real thing and the newest sign-in owns
// the device.
func (s *Service) RegisterToken(ctx context.Context, userID, token, platform, appVersion string, appBuild *int) error {
	cleaned := strings.TrimSpace(token)
	if cleaned == "" {
		return fmt.Errorf("a device token is required")
	}

	if _, err := s.pool.Exec(ctx, `
		INSERT INTO public.device_push_tokens (user_id, fcm_token, platform, last_seen_at)
		VALUES ($1, $2, $3, now())
		ON CONFLICT (fcm_token)
		DO UPDATE SET user_id = EXCLUDED.user_id,
		              platform = EXCLUDED.platform,
		              last_seen_at = now(),
		              revoked = false`,
		userID, cleaned, strings.TrimSpace(platform),
	); err != nil {
		return fmt.Errorf("registering the device token: %w", err)
	}
	return nil
}

// DeleteToken removes one device, on sign-out from that device only.
func (s *Service) DeleteToken(ctx context.Context, userID, token string) error {
	if _, err := s.pool.Exec(ctx,
		`DELETE FROM public.device_push_tokens WHERE user_id = $1 AND fcm_token = $2`,
		userID, strings.TrimSpace(token),
	); err != nil {
		return fmt.Errorf("removing the device token: %w", err)
	}
	return nil
}

// MarkOpened records a tap. It is scoped to the user, so one account cannot
// mark another's notification as read.
func (s *Service) MarkOpened(ctx context.Context, userID, notificationID string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE public.notifications
		SET status = 'opened', opened_at = COALESCE(opened_at, now()), updated_at = now()
		WHERE notification_id = $1 AND user_id = $2`,
		notificationID, userID)
	if err != nil {
		return fmt.Errorf("recording the open: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// SendToUser is the one path every notification takes. It claims the event
// first, so a redelivered message or a retried request produces one buzz.
func (s *Service) SendToUser(ctx context.Context, notification Notification) (string, error) {
	eventKey := notification.EventKey
	if eventKey == "" {
		eventKey = EventKey(notification.Type, notification.UserID, notification.TargetID)
	}

	notificationID, claimed, err := s.claimEvent(ctx, notification, eventKey)
	if err != nil {
		return "", err
	}
	if !claimed {
		// Someone already sent this one.
		return notificationID, nil
	}

	tokens, err := s.tokens(ctx, notification.UserID)
	if err != nil {
		return notificationID, err
	}
	if len(tokens) == 0 {
		// The device usually registers seconds later, so this is left queued
		// for the retry rather than failed.
		return notificationID, ErrNoDeviceTokens
	}

	data := map[string]string{}
	for key, value := range notification.Data {
		data[key] = value
	}
	data["notification_id"] = notificationID

	deliveries, err := s.sender.Send(ctx, tokens, Message{
		Title: notification.Title, Body: notification.Body, Data: data,
	})
	if err != nil {
		s.count("provider_unavailable")
		s.markFailed(ctx, notificationID, "the push provider was unavailable")
		return notificationID, err
	}

	messageID, retryable := "", false
	invalid := []string{}
	for _, delivery := range deliveries {
		switch {
		case delivery.Invalid:
			invalid = append(invalid, delivery.Token)
		case delivery.Retryable:
			retryable = true
		case delivery.MessageID != "":
			messageID = delivery.MessageID
		}
	}

	// A token the provider rejected will never work again.
	if len(invalid) > 0 {
		s.deactivate(ctx, invalid)
	}

	switch {
	case messageID != "":
		s.count("sent")
		s.markSent(ctx, notificationID, messageID)
	case retryable:
		s.count("retryable")
		s.markFailed(ctx, notificationID, "the push provider was unavailable")
		return notificationID, fmt.Errorf("every delivery failed transiently")
	default:
		s.count("rejected")
		s.markFailed(ctx, notificationID, "no device accepted the notification")
		return notificationID, ErrNoDeviceTokens
	}
	return notificationID, nil
}

// claimEvent inserts the notification row, or reports that its event was
// already claimed. The unique event key is what makes sending idempotent.
func (s *Service) claimEvent(ctx context.Context, notification Notification, eventKey string) (string, bool, error) {
	data, err := json.Marshal(notification.Data)
	if err != nil {
		return "", false, fmt.Errorf("encoding notification data: %w", err)
	}

	var notificationID string
	err = s.pool.QueryRow(ctx, `
		INSERT INTO public.notifications
			(notification_id, event_key, user_id, type, target, target_id, status, title, body, data)
		VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, 'queued', $6, $7, $8)
		ON CONFLICT (event_key) DO NOTHING
		RETURNING notification_id::text`,
		eventKey, notification.UserID, notification.Type, notification.Target,
		notification.TargetID, notification.Title, notification.Body, data,
	).Scan(&notificationID)
	if err == nil {
		return notificationID, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", false, fmt.Errorf("claiming the notification event: %w", err)
	}

	// Already claimed. A queued row is worth retrying; anything else is done.
	var status string
	if err := s.pool.QueryRow(ctx,
		`SELECT notification_id::text, status FROM public.notifications WHERE event_key = $1`, eventKey,
	).Scan(&notificationID, &status); err != nil {
		return "", false, fmt.Errorf("reading the claimed event: %w", err)
	}
	return notificationID, status == "queued", nil
}

func (s *Service) tokens(ctx context.Context, userID string) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT fcm_token FROM public.device_push_tokens
		WHERE user_id = $1 AND revoked = false
		ORDER BY last_seen_at DESC
		LIMIT $2`, userID, MaxTokensPerBatch)
	if err != nil {
		return nil, fmt.Errorf("reading device tokens: %w", err)
	}
	defer rows.Close()

	tokens := []string{}
	for rows.Next() {
		var token string
		if err := rows.Scan(&token); err != nil {
			return nil, fmt.Errorf("reading device tokens: %w", err)
		}
		tokens = append(tokens, token)
	}
	return tokens, rows.Err()
}

// deactivate removes tokens the provider rejected. They are deleted rather than
// flagged: a dead token has no other use.
func (s *Service) deactivate(ctx context.Context, tokens []string) {
	if _, err := s.pool.Exec(ctx,
		`DELETE FROM public.device_push_tokens WHERE fcm_token = ANY($1)`, tokens,
	); err != nil {
		// The token itself never reaches the log.
		s.logger.Warn("removing rejected device tokens failed", "count", len(tokens), "error", err)
		return
	}
	s.logger.Info("removed rejected device tokens", "count", len(tokens))
}

func (s *Service) markSent(ctx context.Context, notificationID, messageID string) {
	if _, err := s.pool.Exec(ctx, `
		UPDATE public.notifications
		SET status = 'sent_to_fcm', fcm_message_id = $2, sent_at = now(), updated_at = now()
		WHERE notification_id = $1`, notificationID, messageID,
	); err != nil {
		s.logger.Error("recording a sent notification failed", "error", err)
	}
}

func (s *Service) markFailed(ctx context.Context, notificationID, reason string) {
	if _, err := s.pool.Exec(ctx, `
		UPDATE public.notifications
		SET status = 'failed', failure_reason = $2, updated_at = now()
		WHERE notification_id = $1`, notificationID, reason,
	); err != nil {
		s.logger.Error("recording a failed notification failed", "error", err)
	}
}
