package main

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"

	"github.com/XploY04/reelpin-go/internal/store"
)

type reelStore interface {
	Create(in store.Reel) (store.Reel, error)
	List() []store.Reel
	Get(id string) (store.Reel, error)
}

type server struct {
	reels reelStore
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

func (s *server) createReel(w http.ResponseWriter, r *http.Request) {
	var in store.Reel
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	created, err := s.reels.Create(in)
	if err != nil {
		var ve *store.ValidationError
		if errors.As(err, &ve) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": ve.Error()})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Internal Error"})
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (s *server) listReels(w http.ResponseWriter, r *http.Request) {
	responseArray := s.reels.List()
	writeJSON(w, http.StatusOK, responseArray)

}

func (s *server) getReel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	reel, err := s.reels.Get(id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "Reel not found !"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Internal Error"})
		return
	}
	writeJSON(w, http.StatusOK, reel)
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/health/live", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /api/v1/health/ready", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})
	mux.HandleFunc("POST /api/v1/reels", s.createReel)
	mux.HandleFunc("GET /api/v1/reels", s.listReels)
	mux.HandleFunc("GET /api/v1/reels/{id}", s.getReel)
	return mux
}

func main() {
	srv := &server{reels: store.New()}
	log.Println("listening on port: 8000")
	log.Fatal(http.ListenAndServe(":8000", srv.routes()))
}
