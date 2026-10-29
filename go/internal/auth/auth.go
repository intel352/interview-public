package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"
)

type contextKey struct{}

const DefaultActors = "alice@example.com,bob@example.com"

// Actor IDs are case-sensitive local configuration labels, not verified emails.
func ParseActors(value string) ([]string, error) {
	actors := strings.Split(value, ",")
	for i := range actors {
		actors[i] = strings.TrimSpace(actors[i])
	}
	return actors, ValidateActors(actors)
}

func ValidateActors(actors []string) error {
	if len(actors) == 0 {
		return errors.New("configure at least one actor")
	}
	seen := make(map[string]bool, len(actors))
	for _, actor := range actors {
		if actor == "" || len(actor) > 254 || !utf8.ValidString(actor) || strings.ContainsRune(actor, ',') || strings.ContainsFunc(actor, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
			return errors.New("actor IDs must be 1–254 UTF-8 bytes without whitespace, commas or control characters")
		}
		if seen[actor] {
			return errors.New("actor IDs must be unique")
		}
		seen[actor] = true
	}
	return nil
}

// NewCredentials creates independent ephemeral credentials for explicit actors.
// Only the actor IDs are persisted by the store; credentials stay in memory.
func NewCredentials(actors []string) (map[string]string, error) {
	if err := ValidateActors(actors); err != nil {
		return nil, err
	}
	credentials := make(map[string]string, len(actors))
	for _, actor := range actors {
		token, err := NewToken()
		if err != nil {
			return nil, err
		}
		credentials[token] = actor
	}
	return credentials, nil
}

// NewToken creates a startup-only credential.
// The server keeps it in memory; restarting the server invalidates it.
func NewToken() (string, error) {
	var bytes [32]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", fmt.Errorf("generate local token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(bytes[:]), nil
}

// TokenParser snapshots the token-to-actor mapping so later caller changes do
// not alter authentication or introduce concurrent map access during requests.
// Fixed-size credential digests allow keyed lookup without scanning actors.
func TokenParser(credentials map[string]string) func(http.Handler) http.Handler {
	expected := make(map[[sha256.Size]byte]string, len(credentials))
	for token, actor := range credentials {
		if token == "" || ValidateActors([]string{actor}) != nil {
			expected = nil // Invalid configuration fails closed.
			break
		}
		expected[sha256.Sum256([]byte(token))] = actor
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			values := r.Header.Values("Authorization")
			if len(values) == 1 {
				scheme, token, ok := strings.Cut(values[0], " ")
				if ok && strings.EqualFold(scheme, "Bearer") {
					actor := expected[sha256.Sum256([]byte(token))]
					if actor != "" {
						r = r.WithContext(context.WithValue(r.Context(), contextKey{}, actor))
					}
				}
			}
			w.Header().Set("Cache-Control", "no-store")
			next.ServeHTTP(w, r)
		})
	}
}

func RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if UserID(r.Context()) == "" {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func UserID(ctx context.Context) string {
	v, _ := ctx.Value(contextKey{}).(string)
	return v
}
