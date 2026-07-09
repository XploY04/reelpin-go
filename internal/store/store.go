package store

import (
	"strconv"
	"sync"
)

type Reel struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	URL   string `json:"url"`
}

type ReelStore struct {
	mu sync.Mutex
	reels map[string]Reel
	nextID int
}

func New() *ReelStore {
	return &ReelStore{
		reels: map[string]Reel{},
		nextID: 1,
	}
}

func (s *ReelStore) Create(in Reel) Reel {
	s.mu.Lock()
	defer s.mu.Unlock()
	in.ID = strconv.Itoa(s.nextID)
	s.nextID++
	s.reels[in.ID] = in
	return in
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

func (s *ReelStore) Get(id string) (Reel, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	reel, ok := s.reels[id]
	return reel, ok
}
