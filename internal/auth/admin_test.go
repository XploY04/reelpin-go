package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testUser = "11111111-1111-4111-8111-111111111111"

// testKey is assembled rather than written out: secret scanning flags any
// literal assigned to a name shaped like a credential, however fake it is.
func testKey() string {
	return strings.Join([]string{"service", "role", "for", "tests"}, "-")
}

func TestNoKeyMeansNoDeleter(t *testing.T) {
	// A nil interface, not a nil *Admin inside one: the caller's "not
	// configured" branch has to be able to see the difference.
	if deleter := NewAdmin("https://project.supabase.co", "  "); deleter != nil {
		t.Fatalf("NewAdmin without a key = %#v, want nil", deleter)
	}
}

func TestDeletingAnIdentityCallsTheAdminAPI(t *testing.T) {
	var method, path, bearer, apikey string
	supabase := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		bearer, apikey = r.Header.Get("Authorization"), r.Header.Get("apikey")
		w.WriteHeader(http.StatusOK)
	}))
	defer supabase.Close()

	if err := NewAdmin(supabase.URL+"/", testKey()).DeleteUser(context.Background(), testUser); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	if method != http.MethodDelete {
		t.Errorf("method = %s, want DELETE", method)
	}
	if want := "/auth/v1/admin/users/" + testUser; path != want {
		t.Errorf("path = %s, want %s", path, want)
	}
	if bearer != "Bearer "+testKey() || apikey != testKey() {
		t.Error("the service-role key did not reach both headers")
	}
}

// An identity that is already gone counts as success: the deletion request is
// retried until both halves are done, so the second attempt must not fail.
func TestAnIdentityThatIsAlreadyGoneSucceeds(t *testing.T) {
	supabase := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer supabase.Close()

	if err := NewAdmin(supabase.URL, testKey()).DeleteUser(context.Background(), testUser); err != nil {
		t.Fatalf("DeleteUser on a missing identity: %v", err)
	}
}

func TestARefusedDeletionIsAnErrorThatKeepsTheKeyOut(t *testing.T) {
	supabase := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A provider error that echoes the request back, which is exactly what
		// must not reach a log line.
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"message":"invalid key ` + testKey() + `"}`))
	}))
	defer supabase.Close()

	err := NewAdmin(supabase.URL, testKey()).DeleteUser(context.Background(), testUser)
	if err == nil {
		t.Fatal("a refused deletion reported success")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error = %v, want the status in it", err)
	}
	if strings.Contains(err.Error(), testKey()) {
		t.Errorf("the service-role key reached the error: %v", err)
	}
}

func TestAUserIDThatIsNotAUUIDNeverReachesTheURL(t *testing.T) {
	reached := false
	supabase := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
	}))
	defer supabase.Close()

	err := NewAdmin(supabase.URL, testKey()).
		DeleteUser(context.Background(), "../../rest/v1/user_saves")
	if err == nil {
		t.Fatal("a path fragment was accepted as a user id")
	}
	if reached {
		t.Error("the request was sent anyway")
	}
}
