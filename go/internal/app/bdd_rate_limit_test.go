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
		return rateLimit(next, func() time.Time {
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
	admitted = append(admitted, whenBDDConsumersCreate(t, consumers, "first"))
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
	admitted = append(admitted, whenBDDConsumersCreate(t, consumers, "second"))
	thenBDDCreationsAndAudits(t, world, admitted)
}

type bddAdmission struct{ contactID, actor string }

func whenBDDConsumersCreate(t *testing.T, consumers []bddWorld, wave string) bddAdmission {
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
	// unspecified; only the single shared allowance determines admission.
	close(ready)
	group.Wait()
	close(results)
	accepted, rejected := 0, 0
	var admitted bddAdmission
	for result := range results {
		if result.err != nil {
			t.Errorf("When consumer sends request: %v", result.err)
			continue
		}
		switch result.response.status {
		case http.StatusCreated:
			accepted++
			contact := decodeBDD[store.Contact](t, result.response)
			admitted = bddAdmission{contactID: contact.ID, actor: result.actor}
			if contact.ID == "" || contact.OwnerID != result.actor || !strings.HasPrefix(contact.FirstName, wave+" consumer ") {
				t.Errorf("Then admitted mutation returns its created contact: %+v", contact)
			}
		case http.StatusTooManyRequests:
			rejected++
			body := decodeBDD[map[string]string](t, result.response)
			if len(body) != 1 || body["error"] != "rate limit exceeded" || result.response.header.Get("Retry-After") != "6" || result.response.header.Get("Content-Type") != "application/json" || result.response.header.Get("Cache-Control") != "no-store" {
				t.Errorf("Then overflow returns the documented rejection: body=%v headers=%v", body, result.response.header)
			}
		default:
			t.Errorf("Then consumer gets either 201 or 429, got %d: %.1024s", result.response.status, result.response.body)
		}
	}
	if accepted != 1 || rejected != len(consumers)-1 {
		t.Fatalf("Then one global slot admits one consumer across all actors: accepted=%d rejected=%d, want 1 and %d", accepted, rejected, len(consumers)-1)
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
