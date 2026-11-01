package notify

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

// Device is one registration: the token, and what the app said about the
// build that sent it. The build fields are optional, because an older build
// sends none of them.
type Device struct {
	Token      string
	Platform   string
	AppVersion *string
	AppBuild   *int
	Timezone   *string
	Locale     *string
}

// RegisterToken records a device. The same token arriving again moves it to
// this user: a shared phone is a real thing, and the newest sign-in owns the
// device. The build fields are overwritten every time, absent ones included,
// so a row always describes the build that registered it last.
func (s *Service) RegisterToken(ctx context.Context, userID string, device Device) error {
	if strings.TrimSpace(device.Token) == "" {
		return errors.New("no device token")
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO reelpin.device_push_tokens
			(user_id, fcm_token, platform, app_version, app_build, timezone, locale, last_seen_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, now())
		ON CONFLICT (fcm_token) DO UPDATE
		SET user_id = EXCLUDED.user_id,
		    platform = EXCLUDED.platform,
		    app_version = EXCLUDED.app_version,
		    app_build = EXCLUDED.app_build,
		    timezone = EXCLUDED.timezone,
		    locale = EXCLUDED.locale,
		    last_seen_at = now()`,
		userID, device.Token, device.Platform,
		device.AppVersion, device.AppBuild, device.Timezone, device.Locale)
	if err != nil {
		// The token is a credential: it never enters an error or a log line.
		return fmt.Errorf("registering a device token: %w", err)
	}
	return nil
}

// BuildCount is how many live tokens one platform and build have. AppBuild is
// nil for tokens whose build never said.
type BuildCount struct {
	Platform string
	AppBuild *int
	Tokens   int
}

// LiveBuilds counts the tokens registered or refreshed since a moment, by
// platform and build: what an operator reads before forcing an update, to see
// how many devices a cut-off build would strand.
func (s *Service) LiveBuilds(ctx context.Context, since time.Time) ([]BuildCount, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT platform, app_build, count(*)::int
		FROM reelpin.device_push_tokens
		WHERE last_seen_at >= $1
		GROUP BY platform, app_build
		ORDER BY platform, app_build DESC NULLS LAST`, since)
	if err != nil {
		return nil, fmt.Errorf("counting live device tokens: %w", err)
	}
	defer rows.Close()

	counts := []BuildCount{}
	for rows.Next() {
		var count BuildCount
		if err := rows.Scan(&count.Platform, &count.AppBuild, &count.Tokens); err != nil {
			return nil, fmt.Errorf("reading a build count: %w", err)
		}
		counts = append(counts, count)
	}
	return counts, rows.Err()
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

// SendToUser serializes attempts for one event and retains accepted device receipts.
func (s *Service) SendToUser(ctx context.Context, notification Notification) (string, error) {
	data, err := json.Marshal(notification.Data)
	if err != nil {
		return "", fmt.Errorf("encoding notification data: %w", err)
	}
	if _, err = s.pool.Exec(ctx, `
		INSERT INTO reelpin.notifications (user_id, event_key, kind, title, body, data)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (event_key) DO NOTHING`, notification.UserID, notification.EventKey,
		notification.Type, notification.Title, notification.Body, data); err != nil {
		return "", fmt.Errorf("recording the notification: %w", err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	var notificationID, owner, status string
	var attempts int
	err = tx.QueryRow(ctx, `SELECT id::text,user_id::text,status,delivery_attempts
		FROM reelpin.notifications WHERE event_key=$1 FOR UPDATE`, notification.EventKey).
		Scan(&notificationID, &owner, &status, &attempts)
	if err != nil {
		return "", fmt.Errorf("claiming notification delivery: %w", err)
	}
	if owner != notification.UserID {
		return "", ErrNotFound
	}
	if status == "sent" {
		return notificationID, nil
	}
	if attempts >= MaxDeliveryAttempts {
		return notificationID, ErrDeliveryExhausted
	}
	finish := func(state, messageID, reason string, result error) (string, error) {
		if _, err := tx.Exec(ctx, `UPDATE reelpin.notifications
			SET status=$2,provider_message_id=NULLIF($3,''),failure_reason=NULLIF($4,''),updated_at=now()
			WHERE id=$1`, notificationID, state, messageID, reason); err != nil {
			return notificationID, fmt.Errorf("recording push delivery: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return notificationID, fmt.Errorf("committing push delivery: %w", err)
		}
		return notificationID, result
	}
	tokens, err := tokensFor(ctx, tx, notification.UserID)
	if err != nil {
		return notificationID, err
	}
	if len(tokens) == 0 {
		return finish("no_devices", "", "the user has no registered device", ErrNoDeviceTokens)
	}
	rows, err := tx.Query(ctx, `SELECT token_hash,provider_message_id
		FROM reelpin.notification_delivery_receipts WHERE notification_id=$1`, notificationID)
	if err != nil {
		return notificationID, err
	}
	accepted := map[string]string{}
	for rows.Next() {
		var hash, id string
		if err := rows.Scan(&hash, &id); err != nil {
			rows.Close()
			return notificationID, err
		}
		accepted[hash] = id
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return notificationID, err
	}
	pending := make([]string, 0, len(tokens))
	messageID := ""
	for _, token := range tokens {
		if id, found := accepted[tokenHash(token)]; found {
			messageID = id
		} else {
			pending = append(pending, token)
		}
	}
	if len(pending) == 0 {
		return finish("sent", messageID, "", nil)
	}
	attempts++
	if _, err := tx.Exec(ctx, `UPDATE reelpin.notifications
		SET delivery_attempts=$2,kind=$3,title=$4,body=$5,data=$6,updated_at=now() WHERE id=$1`,
		notificationID, attempts, notification.Type, notification.Title, notification.Body, data); err != nil {
		return notificationID, err
	}
	payload := map[string]string{"notification_id": notificationID}
	for key, value := range notification.Data {
		payload[key] = value
	}
	deliveries, err := s.sender.Send(ctx, pending, Message{Title: notification.Title, Body: notification.Body, Data: payload})
	if err != nil {
		s.count("provider_unavailable")
		if attempts >= MaxDeliveryAttempts {
			return finish("failed", messageID, "push delivery retry budget exhausted", ErrDeliveryExhausted)
		}
		return finish("failed", messageID, "the push provider was unavailable", &RetryError{Attempt: attempts})
	}
	remaining := map[string]bool{}
	for _, token := range pending {
		remaining[token] = true
	}
	retryable := false
	invalid := []string{}
	for _, delivery := range deliveries {
		if !remaining[delivery.Token] {
			continue
		}
		delete(remaining, delivery.Token)
		switch {
		case delivery.Invalid:
			invalid = append(invalid, delivery.Token)
		case delivery.Retryable:
			retryable = true
		case delivery.MessageID != "":
			messageID = delivery.MessageID
			if _, err := tx.Exec(ctx, `INSERT INTO reelpin.notification_delivery_receipts
				(notification_id,token_hash,provider_message_id) VALUES($1,$2,$3)
				ON CONFLICT DO NOTHING`, notificationID, tokenHash(delivery.Token), delivery.MessageID); err != nil {
				return notificationID, err
			}
		}
	}
	if len(invalid) > 0 {
		if _, err := tx.Exec(ctx, `DELETE FROM reelpin.device_push_tokens WHERE user_id=$1 AND fcm_token=ANY($2)`, notification.UserID, invalid); err != nil {
			return notificationID, err
		}
	}
	if retryable || len(remaining) > 0 {
		s.count("retryable")
		if attempts >= MaxDeliveryAttempts {
			return finish("failed", messageID, "push delivery retry budget exhausted", ErrDeliveryExhausted)
		}
		return finish("failed", messageID, "the push provider was unavailable", &RetryError{Attempt: attempts})
	}
	if messageID != "" {
		s.count("sent")
		return finish("sent", messageID, "", nil)
	}
	s.count("rejected")
	return finish("failed", "", "no device accepted the notification", ErrNoDeviceTokens)
}

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func tokensFor(ctx context.Context, tx pgx.Tx, userID string) ([]string, error) {
	rows, err := tx.Query(ctx, `SELECT fcm_token FROM reelpin.device_push_tokens
		WHERE user_id=$1 ORDER BY last_seen_at DESC LIMIT $2`, userID, MaxTokensPerBatch)
	if err != nil {
		return nil, fmt.Errorf("reading device tokens: %w", err)
	}
	defer rows.Close()
	tokens := []string{}
	for rows.Next() {
		var token string
		if err := rows.Scan(&token); err != nil {
			return nil, err
		}
		tokens = append(tokens, token)
	}
	return tokens, rows.Err()
}
