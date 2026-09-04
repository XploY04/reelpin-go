package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/XploY04/reelpin-go/internal/httpapi"
)

type okPinger struct{}

func (okPinger) Ping(context.Context) error { return nil }

func TestScaffoldReelRoutesAreGone(t *testing.T) {
	handler := httpapi.New(okPinger{}, slog.New(slog.NewJSONHandler(io.Discard, nil)), "test").Routes()

	tests := []struct {
		method string
		path   string
	}{
		{"POST", "/api/v1/reels"},
		{"GET", "/api/v1/reels"},
		{"GET", "/api/v1/reels/123"},
	}

	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(tt.method, tt.path, nil))
			if rec.Code != http.StatusNotFound {
				t.Fatalf("got status %d, want %d", rec.Code, http.StatusNotFound)
			}
		})
	}
}
