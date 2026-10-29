package app

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/weave-lab/interview-public/go/internal/store"
)

// Keep this table in step with NewRouter: every business method/path is protected.
var businessScenarios = []struct {
	method, path, contentType, body string
}{
	{http.MethodPost, "/api/auth/token", "", ""},
	{http.MethodGet, "/api/contacts", "", ""},
	{http.MethodPost, "/api/contacts", "application/json", `{}`},
	{http.MethodGet, "/api/contacts/missing", "", ""},
	{http.MethodPut, "/api/contacts/missing", "application/json", `{}`},
	{http.MethodDelete, "/api/contacts/missing", "", ""},
	{http.MethodPost, "/api/contacts/import", "application/json", `[]`},
	{http.MethodGet, "/api/contacts/export", "", ""},
	{http.MethodGet, "/api/files", "", ""},
	{http.MethodPost, "/api/files", "multipart/form-data; boundary=unused", ""},
	{http.MethodGet, "/api/files/missing", "", ""},
	{http.MethodGet, "/api/reports/activity", "", ""},
}

func TestBDDProtectedBusinessRoutes(t *testing.T) {
	// Given real records created through the API, so denied reads and mutations
	// exercise existing resources instead of succeeding only on missing IDs.
	world := givenBDDApp(t)
	created := world.request(t, http.MethodPost, "/api/contacts", "application/json", strings.NewReader(`{"first_name":"Private contact","email":"private@example.com"}`))
	thenStatus(t, created, http.StatusCreated)
	contact := decodeBDD[store.Contact](t, created)
	fileContent := []byte("Private file content")
	uploadBody, uploadMedia := givenBDDFileBody(t, "private.txt", "text/plain", fileContent)
	uploadBytes := bytes.Clone(uploadBody.Bytes())
	uploaded := world.request(t, http.MethodPost, "/api/files", uploadMedia, uploadBody)
	thenStatus(t, uploaded, http.StatusCreated)
	file := decodeBDD[store.File](t, uploaded)

	for _, credential := range []struct{ name, value string }{
		{"missing token", ""}, {"wrong token", "not-the-startup-token"},
		{"email is not a credential", "someone@example.com"},
	} {
		t.Run(credential.name, func(t *testing.T) {
			consumer := *world
			consumer.token = credential.value
			for _, route := range businessScenarios {
				t.Run(route.method+" "+route.path, func(t *testing.T) {
					// When any registered business operation is attempted.
					path := strings.ReplaceAll(route.path, "/contacts/missing", "/contacts/"+contact.ID)
					path = strings.ReplaceAll(path, "/files/missing", "/files/"+file.ID)
					body, media := []byte(route.body), route.contentType
					if route.method == http.MethodPost && route.path == "/api/files" {
						body, media = uploadBytes, uploadMedia
					}
					response := consumer.request(t, route.method, path, media, bytes.NewReader(body))
					// Then authentication wins before parsing, storage, or delivery.
					thenStatus(t, response, http.StatusUnauthorized)
					if response.header.Get("WWW-Authenticate") != "Bearer" || response.header.Get("Cache-Control") != "no-store" {
						t.Errorf("Then unauthorized responses must challenge without caching: %v", response.header)
					}
					if string(response.body) != "Unauthorized\n" {
						t.Errorf("Then denial must not expose resource data: %q", response.body)
					}
				})
			}
			// Malformed input must still be denied before the JSON parser runs.
			thenStatus(t, consumer.request(t, http.MethodPost, "/api/contacts", "application/json", strings.NewReader("{")), http.StatusUnauthorized)
		})
	}
	// Then unauthorized operations leave both data and audit history untouched.
	thenBDDActorStateCounts(t, world, 1, 2)
	persisted, err := world.store.GetContact(t.Context(), world.actor, contact.ID)
	if err != nil || persisted != contact {
		t.Fatalf("Then the existing contact remains unchanged: got %+v, error=%v", persisted, err)
	}
	files, err := world.store.ListFiles(t.Context(), world.actor)
	if err != nil || len(files) != 1 || files[0] != file {
		t.Fatalf("Then the existing file metadata remains unchanged: files=%v error=%v", files, err)
	}
	content, err := os.ReadFile(filepath.Join(world.store.FilesDir(), file.ID))
	if err != nil || !bytes.Equal(content, fileContent) {
		t.Fatalf("Then the existing file content remains unchanged: content=%q error=%v", content, err)
	}
	entries, err := os.ReadDir(world.store.FilesDir())
	if err != nil || len(entries) != 1 {
		t.Fatalf("Then denied uploads leave no extra blobs: entries=%d error=%v", len(entries), err)
	}
}

func TestBDDCredentialVerificationContract(t *testing.T) {
	// Given a consumer holding the generated local workspace token.
	world := givenBDDApp(t)
	// When the consumer asks to verify it.
	response := world.request(t, http.MethodPost, "/api/auth/token", "", nil)
	// Then the response identifies the authenticated actor and issues no new token.
	thenStatus(t, response, http.StatusOK)
	identity := decodeBDD[map[string]string](t, response)
	if len(identity) != 2 || identity["user_id"] != world.actor || identity["status"] != "authenticated" {
		t.Fatalf("Then credential verification identifies only the authenticated actor: %v", identity)
	}
	if response.header.Get("Content-Type") != "application/json" {
		t.Errorf("Then credential verification returns JSON, got %q", response.header.Get("Content-Type"))
	}
	// Given a separate local server with its own generated credential.
	other := givenBDDApp(t)
	stale := *other
	stale.token = world.token
	// When the first server's token is presented to it, Then it is rejected.
	thenStatus(t, stale.request(t, http.MethodPost, "/api/auth/token", "", nil), http.StatusUnauthorized)
	thenStatus(t, other.request(t, http.MethodPost, "/api/auth/token", "", nil), http.StatusOK)
}

func TestBDDHealthListenerIsolation(t *testing.T) {
	// Given separate business and public liveness listeners.
	business := givenBDDApp(t)
	healthServer := httptest.NewServer(NewHealthRouter())
	t.Cleanup(healthServer.Close)
	health := *business
	health.url, health.token = healthServer.URL, ""

	// When health is checked without credentials, Then only liveness is returned.
	response := health.request(t, http.MethodGet, "/health", "", nil)
	thenStatus(t, response, http.StatusOK)
	if string(response.body) != "ok" || response.header.Get("Cache-Control") != "no-store" {
		t.Fatalf("Then liveness is ok and non-cacheable: status=%d headers=%v body=%q", response.status, response.header, response.body)
	}
	// When either listener is used for the other's routes, Then no handler leaks.
	thenStatus(t, business.request(t, http.MethodGet, "/health", "", nil), http.StatusNotFound)
	for _, route := range businessScenarios {
		t.Run(route.method+" "+route.path+" on health", func(t *testing.T) {
			thenStatus(t, health.request(t, route.method, route.path, route.contentType, strings.NewReader(route.body)), http.StatusNotFound)
		})
	}
	// When the health path uses a different method, Then it is not accepted.
	thenStatus(t, health.request(t, http.MethodPost, "/health", "", nil), http.StatusMethodNotAllowed)
}

func TestBDDBusinessRoutingAndErrorContract(t *testing.T) {
	// Given an authenticated consumer of the business API.
	world := givenBDDApp(t)
	for _, scenario := range []struct {
		name, method, path, media, body string
		status                          int
	}{
		{"unknown route", http.MethodGet, "/api/not-a-route", "", "", http.StatusNotFound},
		{"unsupported method", http.MethodPatch, "/api/contacts", "", "", http.StatusMethodNotAllowed},
		{"malformed JSON", http.MethodPost, "/api/contacts", "application/json", `{`, http.StatusBadRequest},
		{"wrong media", http.MethodPost, "/api/contacts", "text/plain", `{}`, http.StatusUnsupportedMediaType},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// When a route/method/input contract is violated.
			response := world.request(t, scenario.method, scenario.path, scenario.media, strings.NewReader(scenario.body))
			// Then the documented status is returned and input errors have one error field.
			thenStatus(t, response, scenario.status)
			if scenario.status == http.StatusBadRequest || scenario.status == http.StatusUnsupportedMediaType {
				problem := decodeBDD[map[string]string](t, response)
				if len(problem) != 1 || problem["error"] == "" || response.header.Get("Content-Type") != "application/json" {
					t.Fatalf("Then API errors use the JSON error contract: %v", problem)
				}
			}
		})
	}
	assertEmptyContactsAndAudits(t, world.store)
}
