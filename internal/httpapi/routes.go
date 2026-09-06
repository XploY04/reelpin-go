package httpapi

import "net/http"

// AuthMode is how a route decides who is calling. Every route names one; there
// is no boolean shortcut, because "authenticated: false" cannot express the
// difference between a health check, a share token and a public share view.
type AuthMode string

const (
	// AuthPublic is open to anyone. Health only.
	AuthPublic AuthMode = "public"
	// AuthBearer is a verified Supabase access token.
	AuthBearer AuthMode = "bearer"
	// AuthShareToken is a long-lived native share token in X-Share-Token. The
	// native extensions hold one; a browser never does.
	AuthShareToken AuthMode = "share-token"
	// AuthPublicShare is an unguessable share link. The token in the path is
	// the authorization, and it grants read-only access to one resource.
	AuthPublicShare AuthMode = "public-share"
)

// Route is one operation. OperationID matches the OpenAPI operation exactly:
// the contract test walks both directions, so a route without an operation and
// an operation without a route are both failures.
type Route struct {
	Method      string   `json:"method"`
	Path        string   `json:"path"`
	OperationID string   `json:"operation_id"`
	Auth        AuthMode `json:"auth"`

	handler http.HandlerFunc
	// claimMethods registers the path again with no method so a wrong method
	// gets a JSON 405 instead of the mux's plain text. It is off for a literal
	// path already covered by a sibling wildcard, which the mux rejects as a
	// conflict.
	claimMethods bool
}

// routeTable is the single source for registration and for the contract test.
// Adding an operation in one place and not the other is not possible.
func (s *Server) routeTable() []Route {
	public := func(method, path, operation string, handler http.HandlerFunc, claim bool) Route {
		return Route{
			Method: method, Path: path, OperationID: operation,
			Auth: AuthPublic, handler: handler, claimMethods: claim,
		}
	}
	bearer := func(method, path, operation string, handler http.HandlerFunc, claim bool) Route {
		return Route{
			Method: method, Path: path, OperationID: operation,
			Auth: AuthBearer, handler: s.authenticated(handler), claimMethods: claim,
		}
	}

	return []Route{
		public(http.MethodGet, "/api/v2/health/live", "getHealthLive", s.handleLive, true),
		public(http.MethodGet, "/api/v2/health/ready", "getHealthReady", s.handleReady, true),

		bearer(http.MethodGet, "/api/v2/reels", "listReels", s.handleListReels, true),
		bearer(http.MethodGet, "/api/v2/reels/filters", "getReelFilters", s.handlePlatformFilters, false),
		bearer(http.MethodGet, "/api/v2/reels/category-filters", "getReelCategoryFilters", s.handleCategoryFilters, false),
		bearer(http.MethodGet, "/api/v2/reels/{reel_id}", "getReel", s.handleGetReel, true),

		bearer(http.MethodGet, "/api/v2/processing-jobs", "listProcessingJobs", s.handleListJobs, true),
		bearer(http.MethodGet, "/api/v2/processing-jobs/{job_id}", "getProcessingJob", s.handleGetJob, true),

		bearer(http.MethodGet, "/api/v2/account/library-stats", "getLibraryStats", s.handleLibraryStats, true),

		bearer(http.MethodGet, "/api/v2/map/pins", "listMapPins", s.handleMapPins, true),
		bearer(http.MethodGet, "/api/v2/map/nearby", "listNearbyPins", s.handleMapNearby, true),
		bearer(http.MethodPost, "/api/v2/map/manual-pins", "createManualPin", s.handleCreateManualPin, true),
		bearer(http.MethodDelete, "/api/v2/map/manual-pins/{pin_id}", "deleteManualPin", s.handleDeleteManualPin, true),
		bearer(http.MethodPost, "/api/v2/map/locations/{location_id}/hidden", "setPinHidden", s.handleHidePin, true),

		bearer(http.MethodPost, "/api/v2/device-push-tokens", "registerDeviceToken", s.handleRegisterPushToken, true),
		bearer(http.MethodDelete, "/api/v2/device-push-tokens", "deleteDeviceToken", s.handleDeletePushToken, false),
		bearer(http.MethodPost, "/api/v2/notifications/{notification_id}/opened", "markNotificationOpened", s.handleNotificationOpened, true),
		bearer(http.MethodDelete, "/api/v2/reels/{reel_id}", "deleteReel", s.handleDeleteReel, false),
		bearer(http.MethodDelete, "/api/v2/account", "deleteAccount", s.handleDeleteAccount, true),

		// Declared now so a client can be generated against the whole surface.
		// Each returns a stable 503 until its own task lands; see
		// docs/decisions/0015-declare-the-whole-v2-surface.md.
		bearer(http.MethodPost, "/api/v2/processing-jobs/reels", "submitReel", s.handleSubmitReel, false),
		bearer(http.MethodPost, "/api/v2/share/resolve", "resolveShare", s.handleResolveShare, true),
		bearer(http.MethodPost, "/api/v2/share-tokens", "mintShareToken", s.handleMintShareToken, false),
		bearer(http.MethodDelete, "/api/v2/share-tokens", "revokeShareTokens", s.handleRevokeShareTokens, false),
		{
			Method: http.MethodPost, Path: "/api/v2/native-shares/reels",
			OperationID: "submitNativeShare", Auth: AuthShareToken,
			handler: s.shareTokenAuthenticated(s.handleNativeShare), claimMethods: true,
		},

		bearer(http.MethodGet, "/api/v2/collections", "listCollections", s.handleListCollections, false),
		bearer(http.MethodPost, "/api/v2/collections", "createCollection", s.handleCreateCollection, true),
		bearer(http.MethodGet, "/api/v2/collections/{collection_id}", "getCollection", s.handleCollectionDetail, false),
		bearer(http.MethodPatch, "/api/v2/collections/{collection_id}", "updateCollection", s.handleUpdateCollection, false),
		bearer(http.MethodDelete, "/api/v2/collections/{collection_id}", "deleteCollection", s.handleDeleteCollection, true),
		bearer(http.MethodPost, "/api/v2/collections/{collection_id}/items", "addCollectionItems", s.handleAddCollectionItems, true),
		bearer(http.MethodDelete, "/api/v2/collections/{collection_id}/items/{reel_id}", "removeCollectionItem", s.handleRemoveCollectionItem, true),
		bearer(http.MethodGet, "/api/v2/collections/{collection_id}/members", "listCollectionMembers", s.handleCollectionMembers, true),
		bearer(http.MethodDelete, "/api/v2/collections/{collection_id}/members/{member_user_id}", "removeCollectionMember", s.handleRemoveCollectionMember, true),
		bearer(http.MethodPost, "/api/v2/collections/{collection_id}/leave", "leaveCollection", s.handleLeaveCollection, true),
		bearer(http.MethodPost, "/api/v2/collections/{collection_id}/link", "enableCollectionLink", s.handleEnableCollectionLink, false),
		bearer(http.MethodDelete, "/api/v2/collections/{collection_id}/link", "disableCollectionLink", s.handleDisableCollectionLink, true),
		bearer(http.MethodPost, "/api/v2/collections/{collection_id}/invites", "createCollectionInvite", s.handleCreateCollectionInvite, true),
		bearer(http.MethodPost, "/api/v2/collection-invites/{token}/accept", "acceptCollectionInvite", s.handleAcceptCollectionInvite, true),
		{
			// The token in the path is the whole authorization: an unguessable
			// capability granting a read-only view of one collection.
			Method: http.MethodGet, Path: "/api/v2/shared-collections/{token}",
			OperationID: "getSharedCollection", Auth: AuthPublicShare,
			handler: s.handleSharedCollection, claimMethods: true,
		},
	}
}

// RouteManifest is the table without its handlers, for the contract test and
// for anything that needs to reason about the surface.
func (s *Server) RouteManifest() []Route {
	table := s.routeTable()
	manifest := make([]Route, 0, len(table))
	for _, route := range table {
		manifest = append(manifest, Route{
			Method:      route.Method,
			Path:        route.Path,
			OperationID: route.OperationID,
			Auth:        route.Auth,
		})
	}
	return manifest
}
