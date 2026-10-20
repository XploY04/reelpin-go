// Package collections owns shared collections: who may read one, who may
// change it, and how a share link or an invite grants that.
//
// Two capabilities live here and are treated as secrets: the view-link token
// and the invite token. Both are minted from crypto/rand, returned exactly
// once, and stored only as SHA-256 hashes, so a database leak cannot be
// replayed as access. Revoking either means forgetting its hash.
package collections

import (
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

// ErrForbidden is a member who may read but not make this change.
var ErrForbidden = errors.New("this collection is read-only for you")

// ErrInviteInvalid covers an unknown, revoked, expired or exhausted invite.
// One error for all four: which one it was is exactly what a token guesser
// wants to learn.
var ErrInviteInvalid = errors.New("this invite is no longer valid")

// Roles, in order of privilege. The owner is never a member row, so ownership
// cannot be demoted by deleting one.
const (
	RoleOwner  = "owner"
	RoleEditor = "editor"
	RoleViewer = "viewer"
)

// InviteExpiry is how long an invite stays usable.
const InviteExpiry = 14 * 24 * time.Hour

// LinkExpiry is how long a share link stays usable. The owner re-mints an
// expired one, which is one tap; a link that lives forever is one paste away
// from being an accidental public archive.
const LinkExpiry = 30 * 24 * time.Hour

// MaxSaveIDsPerRequest bounds a bulk add.
const MaxSaveIDsPerRequest = 100

// Collection is the wire shape. The cover id is a save id, which is the public
// reel id, so the field keeps the name every client already uses.
type Collection struct {
	ID          string  `json:"id"`
	WebURL      string  `json:"web_url"`
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

// Item is one filed save as a full reel. A collaborator or a link viewer
// cannot call getReel for a save that is not theirs, so the item is that reel,
// built by the same reels.BuildReel from the same record getReel reads, plus
// when and by whom it was filed. Transcript is nil under the concise view,
// which leaves it out.
//
// SourcePlatform and Transcript shadow the reel's, and the shallower field
// wins in JSON: an item's platform has always been a non-null string, and the
// concise view leaves the transcript out.
type Item struct {
	reels.Reel
	ReelID         string  `json:"reel_id"`
	SourcePlatform string  `json:"source_platform"`
	Transcript     *string `json:"transcript,omitempty"`
	SavedAt        *string `json:"saved_at"`
	AddedAt        *string `json:"added_at"`
	AddedBy        *string `json:"added_by"`
}

// ItemRecord is one filed save as storage reads it. Reel.CreatedAt is when the
// save was made, the same as every other reel record.
type ItemRecord struct {
	Reel    reels.ReelRecord
	AddedAt *time.Time
	AddedBy *string
}

// ItemQuery pages a collection's items. Transcripts is false for the concise
// view, so a page that leaves the transcript out never reads it.
type ItemQuery struct {
	After       *reels.Cursor
	Limit       int
	Transcripts bool
}

// ItemPage is one keyset page of filed saves as storage reads them.
type ItemPage struct {
	Items []ItemRecord
	Page  Page
}

// Page is one cursor page of items.
type Page struct {
	NextCursor *string `json:"next_cursor"`
	HasMore    bool    `json:"has_more"`
	Limit      int     `json:"limit"`
}

// Detail is the signed-in view: the collection, one page of items, and whether
// this user may change it.
type Detail struct {
	Collection Collection `json:"collection"`
	Items      []Item     `json:"items"`
	Page       Page       `json:"page"`
	CanEdit    bool       `json:"can_edit"`
}

// Shared is the anonymous view a link token grants. It deliberately carries no
// owner id, no member count and no roles: the token grants a view of the
// saves, not of who can see them.
type Shared struct {
	Collection Collection `json:"collection"`
	Items      []Item     `json:"items"`
	Page       Page       `json:"page"`
}

type Member struct {
	UserID    string  `json:"user_id"`
	Role      string  `json:"role"`
	CreatedAt *string `json:"created_at"`
}

// CanEdit reports whether a role may change a collection's contents.
func CanEdit(role string) bool { return role == RoleOwner || role == RoleEditor }

// HashToken is how every capability is stored. One rule for secrets in this
// service: hash on write, compare hashes on read, never store the raw value.
func HashToken(raw string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(raw)))
	return hex.EncodeToString(sum[:])
}

// NewToken mints an unguessable capability.
func NewToken() (string, error) {
	buffer := make([]byte, 32)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buffer), nil
}

// ISOTime renders a nullable timestamp the way every v2 response does.
func ISOTime(value *time.Time) *string {
	if value == nil {
		return nil
	}
	formatted := value.UTC().Format(time.RFC3339Nano)
	return &formatted
}
