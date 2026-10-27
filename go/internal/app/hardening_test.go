package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/weave-lab/interview-public/go/internal/auth"
	"github.com/weave-lab/interview-public/go/internal/store"
)

type integrationClient struct {
	client *http.Client
	url    string
	token  string
	actor  string
}

func newIntegrationServer(t *testing.T, wrappers ...func(http.Handler) http.Handler) (*store.Store, map[string]integrationClient) {
	t.Helper()
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	actors := []string{"alice@example.com", "bob@example.com"}
	if err := s.RegisterActors(t.Context(), actors); err != nil {
		t.Fatal(err)
	}
	credentials, err := auth.NewCredentials(actors)
	if err != nil {
		t.Fatal(err)
	}
	// Leave out RateLimit: these tests must exercise overlapping handlers.
	handler := NewRouter(s, Options{AuthTokens: credentials})
	for _, wrap := range wrappers {
		handler = wrap(handler)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client := server.Client()
	client.Timeout = 10 * time.Second
	t.Cleanup(client.CloseIdleConnections)
	clients := make(map[string]integrationClient, len(credentials))
	for token, actor := range credentials {
		clients[actor] = integrationClient{client: client, url: server.URL, token: token, actor: actor}
	}
	return s, clients
}

func (c integrationClient) request(ctx context.Context, method, path, contentType string, body io.Reader, want int) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.url+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	const maxResponse = 1 << 20
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	closeErr := resp.Body.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return nil, err
	}
	if len(data) > maxResponse {
		return nil, fmt.Errorf("%s %s: response exceeds test limit", method, path)
	}
	if resp.StatusCode != want {
		return nil, fmt.Errorf("%s %s: status %d, want %d: %.1024s", method, path, resp.StatusCode, want, data)
	}
	return data, nil
}

func TestConcurrentHTTPMutationsAndAudits(t *testing.T) {
	s, actorClients := newIntegrationServer(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	const clients = 12
	actors := []string{"alice@example.com", "bob@example.com"}
	start := make(chan struct{})
	failures := make(chan error, clients)
	var wg sync.WaitGroup
	for i := range clients {
		wg.Go(func() {
			<-start
			client := actorClients[actors[i%len(actors)]]
			if err := runContactAndFileWorkflow(ctx, client, i); err != nil {
				failures <- fmt.Errorf("client %d: %w", i, err)
			}
		})
	}
	close(start)
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	if t.Failed() {
		return
	}
	for _, actor := range actors {
		count, err := s.CountContacts(ctx, actor)
		if err != nil || count != clients {
			t.Fatalf("actor %s contacts=%d, error=%v; want %d", actor, count, err, clients)
		}
		files, err := s.ListFiles(ctx, actor)
		if err != nil || len(files) != clients/len(actors) {
			t.Fatalf("actor %s files=%d, error=%v; want %d", actor, len(files), err, clients/len(actors))
		}
		for _, file := range files {
			if file.OwnerID != actor {
				t.Errorf("actor %s listed foreign file owned by %s", actor, file.OwnerID)
			}
		}
		for _, action := range []string{"create", "update", "import", "upload", "download", "delete"} {
			var count int
			if err := s.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM activity_log WHERE action = ? AND user_id = ?", action, actor).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != clients/len(actors) {
				t.Errorf("actor %s audit action %s: got %d, want %d", actor, action, count, clients/len(actors))
			}
		}
	}
	// Compare persisted values to each consumer's submitted data, independently
	// of the scoped read helper; counts alone can hide misplaced or lost writes.
	expected := make(map[string]string, 2*clients)
	for i := range clients {
		for _, suffix := range []string{"A", "B"} {
			expected[fmt.Sprintf("Imported%d%s", i, suffix)] = actors[i%len(actors)]
		}
	}
	rows, err := s.DB().QueryContext(ctx, "SELECT first_name, owner_id FROM contacts")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			t.Error(err)
		}
	}()
	for rows.Next() {
		var name, owner string
		if err := rows.Scan(&name, &owner); err != nil {
			t.Fatal(err)
		}
		if expected[name] != owner {
			t.Errorf("persisted contact %q belongs to %q; want %q", name, owner, expected[name])
		}
		delete(expected, name)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		t.Fatal(err)
	}
	if len(expected) != 0 {
		t.Errorf("missing submitted contacts: %v", expected)
	}
	var audits int
	if err := s.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM activity_log").Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if audits != 6*clients {
		t.Errorf("audit total=%d, want %d", audits, 6*clients)
	}
}

func runContactAndFileWorkflow(ctx context.Context, client integrationClient, index int) error {
	body := fmt.Sprintf(`{"first_name":"Client%d","email":"client%d@example.com"}`, index, index)
	data, err := client.request(ctx, http.MethodPost, "/api/contacts", "application/json", strings.NewReader(body), http.StatusCreated)
	if err != nil {
		return err
	}
	var contact store.Contact
	if err := json.Unmarshal(data, &contact); err != nil {
		return err
	}
	if contact.ID == "" || contact.OwnerID != client.actor {
		return errors.New("created contact must have an ID and the authenticated owner")
	}
	path := "/api/contacts/" + contact.ID
	updated := fmt.Sprintf(`{"first_name":"Updated%d","email":"client%d@example.com"}`, index, index)
	if _, err := client.request(ctx, http.MethodPut, path, "application/json", strings.NewReader(updated), http.StatusOK); err != nil {
		return err
	}
	data, err = client.request(ctx, http.MethodGet, path, "", nil, http.StatusOK)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, &contact); err != nil {
		return err
	}
	if contact.FirstName != fmt.Sprintf("Updated%d", index) {
		return fmt.Errorf("updated name=%q", contact.FirstName)
	}
	imports := fmt.Sprintf(`[{"first_name":"Imported%dA"},{"first_name":"Imported%dB"}]`, index, index)
	data, err = client.request(ctx, http.MethodPost, "/api/contacts/import", "application/json", strings.NewReader(imports), http.StatusOK)
	if err != nil {
		return err
	}
	var result struct{ Imported int }
	if err := json.Unmarshal(data, &result); err != nil {
		return err
	}
	if result.Imported != 2 {
		return fmt.Errorf("imported=%d, want 2", result.Imported)
	}
	var upload bytes.Buffer
	writer := multipart.NewWriter(&upload)
	part, err := writer.CreateFormFile("file", fmt.Sprintf("client-%d.txt", index))
	if err != nil {
		return err
	}
	content := fmt.Sprintf("small file for client %d\n", index)
	if _, err := io.WriteString(part, content); err != nil {
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}
	data, err = client.request(ctx, http.MethodPost, "/api/files", writer.FormDataContentType(), &upload, http.StatusCreated)
	if err != nil {
		return err
	}
	var file store.File
	if err := json.Unmarshal(data, &file); err != nil {
		return err
	}
	if file.ID == "" || file.OwnerID != client.actor || file.Size != int64(len(content)) {
		return fmt.Errorf("invalid uploaded file metadata: %+v", file)
	}
	data, err = client.request(ctx, http.MethodGet, "/api/files/"+file.ID, "", nil, http.StatusOK)
	if err != nil {
		return err
	}
	if string(data) != content {
		return errors.New("downloaded content differs from upload")
	}
	if _, err := client.request(ctx, http.MethodDelete, path, "", nil, http.StatusNoContent); err != nil {
		return err
	}
	_, err = client.request(ctx, http.MethodGet, path, "", nil, http.StatusNotFound)
	return err
}

func TestHTTPInvalidContactBodiesDoNotMutate(t *testing.T) {
	s, clients := newIntegrationServer(t)
	client := clients["alice@example.com"]
	for _, tt := range []struct {
		name, body, contentType string
		status                  int
	}{
		{"unknown field", `{"admin":true}`, "application/json", http.StatusBadRequest},
		{"server-owned ID", `{"id":"chosen"}`, "application/json", http.StatusBadRequest},
		{"null", `null`, "application/json", http.StatusBadRequest},
		{"trailing value", `{} {}`, "application/json", http.StatusBadRequest},
		{"malformed", `{`, "application/json", http.StatusBadRequest},
		{"missing media type", `{}`, "", http.StatusUnsupportedMediaType},
		{"wrong media type", `{}`, "text/plain", http.StatusUnsupportedMediaType},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := client.request(t.Context(), http.MethodPost, "/api/contacts", tt.contentType, strings.NewReader(tt.body), tt.status); err != nil {
				t.Fatal(err)
			}
		})
	}
	assertEmptyContactsAndAudits(t, s)
}

func TestHTTPCanceledMutationDoesNotPersist(t *testing.T) {
	// Hold SQLite's sole connection so an actual authenticated HTTP mutation
	// must wait. A canceled client alone is insufficient evidence: observe the
	// queued database request and the server handler's completion before checking.
	handled := make(chan struct{})
	s, clients := newIntegrationServer(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer close(handled)
			next.ServeHTTP(w, r)
		})
	})
	connection, err := s.DB().Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if connection != nil {
			if err := connection.Close(); err != nil {
				t.Error(err)
			}
		}
	}()
	waitsBefore := s.DB().Stats().WaitCount
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := clients["alice@example.com"].request(ctx, http.MethodPost, "/api/contacts", "application/json", strings.NewReader(`{"first_name":"Canceled"}`), http.StatusCreated)
		result <- err
	}()

	// Poll the pool's observed queue condition, not a guessed amount of time
	// required for the request to arrive. The timer only bounds a broken test.
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for s.DB().Stats().WaitCount == waitsBefore {
		select {
		case <-ticker.C:
		case err := <-result:
			t.Fatalf("request finished before waiting for SQLite: %v", err)
		case <-deadline.C:
			t.Fatal("HTTP mutation never entered the SQLite connection queue")
		}
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled HTTP request error=%v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled HTTP request did not return")
	}
	select {
	case <-handled:
	case <-time.After(5 * time.Second):
		t.Fatal("server handler did not finish while SQLite was still unavailable")
	}
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}
	connection = nil
	assertEmptyContactsAndAudits(t, s)
}

func TestHTTPAuditFailureRollsBackContact(t *testing.T) {
	s, clients := newIntegrationServer(t)
	client := clients["alice@example.com"]
	if _, err := s.DB().ExecContext(t.Context(), "DROP TABLE activity_log"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.request(t.Context(), http.MethodPost, "/api/contacts", "application/json", strings.NewReader(`{"first_name":"Rollback"}`), http.StatusInternalServerError); err != nil {
		t.Fatal(err)
	}
	count, err := s.CountAllContacts(t.Context())
	if err != nil || count != 0 {
		t.Fatalf("contacts after failed audit=%d, error=%v; want 0", count, err)
	}
}

func assertEmptyContactsAndAudits(t *testing.T, s *store.Store) {
	t.Helper()
	count, err := s.CountAllContacts(t.Context())
	if err != nil || count != 0 {
		t.Fatalf("contacts=%d, error=%v; want 0", count, err)
	}
	var audits int
	if err := s.DB().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM activity_log").Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if audits != 0 {
		t.Fatalf("audits=%d, want 0", audits)
	}
}
