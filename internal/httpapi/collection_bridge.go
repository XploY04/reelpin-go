package httpapi

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/XploY04/reelpin-go/internal/collections"
	"github.com/XploY04/reelpin-go/internal/reels"
	"github.com/google/uuid"
)

type collectionBridgeInput struct {
	Operation     string            `json:"operation"`
	UserID        string            `json:"user_id"`
	CollectionID  string            `json:"collection_id"`
	CollectionIDs []string          `json:"collection_ids"`
	Token         string            `json:"token"`
	Name          *string           `json:"name"`
	Description   *string           `json:"description"`
	CoverReelID   *string           `json:"cover_reel_id"`
	ReelIDs       []string          `json:"reel_ids"`
	ReelID        string            `json:"reel_id"`
	MemberUserID  string            `json:"member_user_id"`
	Role          string            `json:"role"`
	Cursor        string            `json:"cursor"`
	Offset        int               `json:"offset"`
	Limit         int               `json:"limit"`
	Snapshots     []json.RawMessage `json:"snapshots"`
}

var collectionBridgeOperations = map[string]bool{
	"list": true, "create": true, "detail": true, "update": true, "delete": true, "add_items": true, "remove_item": true,
	"enable_link": true, "disable_link": true, "members": true, "remove_member": true, "leave": true,
	"create_invite": true, "accept_invite": true, "shared": true, "validate_targets": true, "file_items": true,
	"reel": true, "delete_reel": true, "delete_account": true,
}

func (s *Server) handleCollectionBridge(w http.ResponseWriter, r *http.Request) {
	expected := sha256.Sum256([]byte(s.deps.CollectionBridgeKey))
	supplied := sha256.Sum256([]byte(r.Header.Get("X-ReelPin-Collection-Key")))
	if subtle.ConstantTimeCompare(expected[:], supplied[:]) != 1 {
		writeError(w, http.StatusUnauthorized, errorBody{Code: "authentication_required", Message: "Service authentication is required."})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<20)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var input collectionBridgeInput
	if decoder.Decode(&input) != nil || decoder.Decode(new(any)) != io.EOF {
		validationError(w, "body", "the request body is not valid JSON")
		return
	}
	if !collectionBridgeOperations[input.Operation] {
		validationError(w, "operation", "unknown collection operation")
		return
	}
	if input.Operation != "shared" {
		actor, err := uuid.Parse(input.UserID)
		if err != nil || actor == uuid.Nil {
			validationError(w, "user_id", "a valid actor is required")
			return
		}
		input.UserID = actor.String()
	}
	for _, id := range append(append(append([]string{}, input.ReelIDs...), input.CollectionIDs...), input.CollectionID, input.ReelID, input.MemberUserID) {
		if id != "" {
			if parsed, err := uuid.Parse(id); err != nil || parsed == uuid.Nil {
				validationError(w, "id", "a valid identifier is required")
				return
			}
		}
	}
	if input.CoverReelID != nil && *input.CoverReelID != "" {
		if parsed, err := uuid.Parse(*input.CoverReelID); err != nil || parsed == uuid.Nil {
			validationError(w, "cover_reel_id", "a valid identifier is required")
			return
		}
	}
	if len(input.ReelIDs) > collections.MaxSaveIDsPerRequest || len(input.CollectionIDs) > 100 || input.Offset < 0 || input.Offset > 10000 || input.Limit < 0 || input.Limit > 100 {
		validationError(w, "body", "the request exceeds collection limits")
		return
	}
	needsCollection := map[string]bool{"detail": true, "update": true, "delete": true, "add_items": true, "remove_item": true, "enable_link": true, "disable_link": true, "members": true, "remove_member": true, "leave": true, "create_invite": true}
	if needsCollection[input.Operation] && input.CollectionID == "" {
		validationError(w, "collection_id", "a collection identifier is required")
		return
	}
	if (input.Operation == "reel" || input.Operation == "delete_reel" || input.Operation == "remove_item") && input.ReelID == "" {
		validationError(w, "reel_id", "a reel identifier is required")
		return
	}
	if input.Operation == "remove_member" && input.MemberUserID == "" {
		validationError(w, "member_user_id", "a member identifier is required")
		return
	}
	if (input.Operation == "shared" || input.Operation == "accept_invite") && strings.TrimSpace(input.Token) == "" {
		validationError(w, "token", "a capability token is required")
		return
	}
	if input.Operation == "update" && input.Name != nil && strings.TrimSpace(*input.Name) == "" {
		validationError(w, "name", "a collection name is required")
		return
	}
	for _, id := range append(append([]string{}, input.ReelIDs...), input.CollectionIDs...) {
		if id == "" {
			validationError(w, "id", "a valid identifier is required")
			return
		}
	}
	if input.Limit == 0 {
		input.Limit = 25
	}
	params := collections.ItemParams{Limit: input.Limit, View: reels.ViewDetailed, Offset: input.Offset}
	if input.Cursor != "" {
		cursor, err := reels.DecodeCursor(input.Cursor)
		if err != nil {
			validationError(w, "cursor", "the cursor is not valid")
			return
		}
		params.After = &cursor
		params.Offset = 0
	}
	snapshots, err := collections.ParseSnapshots(input.Snapshots, input.UserID, input.ReelIDs)
	if err != nil {
		validationError(w, "snapshots", "the snapshots do not match the requested saves and actor")
		return
	}
	readOnly := map[string]bool{"list": true, "detail": true, "shared": true, "members": true, "reel": true, "validate_targets": true}
	if !readOnly[input.Operation] && input.Operation != "delete_reel" && input.Operation != "delete_account" {
		if err = s.deps.CollectionBridge.EnsureBridgeUser(r.Context(), input.UserID); err != nil {
			s.writeBridgeError(w, r, err)
			return
		}
	}
	result, err := s.runCollectionBridge(r, input, params, snapshots)
	if err != nil {
		s.writeBridgeError(w, r, err)
		return
	}
	if s.deps.Cache != nil && input.UserID != "" && !readOnly[input.Operation] {
		if err = s.deps.Cache.InvalidateUser(r.Context(), input.UserID); err != nil {
			s.deps.Logger.Warn("collection bridge cache invalidation failed", "error", err)
		}
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) writeBridgeError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, collections.ErrBridgeDeleted) {
		s.deletionPending(w)
		return
	}
	if errors.Is(err, collections.ErrBridgeInput) {
		validationError(w, "body", "the imported save is not valid")
		return
	}
	s.writeCollectionError(w, r, err, "collection_bridge_failed", "Could not complete the collection operation right now.")
}

func (s *Server) runCollectionBridge(r *http.Request, input collectionBridgeInput, params collections.ItemParams, snapshots []collections.Snapshot) (any, error) {
	ctx := r.Context()
	store := s.deps.CollectionBridge
	text := func(value *string) string {
		if value == nil {
			return ""
		}
		return *value
	}
	switch input.Operation {
	case "list":
		list, err := s.collections.List(ctx, input.UserID)
		return collectionListResponse{Collections: list}, err
	case "create":
		if strings.TrimSpace(text(input.Name)) == "" {
			return nil, collections.ErrBridgeInput
		}
		collection, added, err := store.CreateImported(ctx, input.UserID, text(input.Name), text(input.Description), input.ReelIDs, snapshots)
		return collectionMutationResponse{Collection: collection, AddedCount: added}, err
	case "detail":
		return s.collections.Detail(ctx, input.CollectionID, input.UserID, params)
	case "shared":
		return s.collections.SharedByToken(ctx, input.Token, params)
	case "update":
		collection, err := s.collections.Update(ctx, input.CollectionID, input.UserID, input.Name, input.Description, input.CoverReelID)
		return collectionMutationResponse{Collection: collection}, err
	case "delete":
		err := s.collections.Delete(ctx, input.CollectionID, input.UserID)
		return okResponse{OK: err == nil}, err
	case "add_items":
		if len(input.ReelIDs) == 0 {
			return nil, collections.ErrBridgeInput
		}
		collection, added, err := store.AddImported(ctx, input.CollectionID, input.UserID, input.ReelIDs, snapshots)
		return collectionMutationResponse{Collection: collection, AddedCount: added}, err
	case "remove_item":
		err := s.collections.RemoveItem(ctx, input.CollectionID, input.UserID, input.ReelID)
		return okResponse{OK: err == nil}, err
	case "enable_link":
		link, err := s.collections.EnableLink(ctx, input.CollectionID, input.UserID)
		return collectionLinkResponse{URL: link.URL, Token: link.Token, ExpiresAt: link.ExpiresAt.Format("2006-01-02T15:04:05Z07:00")}, err
	case "disable_link":
		err := s.collections.DisableLink(ctx, input.CollectionID, input.UserID)
		return okResponse{OK: err == nil}, err
	case "members":
		owner, members, err := s.collections.Members(ctx, input.CollectionID, input.UserID)
		return collectionMembersResponse{OwnerID: owner, Members: members}, err
	case "remove_member":
		err := s.collections.RemoveMember(ctx, input.CollectionID, input.UserID, input.MemberUserID)
		return okResponse{OK: err == nil}, err
	case "leave":
		err := s.collections.Leave(ctx, input.CollectionID, input.UserID)
		return okResponse{OK: err == nil}, err
	case "create_invite":
		link, err := s.collections.CreateInvite(ctx, input.CollectionID, input.UserID, input.Role)
		return collectionInviteResponse{URL: link.URL, Token: link.Token, Role: link.Role, ExpiresAt: link.ExpiresAt.Format("2006-01-02T15:04:05Z07:00")}, err
	case "accept_invite":
		collection, joined, err := store.AcceptBridgeInvite(ctx, input.Token, input.UserID)
		return struct {
			Collection collections.Collection `json:"collection"`
			Joined     bool                   `json:"joined"`
		}{Collection: collection, Joined: joined}, err
	case "validate_targets":
		ids, err := store.EditableTargets(ctx, input.UserID, input.CollectionIDs)
		return map[string]any{"collection_ids": ids}, err
	case "file_items":
		ids, err := store.FileImported(ctx, input.UserID, input.CollectionIDs, input.ReelIDs, snapshots)
		return map[string]any{"collection_ids": ids}, err
	case "reel":
		record, err := store.VisibleReel(ctx, input.UserID, input.ReelID)
		return map[string]any{"reel": reels.BuildReel(record, reels.Display{Now: s.deps.Now(), WebBaseURL: s.deps.PublicWebBaseURL, View: reels.ViewDetailed})}, err
	case "delete_reel":
		err := store.DeleteImported(ctx, input.UserID, input.ReelID)
		return okResponse{OK: err == nil}, err
	case "delete_account":
		err := store.DeleteBridgeAccount(ctx, input.UserID)
		return okResponse{OK: err == nil}, err
	}
	return nil, collections.ErrBridgeInput
}
