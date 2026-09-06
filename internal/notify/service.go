package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Service struct {
	pool   *pgxpool.Pool
	sender Sender
	logger *slog.Logger
	now    func() time.Time
}

func NewService(pool *pgxpool.Pool, sender Sender, logger *slog.Logger, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{pool: pool, sender: sender, logger: logger, now: now}
}

// RegisterToken records a device. The same token arriving again moves it to
// this user: a shared phone is a real thing, and the newest sign-in owns the
// device.
func (s *Service) RegisterToken(ctx context.Context, userID, token, platform string) error {
	if strings.TrimSpace(token) == "" {
		return errors.New("no device token")
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO reelpin.device_push_tokens (user_id, fcm_token, platform, last_seen_at)
		VALUES ($1, $2, $3, now())
		ON CONFLICT (fcm_token) DO UPDATE
		SET user_id = EXCLUDED.user_id,
		    platform = EXCLUDED.platform,
		    last_seen_at = now()`,
		userID, token, platform)
	if err != nil {
		// The token is a credential: it never enters an error or a log line.
		return fmt.Errorf("registering a device token: %w", err)
	}
	return nil
}

// DeleteToken removes one of this user's devices. Another user's token is not
// found rather than deleted.
func (s *Service) DeleteToken(ctx context.Context, userID, token string) error {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM reelpin.device_push_tokens WHERE user_id = $1 AND fcm_token = $2`,
		userID, token)
	if err != nil {
		return fmt.Errorf("deleting a device token: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// MarkOpened records that the user tapped it. Another user's notification is
// not found.
func (s *Service) MarkOpened(ctx context.Context, userID, notificationID string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE reelpin.notifications SET opened_at = now(), updated_at = now()
		WHERE id = $1 AND user_id = $2 AND opened_at IS NULL`,
		notificationID, userID)
	if err != nil {
		return fmt.Errorf("marking a notification opened: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Already opened or not theirs: both answer the same, so a probe
		// learns nothing about which ids exist.
		return ErrNotFound
	}
	return nil
}

// SendToUser claims the event key, then delivers. The claim is the
// deduplication: a second delivery of the same event finds the row taken and
// sends nothing, so a redelivered outbox message cannot buzz twice.
func (s *Service) SendToUser(ctx context.Context, notification Notification) (string, error) {
	data, err := json.Marshal(notification.Data)
	if err != nil {
		return "", fmt.Errorf("encoding notification data: %w", err)
	}

	var notificationID string
	err = s.pool.QueryRow(ctx, `
		INSERT INTO reelpin.notifications (user_id, event_key, kind, title, body, data)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (event_key) DO NOTHING
		RETURNING id::text`,
		notification.UserID, notification.EventKey, notification.Type,
		notification.Title, notification.Body, data,
	).Scan(&notificationID)
	if errors.Is(err, pgx.ErrNoRows) {
		// Someone already claimed this event. Report its id and send nothing.
		var existing string
		if err := s.pool.QueryRow(ctx,
			`SELECT id::text FROM reelpin.notifications WHERE event_key = $1`,
			notification.EventKey).Scan(&existing); err != nil {
			return "", fmt.Errorf("reading the existing notification: %w", err)
		}
		return existing, nil
	}
	if err != nil {
		return "", fmt.Errorf("claiming the notification: %w", err)
	}

	tokens, err := s.tokensFor(ctx, notification.UserID)
	if err != nil {
		return notificationID, err
	}
	if len(tokens) == 0 {
		// Not a failure: the app often registers its token seconds after a
		// first share. The row records that there was nowhere to send.
		s.mark(ctx, notificationID, "no_devices", "", "the user has no registered device")
		return notificationID, ErrNoDeviceTokens
	}

	payload := map[string]string{"notification_id": notificationID}
	for key, value := range notification.Data {
		payload[key] = value
	}

	deliveries, err := s.sender.Send(ctx, tokens, Message{
		Title: notification.Title, Body: notification.Body, Data: payload,
	})
	if err != nil {
		s.mark(ctx, notificationID, "failed", "", "the push provider was unavailable")
		return notificationID, fmt.Errorf("sending: %w", err)
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

	// A token the provider rejected permanently will never work again.
	if len(invalid) > 0 {
		if _, err := s.pool.Exec(ctx,
			`DELETE FROM reelpin.device_push_tokens WHERE fcm_token = ANY($1)`, invalid); err != nil {
			s.logger.Warn("removing rejected device tokens failed", "count", len(invalid), "error", err)
		}
	}

	switch {
	case messageID != "":
		s.mark(ctx, notificationID, "sent", messageID, "")
		return notificationID, nil
	case retryable:
		s.mark(ctx, notificationID, "failed", "", "the push provider was unavailable")
		return notificationID, ErrRetryable
	default:
		s.mark(ctx, notificationID, "failed", "", "no device accepted the notification")
		return notificationID, ErrNoDeviceTokens
	}
}

func (s *Service) tokensFor(ctx context.Context, userID string) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT fcm_token FROM reelpin.device_push_tokens
		WHERE user_id = $1
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
			return nil, fmt.Errorf("reading a device token: %w", err)
		}
		tokens = append(tokens, token)
	}
	return tokens, rows.Err()
}

func (s *Service) mark(ctx context.Context, notificationID, status, messageID, reason string) {
	if _, err := s.pool.Exec(ctx, `
		UPDATE reelpin.notifications
		SET status = $2, provider_message_id = NULLIF($3, ''), failure_reason = NULLIF($4, ''),
		    updated_at = now()
		WHERE id = $1`,
		notificationID, status, messageID, reason); err != nil {
		s.logger.Error("recording notification delivery failed", "status", status, "error", err)
	}
}
