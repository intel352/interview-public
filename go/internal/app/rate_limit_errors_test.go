package app

import (
	"math"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestRateLimitValidationExplainsInvalidValues(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		config RateLimitConfig
		want   string
	}{
		{"zero requests", RateLimitConfig{0, time.Second, 1}, "--rate-limit-requests=0: must be between 1 and 9223372036854775807"},
		{"negative requests", RateLimitConfig{-1, time.Second, 1}, "--rate-limit-requests=-1: must be between 1 and 9223372036854775807"},
		{"minimum requests integer", RateLimitConfig{math.MinInt64, time.Second, 1}, "--rate-limit-requests=-9223372036854775808: must be between 1 and 9223372036854775807"},
		{"zero period", RateLimitConfig{1, 0, 1}, "--rate-limit-period=0s: must be between 1ns and 2562047h47m16.854775807s (9223372036854775807ns)"},
		{"negative period", RateLimitConfig{1, -time.Second, 1}, "--rate-limit-period=-1s: must be between 1ns and 2562047h47m16.854775807s (9223372036854775807ns)"},
		{"minimum period integer", RateLimitConfig{1, time.Duration(math.MinInt64), 1}, "--rate-limit-period=-2562047h47m16.854775808s: must be between 1ns and 2562047h47m16.854775807s (9223372036854775807ns)"},
		{"zero capacity", RateLimitConfig{1, time.Second, 0}, "--rate-limit-capacity=0: must be between 1 and 9223372036854775807"},
		{"negative capacity", RateLimitConfig{1, time.Second, -1}, "--rate-limit-capacity=-1: must be between 1 and 9223372036854775807"},
		{"minimum capacity integer", RateLimitConfig{1, time.Second, math.MinInt64}, "--rate-limit-capacity=-9223372036854775808: must be between 1 and 9223372036854775807"},
		{"rate exceeds resolution", RateLimitConfig{2, time.Nanosecond, 1}, "--rate-limit-requests=2 exceeds maximum 1 for --rate-limit-period=1ns: reduce requests or increase period (minimum interval 1ns)"},
		{"capacity exceeds work range", RateLimitConfig{1, time.Second, 9223372037}, "--rate-limit-capacity=9223372037 exceeds maximum 9223372036 for --rate-limit-period=1s (1000000000ns): reduce capacity or period so capacity × period-in-nanoseconds is at most 9223372036854775807"},
		{"extreme product exceeds work range", RateLimitConfig{1, time.Duration(math.MaxInt64), math.MaxInt64}, "--rate-limit-capacity=9223372036854775807 exceeds maximum 1 for --rate-limit-period=2562047h47m16.854775807s (9223372036854775807ns): reduce capacity or period so capacity × period-in-nanoseconds is at most 9223372036854775807"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			err := scenario.config.Validate()
			if err == nil || err.Error() != scenario.want {
				t.Fatalf("diagnostic=%v; want %q", err, scenario.want)
			}
			// A caller constructing middleware receives the same actionable reason.
			handler, constructorErr := RateLimit(http.NotFoundHandler(), scenario.config)
			if handler != nil || constructorErr == nil || constructorErr.Error() != scenario.want {
				t.Fatalf("constructor handler=%v error=%v; want nil handler and %q", handler, constructorErr, scenario.want)
			}
		})
	}
}

func TestRateLimitValidationReportsIndependentProblems(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		config RateLimitConfig
		want   []string
	}{
		{"all nonpositive", RateLimitConfig{0, -time.Nanosecond, -2}, []string{
			"--rate-limit-requests=0: must be between 1 and 9223372036854775807",
			"--rate-limit-period=-1ns: must be between 1ns and 2562047h47m16.854775807s (9223372036854775807ns)",
			"--rate-limit-capacity=-2: must be between 1 and 9223372036854775807",
		}},
		{"independent cross-field constraints", RateLimitConfig{3, 2 * time.Nanosecond, math.MaxInt64}, []string{
			"--rate-limit-requests=3 exceeds maximum 2 for --rate-limit-period=2ns: reduce requests or increase period (minimum interval 1ns)",
			"--rate-limit-capacity=9223372036854775807 exceeds maximum 4611686018427387903 for --rate-limit-period=2ns (2ns): reduce capacity or period so capacity × period-in-nanoseconds is at most 9223372036854775807",
		}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			err := scenario.config.Validate()
			want := strings.Join(scenario.want, "\n")
			if err == nil || err.Error() != want {
				t.Fatalf("diagnostic=%v; want %q", err, want)
			}
		})
	}
}

func TestRateLimitValidationAcceptsAdvertisedBounds(t *testing.T) {
	// These values independently pin the documented bounds. Improving error text
	// must neither reject supported extremes nor accidentally accept the next value.
	for _, config := range []RateLimitConfig{
		{1, time.Nanosecond, 1},
		{1, time.Nanosecond, math.MaxInt64},
		{math.MaxInt64, time.Duration(math.MaxInt64), 1},
		{1, time.Second, 9223372036},
		{2, 2 * time.Nanosecond, 4611686018427387903},
	} {
		if err := config.Validate(); err != nil {
			t.Fatalf("supported boundary %+v rejected: %v", config, err)
		}
	}
	for _, config := range []RateLimitConfig{
		{2, time.Nanosecond, 1},
		{1, time.Second, 9223372037},
		{2, 2 * time.Nanosecond, 4611686018427387904},
	} {
		if err := config.Validate(); err == nil {
			t.Fatalf("unsupported boundary %+v accepted", config)
		}
	}
}
