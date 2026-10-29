package app

import (
	"net/http"
	"time"

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
	return newAuthenticatedRouter(s, opts, nil)
}

// NewRateLimitedRouter authenticates each request before selecting its actor's
// bucket, then applies the same routing and authorization as NewRouter.
func NewRateLimitedRouter(s *store.Store, opts Options, config RateLimitConfig) (http.Handler, error) {
	limit, err := RateLimitMiddleware(config)
	if err != nil {
		return nil, err
	}
	return newAuthenticatedRouter(s, opts, limit), nil
}

func newRateLimitedRouter(s *store.Store, opts Options, config RateLimitConfig, now func() time.Time) (http.Handler, error) {
	limit, err := rateLimitMiddleware(config, now)
	if err != nil {
		return nil, err
	}
	return newAuthenticatedRouter(s, opts, limit), nil
}

func newAuthenticatedRouter(s *store.Store, opts Options, limit func(http.Handler) http.Handler) http.Handler {
	handler := newRouter(s)
	if limit != nil {
		handler = limit(handler)
	}
	return withRequestLogging(auth.TokenParser(opts.AuthTokens)(handler), opts.EnableLogging)
}

func withRequestLogging(handler http.Handler, enabled bool) http.Handler {
	if enabled {
		return middleware.Logger(handler)
	}
	return handler
}

func newRouter(s *store.Store) http.Handler {
	a := api.New(s)
	r := chi.NewRouter()

	r.Use(middleware.Recoverer)

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
