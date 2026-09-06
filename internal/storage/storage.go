// Package storage puts thumbnails in Supabase Storage. Keys are derived from
// the content identity, so re-processing overwrites one object instead of
// leaving a trail, and two users sharing a post share one file.
package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"strings"
	"time"
)

// Uploader is the seam the pipeline depends on.
type Uploader interface {
	Upload(ctx context.Context, key string, body io.Reader, contentType string) (string, error)
}

// Key is the deterministic object key for one content's thumbnail.
func Key(platform, contentType, contentID, normalizedURL, extension string) string {
	identity := contentID
	if strings.TrimSpace(identity) == "" {
		sum := sha256.Sum256([]byte(normalizedURL))
		identity = hex.EncodeToString(sum[:])[:32]
	}
	if extension == "" {
		extension = ".jpg"
	}
	return fmt.Sprintf("%s/%s/%s%s", safe(platform), safe(contentType), safe(identity), extension)
}

// Supabase uploads through the Storage REST API with the service key. It is a
// trusted endpoint, so it uses a plain client with a timeout.
type Supabase struct {
	baseURL    string
	bucket     string
	serviceKey string
	client     *http.Client
}

func NewSupabase(baseURL, bucket, serviceKey string, timeout time.Duration) *Supabase {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &Supabase{
		baseURL:    strings.TrimSuffix(strings.TrimSpace(baseURL), "/"),
		bucket:     bucket,
		serviceKey: serviceKey,
		client:     &http.Client{Timeout: timeout},
	}
}

func (s *Supabase) Upload(ctx context.Context, key string, body io.Reader, contentType string) (string, error) {
	if s.baseURL == "" || s.serviceKey == "" {
		return "", fmt.Errorf("supabase storage is not configured")
	}
	if contentType == "" {
		contentType = mime.TypeByExtension(filepath.Ext(key))
		if contentType == "" {
			contentType = "application/octet-stream"
		}
	}

	payload, err := io.ReadAll(io.LimitReader(body, MaxThumbnailBytes+1))
	if err != nil {
		return "", fmt.Errorf("reading the upload body: %w", err)
	}
	if len(payload) > MaxThumbnailBytes {
		return "", fmt.Errorf("the thumbnail is larger than %d bytes", MaxThumbnailBytes)
	}

	endpoint := fmt.Sprintf("%s/storage/v1/object/%s/%s", s.baseURL, s.bucket, key)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("building the upload request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+s.serviceKey)
	request.Header.Set("Content-Type", contentType)
	// Re-processing the same content replaces its object rather than failing.
	request.Header.Set("x-upsert", "true")

	response, err := s.client.Do(request)
	if err != nil {
		return "", fmt.Errorf("uploading the thumbnail: %w", err)
	}
	defer response.Body.Close()
	io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		// The response body can echo the request; only the status is logged.
		return "", fmt.Errorf("the storage service returned HTTP %d", response.StatusCode)
	}

	return fmt.Sprintf("%s/storage/v1/object/public/%s/%s", s.baseURL, s.bucket, key), nil
}

// MaxThumbnailBytes bounds what is accepted from a platform's CDN.
const MaxThumbnailBytes = 5 << 20

func safe(value string) string {
	cleaned := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '-'
		}
	}, strings.TrimSpace(value))
	if cleaned == "" {
		return "unknown"
	}
	if len(cleaned) > 64 {
		cleaned = cleaned[:64]
	}
	return cleaned
}
