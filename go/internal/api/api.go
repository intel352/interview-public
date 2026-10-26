package api

import (
	"encoding/json"
	"log"
	"net/http"

	"github.com/weave-lab/interview-public/go/internal/store"
)

type API struct {
	store *store.Store
}

func New(s *store.Store) *API {
	return &API{store: s}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		log.Print("encode JSON response failed")
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	if _, err := w.Write(append(data, '\n')); err != nil {
		// Abort a partial response rather than making it look complete or trying
		// to append a second status. net/http handles this sentinel without a stack.
		log.Print("write JSON response failed")
		panic(http.ErrAbortHandler)
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
