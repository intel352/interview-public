package app

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/weave-lab/interview-public/go/internal/store"
)

func TestBDDGlobalRateLimitAcrossHTTPConsumers(t *testing.T) {
	// Given one business API bucket, twelve independent HTTP consumers using
	// alternating actor credentials, and a separate public health listener.
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var elapsed atomic.Int64
	world := givenBDDApp(t, func(next http.Handler) http.Handler {
		return defaultRateLimit(t, next, func() time.Time {
			return start.Add(time.Duration(elapsed.Load()))
		})
	})
	const consumerCount = 12
	consumers := make([]bddWorld, consumerCount)
	actors := []string{"alice@example.com", "bob@example.com"}
	for i := range consumers {
		consumers[i] = *world.forActor(t, actors[i%len(actors)])
	}
	var admitted []bddAdmission
	healthServer := httptest.NewServer(NewHealthRouter())
	t.Cleanup(healthServer.Close)
	healthClient := healthServer.Client()
	healthClient.Timeout = 5 * time.Second
	t.Cleanup(healthClient.CloseIdleConnections)
	health := bddWorld{url: healthServer.URL, client: healthClient}

	assertHealthAvailable := func(t *testing.T) {
		t.Helper()
		for range 3 {
			response := health.request(t, http.MethodGet, "/health", "", nil)
			thenStatus(t, response, http.StatusOK)
			if string(response.body) != "ok" || response.header.Get("Retry-After") != "" {
				t.Fatalf("Then health remains available without retry delay: body=%q Retry-After=%q", response.body, response.header.Get("Retry-After"))
			}
		}
	}

	// These are sequential phases of one scenario, not independently selectable
	// subtests: the second phase depends on the first wave filling the bucket.
	// When the first API wave follows health requests, Then one write persists.
	assertHealthAvailable(t)
	admitted = append(admitted, whenBDDConsumersCreate(t, consumers, "first", 1, "6")...)
	thenBDDCreationsAndAudits(t, world, admitted)

	// Given a full API bucket, When health is checked, Then health succeeds
	// without changing the next business admission boundary.
	assertHealthAvailable(t)
	// The wrong listener exposes no business API, even with a valid token.
	health.token = world.token
	response := health.request(t, http.MethodGet, "/api/contacts", "", nil)
	thenStatus(t, response, http.StatusNotFound)

	// When the clock is one nanosecond short of draining, rejection still
	// rounds the remaining duration up to one second and changes no state.
	elapsed.Store(int64(6*time.Second - time.Nanosecond))
	response = consumers[0].request(t, http.MethodPost, "/api/contacts", "application/json", strings.NewReader(`{"first_name":"Too early"}`))
	thenStatus(t, response, http.StatusTooManyRequests)
	if response.header.Get("Retry-After") != "1" {
		t.Fatalf("Then fractional drain time rounds up: Retry-After=%q", response.header.Get("Retry-After"))
	}
	thenBDDCreationsAndAudits(t, world, admitted)

	// Given exactly six seconds, When health and another API wave arrive,
	// Then exactly one more mutation is admitted across all actors.
	elapsed.Store(int64(6 * time.Second))
	assertHealthAvailable(t)
	admitted = append(admitted, whenBDDConsumersCreate(t, consumers, "second", 1, "6")...)
	thenBDDCreationsAndAudits(t, world, admitted)
}

type bddAdmission struct{ contactID, actor string }

func TestBDDConfiguredGlobalRateLimitAcrossHTTPConsumers(t *testing.T) {
	// Given twelve actual HTTP consumers sharing a three-request burst and
	// a fractional one-third-second drain interval across two actor identities.
	var elapsed atomic.Int64
	start := time.Unix(0, 0)
	world := givenBDDApp(t, func(next http.Handler) http.Handler {
		handler, err := rateLimit(next, RateLimitConfig{Requests: 3, Period: time.Second, Capacity: 3}, func() time.Time {
			return start.Add(time.Duration(elapsed.Load()))
		})
		if err != nil {
			t.Fatal(err)
		}
		return handler
	})
	consumers := make([]bddWorld, 12)
	actors := []string{"alice@example.com", "bob@example.com"}
	for i := range consumers {
		consumers[i] = *world.forActor(t, actors[i%len(actors)])
	}
	// When racing at the same instant, Then only the burst capacity reaches
	// the real SQLite mutation/audit transaction, regardless of actor.
	admitted := whenBDDConsumersCreate(t, consumers, "burst", 3, "1")
	thenBDDCreationsAndAudits(t, world, admitted)
	elapsed.Store(333333333)
	whenBDDConsumersCreate(t, consumers, "too early", 0, "1")
	thenBDDCreationsAndAudits(t, world, admitted)
	// When enough fractional work drains, Then one slot, not a whole new
	// burst, becomes available; rejection never advances the deadline.
	elapsed.Store(333333334)
	admitted = append(admitted, whenBDDConsumersCreate(t, consumers, "refilled", 1, "1")...)
	thenBDDCreationsAndAudits(t, world, admitted)
	// When long idle empties the bucket, Then capacity is restored but not exceeded.
	elapsed.Store(int64(time.Hour))
	admitted = append(admitted, whenBDDConsumersCreate(t, consumers, "after idle", 3, "1")...)
	thenBDDCreationsAndAudits(t, world, admitted)
}

func whenBDDConsumersCreate(t *testing.T, consumers []bddWorld, wave string, wantAccepted int, wantRetry string) []bddAdmission {
	t.Helper()
	type outcome struct {
		response bddResponse
		actor    string
		err      error
	}
	results := make(chan outcome, len(consumers))
	ready := make(chan struct{})
	var group sync.WaitGroup
	for i := range consumers {
		group.Go(func() {
			<-ready
			body := fmt.Sprintf(`{"first_name":"%s consumer %d"}`, wave, i)
			response, err := consumers[i].requestResult(t.Context(), http.MethodPost, "/api/contacts", "application/json", strings.NewReader(body))
			results <- outcome{response: response, actor: consumers[i].actor, err: err}
		})
	}
	// When all consumers are released together, request order is deliberately
	// unspecified; only the shared allowance determines admission.
	close(ready)
	group.Wait()
	close(results)
	accepted, rejected := 0, 0
	var admitted []bddAdmission
	for result := range results {
		if result.err != nil {
			t.Errorf("When consumer sends request: %v", result.err)
			continue
		}
		switch result.response.status {
		case http.StatusCreated:
			accepted++
			contact := decodeBDD[store.Contact](t, result.response)
			admitted = append(admitted, bddAdmission{contactID: contact.ID, actor: result.actor})
			if contact.ID == "" || contact.OwnerID != result.actor || !strings.HasPrefix(contact.FirstName, wave+" consumer ") {
				t.Errorf("Then admitted mutation returns its created contact: %+v", contact)
			}
		case http.StatusTooManyRequests:
			rejected++
			body := decodeBDD[map[string]string](t, result.response)
			if len(body) != 1 || body["error"] != "rate limit exceeded" || result.response.header.Get("Retry-After") != wantRetry || result.response.header.Get("Content-Type") != "application/json" || result.response.header.Get("Cache-Control") != "no-store" {
				t.Errorf("Then overflow returns the documented rejection: body=%v headers=%v", body, result.response.header)
			}
		default:
			t.Errorf("Then consumer gets either 201 or 429, got %d: %.1024s", result.response.status, result.response.body)
		}
	}
	if accepted != wantAccepted || rejected != len(consumers)-wantAccepted {
		t.Fatalf("Then the global allowance is shared across all actors: accepted=%d rejected=%d, want %d and %d", accepted, rejected, wantAccepted, len(consumers)-wantAccepted)
	}
	return admitted
}

func thenBDDCreationsAndAudits(t *testing.T, world *bddWorld, admitted []bddAdmission) {
	t.Helper()
	contacts, err := world.store.CountAllContacts(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var audits int
	if err := world.store.DB().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM activity_log").Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if contacts != len(admitted) || audits != len(admitted) {
		t.Fatalf("Then only admitted writes persist: contacts=%d audits=%d, want %d", contacts, audits, len(admitted))
	}
	for _, admission := range admitted {
		var owner, actor, action string
		err := world.store.DB().QueryRowContext(t.Context(), `
			SELECT c.owner_id, a.user_id, a.action
			FROM contacts c JOIN activity_log a ON a.resource_id = c.id
			WHERE c.id = ?
		`, admission.contactID).Scan(&owner, &actor, &action)
		if err != nil {
			t.Fatal(err)
		}
		if owner != admission.actor || actor != admission.actor || action != "create" {
			t.Fatalf("Then the winning actor owns and audits the admitted contact: owner=%q actor=%q action=%q, want %q/%q/create", owner, actor, action, admission.actor, admission.actor)
		}
	}
}
