package httpapi

import "net/http"

// shareTokenAuthenticated guards the one endpoint a native share extension
// calls. The extension runs outside the app process and cannot refresh a
// Supabase session, so it presents a long-lived token instead.
//
// The token is never a substitute for a session anywhere else: only this mode
// accepts it, and it resolves to exactly one user.
func (s *Server) shareTokenAuthenticated(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get("X-Share-Token")
		if token == "" {
			writeError(w, http.StatusUnauthorized, errorBody{
				Code:    "share_token_required",
				Message: "This endpoint requires a share token.",
			})
			return
		}

		// Task 8 exchanges the token for a user id. Until then the mode exists
		// so the contract and its tests are real, and the endpoint answers the
		// same 503 as the rest of the unbuilt surface.
		next(w, r)
	}
}
