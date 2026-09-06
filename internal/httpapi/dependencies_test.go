package httpapi

import (
	"net/http"
	"strings"
	"testing"
)

func TestAFullyWiredServerBuilds(t *testing.T) {
	// This also proves every name in the route table resolves to a real field
	// of Deps: an unknown one is a construction failure, not a typo nobody
	// notices.
	if _, err := New(testDeps(&fakePinger{})); err != nil {
		t.Fatalf("New with complete deps: %v", err)
	}
}

// TestANilDependencyIsRefusedAtStartup is the whole point of the check. Both
// of these were registered against nil in production and answered 500 on every
// request, because a nil call is a panic and the recovery middleware turns a
// panic into a 500.
func TestANilDependencyIsRefusedAtStartup(t *testing.T) {
	tests := []struct {
		name    string
		clear   func(*Deps)
		mustSay []string
	}{
		{
			name:  "collections",
			clear: func(deps *Deps) { deps.Collections = nil },
			mustSay: []string{
				"Collections is nil",
				"GET /api/v2/collections",
				"POST /api/v2/collection-invites/{token}/accept",
				"GET /api/v2/shared-collections/{token}",
			},
		},
		{
			name:  "lifecycle",
			clear: func(deps *Deps) { deps.Lifecycle = nil },
			mustSay: []string{
				"Lifecycle is nil",
				"DELETE /api/v2/reels/{reel_id}",
				"DELETE /api/v2/account",
			},
		},
		{
			name:  "both at once",
			clear: func(deps *Deps) { deps.Collections, deps.Lifecycle = nil, nil },
			mustSay: []string{
				"Collections is nil",
				"Lifecycle is nil",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deps := testDeps(&fakePinger{})
			tt.clear(&deps)

			server, err := New(deps)
			if err == nil {
				t.Fatal("New built a server whose routes are registered against nil")
			}
			if server != nil {
				t.Error("New returned a server alongside the error")
			}
			for _, want := range tt.mustSay {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the error does not name %q:\n%v", want, err)
				}
			}
		})
	}
}

// TestARouteThatDeclaresNoDependencyIsRefused is the guard against drift: a
// route added to the table without saying what it needs is caught at
// construction, not on its first request.
func TestARouteThatDeclaresNoDependencyIsRefused(t *testing.T) {
	deps := testDeps(&fakePinger{})
	server := &Server{deps: deps}

	added := Route{
		Method: http.MethodGet, Path: "/api/v2/brand-new",
		OperationID: "brandNew", Auth: AuthBearer,
		handler: server.authenticated(func(http.ResponseWriter, *http.Request) {}),
	}

	err := checkDependencies(deps, append(server.routeTable(), added))
	if err == nil {
		t.Fatal("a route declaring no dependency was accepted")
	}
	if !strings.Contains(err.Error(), "GET /api/v2/brand-new") {
		t.Errorf("the error does not name the route:\n%v", err)
	}
}

// TestADependencyThatIsNotAFieldOfDepsIsRefused covers the way the names can
// go stale: Deps gains, loses or renames a field and a route still asks for
// the old one.
func TestADependencyThatIsNotAFieldOfDepsIsRefused(t *testing.T) {
	deps := testDeps(&fakePinger{})
	server := &Server{deps: deps}

	added := Route{
		Method: http.MethodGet, Path: "/api/v2/brand-new",
		OperationID: "brandNew", Auth: AuthBearer,
		handler:  server.authenticated(func(http.ResponseWriter, *http.Request) {}),
		requires: []string{"Colections"},
	}

	err := checkDependencies(deps, append(server.routeTable(), added))
	if err == nil {
		t.Fatal("a route asking for a field Deps does not have was accepted")
	}
	if !strings.Contains(err.Error(), "Colections is not a field of Deps") {
		t.Errorf("the error does not name the misspelling:\n%v", err)
	}
}

// TestAnOptionalDependencyIsNotRequired keeps the check honest: a limiter and
// a registry are nil in every setup without Redis or Prometheus, and no route
// may claim them.
func TestAnOptionalDependencyIsNotRequired(t *testing.T) {
	deps := testDeps(&fakePinger{})
	deps.Limiter, deps.Metrics, deps.Redis, deps.Workers = nil, nil, nil, nil

	if _, err := New(deps); err != nil {
		t.Fatalf("New refused deps whose optional parts are absent: %v", err)
	}
}
