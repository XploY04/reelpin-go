package store

import (
	"errors"
	"fmt"
	"strconv"
	"sync"
)

var ErrNotFound = errors.New("reel not found")

type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("validation failed on %s: %s", e.Field, e.Message)
}

type Reel struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	URL   string `json:"url"`
}

type ReelStore struct {
	mu     sync.Mutex
	reels  map[string]Reel
	nextID int
}

func New() *ReelStore {
	return &ReelStore{
		reels:  map[string]Reel{},
		nextID: 1,
	}
}

func (s *ReelStore) Create(in Reel) (Reel, error) {
	if in.Title == "" {
		return Reel{}, &ValidationError{Field: "title", Message: "is required"}
	}
	if in.URL == "" {
		return Reel{}, &ValidationError{Field: "url", Message: "is required"}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	in.ID = strconv.Itoa(s.nextID)
	s.nextID++
	s.reels[in.ID] = in
	return in, nil
}

func (s *ReelStore) List() []Reel {
	s.mu.Lock()
	defer s.mu.Unlock()
	reels := []Reel{}
	for _, reel := range s.reels {
		reels = append(reels, reel)
	}
	return reels
}

func (s *ReelStore) Get(id string) (Reel, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	reel, ok := s.reels[id]
	if !ok {
		return Reel{}, ErrNotFound
	}
	return reel, nil
}
