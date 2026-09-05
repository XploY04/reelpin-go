package httpapi

import "net/http"

// The v2 surface is declared in full so a client can be generated against it
// before every operation is built. Each operation below answers a stable 503
// until its own task lands, which is a contract a client can code against: the
// alternative is a 404 that is indistinguishable from a typo, or a spec that
// changes shape every time a task merges.
//
// Replacing one of these is the first commit of its owning task, not a
// separate cleanup.
func notImplemented(capability string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusServiceUnavailable, errorBody{
			Code:      "processing_unavailable",
			Message:   "This capability is not available on this deployment yet.",
			Retryable: true,
			Details:   map[string]any{"capability": capability},
		})
	}
}

// Task 8 replaces these three.
func (s *Server) handleSubmitReel(w http.ResponseWriter, r *http.Request) {
	notImplemented("submit_reel")(w, r)
}

func (s *Server) handleNativeShare(w http.ResponseWriter, r *http.Request) {
	notImplemented("native_share")(w, r)
}

func (s *Server) handleResolveShare(w http.ResponseWriter, r *http.Request) {
	notImplemented("share_resolve")(w, r)
}

// Task 8 replaces these two, which mint and revoke the token the native
// extensions present.
func (s *Server) handleMintShareToken(w http.ResponseWriter, r *http.Request) {
	notImplemented("share_token_mint")(w, r)
}

func (s *Server) handleRevokeShareTokens(w http.ResponseWriter, r *http.Request) {
	notImplemented("share_token_revoke")(w, r)
}
