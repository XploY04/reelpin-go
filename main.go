package main

import (
	"log"
	"net/http"
)

func main () {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/v1/health/live", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok"}`))
	})

	mux.HandleFunc("GET /api/v1/health/ready", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ready"}`))
	})

	log.Println("listening on port: 8000")
	log.Fatal(http.ListenAndServe(":8000", mux))
}