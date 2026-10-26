package main_test

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
	"os"
	"testing"
	"time"

	"github.com/weave-lab/interview-public/go/internal/app"
	"github.com/weave-lab/interview-public/go/internal/auth"
	"github.com/weave-lab/interview-public/go/internal/seed"
	"github.com/weave-lab/interview-public/go/internal/store"
)

var (
	testServer *httptest.Server
	testStore  *store.Store
	testToken  string
)

func TestMain(m *testing.M) { os.Exit(runTests(m)) }

func runTests(m *testing.M) (code int) {
	dir, err := os.MkdirTemp("", "interview-test-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create temp dir: %v\n", err)
		return 1
	}
	defer func() {
		if err := os.RemoveAll(dir); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to remove test data: %v\n", err)
			code = 1
		}
	}()
	testStore, err = store.New(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create store: %v\n", err)
		return 1
	}
	defer func() {
		if err := testStore.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to close store: %v\n", err)
			code = 1
		}
	}()
	testToken, err = auth.NewToken()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create test token: %v\n", err)
		return 1
	}
	seed.SetQuiet(true)
	if err := seed.Run(context.Background(), testStore, seed.Options{Contacts: 1000, Files: 5}); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to seed: %v\n", err)
		return 1
	}
	testServer = httptest.NewServer(app.NewRouter(testStore, app.Options{AuthToken: testToken}))
	defer testServer.Close()
	return m.Run()
}

func authedRequest(method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequest(method, testServer.URL+path, body)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

func benchmarkRequest(client *http.Client, method, path string, body io.Reader, contentType string, wantStatus int) error {
	req, err := authedRequest(method, path, body)
	if err != nil {
		return err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	_, readErr := io.Copy(io.Discard, resp.Body)
	closeErr := resp.Body.Close()
	var statusErr error
	if resp.StatusCode != wantStatus {
		statusErr = fmt.Errorf("%s %s: status %d, want %d", method, path, resp.StatusCode, wantStatus)
	}
	return errors.Join(statusErr, readErr, closeErr)
}

func benchmarkClient(b *testing.B) *http.Client {
	b.Helper()
	client := testServer.Client()
	client.Timeout = 30 * time.Second
	b.Cleanup(client.CloseIdleConnections)
	return client
}

func benchmarkContactID(b *testing.B) string {
	b.Helper()
	contacts, err := testStore.ListContacts(context.Background(), 1, nil)
	if err != nil {
		b.Fatal(err)
	}
	if len(contacts) == 0 {
		b.Fatal("no contacts")
	}
	return contacts[0].ID
}

func benchmarkFileID(b *testing.B) string {
	b.Helper()
	files, err := testStore.ListFiles(context.Background())
	if err != nil {
		b.Fatal(err)
	}
	if len(files) == 0 {
		b.Fatal("no files")
	}
	return files[0].ID
}

func benchmarkGET(b *testing.B, path string) {
	b.Helper()
	client := benchmarkClient(b)
	for b.Loop() {
		if err := benchmarkRequest(client, http.MethodGet, path, nil, "", http.StatusOK); err != nil {
			b.Fatal(err)
		}
	}
}

func benchmarkGETParallel(b *testing.B, path string) {
	b.Helper()
	client := benchmarkClient(b)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if err := benchmarkRequest(client, http.MethodGet, path, nil, "", http.StatusOK); err != nil {
				b.Error(err)
				return
			}
		}
	})
}

func BenchmarkListContacts(b *testing.B) { benchmarkGET(b, "/api/contacts?limit=50") }

func BenchmarkListContactsParallel(b *testing.B) { benchmarkGETParallel(b, "/api/contacts?limit=50") }

func BenchmarkGetContact(b *testing.B) { benchmarkGET(b, "/api/contacts/"+benchmarkContactID(b)) }

func BenchmarkGetContactParallel(b *testing.B) {
	benchmarkGETParallel(b, "/api/contacts/"+benchmarkContactID(b))
}

func BenchmarkExportContacts(b *testing.B) { benchmarkGET(b, "/api/contacts/export") }

func BenchmarkExportContactsParallel(b *testing.B) { benchmarkGETParallel(b, "/api/contacts/export") }

func BenchmarkDownloadFile(b *testing.B) { benchmarkGET(b, "/api/files/"+benchmarkFileID(b)) }

func BenchmarkDownloadFileParallel(b *testing.B) {
	benchmarkGETParallel(b, "/api/files/"+benchmarkFileID(b))
}

func BenchmarkActivityReport(b *testing.B) { benchmarkGET(b, "/api/reports/activity") }

func BenchmarkActivityReportParallel(b *testing.B) { benchmarkGETParallel(b, "/api/reports/activity") }

const benchmarkContact = `{"first_name":"Test","last_name":"User","email":"test@example.com","phone":"555-1234","company":"Test Corp"}`

func BenchmarkCreateContact(b *testing.B) {
	client := benchmarkClient(b)
	for b.Loop() {
		if err := benchmarkRequest(client, http.MethodPost, "/api/contacts", bytes.NewBufferString(benchmarkContact), "", http.StatusCreated); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCreateContactParallel(b *testing.B) {
	client := benchmarkClient(b)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if err := benchmarkRequest(client, http.MethodPost, "/api/contacts", bytes.NewBufferString(benchmarkContact), "", http.StatusCreated); err != nil {
				b.Error(err)
				return
			}
		}
	})
}

func BenchmarkImportContacts(b *testing.B) {
	client := benchmarkClient(b)
	contacts := make([]map[string]string, 100)
	for i := range contacts {
		contacts[i] = map[string]string{
			"first_name": fmt.Sprintf("Import%d", i),
			"last_name":  "Test",
			"email":      fmt.Sprintf("import%d@test.com", i),
			"phone":      "555-0000",
			"company":    "Test Corp",
		}
	}
	payload, err := json.Marshal(contacts)
	if err != nil {
		b.Fatal(err)
	}
	for b.Loop() {
		if err := benchmarkRequest(client, http.MethodPost, "/api/contacts/import", bytes.NewReader(payload), "", http.StatusOK); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkUploadFile(b *testing.B) {
	client := benchmarkClient(b)
	content := bytes.Repeat([]byte("x"), 1024*1024) // 1MB
	for b.Loop() {
		var buf bytes.Buffer
		w := multipart.NewWriter(&buf)
		fw, err := w.CreateFormFile("file", "test.bin")
		if err != nil {
			b.Fatal(err)
		}
		if _, err := fw.Write(content); err != nil {
			b.Fatal(err)
		}
		if err := w.Close(); err != nil {
			b.Fatal(err)
		}
		if err := benchmarkRequest(client, http.MethodPost, "/api/files", &buf, w.FormDataContentType(), http.StatusCreated); err != nil {
			b.Fatal(err)
		}
	}
}
