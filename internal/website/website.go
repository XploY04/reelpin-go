// Package website records what the marketing site sends the API: feedback
// messages and store-link taps.
package website

import (
	"context"
	"fmt"
	"net/mail"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	MaxMessage   = 2000
	maxEmail     = 254
	maxHeaderCap = 500
	maxCountry   = 8
)

// Topics are the options the feedback form offers.
var Topics = []string{"Feedback", "Support", "Partnership", "Privacy request"}

// Targets are the store links the site counts.
var Targets = []string{"play", "ios"}

// ValidationError names the field a caller can fix.
type ValidationError struct {
	Field  string
	Reason string
}

func (e *ValidationError) Error() string { return e.Field + " " + e.Reason }

type Feedback struct {
	Topic   string
	Email   string
	Message string
}

type Click struct {
	Target    string
	Referer   string
	UserAgent string
	Country   string
}

func contains(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

// NormalizeFeedback trims the input and rejects what the form would reject.
func NormalizeFeedback(feedback Feedback) (Feedback, error) {
	feedback.Email = strings.TrimSpace(feedback.Email)
	feedback.Message = strings.TrimSpace(feedback.Message)

	if !contains(Topics, feedback.Topic) {
		return Feedback{}, &ValidationError{"topic", "must be one of the listed topics"}
	}
	if feedback.Message == "" {
		return Feedback{}, &ValidationError{"message", "is required"}
	}
	if utf8.RuneCountInString(feedback.Message) > MaxMessage {
		return Feedback{}, &ValidationError{"message", "is too long"}
	}
	if feedback.Email != "" {
		address, err := mail.ParseAddress(feedback.Email)
		if len(feedback.Email) > maxEmail || err != nil || address.Address != feedback.Email {
			return Feedback{}, &ValidationError{"email", "must be an email address"}
		}
	}
	return feedback, nil
}

// NormalizeClick rejects an unknown target and shortens the free-text fields
// the way the site always has.
func NormalizeClick(click Click) (Click, error) {
	if !contains(Targets, click.Target) {
		return Click{}, &ValidationError{"target", "must be play or ios"}
	}
	click.Referer = truncate(click.Referer, maxHeaderCap)
	click.UserAgent = truncate(click.UserAgent, maxHeaderCap)
	click.Country = truncate(click.Country, maxCountry)
	return click, nil
}

func truncate(value string, max int) string {
	if utf8.RuneCountInString(value) <= max {
		return value
	}
	return string([]rune(value)[:max])
}

func nullable(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

type Service struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool}
}

// SubmitFeedback stores one message from a signed-in user and returns its id.
func (s *Service) SubmitFeedback(ctx context.Context, userID string, feedback Feedback) (string, error) {
	normalized, err := NormalizeFeedback(feedback)
	if err != nil {
		return "", err
	}
	var id string
	if err := s.pool.QueryRow(ctx, `
		INSERT INTO reelpin.feedback (user_id, topic, email, message)
		VALUES ($1, $2, $3, $4)
		RETURNING id::text`,
		userID, normalized.Topic, nullable(normalized.Email), normalized.Message,
	).Scan(&id); err != nil {
		return "", fmt.Errorf("saving feedback: %w", err)
	}
	return id, nil
}

// RecordClick counts one store-link tap.
func (s *Service) RecordClick(ctx context.Context, click Click) error {
	normalized, err := NormalizeClick(click)
	if err != nil {
		return err
	}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO reelpin.link_clicks (target, referer, user_agent, country)
		VALUES ($1, $2, $3, $4)`,
		normalized.Target, nullable(normalized.Referer), nullable(normalized.UserAgent), nullable(normalized.Country),
	); err != nil {
		return fmt.Errorf("saving a link click: %w", err)
	}
	return nil
}
