package app

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/weave-lab/interview-public/go/internal/api"
	"github.com/weave-lab/interview-public/go/internal/auth"
	"github.com/weave-lab/interview-public/go/internal/store"
)

type Options struct {
	EnableLogging bool
	AuthTokens    map[string]string // Ephemeral token to configured actor ID.
}

func NewRouter(s *store.Store, opts Options) http.Handler {
	a := api.New(s)
	r := chi.NewRouter()

	if opts.EnableLogging {
		r.Use(middleware.Logger)
	}
	r.Use(middleware.Recoverer)
	r.Use(auth.TokenParser(opts.AuthTokens))

	r.Route("/api", func(r chi.Router) {
		r.Use(auth.RequireAuth)

		r.Post("/auth/token", a.HandleToken)

		r.Get("/contacts", a.HandleListContacts)
		r.Post("/contacts", a.HandleCreateContact)
		r.Get("/contacts/{id}", a.HandleGetContact)
		r.Put("/contacts/{id}", a.HandleUpdateContact)
		r.Delete("/contacts/{id}", a.HandleDeleteContact)

		r.Post("/contacts/import", a.HandleImportContacts)
		r.Get("/contacts/export", a.HandleExportContacts)

		r.Get("/files", a.HandleListFiles)
		r.Post("/files", a.HandleUploadFile)
		r.Get("/files/{id}", a.HandleDownloadFile)

		r.Get("/reports/activity", a.HandleActivityReport)
	})

	return r
}
