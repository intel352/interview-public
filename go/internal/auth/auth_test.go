package auth

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTokenAuthentication(t *testing.T) {
	const token = "test-only-opaque-token"
	for _, tt := range []struct {
		name     string
		expected string
		headers  []string
		status   int
	}{
		{"valid", token, []string{"Bearer " + token}, 200},
		{"scheme case", token, []string{"bearer " + token}, 200},
		{"missing", token, nil, 401},
		{"email is not a credential", token, []string{"Bearer victim@example.com"}, 401},
		{"wrong", token, []string{"Bearer incorrect"}, 401},
		{"wrong scheme", token, []string{"Basic " + token}, 401},
		{"extra value", token, []string{"Bearer " + token + " extra"}, 401},
		{"duplicate", token, []string{"Bearer " + token, "Bearer " + token}, 401},
		{"empty configuration", "", []string{"Bearer "}, 401},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := TokenParser(map[string]string{tt.expected: "alice@example.com"})(RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got := UserID(r.Context()); got != "alice@example.com" {
					t.Errorf("principal = %q, want alice@example.com", got)
				}
			})))
			r := httptest.NewRequest(http.MethodGet, "/api/contacts", nil)
			for _, value := range tt.headers {
				r.Header.Add("Authorization", value)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tt.status {
				t.Fatalf("status = %d, want %d", w.Code, tt.status)
			}
			if tt.status == 401 && w.Header().Get("WWW-Authenticate") != "Bearer" {
				t.Fatal("missing bearer challenge")
			}
		})
	}
}

func TestNewToken(t *testing.T) {
	first, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := base64.RawURLEncoding.DecodeString(first)
	if err != nil || len(decoded) != 32 || first == second {
		t.Fatalf("expected distinct tokens containing 32 random bytes; decoded length=%d, error=%v", len(decoded), err)
	}
}

func TestActorCredentialMappingsAreIsolatedAndImmutable(t *testing.T) {
	credentials := map[string]string{"alice-secret": "alice@example.com", "bob-secret": "bob@example.com"}
	handler := TokenParser(credentials)(RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Header().Set("Actor", UserID(r.Context())) })))
	// Given middleware has taken its immutable configuration snapshot.
	credentials["alice-secret"] = "mallory@example.com"
	delete(credentials, "bob-secret")
	for token, actor := range map[string]string{"alice-secret": "alice@example.com", "bob-secret": "bob@example.com"} {
		// When distinct credentials are authenticated, Then each original actor survives.
		request := httptest.NewRequest(http.MethodGet, "/", nil)
		request.Header.Set("Authorization", "Bearer "+token)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK || response.Header().Get("Actor") != actor {
			t.Errorf("actor = %q status=%d; want %q", response.Header().Get("Actor"), response.Code, actor)
		}
	}
}

func TestActorConfiguration(t *testing.T) {
	for _, value := range []string{"", "alice,", ",alice", "alice,alice", "alice\nbob", "alice bob"} {
		if _, err := ParseActors(value); err == nil {
			t.Errorf("accepted invalid actor configuration %q", value)
		}
	}
	actors, err := ParseActors(" alice@example.com , bob@example.com ")
	if err != nil || len(actors) != 2 || actors[0] != "alice@example.com" || actors[1] != "bob@example.com" {
		t.Fatalf("parsed actors=%v error=%v", actors, err)
	}
	credentials, err := NewCredentials(actors)
	if err != nil || len(credentials) != 2 {
		t.Fatalf("credentials count=%d error=%v", len(credentials), err)
	}
	seen := map[string]bool{}
	for token, actor := range credentials {
		decoded, err := base64.RawURLEncoding.DecodeString(token)
		if err != nil || len(decoded) != 32 || seen[actor] {
			t.Fatal("expected one independent 256-bit token per actor")
		}
		seen[actor] = true
	}
	for _, actors := range [][]string{nil, {""}, {"alice", "alice"}, {"alice,bob"}} {
		if _, err := NewCredentials(actors); err == nil {
			t.Error("accepted invalid actor list")
		}
	}
}

func TestActorConfigurationBoundariesAndFailClosedMapping(t *testing.T) {
	actors := make([]string, 64)
	for i := range actors {
		actors[i] = fmt.Sprintf("actor-%d", i)
	}
	if err := ValidateActors(actors); err != nil {
		t.Fatal(err)
	}
	if err := ValidateActors(append(actors, "one-too-many")); err == nil {
		t.Fatal("accepted 65 actors")
	}
	if err := ValidateActors([]string{strings.Repeat("a", 254)}); err != nil {
		t.Fatal(err)
	}
	for _, actor := range []string{strings.Repeat("a", 255), string([]byte{0xff}), "line\nfeed", "two words"} {
		if err := ValidateActors([]string{actor}); err == nil {
			t.Error("accepted invalid actor")
		}
	}
	tooMany := make(map[string]string)
	for i := range 65 {
		tooMany[fmt.Sprintf("token-%d", i)] = fmt.Sprintf("actor-%d", i)
	}
	for _, credentials := range []map[string]string{nil, {"token-0": ""}, {"token-0": "two words"}, {"token-0": "actor-0", "": "actor-1"}, tooMany} {
		handler := TokenParser(credentials)(RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("invalid configuration reached handler") })))
		request := httptest.NewRequest(http.MethodGet, "/", nil)
		request.Header.Set("Authorization", "Bearer token-0")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusUnauthorized {
			t.Errorf("invalid mapping returned %d", response.Code)
		}
	}
}
