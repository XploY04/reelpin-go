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
	// requires names the Deps fields this route's handler reaches for, spelled
	// exactly as the struct spells them. New resolves every name against Deps
	// before it returns a server, so a route registered against something nil
	// fails startup instead of answering 500 for the life of the deployment.
	// A route that declares nothing is refused too: forgetting is the way this
	// goes wrong, so forgetting has to be the loud case.
	requires []string
}

// routeTable is the single source for registration and for the contract test.
// Adding an operation in one place and not the other is not possible.
func (s *Server) routeTable() []Route {
	// Every route logs, and every bearer route authenticates, so the two
	// helpers add those rather than each route repeating them.
	public := func(method, path, operation string, handler http.HandlerFunc, claim bool, requires ...string) Route {
		return Route{
			Method: method, Path: path, OperationID: operation,
			Auth: AuthPublic, handler: handler, claimMethods: claim,
			requires: append([]string{"Logger"}, requires...),
		}
	}
	bearer := func(method, path, operation string, handler http.HandlerFunc, claim bool, requires ...string) Route {
		return Route{
			Method: method, Path: path, OperationID: operation,
			Auth: AuthBearer, handler: s.authenticated(handler), claimMethods: claim,
			requires: append([]string{"Auth", "Logger"}, requires...),
		}
	}

	return []Route{
		public(http.MethodGet, "/api/v2/health/live", "getHealthLive", s.handleLive, true),
		public(http.MethodGet, "/api/v2/health/ready", "getHealthReady", s.handleReady, true, "DB"),

		bearer(http.MethodGet, "/api/v2/reels", "listReels", s.handleListReels, true, "Reels"),
		bearer(http.MethodGet, "/api/v2/reels/filters", "getReelFilters", s.handlePlatformFilters, false, "Reels"),
		bearer(http.MethodGet, "/api/v2/reels/category-filters", "getReelCategoryFilters", s.handleCategoryFilters, false, "Reels"),
		bearer(http.MethodGet, "/api/v2/reels/{reel_id}", "getReel", s.handleGetReel, true, "Reels"),

		bearer(http.MethodGet, "/api/v2/processing-jobs", "listProcessingJobs", s.handleListJobs, true, "Jobs"),
		bearer(http.MethodGet, "/api/v2/processing-jobs/{job_id}", "getProcessingJob", s.handleGetJob, true, "Jobs", "Reels"),

		bearer(http.MethodGet, "/api/v2/account/library-stats", "getLibraryStats", s.handleLibraryStats, true, "Reels"),

		bearer(http.MethodGet, "/api/v2/map/pins", "listMapPins", s.handleMapPins, true, "Map"),
		bearer(http.MethodGet, "/api/v2/map/nearby", "listNearbyPins", s.handleMapNearby, true, "Map"),
		bearer(http.MethodPost, "/api/v2/map/manual-pins", "createManualPin", s.handleCreateManualPin, true, "Map"),
		bearer(http.MethodDelete, "/api/v2/map/manual-pins/{pin_id}", "deleteManualPin", s.handleDeleteManualPin, true, "Map"),
		bearer(http.MethodPost, "/api/v2/map/locations/{location_id}/hidden", "setPinHidden", s.handleHidePin, true, "Map"),
		bearer(http.MethodPost, "/api/v2/search", "searchReels", s.handleSearch, true, "Search"),

		bearer(http.MethodPost, "/api/v2/device-push-tokens", "registerDeviceToken", s.handleRegisterPushToken, true, "Notifications"),
		bearer(http.MethodDelete, "/api/v2/device-push-tokens", "deleteDeviceToken", s.handleDeletePushToken, false, "Notifications"),
		bearer(http.MethodPost, "/api/v2/notifications/{notification_id}/opened", "markNotificationOpened", s.handleNotificationOpened, true, "Notifications"),
		bearer(http.MethodDelete, "/api/v2/reels/{reel_id}", "deleteReel", s.handleDeleteReel, false, "Lifecycle"),
		bearer(http.MethodDelete, "/api/v2/account", "deleteAccount", s.handleDeleteAccount, true, "Lifecycle"),

		// Declared now so a client can be generated against the whole surface.
		// Each returns a stable 503 until its own task lands; see
		// docs/decisions/0015-declare-the-whole-v2-surface.md.
		bearer(http.MethodPost, "/api/v2/processing-jobs/reels", "submitReel", s.handleSubmitReel, false, "Enqueue"),
		bearer(http.MethodPost, "/api/v2/share/resolve", "resolveShare", s.handleResolveShare, true, "Resolver"),
		bearer(http.MethodPost, "/api/v2/share-tokens", "mintShareToken", s.handleMintShareToken, false, "ShareTokens"),
		bearer(http.MethodDelete, "/api/v2/share-tokens", "revokeShareTokens", s.handleRevokeShareTokens, false, "ShareTokens"),
		{
			Method: http.MethodPost, Path: "/api/v2/native-shares/reels",
			OperationID: "submitNativeShare", Auth: AuthShareToken,
			handler: s.shareTokenAuthenticated(s.handleNativeShare), claimMethods: true,
			requires: []string{"Logger", "ShareTokens", "Enqueue"},
		},

		bearer(http.MethodGet, "/api/v2/collections", "listCollections", s.handleListCollections, false, "Collections"),
		bearer(http.MethodPost, "/api/v2/collections", "createCollection", s.handleCreateCollection, true, "Collections"),
		bearer(http.MethodGet, "/api/v2/collections/{collection_id}", "getCollection", s.handleCollectionDetail, false, "Collections"),
		bearer(http.MethodPatch, "/api/v2/collections/{collection_id}", "updateCollection", s.handleUpdateCollection, false, "Collections"),
		bearer(http.MethodDelete, "/api/v2/collections/{collection_id}", "deleteCollection", s.handleDeleteCollection, true, "Collections"),
		bearer(http.MethodPost, "/api/v2/collections/{collection_id}/items", "addCollectionItems", s.handleAddCollectionItems, true, "Collections"),
		bearer(http.MethodDelete, "/api/v2/collections/{collection_id}/items/{reel_id}", "removeCollectionItem", s.handleRemoveCollectionItem, true, "Collections"),
		bearer(http.MethodGet, "/api/v2/collections/{collection_id}/members", "listCollectionMembers", s.handleCollectionMembers, true, "Collections"),
		bearer(http.MethodDelete, "/api/v2/collections/{collection_id}/members/{member_user_id}", "removeCollectionMember", s.handleRemoveCollectionMember, true, "Collections"),
		bearer(http.MethodPost, "/api/v2/collections/{collection_id}/leave", "leaveCollection", s.handleLeaveCollection, true, "Collections"),
		bearer(http.MethodPost, "/api/v2/collections/{collection_id}/link", "enableCollectionLink", s.handleEnableCollectionLink, false, "Collections"),
		bearer(http.MethodDelete, "/api/v2/collections/{collection_id}/link", "disableCollectionLink", s.handleDisableCollectionLink, true, "Collections"),
		bearer(http.MethodPost, "/api/v2/collections/{collection_id}/invites", "createCollectionInvite", s.handleCreateCollectionInvite, true, "Collections"),
		bearer(http.MethodPost, "/api/v2/collection-invites/{token}/accept", "acceptCollectionInvite", s.handleAcceptCollectionInvite, true, "Collections"),
		{
			// The token in the path is the whole authorization: an unguessable
			// capability granting a read-only view of one collection.
			Method: http.MethodGet, Path: "/api/v2/shared-collections/{token}",
			OperationID: "getSharedCollection", Auth: AuthPublicShare,
			handler: s.handleSharedCollection, claimMethods: true,
			requires: []string{"Logger", "Collections"},
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
