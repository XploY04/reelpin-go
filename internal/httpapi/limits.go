package httpapi

import (
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"

	"github.com/XploY04/reelpin-go/internal/auth"
	"github.com/XploY04/reelpin-go/internal/ratelimit"
)

// routeLimit is the pair of buckets a route spends: one for the signed-in user
// and one for the network address. FailOpen decides what an unreachable Redis
// means for this route.
type routeLimit struct {
	User     *ratelimit.Policy
	IP       *ratelimit.Policy
	FailOpen bool
}

// rateLimited spends the route's buckets before the handler runs. It sits
// inside the auth middleware, so the user bucket has a user to charge.
func (s *Server) rateLimited(limit routeLimit, next http.HandlerFunc) http.HandlerFunc {
	if limit.User == nil && limit.IP == nil {
		return next
	}

	return func(w http.ResponseWriter, r *http.Request) {
		if s.deps.Limiter == nil {
			// No limiter configured at all: a paid route still refuses.
			if limit.FailOpen {
				next(w, r)
				return
			}
			s.rejectUnavailable(w, r, errors.New("no rate limiter is configured"))
			return
		}

		subjects := []struct {
			policy  *ratelimit.Policy
			subject string
		}{}
		if limit.User != nil {
			if userID, ok := auth.UserID(r.Context()); ok {
				subjects = append(subjects, struct {
					policy  *ratelimit.Policy
					subject string
				}{limit.User, "user:" + userID})
			}
		}
		if limit.IP != nil {
			subjects = append(subjects, struct {
				policy  *ratelimit.Policy
				subject string
			}{limit.IP, "ip:" + clientIP(r, s.deps.TrustedProxies)})
		}

		for _, entry := range subjects {
			decision, err := s.deps.Limiter.Allow(r.Context(), *entry.policy, entry.subject)
			if err != nil {
				if limit.FailOpen {
					s.deps.Logger.Warn("rate limiter unavailable, allowing a read",
						"policy", entry.policy.Name, "path", r.URL.Path)
					continue
				}
				s.rejectUnavailable(w, r, err)
				return
			}
			if !decision.Allowed {
				w.Header().Set("Retry-After", strconv.Itoa(int(decision.RetryAfter.Seconds())))
				writeError(w, http.StatusTooManyRequests, errorResponse{
					ErrorCode: "rate_limited",
					Message:   "Too many requests. Please try again shortly.",
					Detail: "Rate limit exceeded for " + entry.policy.Name + ". Limit is " +
						strconv.Itoa(entry.policy.Requests) + " request(s) per " +
						strconv.Itoa(int(entry.policy.Window.Seconds())) + " second(s).",
					Retryable: true,
				})
				return
			}
		}

		next(w, r)
	}
}

// rejectUnavailable is the fail-closed answer: work that costs money or changes
// state does not run unmetered.
func (s *Server) rejectUnavailable(w http.ResponseWriter, r *http.Request, err error) {
	s.deps.Logger.Error("rate limiter unavailable, refusing", "path", r.URL.Path, "error", err)
	w.Header().Set("Retry-After", "30")
	writeError(w, http.StatusServiceUnavailable, errorResponse{
		ErrorCode: "rate_limit_unavailable",
		Message:   "This request could not be rate limited right now.",
		Detail:    "Rate limiting is temporarily unavailable",
		Retryable: true,
	})
}

// clientIP believes a forwarding header only when the connection itself came
// from a configured proxy. Otherwise any client could claim any address and
// spend someone else's bucket.
func clientIP(r *http.Request, trusted []netip.Prefix) string {
	remote := remoteAddr(r)
	if len(trusted) == 0 || !isTrusted(remote, trusted) {
		return remote.String()
	}

	// Walk right to left and take the first address the proxy chain did not
	// vouch for.
	forwarded := r.Header.Get("X-Forwarded-For")
	parts := strings.Split(forwarded, ",")
	for i := len(parts) - 1; i >= 0; i-- {
		candidate, err := netip.ParseAddr(strings.TrimSpace(parts[i]))
		if err != nil {
			continue
		}
		if isTrusted(candidate, trusted) {
			continue
		}
		return candidate.String()
	}
	return remote.String()
}

func remoteAddr(r *http.Request) netip.Addr {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	addr, err := netip.ParseAddr(strings.Trim(host, "[]"))
	if err != nil {
		return netip.Addr{}
	}
	return addr.Unmap()
}

func isTrusted(addr netip.Addr, trusted []netip.Prefix) bool {
	if !addr.IsValid() {
		return false
	}
	for _, prefix := range trusted {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}
