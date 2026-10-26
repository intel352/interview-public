package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
)

type contextKey struct{}

// NewToken creates a startup-only credential for the single local workspace.
// The server keeps it in memory; restarting the server invalidates it.
func NewToken() (string, error) {
	var bytes [32]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", fmt.Errorf("generate local token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(bytes[:]), nil
}

// TokenParser authenticates possession of the local server's random token.
// There are no separate user accounts: every valid client acts on one workspace.
func TokenParser(expected string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			values := r.Header.Values("Authorization")
			if expected != "" && len(values) == 1 {
				scheme, token, ok := strings.Cut(values[0], " ")
				if ok && strings.EqualFold(scheme, "Bearer") && subtle.ConstantTimeCompare([]byte(token), []byte(expected)) == 1 {
					r = r.WithContext(context.WithValue(r.Context(), contextKey{}, "local-demo"))
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
