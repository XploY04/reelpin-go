// Package profiles owns the one row of display details per user. The user id
// is the profile's id, and it always comes from the verified token.
package profiles

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNotFound = errors.New("profile not found")

const (
	maxEmail     = 254
	maxFullName  = 200
	maxAvatarURL = 2048
)

// ValidationError names the field a caller can fix.
type ValidationError struct {
	Field  string
	Reason string
}

func (e *ValidationError) Error() string { return e.Field + " " + e.Reason }

type Profile struct {
	ID        string    `json:"id"`
	Email     *string   `json:"email"`
	FullName  *string   `json:"full_name"`
	AvatarURL *string   `json:"avatar_url"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Params are the fields a client may set. A nil or blank field leaves the
// stored value alone, which is how the app has always called it: it sends only
// what it has.
type Params struct {
	Email     *string
	FullName  *string
	AvatarURL *string
}

// Normalize trims every field, drops blank ones, and rejects what would not
// be stored sensibly.
func Normalize(params Params) (Params, error) {
	var out Params
	var err error
	if out.Email, err = trimmed(params.Email, "email", maxEmail); err != nil {
		return Params{}, err
	}
	if out.Email != nil {
		if address, parseErr := mail.ParseAddress(*out.Email); parseErr != nil || address.Address != *out.Email {
			return Params{}, &ValidationError{"email", "must be an email address"}
		}
	}
	if out.FullName, err = trimmed(params.FullName, "full_name", maxFullName); err != nil {
		return Params{}, err
	}
	if out.AvatarURL, err = trimmed(params.AvatarURL, "avatar_url", maxAvatarURL); err != nil {
		return Params{}, err
	}
	if out.AvatarURL != nil {
		parsed, parseErr := url.Parse(*out.AvatarURL)
		if parseErr != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" {
			return Params{}, &ValidationError{"avatar_url", "must be an http or https URL"}
		}
	}
	return out, nil
}

func trimmed(value *string, field string, max int) (*string, error) {
	if value == nil {
		return nil, nil
	}
	text := strings.TrimSpace(*value)
	if text == "" {
		return nil, nil
	}
	if utf8.RuneCountInString(text) > max {
		return nil, &ValidationError{field, "is too long"}
	}
	return &text, nil
}

type Service struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool}
}

const profileColumns = `user_id::text, email, full_name, avatar_url, updated_at`

func (s *Service) Get(ctx context.Context, userID string) (Profile, error) {
	var profile Profile
	err := s.pool.QueryRow(ctx,
		`SELECT `+profileColumns+` FROM reelpin.profiles WHERE user_id = $1`, userID,
	).Scan(&profile.ID, &profile.Email, &profile.FullName, &profile.AvatarURL, &profile.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Profile{}, ErrNotFound
	}
	if err != nil {
		return Profile{}, fmt.Errorf("reading the profile: %w", err)
	}
	return profile, nil
}

// Upsert creates the profile or updates the fields params carries.
func (s *Service) Upsert(ctx context.Context, userID string, params Params) (Profile, error) {
	normalized, err := Normalize(params)
	if err != nil {
		return Profile{}, err
	}

	var profile Profile
	err = s.pool.QueryRow(ctx, `
		INSERT INTO reelpin.profiles (user_id, email, full_name, avatar_url)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (user_id) DO UPDATE SET
			email      = COALESCE(EXCLUDED.email, reelpin.profiles.email),
			full_name  = COALESCE(EXCLUDED.full_name, reelpin.profiles.full_name),
			avatar_url = COALESCE(EXCLUDED.avatar_url, reelpin.profiles.avatar_url),
			updated_at = now()
		RETURNING `+profileColumns,
		userID, normalized.Email, normalized.FullName, normalized.AvatarURL,
	).Scan(&profile.ID, &profile.Email, &profile.FullName, &profile.AvatarURL, &profile.UpdatedAt)
	if err != nil {
		return Profile{}, fmt.Errorf("saving the profile: %w", err)
	}
	return profile, nil
}
