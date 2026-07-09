package main

import (
	"encoding/json"
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
		if in.Title == "" || in.URL == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "title and url are empty."})
			return
		}
		created := reels.Create(in)
		writeJSON(w, http.StatusCreated, created)
	})

	mux.HandleFunc("GET /api/v1/reels", func(w http.ResponseWriter, r *http.Request) {
		responseArray := reels.List()
		writeJSON(w, http.StatusOK, responseArray)
	})

	mux.HandleFunc("GET /api/v1/reels/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		reel, ok := reels.Get(id)
		if ok {
			writeJSON(w, http.StatusOK, reel)
			return
		}
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Reel not found !"})
	})

	log.Println("listening on port: 8000")
	log.Fatal(http.ListenAndServe(":8000", mux))
}
