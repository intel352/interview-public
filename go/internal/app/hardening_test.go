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
}

func newIntegrationServer(t *testing.T) (*store.Store, integrationClient) {
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
	token, err := auth.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	// Leave out RateLimit: these tests must exercise overlapping handlers.
	server := httptest.NewServer(NewRouter(s, Options{AuthToken: token}))
	t.Cleanup(server.Close)
	client := server.Client()
	client.Timeout = 10 * time.Second
	return s, integrationClient{client: client, url: server.URL, token: token}
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
	s, client := newIntegrationServer(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	const clients = 12
	start := make(chan struct{})
	failures := make(chan error, clients)
	var wg sync.WaitGroup
	for i := range clients {
		wg.Go(func() {
			<-start
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
	count, err := s.CountContacts(ctx)
	if err != nil || count != 2*clients {
		t.Fatalf("contacts=%d, error=%v; want %d", count, err, 2*clients)
	}
	files, err := s.ListFiles(ctx)
	if err != nil || len(files) != clients {
		t.Fatalf("files=%d, error=%v; want %d", len(files), err, clients)
	}
	for _, action := range []string{"create", "update", "import", "upload", "download_attempt", "delete"} {
		var count int
		if err := s.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM activity_log WHERE action = ? AND user_id = ?", action, "local-demo").Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != clients {
			t.Errorf("audit action %s: got %d, want %d", action, count, clients)
		}
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
	if contact.ID == "" {
		return errors.New("created contact has no ID")
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
	if file.ID == "" || file.Size != int64(len(content)) {
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
	s, client := newIntegrationServer(t)
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

func TestCanceledMutationDoesNotPersist(t *testing.T) {
	s, _ := newIntegrationServer(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	contact := store.Contact{ID: "canceled-contact", FirstName: "Canceled"}
	if err := s.CreateContact(ctx, "local-demo", &contact); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled create error=%v, want context.Canceled", err)
	}
	assertEmptyContactsAndAudits(t, s)
}

func TestHTTPAuditFailureRollsBackContact(t *testing.T) {
	s, client := newIntegrationServer(t)
	if _, err := s.DB().ExecContext(t.Context(), "DROP TABLE activity_log"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.request(t.Context(), http.MethodPost, "/api/contacts", "application/json", strings.NewReader(`{"first_name":"Rollback"}`), http.StatusInternalServerError); err != nil {
		t.Fatal(err)
	}
	count, err := s.CountContacts(t.Context())
	if err != nil || count != 0 {
		t.Fatalf("contacts after failed audit=%d, error=%v; want 0", count, err)
	}
}

func assertEmptyContactsAndAudits(t *testing.T, s *store.Store) {
	t.Helper()
	count, err := s.CountContacts(t.Context())
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
