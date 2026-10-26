package auth

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
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
			h := TokenParser(tt.expected)(RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got := UserID(r.Context()); got != "local-demo" {
					t.Errorf("principal = %q, want local-demo", got)
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
