// Package platformtest holds the fakes every platform handler's tests need.
// One copy, because the question they all ask is the same: did the preview
// image reach our own storage instead of staying a URL on the source's CDN.
package platformtest

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// StoredPrefix is what a stored thumbnail URL starts with. It stands in for
// the public bucket the reader renders from, so a test can tell a stored URL
// from a third-party one by looking at it.
const StoredPrefix = "https://storage.example/"

// Uploader stands in for object storage and records what it was handed.
type Uploader struct {
	Uploads int
	LastKey string
	// Err makes every upload fail. That is how a test proves a failed upload
	// costs the thumbnail and not the run.
	Err error
}

func (u *Uploader) Upload(_ context.Context, key string, _ io.Reader, _ string) (string, error) {
	u.Uploads++
	u.LastKey = key
	if u.Err != nil {
		return "", u.Err
	}
	return StoredPrefix + key, nil
}

// Site serves one page at the root with cdnHost rewritten to the test server,
// and image bytes on every other path. The rewrite is the point: a fixture
// that kept its real CDN host would send the thumbnail fetch to the internet.
func Site(t *testing.T, body, cdnHost string) *httptest.Server {
	t.Helper()
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			io.WriteString(w, strings.ReplaceAll(body, cdnHost, server.URL))
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		io.WriteString(w, "image-bytes")
	}))
	t.Cleanup(server.Close)
	return server
}
