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

func TestBDDActorRateLimitAcrossHTTPConsumers(t *testing.T) {
	// Given independent actor buckets, twelve HTTP consumers using alternating
	// actor credentials, and a separate public health listener.
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var elapsed atomic.Int64
	actors := []string{"alice@example.com", "bob@example.com"}
	world := givenBDDRateLimitedApp(t, actors, DefaultRateLimitConfig(), func() time.Time {
		return start.Add(time.Duration(elapsed.Load()))
	})
	const consumerCount = 12
	consumers := make([]bddWorld, consumerCount)
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
	// When the first API wave follows health requests, Then one write per actor persists.
	assertHealthAvailable(t)
	admitted = append(admitted, whenBDDConsumersCreate(t, consumers, "first", 1, "6")...)
	thenBDDCreationsAndAudits(t, world, admitted)

	// Given full actor buckets, When health is checked, Then health succeeds
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
	// Then exactly one more mutation per actor is admitted.
	elapsed.Store(int64(6 * time.Second))
	assertHealthAvailable(t)
	admitted = append(admitted, whenBDDConsumersCreate(t, consumers, "second", 1, "6")...)
	thenBDDCreationsAndAudits(t, world, admitted)
}

type bddAdmission struct{ contactID, actor string }

func TestBDDActorRateLimitSupportsMoreThan64Actors(t *testing.T) {
	// Given 128 real registered actors with independent credentials, When all
	// first requests race, Then each actor receives one admission and one audit.
	actors := make([]string, 128)
	for i := range actors {
		actors[i] = fmt.Sprintf("actor-%03d", i)
	}
	world := givenBDDRateLimitedApp(t, actors, DefaultRateLimitConfig(), func() time.Time { return time.Unix(0, 0) })
	consumers := make([]bddWorld, len(actors))
	for i, actor := range actors {
		consumers[i] = *world.forActor(t, actor)
	}
	admitted := whenBDDConsumersCreate(t, consumers, "many actors", 1, "6")
	thenBDDCreationsAndAudits(t, world, admitted)
	// When every actor retries immediately, Then none acquires extra allowance.
	whenBDDConsumersCreate(t, consumers, "full actors", 0, "6")
	thenBDDCreationsAndAudits(t, world, admitted)
}

func TestBDDActorRateLimitRefillsIndependentlyUnderNoisyNeighbor(t *testing.T) {
	var elapsed atomic.Int64
	start := time.Unix(0, 0)
	world := givenBDDRateLimitedApp(t, []string{"alice", "bob"}, DefaultRateLimitConfig(), func() time.Time {
		return start.Add(time.Duration(elapsed.Load()))
	})
	alice, bob := world.forActor(t, "alice"), world.forActor(t, "bob")
	var admitted []bddAdmission
	attempt := func(actor *bddWorld, want int, retry string) {
		t.Helper()
		response := actor.request(t, http.MethodPost, "/api/contacts", "application/json", strings.NewReader(`{"first_name":"Independent allowance"}`))
		thenStatus(t, response, want)
		if response.header.Get("Retry-After") != retry {
			t.Fatalf("Then actor %s has its own retry boundary: got %q, want %q", actor.actor, response.header.Get("Retry-After"), retry)
		}
		if want == http.StatusCreated {
			contact := decodeBDD[store.Contact](t, response)
			admitted = append(admitted, bddAdmission{contact.ID, actor.actor})
		}
	}
	// Given Alice uses her allowance at t=0 and keeps retrying, Bob can still
	// make his first write at t=2, establishing a different refill boundary.
	attempt(alice, http.StatusCreated, "")
	for range 12 {
		attempt(alice, http.StatusTooManyRequests, "6")
	}
	elapsed.Store(int64(2 * time.Second))
	attempt(bob, http.StatusCreated, "")
	elapsed.Store(int64(6 * time.Second))
	attempt(alice, http.StatusCreated, "")
	attempt(bob, http.StatusTooManyRequests, "2")
	elapsed.Store(int64(8 * time.Second))
	attempt(bob, http.StatusCreated, "")
	attempt(alice, http.StatusTooManyRequests, "4")
	thenBDDCreationsAndAudits(t, world, admitted)
}

func TestBDDActorRateLimitIsolatesAnonymousAbuse(t *testing.T) {
	world := givenBDDRateLimitedApp(t, []string{"alice", "bob"}, DefaultRateLimitConfig(), func() time.Time { return time.Unix(0, 0) })
	anonymous := *world
	anonymous.token = ""
	// Given a missing credential consumes the anonymous allowance, When many
	// different invalid/malformed credentials arrive, Then they share its 429.
	thenStatus(t, anonymous.request(t, http.MethodPost, "/api/contacts", "application/json", strings.NewReader(`{"first_name":"Unauthorized"}`)), http.StatusUnauthorized)
	for _, token := range []string{"invalid-one", "invalid-two", "malformed extra", "alice", "bob"} {
		anonymous.token = token
		response := anonymous.request(t, http.MethodPost, "/api/contacts", "application/json", strings.NewReader(`{"first_name":"Rejected anonymous"}`))
		thenStatus(t, response, http.StatusTooManyRequests)
		if response.header.Get("Retry-After") != "6" {
			t.Fatalf("Then anonymous attempts share their retry boundary: %q", response.header.Get("Retry-After"))
		}
	}
	// Then both authenticated actors retain their complete independent budget.
	consumers := []bddWorld{*world.forActor(t, "alice"), *world.forActor(t, "bob")}
	admitted := whenBDDConsumersCreate(t, consumers, "after anonymous abuse", 1, "6")
	whenBDDConsumersCreate(t, consumers, "actor retries", 0, "6")
	thenBDDCreationsAndAudits(t, world, admitted)
	healthServer := httptest.NewServer(NewHealthRouter())
	t.Cleanup(healthServer.Close)
	healthClient := healthServer.Client()
	healthClient.Timeout = 5 * time.Second
	t.Cleanup(healthClient.CloseIdleConnections)
	health := bddWorld{url: healthServer.URL, client: healthClient}
	thenStatus(t, health.request(t, http.MethodGet, "/health", "", nil), http.StatusOK)
}

func TestBDDConfiguredActorRateLimitAcrossHTTPConsumers(t *testing.T) {
	// Given twelve HTTP consumers, with a three-request burst and a fractional
	// one-third-second drain interval independently for each actor.
	var elapsed atomic.Int64
	start := time.Unix(0, 0)
	actors := []string{"alice@example.com", "bob@example.com"}
	world := givenBDDRateLimitedApp(t, actors, RateLimitConfig{Requests: 3, Period: time.Second, Capacity: 3}, func() time.Time {
		return start.Add(time.Duration(elapsed.Load()))
	})
	consumers := make([]bddWorld, 12)
	for i := range consumers {
		consumers[i] = *world.forActor(t, actors[i%len(actors)])
	}
	// When racing at the same instant, Then each actor receives exactly its
	// burst capacity through the real SQLite mutation/audit transaction.
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

func whenBDDConsumersCreate(t *testing.T, consumers []bddWorld, wave string, wantPerActor int, wantRetry string) []bddAdmission {
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
	// unspecified; each actor shares only its own allowance.
	close(ready)
	group.Wait()
	close(results)
	accepted, rejected := 0, 0
	acceptedByActor := make(map[string]int)
	for _, consumer := range consumers {
		acceptedByActor[consumer.actor] = 0
	}
	var admitted []bddAdmission
	for result := range results {
		if result.err != nil {
			t.Errorf("When consumer sends request: %v", result.err)
			continue
		}
		switch result.response.status {
		case http.StatusCreated:
			accepted++
			acceptedByActor[result.actor]++
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
	for actor, count := range acceptedByActor {
		if count != wantPerActor {
			t.Errorf("Then each actor receives its own allowance: actor=%s accepted=%d want=%d", actor, count, wantPerActor)
		}
	}
	wantTotal := wantPerActor * len(acceptedByActor)
	if accepted != wantTotal || rejected != len(consumers)-wantTotal {
		t.Fatalf("Then per-actor allowances determine admissions: accepted=%d rejected=%d, want %d and %d", accepted, rejected, wantTotal, len(consumers)-wantTotal)
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
	wantByActor := make(map[string]int)
	for _, admission := range admitted {
		wantByActor[admission.actor]++
	}
	for actor := range world.tokens {
		var actorContacts, actorAudits int
		if err := world.store.DB().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM contacts WHERE owner_id=?", actor).Scan(&actorContacts); err != nil {
			t.Fatal(err)
		}
		if err := world.store.DB().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM activity_log WHERE user_id=?", actor).Scan(&actorAudits); err != nil {
			t.Fatal(err)
		}
		if actorContacts != wantByActor[actor] || actorAudits != wantByActor[actor] {
			t.Fatalf("Then actor %s persists only its admissions: contacts=%d audits=%d want=%d", actor, actorContacts, actorAudits, wantByActor[actor])
		}
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
