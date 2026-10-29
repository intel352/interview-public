package app

import (
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/weave-lab/interview-public/go/internal/auth"
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
	var problems []error
	if c.Requests <= 0 {
		problems = append(problems, fmt.Errorf("--rate-limit-requests=%d: must be between 1 and %d", c.Requests, int64(math.MaxInt64)))
	}
	if c.Period <= 0 {
		problems = append(problems, fmt.Errorf("--rate-limit-period=%s: must be between 1ns and %s (%dns)", c.Period, time.Duration(math.MaxInt64), int64(math.MaxInt64)))
	}
	if c.Capacity <= 0 {
		problems = append(problems, fmt.Errorf("--rate-limit-capacity=%d: must be between 1 and %d", c.Capacity, int64(math.MaxInt64)))
	}
	if len(problems) > 0 {
		return errors.Join(problems...)
	}
	// Check relationships only after positivity, so even invalid configurations
	// can be explained without division by zero or overflowing arithmetic.
	if c.Requests > int64(c.Period) {
		problems = append(problems, fmt.Errorf("--rate-limit-requests=%d exceeds maximum %d for --rate-limit-period=%s: reduce requests or increase period (minimum interval 1ns)", c.Requests, int64(c.Period), c.Period))
	}
	maxCapacity := math.MaxInt64 / int64(c.Period)
	if c.Capacity > maxCapacity {
		problems = append(problems, fmt.Errorf("--rate-limit-capacity=%d exceeds maximum %d for --rate-limit-period=%s (%dns): reduce capacity or period so capacity × period-in-nanoseconds is at most %d", c.Capacity, maxCapacity, c.Period, int64(c.Period), int64(math.MaxInt64)))
	}
	return errors.Join(problems...)
}

// RateLimit applies an independent leaky bucket per authenticated actor to next.
// Overflow is rejected instead of queued. Requests without an authenticated actor
// share one anonymous bucket. Authentication must run before this handler;
// application callers should use NewRateLimitedRouter for that composition.
// Create this wrapper once for the server, not once per request or client.
func RateLimit(next http.Handler, config RateLimitConfig) (http.Handler, error) {
	return rateLimit(next, config, time.Now)
}

func rateLimit(next http.Handler, config RateLimitConfig, now func() time.Time) (http.Handler, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	// Keys come only from authenticated context, never raw credentials or client
	// labels. State grows with configured actors that actually make requests.
	var buckets sync.Map // map[string]*leakyBucket; entries live for this wrapper.

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actor := auth.UserID(r.Context())
		value, ok := buckets.Load(actor)
		if !ok {
			value, _ = buckets.LoadOrStore(actor, &leakyBucket{})
		}
		retryAfter := value.(*leakyBucket).admit(config, now)
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

type leakyBucket struct {
	mu          sync.Mutex
	level       int64
	last        time.Time
	initialized bool
}

// admit returns zero on admission, or nanoseconds until another request fits.
func (b *leakyBucket) admit(config RateLimitConfig, now func() time.Time) int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	current := now()
	if !b.initialized {
		b.last, b.initialized = current, true
	}
	// Lazy draining avoids a background ticker. Never move last backwards.
	if current.After(b.last) {
		elapsed := int64(current.Sub(b.last))
		// Saturate before multiplying: long idle cannot overflow or accumulate
		// credit beyond the configured capacity.
		if elapsed >= ceilDivide(b.level, config.Requests) {
			b.level = 0
		} else {
			b.level -= elapsed * config.Requests
		}
		b.last = current
	}
	// One admission adds Period nanoseconds of work; each elapsed nanosecond
	// drains Requests units, retaining fractional rates such as three/second.
	cost := int64(config.Period)
	threshold := cost*config.Capacity - cost
	if b.level > threshold {
		return ceilDivide(b.level-threshold, config.Requests)
	}
	b.level += cost
	return 0
}

// ceilDivide rounds nonnegative n up without overflowing n+d-1. d is positive.
func ceilDivide(n, d int64) int64 {
	result := n / d
	if n%d != 0 {
		result++
	}
	return result
}
