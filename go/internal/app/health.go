package app

import (
	"log"
	"net/http"

	"github.com/go-chi/chi/v5"
)

// NewHealthRouter serves public liveness on its own listener. It exposes no
// business routes or database details and is never wrapped in the API limiter.
func NewHealthRouter() http.Handler {
	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			next.ServeHTTP(w, r)
		})
	})
	router.Get("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if _, err := w.Write([]byte("ok")); err != nil {
			log.Print("write health response failed")
			panic(http.ErrAbortHandler)
		}
	})
	return router
}
