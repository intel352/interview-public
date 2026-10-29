package app

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/weave-lab/interview-public/go/internal/auth"
)

func TestRateLimitMiddlewareSharesPolicyAcrossHandlers(t *testing.T) {
	config := RateLimitConfig{Requests: 1, Period: time.Hour, Capacity: 1}
	shared, err := RateLimitMiddleware(config)
	if err != nil {
		t.Fatal(err)
	}
	independent, err := RateLimitMiddleware(config)
	if err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	r.Use(auth.TokenParser(map[string]string{"alice-token": "alice", "bob-token": "bob"}))
	r.Use(auth.RequireAuth)
	handler := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }
	r.With(shared).Get("/first", handler)
	r.With(shared).Get("/second", handler)
	r.With(independent).Get("/independent", handler)
	for _, step := range []struct {
		path, token string
		want        int
	}{
		{"/first", "alice-token", 204},
		{"/second", "alice-token", 429},
		{"/independent", "alice-token", 204},
		{"/second", "bob-token", 204},
		{"/first", "bob-token", 429},
	} {
		request := httptest.NewRequest(http.MethodGet, step.path, nil)
		request.Header.Set("Authorization", "Bearer "+step.token)
		response := httptest.NewRecorder()
		r.ServeHTTP(response, request)
		if response.Code != step.want {
			t.Fatalf("%s %s: status=%d, want %d", step.path, step.token, response.Code, step.want)
		}
	}
}

func TestRateLimitMiddlewareLayersEndpointPolicy(t *testing.T) {
	common, err := RateLimitMiddleware(RateLimitConfig{Requests: 10, Period: time.Hour, Capacity: 10})
	if err != nil {
		t.Fatal(err)
	}
	strict, err := RateLimitMiddleware(RateLimitConfig{Requests: 1, Period: time.Hour, Capacity: 1})
	if err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	r.Use(auth.TokenParser(map[string]string{"alice-token": "alice", "bob-token": "bob"}))
	r.Use(auth.RequireAuth)
	r.Use(common)
	handler := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }
	r.Get("/light", handler)
	r.With(strict).Get("/heavy", handler)
	attempt := func(token, path string, want int) {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Header.Set("Authorization", "Bearer "+token)
		response := httptest.NewRecorder()
		r.ServeHTTP(response, request)
		if response.Code != want {
			t.Fatalf("%s %s: status=%d, want %d", path, token, response.Code, want)
		}
	}
	attempt("alice-token", "/heavy", 204)
	attempt("alice-token", "/heavy", 429)
	// The outer policy counts the attempt that the inner policy rejects.
	for range 8 {
		attempt("alice-token", "/light", 204)
	}
	attempt("alice-token", "/light", 429)
	attempt("bob-token", "/light", 204)
	attempt("bob-token", "/heavy", 204)
}

func TestRateLimitTenPerMinuteWithTenRequestBurst(t *testing.T) {
	fixed := time.Unix(0, 0)
	handler, err := rateLimit(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}), RateLimitConfig{Requests: 10, Period: time.Minute, Capacity: 10}, func() time.Time { return fixed })
	if err != nil {
		t.Fatal(err)
	}
	for i := range 11 {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
		want := http.StatusNoContent
		if i == 10 {
			want = http.StatusTooManyRequests
		}
		if response.Code != want {
			t.Fatalf("burst request %d: status=%d, want %d", i, response.Code, want)
		}
		if i == 10 && response.Header().Get("Retry-After") != "6" {
			t.Fatalf("full burst Retry-After=%q, want 6", response.Header().Get("Retry-After"))
		}
	}
	start := fixed
	for _, step := range []struct {
		at    time.Duration
		want  int
		retry string
	}{
		{6*time.Second - time.Nanosecond, 429, "1"},
		{6 * time.Second, 204, ""},
		{6 * time.Second, 429, "6"},
	} {
		fixed = start.Add(step.at)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
		if response.Code != step.want || response.Header().Get("Retry-After") != step.retry {
			t.Fatalf("at %s: status=%d Retry-After=%q, want %d %q", step.at, response.Code, response.Header().Get("Retry-After"), step.want, step.retry)
		}
	}
}
