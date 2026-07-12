package main

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"

	"github.com/XploY04/reelpin-go/internal/store"
)

type healthResponse struct {
	Status string `json:"status"`
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

func main() {

	reels := store.New()

	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/v1/health/live", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, healthResponse{Status: "ok"})
	})

	mux.HandleFunc("GET /api/v1/health/ready", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, healthResponse{Status: "ready"})
	})

	mux.HandleFunc("POST /api/v1/reels", func(w http.ResponseWriter, r *http.Request) {
		var in store.Reel
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
			return
		}
		created, err := reels.Create(in)
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
	})

	mux.HandleFunc("GET /api/v1/reels", func(w http.ResponseWriter, r *http.Request) {
		responseArray := reels.List()
		writeJSON(w, http.StatusOK, responseArray)
	})

	mux.HandleFunc("GET /api/v1/reels/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		reel, err := reels.Get(id)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "Reel not found !"})
				return
			}
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Internal Error"})
			return
		}
		writeJSON(w, http.StatusOK, reel)
	})

	log.Println("listening on port: 8000")
	log.Fatal(http.ListenAndServe(":8000", mux))
}
