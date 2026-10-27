package app

import (
	"errors"
	"log"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// RateLimitConfig defines a drain rate and a maximum burst, not a fixed window.
// Period/Requests may be fractional; the limiter retains that precision in its
// work accounting instead of rounding an interval once per request.
type RateLimitConfig struct {
	Requests int64
	Period   time.Duration
	Capacity int64
}

func DefaultRateLimitConfig() RateLimitConfig {
	return RateLimitConfig{Requests: 10, Period: time.Minute, Capacity: 1}
}

func (c RateLimitConfig) Validate() error {
	if c.Requests <= 0 || c.Period <= 0 || c.Capacity <= 0 {
		return errors.New("rate limit requests, period and capacity must be positive")
	}
	if c.Requests > int64(c.Period) {
		return errors.New("rate limit must allow at least one nanosecond per request")
	}
	if c.Capacity > math.MaxInt64/int64(c.Period) {
		return errors.New("rate limit capacity times period exceeds integer work precision")
	}
	return nil
}

// RateLimit applies one shared leaky bucket to next. Overflow is rejected instead
// of queued. The default policy is ten requests/minute with capacity one.
// Create this wrapper once for the server, not once per request or client.
func RateLimit(next http.Handler, config RateLimitConfig) (http.Handler, error) {
	return rateLimit(next, config, time.Now)
}

func rateLimit(next http.Handler, config RateLimitConfig, now func() time.Time) (http.Handler, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	// One request adds Period nanoseconds of work; each elapsed nanosecond drains
	// Requests units. This integer scale preserves rates such as three/second.
	cost := int64(config.Period)
	admissionThreshold := cost*config.Capacity - cost
	var mu sync.Mutex
	var level int64
	last := now()

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		current := now()
		// Lazy draining avoids a background ticker. Never move last backwards.
		if current.After(last) {
			elapsed := int64(current.Sub(last))
			// Saturate before multiplying: a long idle must never overflow or
			// accumulate credit beyond the configured capacity.
			if elapsed >= ceilDivide(level, config.Requests) {
				level = 0
			} else {
				level -= elapsed * config.Requests
			}
			last = current
		}
		var retryAfter int64
		if level <= admissionThreshold {
			level += cost
		} else {
			retryAfter = ceilDivide(level-admissionThreshold, config.Requests)
		}
		mu.Unlock()

		if retryAfter > 0 {
			seconds := ceilDivide(retryAfter, int64(time.Second))
			w.Header().Set("Retry-After", strconv.FormatInt(seconds, 10))
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusTooManyRequests)
			// Abort an interrupted response; its status is already committed.
			if _, err := w.Write([]byte("{\"error\":\"rate limit exceeded\"}\n")); err != nil {
				log.Print("write rate limit response failed")
				panic(http.ErrAbortHandler)
			}
			return
		}
		next.ServeHTTP(w, r)
	}), nil
}

// ceilDivide rounds nonnegative n up without overflowing n+d-1. d is positive.
func ceilDivide(n, d int64) int64 {
	result := n / d
	if n%d != 0 {
		result++
	}
	return result
}
