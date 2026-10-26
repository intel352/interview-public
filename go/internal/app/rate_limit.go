package app

import (
	"net/http"
	"strconv"
	"sync"
	"time"
)

const requestDrainTime = time.Minute / 10

// RateLimit applies one shared, capacity-one leaky bucket to next. Each admitted
// request adds six seconds of work; overflow is rejected instead of queued.
// Create this wrapper once for the server, not once per request or client.
func RateLimit(next http.Handler) http.Handler {
	return rateLimit(next, time.Now)
}

func rateLimit(next http.Handler, now func() time.Time) http.Handler {
	var mu sync.Mutex
	var level time.Duration // Remaining work; capacity is requestDrainTime.
	last := now()

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		current := now()
		// Lazy draining avoids a background ticker. Never move last backwards.
		if current.After(last) {
			level = max(0, level-current.Sub(last))
			last = current
		}
		retryAfter := level
		if level == 0 {
			level = requestDrainTime
		}
		mu.Unlock()

		if retryAfter > 0 {
			seconds := (retryAfter + time.Second - 1) / time.Second
			w.Header().Set("Retry-After", strconv.FormatInt(int64(seconds), 10))
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte("{\"error\":\"rate limit exceeded\"}\n"))
			return
		}
		next.ServeHTTP(w, r)
	})
}
