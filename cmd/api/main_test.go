package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/XploY04/reelpin-go/internal/store"
)

func TestGetReel_NotFound(t *testing.T) {
	srv := &server{reels: store.New()} //empty store
	req := httptest.NewRequest("GET", "/api/v1/reels/999", nil)
	rec := httptest.NewRecorder()

	srv.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("got status %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestCreateReel_Succes(t *testing.T) {
	srv := &server{reels: store.New()}
	req := httptest.NewRequest("POST", "/api/v1/reels", strings.NewReader(`{"title": "Hello", "url": "https://ig.com/r/abc"}`))
	rec := httptest.NewRecorder()

	srv.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("got status %d, want %d", rec.Code, http.StatusCreated)
	}
}
