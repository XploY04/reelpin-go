package httpapi

import "net/http"

// handleMetrics serves the Prometheus exposition format. It answers 404 rather
// than 503 when metrics are not configured, because a scrape target that does
// not exist should look like it does not exist.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if s.deps.Metrics == nil {
		notFound(w, r)
		return
	}
	if !s.requireAdminKey(w, r) {
		return
	}
	s.deps.Metrics.Handler().ServeHTTP(w, r)
}
