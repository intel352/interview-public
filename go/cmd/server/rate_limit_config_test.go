package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/weave-lab/interview-public/go/internal/auth"
	"github.com/weave-lab/interview-public/go/internal/store"
)

func TestBDDRateLimitConfigurationFailsBeforeOpeningData(t *testing.T) {
	for _, args := range [][]string{
		{"--rate-limit-requests=0"},
		{"--rate-limit-requests=-1"},
		{"--rate-limit-requests=NaN"},
		{"--rate-limit-requests=1.5"},
		{"--rate-limit-requests=9223372036854775808"},
		{"--rate-limit-period=0s"},
		{"--rate-limit-period=-1s"},
		{"--rate-limit-period=NaN"},
		{"--rate-limit-period=2562048h"},
		{"--rate-limit-requests=1", "--rate-limit-period=1.5ns"},
		{"--rate-limit-requests=1", "--rate-limit-period=1.0000000001s"},
		{"--rate-limit-requests=1", "--rate-limit-period=0.5ns0.5ns"},
		{"--rate-limit-capacity=0"},
		{"--rate-limit-capacity=-1"},
		{"--rate-limit-capacity=Inf"},
		{"--rate-limit-capacity=9223372036854775808"},
		{"--rate-limit-requests=2", "--rate-limit-period=1ns"},
		{"--rate-limit-period=2562047h", "--rate-limit-capacity=2"},
		{"--seed", "--rate-limit-capacity=0"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			// Given invalid explicit policy, When the real CLI parses it,
			// Then it rejects startup before data, listeners or credentials.
			data := filepath.Join(t.TempDir(), "must-not-create")
			var output bytes.Buffer
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			err := run(ctx, append(args, "--addr=127.0.0.1:0", "--data="+data), &output)
			if err == nil {
				t.Fatal("Then invalid limiter policy must fail startup")
			}
			if _, err := os.Stat(data); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("Then invalid policy must leave data absent: %v", err)
			}
			if strings.Contains(output.String(), "Local demo") {
				t.Fatal("Then startup must not announce listeners or credentials")
			}
		})
	}
}

func TestRateLimitPeriodParsing(t *testing.T) {
	for _, scenario := range []struct {
		value string
		want  time.Duration
	}{
		{"1m", time.Minute},
		{"1.5s", 1500 * time.Millisecond},
		{"1h30m", 90 * time.Minute},
		{".000000001s", time.Nanosecond},
		{"1.000000001s", time.Second + time.Nanosecond},
		{"1.0000000000000000000000s", time.Second},
		{"010s", 10 * time.Second},
		{"0.1s0.2s", 300 * time.Millisecond},
		{"1.5µs", 1500 * time.Nanosecond},
		{"9223372036854775807ns", time.Duration(1<<63 - 1)},
		{"1.us", time.Microsecond},
		{"1µs", time.Microsecond},
		{"1μs", time.Microsecond},
		{"+1s", time.Second},
		{"0", 0},
		{"-1s", -time.Second},
	} {
		t.Run(scenario.value, func(t *testing.T) {
			got, err := parseRateLimitPeriod(scenario.value)
			if err != nil || got != scenario.want {
				t.Fatalf("got duration %s, error %v; want %s", got, err, scenario.want)
			}
		})
	}
	for _, value := range []string{"1.5ns", "-1.5ns", "1.0000000001s", "0.5ns0.5ns", "NaN", "Inf", "2562048h", "", "1"} {
		t.Run("reject/"+value, func(t *testing.T) {
			if got, err := parseRateLimitPeriod(value); err == nil {
				t.Fatalf("unsupported duration %q silently became %s", value, got)
			}
		})
	}
}

func TestBDDConfiguredRateLimitGovernsActualServer(t *testing.T) {
	for _, scenario := range []struct {
		name       string
		args       []string
		capacity   int
		retryAfter int64
	}{
		{"defaults", nil, 1, 6},
		{"custom count period and burst", []string{"--rate-limit-requests=3", "--rate-limit-period=30m", "--rate-limit-capacity=2"}, 2, 600},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// Given the actual CLI, independent listeners and ephemeral actor
			// credentials, When clients consume the configured shared burst,
			// Then only admitted writes and their correct audit actors persist.
			data := t.TempDir()
			apiURL, healthURL, tokens, stop := givenRateLimitedServer(t, data, scenario.args)
			client := &http.Client{Timeout: 3 * time.Second}
			t.Cleanup(client.CloseIdleConnections)
			for range 3 {
				demoRequest(t, client, http.MethodGet, healthURL+"/health", "", http.StatusOK)
			}
			actors := []string{"alice@example.com", "bob@example.com"}
			var created []store.Contact
			started := time.Now()
			for i := range scenario.capacity {
				actor := actors[i%len(actors)]
				body := demoJSONRequest(t, client, http.MethodPost, apiURL+"/api/contacts", tokens[actor],
					strings.NewReader(fmt.Sprintf(`{"first_name":"Admission %d"}`, i)), http.StatusCreated)
				var contact store.Contact
				if err := json.Unmarshal(body, &contact); err != nil || contact.ID == "" || contact.OwnerID != actor || contact.FirstName != fmt.Sprintf("Admission %d", i) {
					t.Fatalf("Then admitted record must retain submitted fields and actor: %+v, %v", contact, err)
				}
				created = append(created, contact)
			}
			for _, actor := range actors {
				request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, apiURL+"/api/contacts", strings.NewReader(`{"first_name":"Rejected"}`))
				if err != nil {
					t.Fatal(err)
				}
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set("Authorization", "Bearer "+tokens[actor])
				response, err := client.Do(request)
				if err != nil {
					t.Fatal(err)
				}
				body, readErr := io.ReadAll(io.LimitReader(response.Body, 1<<20))
				if err := errors.Join(readErr, response.Body.Close()); err != nil {
					t.Fatal(err)
				}
				retry, err := strconv.ParseInt(response.Header.Get("Retry-After"), 10, 64)
				// Wall-clock integration allows elapsed request processing time;
				// exact nanosecond and refill boundaries use injected-clock tests.
				minimumRetry := max(1, scenario.retryAfter-int64(time.Since(started)/time.Second)-1)
				if response.StatusCode != http.StatusTooManyRequests || err != nil || retry < minimumRetry || retry > scenario.retryAfter || string(body) != "{\"error\":\"rate limit exceeded\"}\n" {
					t.Fatalf("Then both actors share configured overflow: status=%d, retry=%q, body=%q", response.StatusCode, response.Header.Get("Retry-After"), body)
				}
			}
			// Health still bypasses the exhausted business bucket.
			demoRequest(t, client, http.MethodGet, healthURL+"/health", "", http.StatusOK)
			stop()
			s, err := store.New(data)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := s.Close(); err != nil {
					t.Error(err)
				}
			})
			var contacts, audits int
			if err := errors.Join(s.DB().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM contacts").Scan(&contacts),
				s.DB().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM activity_log").Scan(&audits)); err != nil {
				t.Fatal(err)
			}
			if contacts != scenario.capacity || audits != scenario.capacity {
				t.Fatalf("Then rejected writes leave no record or audit: contacts=%d, audits=%d, want %d each", contacts, audits, scenario.capacity)
			}
			for _, contact := range created {
				stored, err := s.GetContact(t.Context(), contact.OwnerID, contact.ID)
				if err != nil || stored != contact {
					t.Fatalf("Then response must match persisted owned record: got %+v, want %+v, error=%v", stored, contact, err)
				}
				var actor string
				if err := s.DB().QueryRowContext(t.Context(), "SELECT user_id FROM activity_log WHERE action='create' AND resource_type='contact' AND resource_id=?", contact.ID).Scan(&actor); err != nil || actor != contact.OwnerID {
					t.Fatalf("Then audit preserves each admission's actor: got %q, want %q, error=%v", actor, contact.OwnerID, err)
				}
			}
		})
	}
}

func givenRateLimitedServer(t *testing.T, data string, policyArgs []string) (apiURL, healthURL string, tokens map[string]string, stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	output := &startupOutput{ready: make(chan struct{})}
	done := make(chan error, 1)
	args := append([]string{"--addr=127.0.0.1:0", "--data=" + data}, policyArgs...)
	go func() { done <- run(ctx, args, output) }()
	finished := false
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			if !finished {
				if err := await(t, done); err != nil {
					t.Errorf("Then configured server shutdown must succeed: %v", err)
				}
			}
		})
	}
	t.Cleanup(stop)
	select {
	case <-output.ready:
	case err := <-done:
		finished = true
		t.Fatalf("Given configured server startup failed: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("Given configured server startup timed out")
	}
	lines := strings.Split(strings.TrimSpace(output.snapshot()), "\n")
	actors, err := auth.ParseActors(auth.DefaultActors)
	if err != nil || len(lines) != 2+len(actors) {
		t.Fatal("Then startup must report two listeners and each configured actor")
	}
	var apiOK, healthOK bool
	apiURL, apiOK = strings.CutPrefix(lines[0], "Local demo API listening on ")
	healthURL, healthOK = strings.CutPrefix(lines[1], "Local demo health listening on ")
	if !apiOK || !healthOK || apiURL == healthURL {
		t.Fatal("Then configured server must expose separate listeners")
	}
	tokens = make(map[string]string, len(actors))
	for i, actor := range actors {
		credential, ok := strings.CutPrefix(lines[2+i], fmt.Sprintf("Local demo bearer token for %s (expires on restart): ", actor))
		if !ok || len(credential) != 43 {
			t.Fatal("Then configured server must provide each actor's credential")
		}
		tokens[actor] = credential
	}
	return apiURL, healthURL, tokens, stop
}
