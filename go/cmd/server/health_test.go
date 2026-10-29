package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/weave-lab/interview-public/go/internal/auth"
	"github.com/weave-lab/interview-public/go/internal/store"
)

const scenarioActor = "alice@example.com"

func TestBDDListenerAddressSelection(t *testing.T) {
	// Given numeric loopback API addresses, When health is not overridden,
	// Then ordinary ports increment and ephemeral ports remain OS allocated.
	for _, scenario := range []struct{ api, override, health string }{
		{"127.0.0.1:8080", "", "127.0.0.1:8081"},
		{"[::1]:8080", "", "[::1]:8081"},
		{"127.0.0.1:0", "", "127.0.0.1:0"},
		{"[::1]:0", "", "[::1]:0"},
		{"127.0.0.1:65535", "127.0.0.1:0", "127.0.0.1:0"},
		{"127.0.0.1:8080", "[::1]:9090", "[::1]:9090"},
	} {
		t.Run(scenario.api+"/health="+scenario.override, func(t *testing.T) {
			api, health, err := listenerAddresses(scenario.api, scenario.override)
			if err != nil || api != scenario.api || health != scenario.health {
				t.Fatalf("Then addresses=(%q,%q), error=%v; want (%q,%q)", api, health, err, scenario.api, scenario.health)
			}
		})
	}
	for _, scenario := range []struct{ api, health string }{
		{"127.0.0.1:65535", ""},
		{"127.0.0.1:65536", ""},
		{"127.0.0.1:8080", "0.0.0.0:8081"},
		{"127.0.0.1:8080", "localhost:8081"},
		{"[::]:8080", "[::1]:8081"},
		{"127.0.0.1:8080", "127.0.0.1:-1"},
		{"127.0.0.1:8080", "127.0.0.1:8080"},
	} {
		t.Run("invalid/"+scenario.api+"/health="+scenario.health, func(t *testing.T) {
			if _, _, err := listenerAddresses(scenario.api, scenario.health); err == nil {
				t.Fatal("Then invalid, overflowing, or colliding addresses must fail")
			}
		})
	}
}

func TestBDDHealthConfigurationFailsBeforeOpeningData(t *testing.T) {
	for _, args := range [][]string{
		{"--addr=127.0.0.1:65535"},
		{"--health-addr=0.0.0.0:8081"},
		{"--health-addr=127.0.0.1:8080"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			// Given invalid health configuration and a nonexistent data directory,
			// When startup runs, Then it must fail before data or token output.
			data := filepath.Join(t.TempDir(), "must-not-create")
			var output bytes.Buffer
			err := run(t.Context(), append(args, "--data="+data), &output)
			if err == nil {
				t.Fatal("Then startup must fail")
			}
			if _, err := os.Stat(data); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("Then data must remain absent: %v", err)
			}
			if strings.Contains(output.String(), "bearer token") {
				t.Fatal("Then no credential may be printed")
			}
		})
	}
}

func TestBDDBindFailureClosesOtherListener(t *testing.T) {
	// Given an occupied health port and a free API port, When binding fails,
	// Then the partially opened API listener is closed and can be rebound.
	health := testListener(t)
	api := testListener(t)
	apiAddress := api.Addr().String()
	if err := api.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := listenPair(apiAddress, health.Addr().String()); err == nil {
		t.Fatal("Then the occupied health port must fail startup")
	}
	rebound, err := net.Listen("tcp", apiAddress)
	if err != nil {
		t.Fatalf("Then API port must have been released: %v", err)
	}
	if err := rebound.Close(); err != nil {
		t.Fatal(err)
	}
	// An occupied API port fails before a health listener is acquired.
	if _, _, err := listenPair(health.Addr().String(), "127.0.0.1:0"); err == nil {
		t.Fatal("Then the occupied API port must fail startup")
	}
}

func TestBDDEphemeralListenersAreSeparate(t *testing.T) {
	// Given two ephemeral addresses, When both are bound, Then they have
	// distinct actual ports and both resources can be closed.
	for _, address := range []string{"127.0.0.1:0", "[::1]:0"} {
		t.Run(address, func(t *testing.T) {
			api, health, err := listenPair(address, address)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := errors.Join(api.Close(), health.Close()); err != nil {
					t.Error(err)
				}
			}()
			if api.Addr().String() == health.Addr().String() {
				t.Fatal("Then API and health must not share a listener")
			}
		})
	}
}

func TestBDDHealthBindFailurePrintsNoCredential(t *testing.T) {
	// Given an occupied health port, When the complete startup path runs,
	// Then binding fails before any startup credential or URL is printed.
	health := testListener(t)
	var output bytes.Buffer
	err := run(t.Context(), []string{"--addr=127.0.0.1:0", "--health-addr=" + health.Addr().String(), "--data=" + givenSeededDirectory(t)}, &output)
	if err == nil || !strings.Contains(err.Error(), "listen health") {
		t.Fatalf("Then health bind failure must propagate: %v", err)
	}
	if output.Len() != 0 {
		t.Fatal("Then unsuccessful binding must produce no credential or startup URLs")
	}
}

func TestBDDHelpDoesNotOpenResources(t *testing.T) {
	// Given a help invocation, When flags are parsed, Then both listener flags
	// are described without opening a data directory or printing credentials.
	data := filepath.Join(t.TempDir(), "absent")
	var output bytes.Buffer
	if err := run(t.Context(), []string{"--data=" + data, "--help"}, &output); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("Then help result must be distinguishable: %v", err)
	}
	for _, name := range []string{"-addr", "-health-addr", "-actors"} {
		if !strings.Contains(output.String(), name) {
			t.Fatalf("Then help must describe %s", name)
		}
	}
	if _, err := os.Stat(data); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Then help must not open data: %v", err)
	}
}

func TestBDDCanceledStartupDoesNotOpenResources(t *testing.T) {
	// Given cancellation before startup, When valid flags are supplied,
	// Then neither data creation nor credential output should occur.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	data := filepath.Join(t.TempDir(), "absent")
	var output bytes.Buffer
	if err := run(ctx, []string{"--addr=127.0.0.1:0", "--data=" + data}, &output); !errors.Is(err, context.Canceled) {
		t.Fatalf("Then cancellation must propagate: %v", err)
	}
	if _, err := os.Stat(data); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Then canceled startup must not create data: %v", err)
	}
	if output.Len() != 0 {
		t.Fatal("Then canceled startup must not print credentials")
	}
}

func TestBDDServersDrainTogether(t *testing.T) {
	// Given an active request on each listener, When shutdown begins, Then
	// both stop accepting while both active requests retain their grace period.
	api, health := testListener(t), testListener(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	entered := make(chan context.Context, 2)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- r.Context()
		<-release
		w.WriteHeader(http.StatusNoContent)
	})
	served := make(chan error, 1)
	go func() { served <- serveBoth(ctx, api, health, handler, handler, time.Second) }()
	clients := make(chan error, 2)
	go func() { clients <- requestHealth(api.Addr().String()) }()
	go func() { clients <- requestHealth(health.Addr().String()) }()
	first, second := await(t, entered), await(t, entered)
	cancel()
	await(t, api.closed)
	await(t, health.closed)
	if first.Err() != nil || second.Err() != nil {
		t.Fatal("Then graceful shutdown must not prematurely cancel either request")
	}
	unblock()
	for range 2 {
		if err := await(t, clients); err != nil {
			t.Fatal(err)
		}
	}
	if err := await(t, served); err != nil {
		t.Fatal(err)
	}
}

func TestBDDServerFailureStopsPeer(t *testing.T) {
	for _, failed := range []string{"API", "health"} {
		t.Run(failed, func(t *testing.T) {
			// Given one failed listener and an active peer, When serving starts,
			// Then both loops terminate and the healthy peer releases its port.
			api, health := testListener(t), testListener(t)
			broken, peer := api, health
			if failed == "health" {
				broken, peer = health, api
			}
			if err := broken.Close(); err != nil {
				t.Fatal(err)
			}
			served := make(chan error, 1)
			go func() {
				served <- serveBoth(t.Context(), api, health, http.NotFoundHandler(), http.NotFoundHandler(), time.Second)
			}()
			if err := await(t, served); !errors.Is(err, net.ErrClosed) {
				t.Fatalf("Then failure must propagate: %v", err)
			}
			await(t, peer.closed)
		})
	}
}

func TestBDDServersForceCloseTogether(t *testing.T) {
	// Given active handlers waiting for cancellation on both listeners,
	// When their common shutdown grace expires, Then both contexts and loops end.
	api, health := testListener(t), testListener(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	entered, finished := make(chan struct{}, 2), make(chan struct{}, 2)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		<-r.Context().Done()
		finished <- struct{}{}
	})
	served := make(chan error, 1)
	go func() { served <- serveBoth(ctx, api, health, handler, handler, 20*time.Millisecond) }()
	clients := make(chan error, 2)
	go func() { clients <- requestHealth(api.Addr().String()) }()
	go func() { clients <- requestHealth(health.Addr().String()) }()
	for range 2 {
		await(t, entered)
	}
	cancel()
	if err := await(t, served); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Then forced shutdown must report its deadline: %v", err)
	}
	for range 2 {
		await(t, finished)
		if err := await(t, clients); err != nil {
			var networkError net.Error
			if errors.As(err, &networkError) && networkError.Timeout() {
				t.Fatalf("Then clients must stop on forced close, not time out: %v", err)
			}
		}
	}
	await(t, api.closed)
	await(t, health.closed)
}

type gatedFailureListener struct {
	net.Listener
	fail <-chan struct{}
}

func (l *gatedFailureListener) Accept() (net.Conn, error) {
	<-l.fail
	return nil, errAcceptFailed
}

func TestBDDServerFailureCancelsActivePeerAfterGrace(t *testing.T) {
	for _, failed := range []string{"API", "health"} {
		t.Run(failed, func(t *testing.T) {
			// Given an active peer handler, When the other listener fails,
			// Then the peer receives bounded grace and cancellation, and both
			// the original failure and forced-shutdown deadline reach the caller.
			api, health := testListener(t), testListener(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			fail := make(chan struct{})
			var once sync.Once
			triggerFailure := func() { once.Do(func() { close(fail) }) }
			defer triggerFailure()
			var apiListener, healthListener net.Listener = api, health
			peer := health
			if failed == "API" {
				apiListener = &gatedFailureListener{Listener: api, fail: fail}
			} else {
				healthListener = &gatedFailureListener{Listener: health, fail: fail}
				peer = api
			}
			entered, finished := make(chan struct{}), make(chan struct{})
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(entered)
				<-r.Context().Done()
				close(finished)
			})
			served := make(chan error, 1)
			go func() { served <- serveBoth(ctx, apiListener, healthListener, handler, handler, 20*time.Millisecond) }()
			client := make(chan error, 1)
			go func() { client <- requestHealth(peer.Addr().String()) }()
			await(t, entered)
			triggerFailure()
			if err := await(t, served); !errors.Is(err, errAcceptFailed) || !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("Then listener and forced-shutdown errors must both propagate: %v", err)
			}
			await(t, finished)
			if err := await(t, client); err != nil {
				var networkError net.Error
				if errors.As(err, &networkError) && networkError.Timeout() {
					t.Fatalf("Then peer client must observe closure before its timeout: %v", err)
				}
			}
			await(t, api.closed)
			await(t, health.closed)
		})
	}
}

type startupOutput struct {
	mu     sync.Mutex
	buffer bytes.Buffer
	ready  chan struct{}
	once   sync.Once
}

func (s *startupOutput) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, err := s.buffer.Write(p)
	if strings.Contains(s.buffer.String(), "Local demo bearer token for ") {
		s.once.Do(func() { close(s.ready) })
	}
	return n, err
}

func (s *startupOutput) snapshot() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buffer.String()
}

func givenSeededDirectory(t *testing.T) string {
	t.Helper()
	data := t.TempDir()
	s, err := store.New(data)
	if err != nil {
		t.Fatal(err)
	}
	contact := store.Contact{ID: "scenario-contact", FirstName: "Synthetic"}
	actors, err := auth.ParseActors(auth.DefaultActors)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterActors(t.Context(), actors); err != nil {
		t.Fatal(errors.Join(err, s.Close()))
	}
	err = errors.Join(s.CreateContact(t.Context(), scenarioActor, &contact), s.Close())
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func givenRunningDemo(t *testing.T, data string) (apiURL, healthURL, token string, stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	output := &startupOutput{ready: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- run(ctx, []string{"--addr=127.0.0.1:0", "--data=" + data}, output) }()
	finished := false
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			if !finished {
				if err := await(t, done); err != nil {
					t.Errorf("Then demo shutdown must succeed: %v", err)
				}
			}
		})
	}
	t.Cleanup(stop)
	select {
	case <-output.ready:
	case err := <-done:
		finished = true
		t.Fatalf("Given demo startup failed: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("Given demo startup timed out")
	}
	lines := strings.Split(strings.TrimSpace(output.snapshot()), "\n")
	actors, err := auth.ParseActors(auth.DefaultActors)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2+len(actors) {
		t.Fatal("Then startup must report two listeners and one credential per actor")
	}
	var apiOK, healthOK bool
	apiURL, apiOK = strings.CutPrefix(lines[0], "Local demo API listening on ")
	healthURL, healthOK = strings.CutPrefix(lines[1], "Local demo health listening on ")
	if !apiOK || !healthOK || apiURL == healthURL {
		t.Fatal("Then startup must provide separate listener URLs")
	}
	seenTokens := make(map[string]bool)
	for i, actor := range actors {
		credential, ok := strings.CutPrefix(lines[2+i], fmt.Sprintf("Local demo bearer token for %s (expires on restart): ", actor))
		if !ok || len(credential) != 43 || seenTokens[credential] {
			t.Fatal("Then startup must provide a unique generated credential per actor, in configured order")
		}
		seenTokens[credential] = true
		if actor == scenarioActor {
			token = credential
		}
	}
	if token == "" {
		t.Fatal("Given scenario actor must be present in the configured actors")
	}
	return apiURL, healthURL, token, stop
}

func demoRequest(t *testing.T, client *http.Client, method, url, token string, want int) []byte {
	t.Helper()
	return demoJSONRequest(t, client, method, url, token, nil, want)
}

func demoJSONRequest(t *testing.T, client *http.Client, method, url, token string, body io.Reader, want int) []byte {
	t.Helper()
	r, err := http.NewRequestWithContext(t.Context(), method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	responseBody, readErr := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err := errors.Join(readErr, response.Body.Close()); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != want {
		t.Fatalf("Then %s %s status = %d, want %d", method, url, response.StatusCode, want)
	}
	return responseBody
}

func TestBDDHealthIsIndependentOfAPILimit(t *testing.T) {
	// Given the actual startup path with two listeners and seeded SQLite,
	// When health is polled repeatedly, Then the API's first allowance remains.
	apiURL, healthURL, token, _ := givenRunningDemo(t, givenSeededDirectory(t))
	client := &http.Client{Timeout: 3 * time.Second}
	t.Cleanup(client.CloseIdleConnections)
	for range 12 {
		if body := demoRequest(t, client, http.MethodGet, healthURL+"/health", "", http.StatusOK); string(body) != "ok" {
			t.Fatalf("Then health body = %q, want ok", body)
		}
	}
	demoRequest(t, client, http.MethodGet, apiURL+"/api/contacts", token, http.StatusOK)
	demoRequest(t, client, http.MethodGet, apiURL+"/api/contacts", token, http.StatusTooManyRequests)
	// When the API bucket is full, Then health is still available without auth.
	demoRequest(t, client, http.MethodGet, healthURL+"/health", "", http.StatusOK)
}

func TestBDDListenersExposeOnlyTheirOwnRoutes(t *testing.T) {
	// Given separate listeners, When requests target the wrong one, Then no
	// business data is exposed by health and API does not provide health.
	apiURL, healthURL, token, _ := givenRunningDemo(t, givenSeededDirectory(t))
	client := &http.Client{Timeout: 3 * time.Second}
	t.Cleanup(client.CloseIdleConnections)
	demoRequest(t, client, http.MethodGet, apiURL+"/health", token, http.StatusNotFound)
	demoRequest(t, client, http.MethodGet, healthURL+"/api/contacts", token, http.StatusNotFound)
	demoRequest(t, client, http.MethodPost, healthURL+"/health", "", http.StatusMethodNotAllowed)
}

func TestBDDRestartChangesCredentialAndKeepsData(t *testing.T) {
	// Given a contact created through the real authenticated startup path,
	// When the server restarts, Then the new credential can read the same
	// persisted record, and a credential from the previous run is rejected.
	data := t.TempDir()
	apiURL, _, oldToken, stop := givenRunningDemo(t, data)
	client := &http.Client{Timeout: 3 * time.Second}
	t.Cleanup(client.CloseIdleConnections)
	createdBody := demoJSONRequest(t, client, http.MethodPost, apiURL+"/api/contacts", oldToken,
		strings.NewReader(`{"first_name":"Persisted through restart","email":"alice@example.com"}`), http.StatusCreated)
	var created store.Contact
	if err := json.Unmarshal(createdBody, &created); err != nil || created.ID == "" || created.OwnerID != scenarioActor {
		t.Fatalf("Then create must return the authenticated actor's persisted record: %+v, %v", created, err)
	}
	stop()
	apiURL, healthURL, newToken, stop := givenRunningDemo(t, data)
	if oldToken == newToken {
		t.Fatal("Then restart must generate a fresh credential")
	}
	body := demoRequest(t, client, http.MethodGet, apiURL+"/api/contacts/"+created.ID, newToken, http.StatusOK)
	var restored store.Contact
	if err := json.Unmarshal(body, &restored); err != nil || restored != created {
		t.Fatalf("Then the new credential must restore access to the exact record: got %+v, want %+v, error %v", restored, created, err)
	}
	demoRequest(t, client, http.MethodGet, healthURL+"/health", "", http.StatusOK)
	stop()
	// A further real restart resets actor and anonymous allowances without sleeps. Its
	// first request proves a previously valid token is rejected by auth itself,
	// rather than merely receiving 429 from an already full limiter.
	apiURL, _, _, stop = givenRunningDemo(t, data)
	demoRequest(t, client, http.MethodGet, apiURL+"/api/contacts/"+created.ID, newToken, http.StatusUnauthorized)
	stop()

	s, err := store.New(data)
	if err != nil {
		t.Fatal(err)
	}
	stored, readErr := s.GetContact(t.Context(), scenarioActor, created.ID)
	var createAudits, allAudits int
	auditErr := s.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM activity_log WHERE user_id=? AND action='create' AND resource_type='contact' AND resource_id=?`, scenarioActor, created.ID).Scan(&createAudits)
	allAuditErr := s.DB().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM activity_log").Scan(&allAudits)
	if err := errors.Join(readErr, auditErr, allAuditErr, s.Close()); err != nil {
		t.Fatal(err)
	}
	if stored.ID != created.ID || stored.OwnerID != scenarioActor || stored.FirstName != created.FirstName || createAudits != 1 || allAudits != 1 {
		t.Fatalf("Then record and its sole creation audit must persist across restarts: stored=%+v, create audits=%d, all audits=%d", stored, createAudits, allAudits)
	}
}

type failedStartupOutput struct{}

var errStartupOutput = errors.New("synthetic closed startup output")

func (failedStartupOutput) Write([]byte) (int, error) { return 0, errStartupOutput }

func TestBDDStartupOutputFailureReleasesBothPorts(t *testing.T) {
	// Given valid ports but a broken operator output pipe, When startup cannot
	// deliver its credential, Then neither listener remains open.
	data := givenSeededDirectory(t)
	api, health := testListener(t), testListener(t)
	apiAddress, healthAddress := api.Addr().String(), health.Addr().String()
	if err := errors.Join(api.Close(), health.Close()); err != nil {
		t.Fatal(err)
	}
	err := run(t.Context(), []string{"--addr=" + apiAddress, "--health-addr=" + healthAddress, "--data=" + data}, failedStartupOutput{})
	if !errors.Is(err, errStartupOutput) {
		t.Fatalf("Then startup output failure must propagate: %v", err)
	}
	first, second, err := listenPair(apiAddress, healthAddress)
	if err != nil {
		t.Fatalf("Then both ports must be reusable: %v", err)
	}
	if err := errors.Join(first.Close(), second.Close()); err != nil {
		t.Fatal(err)
	}
}

func TestBDDCLIValidationAndSeeding(t *testing.T) {
	t.Run("invalid_arguments_do_not_create_data", func(t *testing.T) {
		for _, flag := range []string{"--contacts=-1", "--files=-1", "--actors=", "--unknown", "unexpected-argument"} {
			data := filepath.Join(t.TempDir(), "absent")
			var output bytes.Buffer
			if err := run(t.Context(), []string{"--data=" + data, flag}, &output); err == nil {
				t.Fatalf("Then %s must fail validation", flag)
			}
			if _, err := os.Stat(data); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("Then validation must precede data creation: %v", err)
			}
		}
	})
	t.Run("fresh_database_starts_without_seed_data", func(t *testing.T) {
		// Given a new database, When startup creates its schema, Then zero
		// contacts are a usable state rather than an initialization failure.
		apiURL, _, token, _ := givenRunningDemo(t, t.TempDir())
		client := &http.Client{Timeout: 3 * time.Second}
		t.Cleanup(client.CloseIdleConnections)
		body := demoRequest(t, client, http.MethodGet, apiURL+"/api/contacts", token, http.StatusOK)
		var result struct{ Contacts []store.Contact }
		if err := json.Unmarshal(body, &result); err != nil || len(result.Contacts) != 0 {
			t.Fatalf("Then new workspace must list zero contacts: %v", err)
		}
	})
	t.Run("seeding_is_explicit_and_repeat_safe", func(t *testing.T) {
		// Given an empty private directory, When seeded twice, Then one contact
		// remains and both invocations complete successfully without duplication.
		data := t.TempDir()
		args := []string{"--seed", "--contacts=1", "--files=0", "--data=" + data}
		var first, second bytes.Buffer
		if err := run(t.Context(), args, &first); err != nil {
			t.Fatal(err)
		}
		if err := run(t.Context(), args, &second); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(first.String(), "Done seeding") || !strings.Contains(second.String(), "Done seeding") {
			t.Fatal("Then initial and repeated seeding must report completion")
		}
		s, err := store.New(data)
		if err != nil {
			t.Fatal(err)
		}
		count, countErr := s.CountAllContacts(t.Context())
		files, filesErr := s.ListFiles(t.Context(), scenarioActor)
		if err := errors.Join(countErr, filesErr, s.Close()); err != nil || count != 1 || len(files) != 0 {
			t.Fatalf("Then seeded contacts=%d files=%d error=%v; want 1, 0", count, len(files), err)
		}
	})
	t.Run("zero_contact_seed_still_starts", func(t *testing.T) {
		// Given an explicit zero-contact seed, When normal startup follows,
		// Then the empty dataset does not masquerade as an uninitialized store.
		data := t.TempDir()
		var output bytes.Buffer
		if err := run(t.Context(), []string{"--seed", "--contacts=0", "--files=0", "--data=" + data}, &output); err != nil {
			t.Fatal(err)
		}
		apiURL, _, token, _ := givenRunningDemo(t, data)
		client := &http.Client{Timeout: 3 * time.Second}
		t.Cleanup(client.CloseIdleConnections)
		demoRequest(t, client, http.MethodGet, apiURL+"/api/contacts", token, http.StatusOK)
	})
}

func TestBDDDeletingLastContactDoesNotBlockRestartOrFiles(t *testing.T) {
	// Given one contact and an independently stored file for the same actor,
	// When the last contact is deleted and the server restarts, Then files and
	// new contact creation remain available. Restarts reset the real limiter,
	// keeping this startup regression independent of wall-clock sleeps.
	data := givenSeededDirectory(t)
	s, err := store.New(data)
	if err != nil {
		t.Fatal(err)
	}
	file := store.File{ID: "retained-file", Filename: "retained.txt", ContentType: "text/plain"}
	if err := errors.Join(s.CreateFile(t.Context(), scenarioActor, &file, strings.NewReader("still available")), s.Close()); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: 3 * time.Second}
	t.Cleanup(client.CloseIdleConnections)
	apiURL, _, token, stop := givenRunningDemo(t, data)
	demoRequest(t, client, http.MethodDelete, apiURL+"/api/contacts/scenario-contact", token, http.StatusNoContent)
	stop()

	apiURL, _, token, stop = givenRunningDemo(t, data)
	if body := demoRequest(t, client, http.MethodGet, apiURL+"/api/files/retained-file", token, http.StatusOK); string(body) != "still available" {
		t.Fatal("Then deleting the last contact must not remove file access")
	}
	stop()

	apiURL, _, token, _ = givenRunningDemo(t, data)
	r, err := http.NewRequestWithContext(t.Context(), http.MethodPost, apiURL+"/api/contacts", strings.NewReader(`{"first_name":"New after restart"}`))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", "application/json")
	response, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	_, readErr := io.Copy(io.Discard, response.Body)
	if err := errors.Join(readErr, response.Body.Close()); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("Then the empty restarted workspace must accept new contacts: status %d", response.StatusCode)
	}
}

func TestBDDCLISeedingDoesNotLeakActorRegistration(t *testing.T) {
	t.Run("failed_seed_rolls_back_new_actors", func(t *testing.T) {
		// Given a synthetic database failure during seeding, When the CLI seeds
		// a new actor, Then the actor registration rolls back with the dataset.
		data := t.TempDir()
		s, err := store.New(data)
		if err != nil {
			t.Fatal(err)
		}
		_, triggerErr := s.DB().ExecContext(t.Context(), `CREATE TRIGGER fail_contact BEFORE INSERT ON contacts BEGIN SELECT RAISE(FAIL, 'synthetic seed failure'); END`)
		if err := errors.Join(triggerErr, s.Close()); err != nil {
			t.Fatal(err)
		}
		var output bytes.Buffer
		err = run(t.Context(), []string{"--seed", "--contacts=1", "--files=0", "--actors=seed-test@example.com", "--data=" + data}, &output)
		if err == nil || !strings.Contains(err.Error(), "synthetic seed failure") {
			t.Fatalf("Then the synthetic seed error must propagate: %v", err)
		}
		thenActorRows(t, data, 0)
	})
	t.Run("completed_seed_does_not_register_different_actors", func(t *testing.T) {
		// Given a completed seed, When it is invoked with another actor list,
		// Then the no-op preserves the original identities as well as the data.
		data := t.TempDir()
		var output bytes.Buffer
		if err := run(t.Context(), []string{"--seed", "--contacts=0", "--files=0", "--actors=first@example.com", "--data=" + data}, &output); err != nil {
			t.Fatal(err)
		}
		if err := run(t.Context(), []string{"--seed", "--contacts=1", "--files=0", "--actors=second@example.com", "--data=" + data}, &output); err != nil {
			t.Fatal(err)
		}
		thenActorRows(t, data, 1)
	})
}

func thenActorRows(t *testing.T, data string, want int) {
	t.Helper()
	s, err := store.New(data)
	if err != nil {
		t.Fatal(err)
	}
	var count int
	queryErr := s.DB().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM actors").Scan(&count)
	if err := errors.Join(queryErr, s.Close()); err != nil {
		t.Fatal(err)
	}
	if count != want {
		t.Fatalf("Then persisted actor rows = %d, want %d", count, want)
	}
}
