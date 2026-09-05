package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/XploY04/reelpin-go/internal/collections"
)

// Collections is everything the API needs from the collections service.
type Collections interface {
	List(ctx context.Context, userID string) ([]collections.Collection, error)
	Create(ctx context.Context, userID, name, description string, reelIDs []string) (collections.Collection, int, error)
	Get(ctx context.Context, collectionID, userID string) (collections.Collection, error)
	Update(ctx context.Context, collectionID, userID string, name, description, coverReelID *string) (collections.Collection, error)
	Delete(ctx context.Context, collectionID, userID string) error
	Detail(ctx context.Context, collectionID, userID string, offset, limit int, now time.Time) (collections.Detail, error)
	SharedByToken(ctx context.Context, token string, offset, limit int, now time.Time) (collections.Shared, error)
	AddItems(ctx context.Context, collectionID, userID string, reelIDs []string) (int, error)
	RemoveItem(ctx context.Context, collectionID, userID, reelID string) error
	EnableLink(ctx context.Context, collectionID, userID string) (string, string, error)
	DisableLink(ctx context.Context, collectionID, userID string) error
	Members(ctx context.Context, collectionID, userID string) (string, []collections.Member, error)
	RemoveMember(ctx context.Context, collectionID, userID, memberUserID string) error
	Leave(ctx context.Context, collectionID, userID string) error
	CreateInvite(ctx context.Context, collectionID, userID, role string) (string, string, string, time.Time, error)
	AcceptInvite(ctx context.Context, token, userID string) (collections.Collection, error)
}

type collectionListResponse struct {
	Collections []collections.Collection `json:"collections"`
}

type collectionMutationResponse struct {
	Collection collections.Collection `json:"collection"`
	AddedCount int                    `json:"added_count"`
}

type collectionMembersResponse struct {
	OwnerID string               `json:"owner_id"`
	Members []collections.Member `json:"members"`
}

type collectionLinkResponse struct {
	URL   string `json:"url"`
	Token string `json:"token"`
}

type collectionInviteResponse struct {
	URL       string  `json:"url"`
	Token     string  `json:"token"`
	Role      string  `json:"role"`
	ExpiresAt *string `json:"expires_at"`
}

type okResponse struct {
	OK bool `json:"ok"`
}

type collectionCreateInput struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	ReelIDs     []string `json:"reel_ids"`
}

type collectionUpdateInput struct {
	Name        *string `json:"name"`
	Description *string `json:"description"`
	CoverReelID *string `json:"cover_reel_id"`
}

type collectionItemsInput struct {
	ReelIDs []string `json:"reel_ids"`
}

type collectionInviteInput struct {
	Role string `json:"role"`
}

// writeCollectionError keeps a private collection private: no access and no
// such collection are the same 404, so the API cannot be used to probe.
func (s *Server) writeCollectionError(w http.ResponseWriter, r *http.Request, err error, code, message string) {
	switch {
	case errors.Is(err, collections.ErrNotFound):
		writeError(w, http.StatusNotFound, errorResponse{
			ErrorCode: "collection_not_found",
			Message:   "That collection was not found.",
			Detail:    "Collection not found",
		})
	case errors.Is(err, collections.ErrForbidden):
		writeError(w, http.StatusForbidden, errorResponse{
			ErrorCode: "collection_forbidden",
			Message:   "You do not have permission to change this collection.",
			Detail:    "Insufficient collection role",
		})
	case errors.Is(err, collections.ErrInviteInvalid):
		writeError(w, http.StatusBadRequest, errorResponse{
			ErrorCode: "collection_invite_invalid",
			Message:   "This invite is no longer valid.",
			Detail:    "Invite expired, revoked or already used",
		})
	default:
		s.deps.Logger.Error(code, "path", r.URL.Path, "error", err)
		internalError(w, code, message)
	}
}

func (s *Server) handleListCollections(w http.ResponseWriter, r *http.Request) {
	list, err := s.deps.Collections.List(r.Context(), requestUserID(r))
	if err != nil {
		s.writeCollectionError(w, r, err, "collection_list_failed", "Could not load collections right now.")
		return
	}
	writeJSON(w, http.StatusOK, collectionListResponse{Collections: list})
}

func (s *Server) handleCreateCollection(w http.ResponseWriter, r *http.Request) {
	var input collectionCreateInput
	if !decodeJSONBody(w, r, &input) {
		return
	}
	if strings.TrimSpace(input.Name) == "" {
		validationError(w, "a collection name is required")
		return
	}

	collection, added, err := s.deps.Collections.Create(r.Context(), requestUserID(r),
		input.Name, input.Description, input.ReelIDs)
	if err != nil {
		s.writeCollectionError(w, r, err, "collection_create_failed", "Could not create the collection right now.")
		return
	}
	writeJSON(w, http.StatusOK, collectionMutationResponse{Collection: collection, AddedCount: added})
}

func (s *Server) handleCollectionDetail(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "collection_id", "collection_not_found")
	if !ok {
		return
	}
	offset, limit, ok := pageParams(w, r)
	if !ok {
		return
	}

	detail, err := s.deps.Collections.Detail(r.Context(), id.String(), requestUserID(r), offset, limit, s.now())
	if err != nil {
		s.writeCollectionError(w, r, err, "collection_detail_failed", "Could not load the collection right now.")
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

func (s *Server) handleUpdateCollection(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "collection_id", "collection_not_found")
	if !ok {
		return
	}
	var input collectionUpdateInput
	if !decodeJSONBody(w, r, &input) {
		return
	}

	collection, err := s.deps.Collections.Update(r.Context(), id.String(), requestUserID(r),
		input.Name, input.Description, input.CoverReelID)
	if err != nil {
		s.writeCollectionError(w, r, err, "collection_update_failed", "Could not update the collection right now.")
		return
	}
	writeJSON(w, http.StatusOK, collectionMutationResponse{Collection: collection})
}

func (s *Server) handleDeleteCollection(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "collection_id", "collection_not_found")
	if !ok {
		return
	}
	if err := s.deps.Collections.Delete(r.Context(), id.String(), requestUserID(r)); err != nil {
		s.writeCollectionError(w, r, err, "collection_delete_failed", "Could not delete the collection right now.")
		return
	}
	writeJSON(w, http.StatusOK, okResponse{OK: true})
}

// handleSharedCollection is the only unauthenticated collection route: the
// unguessable token is the capability.
func (s *Server) handleSharedCollection(w http.ResponseWriter, r *http.Request) {
	offset, limit, ok := pageParams(w, r)
	if !ok {
		return
	}

	shared, err := s.deps.Collections.SharedByToken(r.Context(), r.PathValue("token"), offset, limit, s.now())
	if err != nil {
		s.writeCollectionError(w, r, err, "collection_detail_failed", "Could not load the collection right now.")
		return
	}
	writeJSON(w, http.StatusOK, shared)
}

func (s *Server) handleAddCollectionItems(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "collection_id", "collection_not_found")
	if !ok {
		return
	}
	var input collectionItemsInput
	if !decodeJSONBody(w, r, &input) {
		return
	}

	added, err := s.deps.Collections.AddItems(r.Context(), id.String(), requestUserID(r), input.ReelIDs)
	if err != nil {
		s.writeCollectionError(w, r, err, "collection_items_failed", "Could not update the collection right now.")
		return
	}
	collection, err := s.deps.Collections.Get(r.Context(), id.String(), requestUserID(r))
	if err != nil {
		s.writeCollectionError(w, r, err, "collection_items_failed", "Could not update the collection right now.")
		return
	}
	writeJSON(w, http.StatusOK, collectionMutationResponse{Collection: collection, AddedCount: added})
}

func (s *Server) handleRemoveCollectionItem(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "collection_id", "collection_not_found")
	if !ok {
		return
	}
	reelID, ok := pathUUID(w, r, "reel_id", "reel_not_found")
	if !ok {
		return
	}

	if err := s.deps.Collections.RemoveItem(r.Context(), id.String(), requestUserID(r), reelID.String()); err != nil {
		s.writeCollectionError(w, r, err, "collection_items_failed", "Could not update the collection right now.")
		return
	}
	writeJSON(w, http.StatusOK, okResponse{OK: true})
}

func (s *Server) handleEnableCollectionLink(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "collection_id", "collection_not_found")
	if !ok {
		return
	}

	url, token, err := s.deps.Collections.EnableLink(r.Context(), id.String(), requestUserID(r))
	if err != nil {
		s.writeCollectionError(w, r, err, "collection_link_failed", "Could not create a share link right now.")
		return
	}
	// The raw token exists in this response and nowhere else.
	writeJSON(w, http.StatusOK, collectionLinkResponse{URL: url, Token: token})
}

func (s *Server) handleDisableCollectionLink(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "collection_id", "collection_not_found")
	if !ok {
		return
	}
	if err := s.deps.Collections.DisableLink(r.Context(), id.String(), requestUserID(r)); err != nil {
		s.writeCollectionError(w, r, err, "collection_link_failed", "Could not update the share link right now.")
		return
	}
	writeJSON(w, http.StatusOK, okResponse{OK: true})
}

// handleCollectionSubresource routes the GET sub-resources of a collection.
// Only the ones named here exist; anything else is a 404 rather than a
// surprising fallthrough.
func (s *Server) handleCollectionSubresource(w http.ResponseWriter, r *http.Request) {
	switch r.PathValue("subresource") {
	case "members":
		s.handleCollectionMembers(w, r)
	default:
		notFound(w, r)
	}
}

func (s *Server) handleCollectionMembers(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "collection_id", "collection_not_found")
	if !ok {
		return
	}

	ownerID, members, err := s.deps.Collections.Members(r.Context(), id.String(), requestUserID(r))
	if err != nil {
		s.writeCollectionError(w, r, err, "collection_members_failed", "Could not load collaborators right now.")
		return
	}
	writeJSON(w, http.StatusOK, collectionMembersResponse{OwnerID: ownerID, Members: members})
}

func (s *Server) handleRemoveCollectionMember(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "collection_id", "collection_not_found")
	if !ok {
		return
	}
	if err := s.deps.Collections.RemoveMember(r.Context(), id.String(), requestUserID(r),
		r.PathValue("member_user_id")); err != nil {
		s.writeCollectionError(w, r, err, "collection_member_remove_failed", "Could not remove the collaborator right now.")
		return
	}
	writeJSON(w, http.StatusOK, okResponse{OK: true})
}

func (s *Server) handleLeaveCollection(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "collection_id", "collection_not_found")
	if !ok {
		return
	}
	if err := s.deps.Collections.Leave(r.Context(), id.String(), requestUserID(r)); err != nil {
		s.writeCollectionError(w, r, err, "collection_leave_failed", "Could not leave the collection right now.")
		return
	}
	writeJSON(w, http.StatusOK, okResponse{OK: true})
}

func (s *Server) handleCreateCollectionInvite(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "collection_id", "collection_not_found")
	if !ok {
		return
	}
	var input collectionInviteInput
	if !decodeJSONBody(w, r, &input) {
		return
	}

	url, token, role, expiresAt, err := s.deps.Collections.CreateInvite(r.Context(), id.String(),
		requestUserID(r), input.Role)
	if err != nil {
		s.writeCollectionError(w, r, err, "collection_invite_failed", "Could not create an invite right now.")
		return
	}

	expires := expiresAt.UTC().Format(time.RFC3339Nano)
	writeJSON(w, http.StatusOK, collectionInviteResponse{
		URL: url, Token: token, Role: role, ExpiresAt: &expires,
	})
}

func (s *Server) handleAcceptCollectionInvite(w http.ResponseWriter, r *http.Request) {
	collection, err := s.deps.Collections.AcceptInvite(r.Context(), r.PathValue("token"), requestUserID(r))
	if err != nil {
		s.writeCollectionError(w, r, err, "collection_invite_accept_failed", "Could not accept this invite right now.")
		return
	}
	writeJSON(w, http.StatusOK, collectionMutationResponse{Collection: collection})
}

// pageParams are the offset and limit every paginated collection view accepts.
func pageParams(w http.ResponseWriter, r *http.Request) (int, int, bool) {
	query := r.URL.Query()
	offset, ok := intParam(w, query, "offset", 0, 0, 1<<31-1)
	if !ok {
		return 0, 0, false
	}
	limit, ok := intParam(w, query, "limit", 25, 1, 100)
	if !ok {
		return 0, 0, false
	}
	return offset, limit, true
}
