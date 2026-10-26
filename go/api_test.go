package main_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/weave-lab/interview-public/go/internal/app"
	"github.com/weave-lab/interview-public/go/internal/seed"
	"github.com/weave-lab/interview-public/go/internal/store"
)

var (
	testServer *httptest.Server
	testStore  *store.Store
)

func TestMain(m *testing.M) {
	os.Exit(runTests(m))
}

func runTests(m *testing.M) int {
	dir, err := os.MkdirTemp("", "interview-test-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create temp dir: %v\n", err)
		return 1
	}
	defer os.RemoveAll(dir)

	testStore, err = store.New(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create store: %v\n", err)
		return 1
	}
	defer testStore.Close()

	opts := seed.Options{
		Contacts: 1000,
		Files:    5,
	}
	seed.SetQuiet(true)
	if err := seed.Run(context.Background(), testStore, opts); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to seed: %v\n", err)
		return 1
	}

	r := app.NewRouter(testStore, app.Options{EnableLogging: false})
	testServer = httptest.NewServer(r)
	defer testServer.Close()

	return m.Run()
}

func authedRequest(method, path string, body io.Reader) *http.Request {
	req, _ := http.NewRequest(method, testServer.URL+path, body)
	req.Header.Set("Authorization", "Bearer test@example.com")
	return req
}

// Serial benchmarks

func BenchmarkListContacts(b *testing.B) {
	client := &http.Client{}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req := authedRequest("GET", "/api/contacts?limit=50", nil)
		resp, err := client.Do(req)
		if err != nil {
			b.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}

func BenchmarkGetContact(b *testing.B) {
	contacts, _ := testStore.ListContacts(context.Background(), 1, nil)
	if len(contacts) == 0 {
		b.Fatal("no contacts")
	}
	id := contacts[0].ID

	client := &http.Client{}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req := authedRequest("GET", "/api/contacts/"+id, nil)
		resp, err := client.Do(req)
		if err != nil {
			b.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}

func BenchmarkCreateContact(b *testing.B) {
	client := &http.Client{}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		body := bytes.NewBufferString(`{
			"first_name": "Test",
			"last_name": "User",
			"email": "test@example.com",
			"phone": "555-1234",
			"company": "Test Corp"
		}`)
		req := authedRequest("POST", "/api/contacts", body)
		req.Header.Set("Content-Type", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			b.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}

func BenchmarkExportContacts(b *testing.B) {
	client := &http.Client{}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req := authedRequest("GET", "/api/contacts/export", nil)
		resp, err := client.Do(req)
		if err != nil {
			b.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}

func BenchmarkImportContacts(b *testing.B) {
	client := &http.Client{}

	contacts := make([]map[string]string, 100)
	for i := range contacts {
		contacts[i] = map[string]string{
			"first_name": fmt.Sprintf("Import%d", i),
			"last_name":  "Test",
			"email":      fmt.Sprintf("import%d@test.com", i),
			"phone":      "555-0000",
			"company":    "Import Corp",
		}
	}
	payload, _ := json.Marshal(contacts)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req := authedRequest("POST", "/api/contacts/import", bytes.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			b.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}

func BenchmarkDownloadFile(b *testing.B) {
	files, _ := testStore.ListFiles(context.Background())
	if len(files) == 0 {
		b.Fatal("no files")
	}
	id := files[0].ID

	client := &http.Client{}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req := authedRequest("GET", "/api/files/"+id, nil)
		resp, err := client.Do(req)
		if err != nil {
			b.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}

func BenchmarkUploadFile(b *testing.B) {
	client := &http.Client{}
	content := bytes.Repeat([]byte("x"), 1024*1024) // 1MB

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var buf bytes.Buffer
		w := multipart.NewWriter(&buf)
		fw, _ := w.CreateFormFile("file", "test.bin")
		fw.Write(content)
		w.Close()

		req := authedRequest("POST", "/api/files", &buf)
		req.Header.Set("Content-Type", w.FormDataContentType())

		resp, err := client.Do(req)
		if err != nil {
			b.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}

func BenchmarkActivityReport(b *testing.B) {
	client := &http.Client{}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req := authedRequest("GET", "/api/reports/activity", nil)
		resp, err := client.Do(req)
		if err != nil {
			b.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}

// Parallel benchmarks

func BenchmarkListContactsParallel(b *testing.B) {
	b.RunParallel(func(pb *testing.PB) {
		client := &http.Client{}
		for pb.Next() {
			req := authedRequest("GET", "/api/contacts?limit=50", nil)
			resp, err := client.Do(req)
			if err != nil {
				b.Fatal(err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	})
}

func BenchmarkGetContactParallel(b *testing.B) {
	contacts, _ := testStore.ListContacts(context.Background(), 1, nil)
	if len(contacts) == 0 {
		b.Fatal("no contacts")
	}
	id := contacts[0].ID

	b.RunParallel(func(pb *testing.PB) {
		client := &http.Client{}
		for pb.Next() {
			req := authedRequest("GET", "/api/contacts/"+id, nil)
			resp, err := client.Do(req)
			if err != nil {
				b.Fatal(err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	})
}

func BenchmarkCreateContactParallel(b *testing.B) {
	b.RunParallel(func(pb *testing.PB) {
		client := &http.Client{}
		for pb.Next() {
			body := bytes.NewBufferString(`{
				"first_name": "Test",
				"last_name": "User",
				"email": "test@example.com",
				"phone": "555-1234",
				"company": "Test Corp"
			}`)
			req := authedRequest("POST", "/api/contacts", body)
			req.Header.Set("Content-Type", "application/json")

			resp, err := client.Do(req)
			if err != nil {
				b.Fatal(err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	})
}

func BenchmarkExportContactsParallel(b *testing.B) {
	b.RunParallel(func(pb *testing.PB) {
		client := &http.Client{}
		for pb.Next() {
			req := authedRequest("GET", "/api/contacts/export", nil)
			resp, err := client.Do(req)
			if err != nil {
				b.Fatal(err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	})
}

func BenchmarkDownloadFileParallel(b *testing.B) {
	files, _ := testStore.ListFiles(context.Background())
	if len(files) == 0 {
		b.Fatal("no files")
	}
	id := files[0].ID

	b.RunParallel(func(pb *testing.PB) {
		client := &http.Client{}
		for pb.Next() {
			req := authedRequest("GET", "/api/files/"+id, nil)
			resp, err := client.Do(req)
			if err != nil {
				b.Fatal(err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	})
}

func BenchmarkActivityReportParallel(b *testing.B) {
	b.RunParallel(func(pb *testing.PB) {
		client := &http.Client{}
		for pb.Next() {
			req := authedRequest("GET", "/api/reports/activity", nil)
			resp, err := client.Do(req)
			if err != nil {
				b.Fatal(err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	})
}
