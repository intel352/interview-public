package app

import (
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRateLimitConfigurationValidation(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		config RateLimitConfig
		valid  bool
	}{
		{"defaults", RateLimitConfig{10, time.Minute, 1}, true},
		{"fractional interval", RateLimitConfig{3, time.Second, 2}, true},
		{"seven per minute", RateLimitConfig{7, time.Minute, 3}, true},
		{"nanosecond resolution", RateLimitConfig{1, time.Nanosecond, 1}, true},
		{"maximum period", RateLimitConfig{1, time.Duration(math.MaxInt64), 1}, true},
		{"maximum request count", RateLimitConfig{math.MaxInt64, time.Duration(math.MaxInt64), 1}, true},
		{"maximum capacity", RateLimitConfig{1, time.Nanosecond, math.MaxInt64}, true},
		{"zero count", RateLimitConfig{0, time.Second, 1}, false},
		{"negative count", RateLimitConfig{-1, time.Second, 1}, false},
		{"zero period", RateLimitConfig{1, 0, 1}, false},
		{"negative period", RateLimitConfig{1, -time.Second, 1}, false},
		{"zero capacity", RateLimitConfig{1, time.Second, 0}, false},
		{"negative capacity", RateLimitConfig{1, time.Second, -1}, false},
		{"subnanosecond interval", RateLimitConfig{2, time.Nanosecond, 1}, false},
		{"capacity overflow", RateLimitConfig{1, time.Second, math.MaxInt64/int64(time.Second) + 1}, false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			if err := scenario.config.Validate(); (err == nil) != scenario.valid {
				t.Fatalf("validation error=%v, want valid=%v", err, scenario.valid)
			}
			handler, err := RateLimit(http.NotFoundHandler(), scenario.config)
			if (err == nil) != scenario.valid || (handler != nil) != scenario.valid {
				t.Fatalf("constructor handler=%v error=%v, want valid=%v", handler, err, scenario.valid)
			}
			handler, err = NewRateLimitedRouter(nil, Options{}, scenario.config)
			if (err == nil) != scenario.valid || (handler != nil) != scenario.valid {
				t.Fatalf("composed constructor handler=%v error=%v, want valid=%v", handler, err, scenario.valid)
			}
		})
	}
	if got := DefaultRateLimitConfig(); got != (RateLimitConfig{10, time.Minute, 1}) {
		t.Fatalf("default policy=%+v; want 10/minute, capacity 1", got)
	}
}

func TestRateLimitConfiguredBoundaries(t *testing.T) {
	type attempt struct {
		at    time.Duration
		code  int
		retry string
	}
	for _, scenario := range []struct {
		name     string
		config   RateLimitConfig
		attempts []attempt
	}{
		{"two per five seconds", RateLimitConfig{2, 5 * time.Second, 1}, []attempt{
			{0, 204, ""}, {0, 429, "3"}, {time.Second, 429, "2"},
			{2500*time.Millisecond - 1, 429, "1"}, {2500 * time.Millisecond, 204, ""},
		}},
		{"rational drain retains fractional work", RateLimitConfig{3, time.Second, 2}, []attempt{
			{0, 204, ""}, {0, 204, ""}, {0, 429, "1"},
			{333333333, 429, "1"}, {333333334, 204, ""},
			{666666666, 429, "1"}, {666666667, 204, ""},
			{999999999, 429, "1"}, {time.Second, 204, ""},
		}},
		{"seven per minute", RateLimitConfig{7, time.Minute, 1}, []attempt{
			{0, 204, ""}, {0, 429, "9"},
			{8571428571, 429, "1"}, {8571428572, 204, ""},
		}},
		{"nanosecond drain", RateLimitConfig{1, time.Nanosecond, 2}, []attempt{
			{0, 204, ""}, {0, 204, ""}, {0, 429, "1"},
			{time.Nanosecond, 204, ""}, {time.Nanosecond, 429, "1"},
		}},
		{"maximum period fractional interval", RateLimitConfig{2, time.Duration(math.MaxInt64), 1}, []attempt{
			{0, 204, ""}, {0, 429, "4611686019"},
			{(1 << 62) - 1, 429, "1"}, {1 << 62, 204, ""},
		}},
		{"maximum count drain", RateLimitConfig{math.MaxInt64, time.Duration(math.MaxInt64), 1}, []attempt{
			{0, 204, ""}, {0, 429, "1"},
			{time.Nanosecond, 204, ""}, {time.Nanosecond, 429, "1"},
		}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			now := time.Unix(0, 0)
			calls := 0
			h, err := rateLimit(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				w.WriteHeader(http.StatusNoContent)
			}), scenario.config, func() time.Time { return now })
			if err != nil {
				t.Fatal(err)
			}
			wantCalls := 0
			for _, step := range scenario.attempts {
				now = time.Unix(0, 0).Add(step.at)
				response := httptest.NewRecorder()
				h.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
				if response.Code != step.code || response.Header().Get("Retry-After") != step.retry {
					t.Fatalf("at %s got status=%d retry=%q; want %d %q", step.at, response.Code, response.Header().Get("Retry-After"), step.code, step.retry)
				}
				if step.code == http.StatusNoContent {
					wantCalls++
				}
				if calls != wantCalls {
					t.Fatalf("handler calls=%d want=%d", calls, wantCalls)
				}
			}
		})
	}
}

func TestRateLimitConfiguredIdleAndBackwardClock(t *testing.T) {
	now := time.Unix(0, 0)
	h, err := rateLimit(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), RateLimitConfig{3, time.Second, 4}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	assertBurst := func() {
		t.Helper()
		for i := range 5 {
			response := httptest.NewRecorder()
			h.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
			want := http.StatusOK
			if i == 4 {
				want = http.StatusTooManyRequests
			}
			if response.Code != want {
				t.Fatalf("burst request %d at %v: status=%d want=%d", i, now, response.Code, want)
			}
		}
	}
	assertBurst()
	now = now.Add(200 * 365 * 24 * time.Hour) // elapsed*requests would overflow.
	assertBurst()
	now = now.Add(-time.Hour)
	response := httptest.NewRecorder()
	h.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	if response.Code != http.StatusTooManyRequests || response.Header().Get("Retry-After") != "1" {
		t.Fatalf("backward clock must not add credit: %d %v", response.Code, response.Header())
	}
}

func TestRateLimitMaximumDurationRetryDoesNotOverflow(t *testing.T) {
	now := time.Unix(0, 0)
	h, err := rateLimit(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), RateLimitConfig{1, time.Duration(math.MaxInt64), 1}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	response := httptest.NewRecorder()
	h.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	if response.Code != http.StatusTooManyRequests || response.Header().Get("Retry-After") != "9223372037" {
		t.Fatalf("maximum retry: status=%d retry=%q", response.Code, response.Header().Get("Retry-After"))
	}
	now = now.Add(time.Duration(math.MaxInt64))
	response = httptest.NewRecorder()
	h.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("maximum-duration drain should admit: status=%d", response.Code)
	}
}

func TestRateLimitConfiguredConcurrentCapacity(t *testing.T) {
	for _, capacity := range []int64{1, 2, 7, 32} {
		t.Run(strconv.FormatInt(capacity, 10), func(t *testing.T) {
			var accepted atomic.Int64
			var elapsed atomic.Int64
			start := time.Unix(0, 0)
			h, err := rateLimit(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { accepted.Add(1) }), RateLimitConfig{3, time.Second, capacity}, func() time.Time { return start.Add(time.Duration(elapsed.Load())) })
			if err != nil {
				t.Fatal(err)
			}
			wave := func(want int64) {
				t.Helper()
				ready := make(chan struct{})
				var wg sync.WaitGroup
				for range 64 {
					wg.Go(func() {
						<-ready
						r := httptest.NewRecorder()
						h.ServeHTTP(r, httptest.NewRequest(http.MethodGet, "/", nil))
						if r.Code != http.StatusOK && r.Code != http.StatusTooManyRequests {
							t.Errorf("unexpected response %d", r.Code)
						}
					})
				}
				close(ready)
				wg.Wait()
				if accepted.Load() != want {
					t.Fatalf("accepted=%d want=%d", accepted.Load(), want)
				}
			}
			wave(capacity)
			elapsed.Store(333333333)
			wave(capacity)
			elapsed.Store(333333334)
			wave(capacity + 1)
			elapsed.Store(int64(time.Hour))
			wave(2*capacity + 1)
		})
	}
}
