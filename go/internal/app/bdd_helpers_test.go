package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/weave-lab/interview-public/go/internal/auth"
	"github.com/weave-lab/interview-public/go/internal/store"
)

// BDD scenarios use the real router, SQLite and files. Wrappers let limiter
// scenarios inject time without changing the production implementation.
type bddWorld struct {
	store  *store.Store
	client *http.Client
	url    string
	token  string
	actor  string
	tokens map[string]string // Actor ID to test credential; immutable after setup.
}

type bddResponse struct {
	status int
	header http.Header
	body   []byte
}

func givenBDDApp(t *testing.T, wrappers ...func(http.Handler) http.Handler) *bddWorld {
	t.Helper()
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close scenario store: %v", err)
		}
	})
	actors, err := auth.ParseActors(auth.DefaultActors)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterActors(t.Context(), actors); err != nil {
		t.Fatal(err)
	}
	credentials, err := auth.NewCredentials(actors)
	if err != nil {
		t.Fatal(err)
	}
	tokens := make(map[string]string, len(credentials))
	for token, actor := range credentials {
		tokens[actor] = token
	}
	handler := NewRouter(s, Options{AuthTokens: credentials})
	for _, wrap := range wrappers {
		handler = wrap(handler)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client := server.Client()
	client.Timeout = 5 * time.Second
	t.Cleanup(client.CloseIdleConnections)
	return &bddWorld{store: s, client: client, url: server.URL, token: tokens[actors[0]], actor: actors[0], tokens: tokens}
}

func (w *bddWorld) forActor(t *testing.T, actor string) *bddWorld {
	t.Helper()
	token, ok := w.tokens[actor]
	if !ok {
		t.Fatalf("scenario actor %q was not configured", actor)
	}
	consumer := *w
	consumer.token, consumer.actor = token, actor
	consumer.client = &http.Client{Transport: &http.Transport{}, Timeout: 5 * time.Second}
	t.Cleanup(consumer.client.CloseIdleConnections)
	return &consumer
}

func (w *bddWorld) request(t *testing.T, method, path, contentType string, body io.Reader) bddResponse {
	t.Helper()
	response, err := w.requestResult(t.Context(), method, path, contentType, body)
	if err != nil {
		t.Fatalf("When %s %s: %v", method, path, err)
	}
	return response
}

// This form returns errors so concurrent consumers report through a channel,
// rather than invoking Fatal from a worker goroutine.
func (w *bddWorld) requestResult(ctx context.Context, method, path, contentType string, body io.Reader) (bddResponse, error) {
	r, err := http.NewRequestWithContext(ctx, method, w.url+path, body)
	if err != nil {
		return bddResponse{}, err
	}
	if w.token != "" {
		r.Header.Set("Authorization", "Bearer "+w.token)
	}
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	response, err := w.client.Do(r)
	if err != nil {
		return bddResponse{}, err
	}
	const maxResponse = 2 << 20
	data, readErr := io.ReadAll(io.LimitReader(response.Body, maxResponse+1))
	if err := errors.Join(readErr, response.Body.Close()); err != nil {
		return bddResponse{}, err
	}
	if len(data) > maxResponse {
		return bddResponse{}, fmt.Errorf("response exceeded scenario limit of %d bytes", maxResponse)
	}
	return bddResponse{status: response.StatusCode, header: response.Header, body: data}, nil
}

func thenStatus(t *testing.T, response bddResponse, want int) {
	t.Helper()
	if response.status != want {
		t.Fatalf("Then status = %d, want %d; body: %.1024s", response.status, want, response.body)
	}
}

func decodeBDD[T any](t *testing.T, response bddResponse) T {
	t.Helper()
	var value T
	if err := json.Unmarshal(response.body, &value); err != nil {
		t.Fatalf("Then response must be valid JSON: %v", err)
	}
	return value
}
