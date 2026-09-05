// Package collections owns shared collections: who may read one, who may add
// to it, and how a share link or an invite grants that.
//
// Two capabilities live here and are treated as secrets: the view-link token
// and the invite token. Only their hashes are stored, exactly like a share
// token, so a database leak cannot be replayed as access.
package collections

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/XploY04/reelpin-go/internal/reels"
)

// ErrNotFound is a collection that does not exist, and equally one this user
// may not see. The two must be indistinguishable, or the API becomes a way to
// probe for other people's collections.
var ErrNotFound = errors.New("collection not found")

// ErrForbidden is a member who may read but not change.
var ErrForbidden = errors.New("this collection is read-only for you")

// ErrInviteInvalid covers an unknown, revoked, expired or exhausted invite.
var ErrInviteInvalid = errors.New("this invite is no longer valid")

// Roles, in order of privilege.
const (
	RoleOwner  = "owner"
	RoleEditor = "editor"
	RoleViewer = "viewer"
)

// InviteExpiry is how long an invite stays usable.
const InviteExpiry = 14 * 24 * time.Hour

// MaxReelIDsPerRequest bounds a bulk add.
const MaxReelIDsPerRequest = 100

type Collection struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Description string  `json:"description"`
	CoverReelID *string `json:"cover_reel_id"`
	Visibility  string  `json:"visibility"`
	Role        string  `json:"role"`
	ItemCount   int     `json:"item_count"`
	MemberCount int     `json:"member_count"`
	CreatedAt   *string `json:"created_at"`
	UpdatedAt   *string `json:"updated_at"`

	OwnerID string `json:"-"`
}

type Member struct {
	UserID      string  `json:"user_id"`
	Role        string  `json:"role"`
	CreatedAt   *string `json:"created_at"`
	DisplayName *string `json:"display_name"`
}

type Detail struct {
	Collection Collection          `json:"collection"`
	Reels      []reels.DisplayReel `json:"reels"`
	Pagination Pagination          `json:"pagination"`
	CanEdit    bool                `json:"can_edit"`
	OwnerName  *string             `json:"owner_name"`
}

type Shared struct {
	Collection Collection          `json:"collection"`
	Reels      []reels.DisplayReel `json:"reels"`
	Pagination Pagination          `json:"pagination"`
	OwnerName  *string             `json:"owner_name"`
}

type Pagination struct {
	NextCursor *string `json:"next_cursor"`
	NextOffset *int    `json:"next_offset"`
	HasMore    bool    `json:"has_more"`
	TotalCount int     `json:"total_count"`
	Limit      int     `json:"limit"`
	Offset     int     `json:"offset"`
}

// CanEdit reports whether a role may change a collection's contents.
func CanEdit(role string) bool { return role == RoleOwner || role == RoleEditor }

// hashToken is how every capability is stored. It matches the share-token
// hashing, so there is one rule for secrets in this service.
func hashToken(raw string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(raw)))
	return hex.EncodeToString(sum[:])
}

// newToken mints an unguessable capability.
func newToken() (string, error) {
	buffer := make([]byte, 32)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buffer), nil
}

// Store is the persistence seam. Everything above it works in terms of roles.
type Store interface {
	List(ctx context.Context, userID string) ([]Collection, error)
	Get(ctx context.Context, collectionID string) (Collection, error)
	Role(ctx context.Context, collectionID, userID string) (string, error)
}
