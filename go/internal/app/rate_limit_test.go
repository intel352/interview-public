package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5/middleware"

	"github.com/weave-lab/interview-public/go/internal/auth"
	"github.com/weave-lab/interview-public/go/internal/store"
)

type failedResponseWriter struct{ *httptest.ResponseRecorder }

func (w failedResponseWriter) Write([]byte) (int, error) {
	return 0, errors.New("synthetic response failure")
}

func TestRateLimitedRouterLogsAdmissionsAndRejections(t *testing.T) {
	// Logging must surround the limiter, so an enabled router logs every outcome
	// once even when admission stops the request before routing.
	var output bytes.Buffer
	original := middleware.DefaultLogger
	middleware.DefaultLogger = middleware.RequestLogger(&middleware.DefaultLogFormatter{
		Logger: log.New(&output, "", 0), NoColor: true,
	})
	t.Cleanup(func() { middleware.DefaultLogger = original })
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("logging=%t", enabled), func(t *testing.T) {
			output.Reset()
			h, err := newRateLimitedRouter(nil, Options{
				EnableLogging: enabled, AuthTokens: map[string]string{"test-logging-token": "alice"},
			}, DefaultRateLimitConfig(), func() time.Time { return time.Unix(0, 0) })
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []int{http.StatusOK, http.StatusTooManyRequests} {
				r := httptest.NewRequest(http.MethodPost, "/api/auth/token", nil)
				r.Header.Set("Authorization", "Bearer test-logging-token")
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				if w.Code != want {
					t.Fatalf("status=%d, want %d", w.Code, want)
				}
			}
			wantLogs := 0
			if enabled {
				wantLogs = 2
			}
			if got := strings.Count(output.String(), "/api/auth/token"); got != wantLogs {
				t.Fatalf("request log count=%d, want %d; logs=%q", got, wantLogs, output.String())
			}
			if enabled && (!strings.Contains(output.String(), "200") || !strings.Contains(output.String(), "429")) {
				t.Fatalf("logs must contain both response statuses: %q", output.String())
			}
			if strings.Contains(output.String(), "test-logging-token") {
				t.Fatal("request logs exposed a credential")
			}
		})
	}
}

func TestRateLimitAbortsInterruptedRejection(t *testing.T) {
	h := defaultRateLimit(t, http.NotFoundHandler(), func() time.Time { return time.Time{} })
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	w := failedResponseWriter{httptest.NewRecorder()}
	defer func() {
		if got := recover(); got != http.ErrAbortHandler {
			t.Errorf("panic = %v, want response abort", got)
		}
		if w.Code != http.StatusTooManyRequests {
			t.Errorf("status = %d, want 429", w.Code)
		}
	}()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
}

func TestRateLimitDrainBoundaries(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := start
	calls := 0
	h := defaultRateLimit(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("X-Handler", "reached")
		w.WriteHeader(http.StatusNoContent)
	}), func() time.Time { return now })

	for _, tt := range []struct {
		at    time.Duration
		code  int
		retry string
	}{
		{0, 204, ""},
		{0, 429, "6"},
		{time.Nanosecond, 429, "6"},
		{time.Second, 429, "5"},
		{5100 * time.Millisecond, 429, "1"},
		{6*time.Second - time.Nanosecond, 429, "1"},
		{6 * time.Second, 204, ""},
		{6*time.Second + time.Nanosecond, 429, "6"},
		{time.Hour, 204, ""},
		{time.Hour, 429, "6"}, // Idle time never accumulates burst credit.
	} {
		now = start.Add(tt.at)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
		if w.Code != tt.code || w.Header().Get("Retry-After") != tt.retry {
			t.Fatalf("at %s: got status %d, Retry-After %q; want %d, %q", tt.at, w.Code, w.Header().Get("Retry-After"), tt.code, tt.retry)
		}
		if tt.code == http.StatusTooManyRequests {
			if w.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("rejection must not be cached")
			}
			if w.Header().Get("Content-Type") != "application/json" || w.Body.String() != "{\"error\":\"rate limit exceeded\"}\n" {
				t.Fatalf("unexpected rejection: headers=%v body=%q", w.Header(), w.Body.String())
			}
			if w.Header().Get("X-Handler") != "" {
				t.Fatal("rejected request reached downstream handler")
			}
		} else if w.Header().Get("X-Handler") != "reached" {
			t.Fatal("admitted request did not preserve handler response")
		}
	}
	if calls != 3 {
		t.Fatalf("handler called %d times, want 3", calls)
	}
}

func TestRateLimitTenRequestsPerMinute(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	h := defaultRateLimit(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}), func() time.Time { return now })
	accepted := 0
	for second := range 60 {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
		want := http.StatusTooManyRequests
		if second%6 == 0 {
			want = http.StatusOK
		}
		if w.Code != want {
			t.Fatalf("at second %d: got status %d, want %d", second, w.Code, want)
		}
		if w.Code == http.StatusOK {
			accepted++
		}
		now = now.Add(time.Second)
	}
	if accepted != 10 {
		t.Fatalf("admitted %d requests in [0, 60s), want 10", accepted)
	}
}

func TestRateLimitBackwardClock(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := start
	h := defaultRateLimit(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}), func() time.Time { return now })
	for _, tt := range []struct {
		at   time.Duration
		code int
	}{
		{0, 200}, {-time.Second, 429}, {0, 429},
		{5 * time.Second, 429}, {6 * time.Second, 200},
	} {
		now = start.Add(tt.at)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
		if w.Code != tt.code {
			t.Fatalf("at %s: got status %d, want %d", tt.at, w.Code, tt.code)
		}
	}
}

func TestRateLimitConcurrentAnonymousClientsShareBucket(t *testing.T) {
	var accepted atomic.Int32
	var rejected atomic.Int32
	fixed := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	h := defaultRateLimit(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		accepted.Add(1)
	}), func() time.Time { return fixed })
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range 100 {
		wg.Go(func() {
			r := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/client/%d", i), nil)
			r.RemoteAddr = fmt.Sprintf("192.0.2.%d:1234", i+1)
			r.Header.Set("Authorization", fmt.Sprintf("Bearer user%d@example.com", i))
			w := httptest.NewRecorder()
			<-start
			h.ServeHTTP(w, r)
			if w.Code == http.StatusTooManyRequests {
				rejected.Add(1)
				if w.Header().Get("Retry-After") != "6" {
					t.Errorf("client %d: unexpected Retry-After %q", i, w.Header().Get("Retry-After"))
				}
			} else if w.Code != http.StatusOK {
				t.Errorf("client %d: unexpected status %d", i, w.Code)
			}
		})
	}
	close(start)
	wg.Wait()
	if accepted.Load() != 1 || rejected.Load() != 99 {
		t.Fatalf("accepted=%d rejected=%d, want 1 and 99", accepted.Load(), rejected.Load())
	}
}

func TestRateLimitActorBudgetsAndRefillsAreIndependent(t *testing.T) {
	start := time.Unix(0, 0)
	now := start
	h := auth.TokenParser(map[string]string{"alice": "alice", "alice-alias": "alice", "bob": "bob"})(
		defaultRateLimit(t, auth.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Actor", auth.UserID(r.Context()))
		})), func() time.Time { return now }))
	for _, step := range []struct {
		at    time.Duration
		token string
		code  int
		retry string
	}{
		{0, "alice", 200, ""},
		{0, "alice-alias", 429, "6"},
		{0, "invalid", 401, ""},
		{0, "another-invalid", 429, "6"},
		{2 * time.Second, "bob", 200, ""},
		{2 * time.Second, "alice", 429, "4"},
		{6 * time.Second, "alice", 200, ""},
		{6 * time.Second, "bob", 429, "2"},
		{8 * time.Second, "bob", 200, ""},
	} {
		now = start.Add(step.at)
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("Authorization", "Bearer "+step.token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != step.code || w.Header().Get("Retry-After") != step.retry {
			t.Fatalf("at %s token %q: status=%d retry=%q, want %d %q", step.at, step.token, w.Code, w.Header().Get("Retry-After"), step.code, step.retry)
		}
	}
}

func TestRateLimitConcurrentActorsHaveIndependentCapacity(t *testing.T) {
	accepted := map[string]*atomic.Int64{"alice": {}, "bob": {}}
	h, err := rateLimit(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		accepted[auth.UserID(r.Context())].Add(1)
	}), RateLimitConfig{Requests: 3, Period: time.Second, Capacity: 2}, func() time.Time { return time.Unix(0, 0) })
	if err != nil {
		t.Fatal(err)
	}
	h = auth.TokenParser(map[string]string{"alice": "alice", "bob": "bob"})(h)
	ready := make(chan struct{})
	var wg sync.WaitGroup
	for actor := range accepted {
		for range 32 {
			wg.Go(func() {
				r := httptest.NewRequest(http.MethodGet, "/", nil)
				r.Header.Set("Authorization", "Bearer "+actor)
				w := httptest.NewRecorder()
				<-ready
				h.ServeHTTP(w, r)
				if w.Code != http.StatusOK && w.Code != http.StatusTooManyRequests {
					t.Errorf("unexpected status %d", w.Code)
				}
			})
		}
	}
	close(ready)
	wg.Wait()
	for actor, count := range accepted {
		if got := count.Load(); got != 2 {
			t.Errorf("actor %s admitted %d requests, want 2", actor, got)
		}
	}
}

func TestRateLimitRejectsWhileHandlerIsBusy(t *testing.T) {
	// With a fixed clock the bucket stays full. This proves prompt rejection,
	// not a restriction on the number of concurrently running handlers.
	entered := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan struct{})
	fixed := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	h := defaultRateLimit(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
	}), func() time.Time { return fixed })
	t.Cleanup(func() {
		close(release)
		<-finished
	})
	go func() {
		defer close(finished)
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/slow", nil))
	}()
	<-entered
	rejected := make(chan int, 1)
	go func() {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/another", nil))
		rejected <- w.Code
	}()
	select {
	case code := <-rejected:
		if code != http.StatusTooManyRequests {
			t.Fatalf("got status %d while handler busy, want 429", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("rejection waited for the admitted handler to finish")
	}
}

func TestRateLimitAdmitsAfterRefillWhileHandlerIsBusy(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan struct{})
	var nextFinished chan struct{}
	var elapsed atomic.Int64
	start := time.Unix(0, 0)
	h := auth.TokenParser(map[string]string{"slow-test-token": "alice"})(defaultRateLimit(t,
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/slow" {
				close(entered)
				<-release
			}
			w.WriteHeader(http.StatusNoContent)
		}), func() time.Time { return start.Add(time.Duration(elapsed.Load())) }))
	request := func(path string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set("Authorization", "Bearer slow-test-token")
		return r
	}
	t.Cleanup(func() {
		close(release)
		<-finished
		if nextFinished != nil {
			<-nextFinished
		}
	})
	go func() {
		defer close(finished)
		response := httptest.NewRecorder()
		h.ServeHTTP(response, request("/slow"))
		if response.Code != http.StatusNoContent {
			t.Errorf("slow response status=%d, want 204", response.Code)
		}
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first handler did not start")
	}
	// Advancing time drains the bucket without releasing the first handler.
	elapsed.Store(int64(6 * time.Second))
	completed := make(chan int, 1)
	nextFinished = make(chan struct{})
	go func() {
		defer close(nextFinished)
		response := httptest.NewRecorder()
		h.ServeHTTP(response, request("/next"))
		completed <- response.Code
	}()
	select {
	case code := <-completed:
		if code != http.StatusNoContent {
			t.Fatalf("refilled request status=%d, want 204", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("refilled request waited for the first handler to finish")
	}
	select {
	case <-finished:
		t.Fatal("first handler finished before being released")
	default:
	}
}

func TestRateLimitRouterWrites(t *testing.T) {
	const token = "test-only-rate-limit-token"
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	if err := s.RegisterActors(t.Context(), []string{"alice@example.com"}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	h, err := newRateLimitedRouter(s, Options{AuthTokens: map[string]string{token: "alice@example.com"}}, DefaultRateLimitConfig(), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []int{http.StatusCreated, http.StatusTooManyRequests} {
		r := httptest.NewRequest(http.MethodPost, "/api/contacts", strings.NewReader(`{"first_name":"Test","email":"test@example.com"}`))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/json")
		r.RemoteAddr = fmt.Sprintf("127.0.0.%d:1234", i+1)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("write %d: got status %d, want %d: %s", i, w.Code, want, w.Body.String())
		}
	}
	count, err := s.CountContacts(context.Background(), "alice@example.com")
	if err != nil || count != 1 {
		t.Fatalf("stored contacts=%d, err=%v; want 1", count, err)
	}
	now = now.Add(6 * time.Second)
	r := httptest.NewRequest(http.MethodGet, "/api/contacts", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var body struct{ Contacts []store.Contact }
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || w.Code != http.StatusOK || len(body.Contacts) != 1 {
		t.Fatalf("read after drain: status=%d body=%s err=%v", w.Code, w.Body.String(), err)
	}
}

func TestRateLimitIncludesAuthAndUnknownAPIRoutes(t *testing.T) {
	const token = "test-only-rate-limit-token"
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	h, err := newRateLimitedRouter(nil, Options{AuthTokens: map[string]string{token: "alice@example.com"}}, DefaultRateLimitConfig(), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		advance time.Duration
		method  string
		path    string
		token   string
		code    int
	}{
		{0, "GET", "/health", "", 404},
		{0, "GET", "/api/contacts", "", 429},
		{0, "POST", "/api/auth/token", "Bearer " + token, 200},
		{0, "GET", "/missing", "Bearer " + token, 429},
		{6 * time.Second, "GET", "/api/contacts", "", 401},
		{0, "GET", "/health", "", 429},
		{0, "GET", "/missing", "Bearer " + token, 404},
		{0, "POST", "/api/auth/token", "Bearer " + token, 429},
		{0, "GET", "/missing", "", 429},
		{6 * time.Second, "GET", "/missing", "", 404},
		{0, "GET", "/health", "", 429},
	} {
		now = now.Add(tt.advance)
		r := httptest.NewRequest(tt.method, tt.path, nil)
		r.Header.Set("Authorization", tt.token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tt.code {
			t.Fatalf("%s %s: got status %d, want %d", tt.method, tt.path, w.Code, tt.code)
		}
	}
}

// Existing scenarios independently pin the original 10/minute, capacity-one defaults.
func defaultRateLimit(t *testing.T, next http.Handler, now func() time.Time) http.Handler {
	t.Helper()
	handler, err := rateLimit(next, DefaultRateLimitConfig(), now)
	if err != nil {
		t.Fatal(err)
	}
	return handler
}
