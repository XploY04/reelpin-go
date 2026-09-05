package storage

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestKeyIsDeterministicAndDerivedFromTheIdentity(t *testing.T) {
	first := Key("instagram", "reel", "C8abc123", "https://www.instagram.com/reel/C8abc123/", ".jpg")
	if first != Key("instagram", "reel", "C8abc123", "https://www.instagram.com/reel/C8abc123/", ".jpg") {
		t.Fatal("the same content produced two keys, so re-processing would leave a trail")
	}
	if !strings.Contains(first, "C8abc123") {
		t.Errorf("key = %q, want the content id in it", first)
	}
	if Key("instagram", "reel", "C8xyz", "", ".jpg") == first {
		t.Error("two contents share a key")
	}

	// A link with no id of its own still gets a stable key.
	generic := Key("someblog.com", "link", "", "https://someblog.com/a", ".jpg")
	if generic != Key("someblog.com", "link", "", "https://someblog.com/a", ".jpg") {
		t.Fatal("a generic link's key is not stable")
	}
	if generic == Key("someblog.com", "link", "", "https://someblog.com/b", ".jpg") {
		t.Error("two links share a key")
	}
	// Path separators from an identity must not escape the prefix.
	if strings.Contains(Key("a/b", "c/d", "e/f", "", ".jpg"), "a/b") {
		t.Error("an identity's slashes reached the key")
	}
}

func TestUploadPostsWithUpsertAndReturnsThePublicURL(t *testing.T) {
	var gotAuth, gotUpsert, gotType string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotUpsert = r.Header.Get("x-upsert")
		gotType = r.Header.Get("Content-Type")
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"Key":"thumbnails/x"}`))
	}))
	defer server.Close()

	uploader := NewSupabase(server.URL, "thumbnails", "service-key", 0)
	url, err := uploader.Upload(context.Background(), "instagram/reel/C8abc123.jpg",
		strings.NewReader("image-bytes"), "image/jpeg")
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}

	if !strings.Contains(url, "/object/public/thumbnails/instagram/reel/C8abc123.jpg") {
		t.Errorf("url = %q", url)
	}
	if gotAuth != "Bearer service-key" || gotUpsert != "true" || gotType != "image/jpeg" {
		t.Errorf("headers = %q / %q / %q", gotAuth, gotUpsert, gotType)
	}
}

func TestUploadReportsOnlyTheStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		// A service can echo the request back; none of it may reach the error.
		w.Write([]byte(`{"error":"invalid key service-key-secret"}`))
	}))
	defer server.Close()

	_, err := NewSupabase(server.URL, "thumbnails", "service-key-secret", 0).
		Upload(context.Background(), "k.jpg", strings.NewReader("x"), "image/jpeg")
	if err == nil {
		t.Fatal("a rejected upload reported success")
	}
	if strings.Contains(err.Error(), "service-key-secret") {
		t.Fatalf("the error leaks the service key: %v", err)
	}
}

func TestUploadRefusesAnOversizedThumbnail(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	body := strings.NewReader(strings.Repeat("x", MaxThumbnailBytes+1))
	if _, err := NewSupabase(server.URL, "thumbnails", "key", 0).
		Upload(context.Background(), "k.jpg", body, "image/jpeg"); err == nil {
		t.Fatal("an oversized thumbnail was uploaded")
	}
}

func TestUploadWithoutConfigurationFails(t *testing.T) {
	if _, err := NewSupabase("", "thumbnails", "", 0).
		Upload(context.Background(), "k.jpg", strings.NewReader("x"), ""); err == nil {
		t.Fatal("an unconfigured uploader reported success")
	}
}
