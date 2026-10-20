package httpapi

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/XploY04/reelpin-go/internal/collections"
	"github.com/XploY04/reelpin-go/internal/reels"
)

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
	URL       string `json:"url"`
	Token     string `json:"token"`
	ExpiresAt string `json:"expires_at"`
}

type collectionInviteResponse struct {
	URL       string `json:"url"`
	Token     string `json:"token"`
	Role      string `json:"role"`
	ExpiresAt string `json:"expires_at"`
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
		notFoundError(w, "collection_not_found")
	case errors.Is(err, reels.ErrNotFound):
		notFoundError(w, "reel_not_found")
	case errors.Is(err, collections.ErrForbidden):
		writeError(w, http.StatusForbidden, errorBody{
			Code:    "collection_forbidden",
			Message: "You do not have permission to change this collection.",
		})
	case errors.Is(err, collections.ErrInviteInvalid):
		writeError(w, http.StatusBadRequest, errorBody{
			Code:    "collection_invite_invalid",
			Message: "This invite is no longer valid.",
		})
	default:
		s.deps.Logger.Error(code, "route", r.Pattern, "error", err)
		internalError(w, code, message)
	}
}

func (s *Server) handleListCollections(w http.ResponseWriter, r *http.Request) {
	list, err := s.collections.List(r.Context(), requestUserID(r))
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
		validationError(w, "name", "a collection name is required")
		return
	}

	collection, added, err := s.collections.Create(r.Context(), requestUserID(r),
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
	params, ok := itemPageParams(w, r)
	if !ok {
		return
	}

	detail, err := s.collections.Detail(r.Context(), id.String(), requestUserID(r), params)
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

	collection, err := s.collections.Update(r.Context(), id.String(), requestUserID(r),
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
	if err := s.collections.Delete(r.Context(), id.String(), requestUserID(r)); err != nil {
		s.writeCollectionError(w, r, err, "collection_delete_failed", "Could not delete the collection right now.")
		return
	}
	writeJSON(w, http.StatusOK, okResponse{OK: true})
}

// handleSharedCollection serves the one public-share route: the unguessable
// token in the path is the whole authorization.
func (s *Server) handleSharedCollection(w http.ResponseWriter, r *http.Request) {
	params, ok := itemPageParams(w, r)
	if !ok {
		return
	}

	shared, err := s.collections.SharedByToken(r.Context(), r.PathValue("token"), params)
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
	if len(input.ReelIDs) == 0 {
		validationError(w, "reel_ids", "at least one reel id is required")
		return
	}

	collection, added, err := s.collections.AddItems(r.Context(), id.String(), requestUserID(r), input.ReelIDs)
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
	saveID, ok := pathUUID(w, r, "reel_id", "reel_not_found")
	if !ok {
		return
	}

	if err := s.collections.RemoveItem(r.Context(), id.String(), requestUserID(r), saveID.String()); err != nil {
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

	link, err := s.collections.EnableLink(r.Context(), id.String(), requestUserID(r))
	if err != nil {
		s.writeCollectionError(w, r, err, "collection_link_failed", "Could not create a share link right now.")
		return
	}
	// The raw token exists in this response and nowhere else.
	writeJSON(w, http.StatusOK, collectionLinkResponse{
		URL: link.URL, Token: link.Token, ExpiresAt: link.ExpiresAt.UTC().Format(time.RFC3339Nano),
	})
}

func (s *Server) handleDisableCollectionLink(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "collection_id", "collection_not_found")
	if !ok {
		return
	}
	if err := s.collections.DisableLink(r.Context(), id.String(), requestUserID(r)); err != nil {
		s.writeCollectionError(w, r, err, "collection_link_failed", "Could not update the share link right now.")
		return
	}
	writeJSON(w, http.StatusOK, okResponse{OK: true})
}

func (s *Server) handleCollectionMembers(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "collection_id", "collection_not_found")
	if !ok {
		return
	}

	ownerID, members, err := s.collections.Members(r.Context(), id.String(), requestUserID(r))
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
	memberID, ok := pathUUID(w, r, "member_user_id", "collection_not_found")
	if !ok {
		return
	}
	if err := s.collections.RemoveMember(r.Context(), id.String(), requestUserID(r),
		memberID.String()); err != nil {
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
	if err := s.collections.Leave(r.Context(), id.String(), requestUserID(r)); err != nil {
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

	invite, err := s.collections.CreateInvite(r.Context(), id.String(), requestUserID(r), input.Role)
	if err != nil {
		s.writeCollectionError(w, r, err, "collection_invite_failed", "Could not create an invite right now.")
		return
	}
	writeJSON(w, http.StatusOK, collectionInviteResponse{
		URL: invite.URL, Token: invite.Token, Role: invite.Role,
		ExpiresAt: invite.ExpiresAt.UTC().Format(time.RFC3339Nano),
	})
}

func (s *Server) handleAcceptCollectionInvite(w http.ResponseWriter, r *http.Request) {
	collection, err := s.collections.AcceptInvite(r.Context(), r.PathValue("token"), requestUserID(r))
	if err != nil {
		s.writeCollectionError(w, r, err, "collection_invite_accept_failed", "Could not accept this invite right now.")
		return
	}
	writeJSON(w, http.StatusOK, collectionMutationResponse{Collection: collection})
}

// itemPageParams are the cursor, limit and view every item page accepts, the
// same opaque-cursor rule the reel list uses.
func itemPageParams(w http.ResponseWriter, r *http.Request) (collections.ItemParams, bool) {
	query := r.URL.Query()
	limit, ok := intParam(w, query, "limit", reels.DefaultPageSize, 1, reels.MaxPageSize)
	if !ok {
		return collections.ItemParams{}, false
	}
	after, ok := cursorParam(w, query)
	if !ok {
		return collections.ItemParams{}, false
	}
	view, ok := viewParam(w, query)
	if !ok {
		return collections.ItemParams{}, false
	}
	return collections.ItemParams{After: after, Limit: limit, View: view}, true
}
