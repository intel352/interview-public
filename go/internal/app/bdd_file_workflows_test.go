package app

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/weave-lab/interview-public/go/internal/store"
)

func TestBDDActivityReportsReflectAuthenticatedHTTPWorkflows(t *testing.T) {
	// Given independent actor credentials against one real HTTP server.
	alice := givenBDDApp(t)
	bob := alice.forActor(t, "bob@example.com")
	type audit struct{ actor, action, resource, id string }
	var wantAudits []audit
	for _, consumer := range []*bddWorld{alice, bob} {
		// When each actor performs real mutations and reads, no audit fixture is
		// inserted directly: the handlers must persist the events themselves.
		created := consumer.request(t, http.MethodPost, "/api/contacts", "application/json", strings.NewReader(`{"first_name":"Synthetic"}`))
		thenStatus(t, created, http.StatusCreated)
		contact := decodeBDD[store.Contact](t, created)
		updated := consumer.request(t, http.MethodPut, "/api/contacts/"+contact.ID, "application/json", strings.NewReader(`{"first_name":"Updated"}`))
		thenStatus(t, updated, http.StatusOK)
		thenStatus(t, consumer.request(t, http.MethodGet, "/api/contacts/export", "", nil), http.StatusOK)
		imported := consumer.request(t, http.MethodPost, "/api/contacts/import", "application/json", strings.NewReader(`[{"first_name":"Imported one"},{"first_name":"Imported two"}]`))
		thenStatus(t, imported, http.StatusOK)
		if count := decodeBDD[map[string]int](t, imported)["imported"]; count != 2 {
			t.Fatalf("Then the real import writes both contacts: %d", count)
		}
		thenStatus(t, consumer.request(t, http.MethodDelete, "/api/contacts/"+contact.ID, "", nil), http.StatusNoContent)
		body, contentType := givenBDDFileBody(t, consumer.actor+".txt", "text/plain", []byte(consumer.actor))
		uploaded := consumer.request(t, http.MethodPost, "/api/files", contentType, body)
		thenStatus(t, uploaded, http.StatusCreated)
		file := thenBDDFileSchema(t, uploaded)
		downloaded := consumer.request(t, http.MethodGet, "/api/files/"+file.ID, "", nil)
		thenStatus(t, downloaded, http.StatusOK)
		if string(downloaded.body) != consumer.actor {
			t.Fatalf("Then %s receives its own bytes: %q", consumer.actor, downloaded.body)
		}
		var owner, filename string
		var size int64
		if err := alice.store.DB().QueryRowContext(t.Context(), `SELECT owner_id, filename, size FROM files WHERE id = ?`, file.ID).Scan(&owner, &filename, &size); err != nil {
			t.Fatal(err)
		}
		if owner != consumer.actor || filename != consumer.actor+".txt" || size != int64(len(consumer.actor)) {
			t.Fatalf("Then authenticated ownership persists: owner=%q filename=%q size=%d", owner, filename, size)
		}
		wantAudits = append(wantAudits,
			audit{consumer.actor, "create", "contact", contact.ID},
			audit{consumer.actor, "update", "contact", contact.ID},
			audit{consumer.actor, "export", "contacts", ""},
			audit{consumer.actor, "import", "contacts", ""},
			audit{consumer.actor, "delete", "contact", contact.ID},
			audit{consumer.actor, "upload", "file", file.ID},
			audit{consumer.actor, "download", "file", file.ID})
	}

	// Then persisted actor/action/resource tuples match the actual operations;
	// plausible response counts cannot conceal missing or misattributed events.
	rows, err := alice.store.DB().QueryContext(t.Context(), `SELECT user_id, action, resource_type, resource_id FROM activity_log ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	var gotAudits []audit
	for rows.Next() {
		var got audit
		if err := rows.Scan(&got.actor, &got.action, &got.resource, &got.id); err != nil {
			_ = rows.Close()
			t.Fatal(err)
		}
		gotAudits = append(gotAudits, got)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		t.Fatal(err)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(gotAudits, wantAudits) {
		t.Fatalf("Then exactly the expected audits persist: got %v, want %v", gotAudits, wantAudits)
	}
	// When reports are requested, then they reflect the real persisted history,
	// excluding the other actor and the current report's own view event.
	for _, consumer := range []*bddWorld{alice, bob} {
		report := consumer.request(t, http.MethodGet, "/api/reports/activity", "", nil)
		thenBDDReport(t, report, consumer.actor,
			map[string]int{"create": 1, "update": 1, "export": 1, "import": 1, "delete": 1, "upload": 1, "download": 1},
			map[string]int{"contact": 3, "contacts": 2, "file": 2})
	}
}

func TestBDDFileUploadAuditFailureRollsBackAndRecovers(t *testing.T) {
	// Given the real database rejects audit inserts while file copying can succeed.
	world := givenBDDApp(t)
	if _, err := world.store.DB().ExecContext(t.Context(), `CREATE TRIGGER reject_upload_audit BEFORE INSERT ON activity_log BEGIN SELECT RAISE(FAIL, 'synthetic audit failure'); END`); err != nil {
		t.Fatal(err)
	}
	body, contentType := givenBDDFileBody(t, "retry.txt", "text/plain", []byte("retryable content"))
	thenStatus(t, world.request(t, http.MethodPost, "/api/files", contentType, body), http.StatusInternalServerError)
	thenBDDNoFilesOrAudits(t, world)

	// When the audit sink recovers, then retrying the real HTTP upload succeeds
	// with one file/audit and no durable leftovers from the failed attempt.
	if _, err := world.store.DB().ExecContext(t.Context(), `DROP TRIGGER reject_upload_audit`); err != nil {
		t.Fatal(err)
	}
	body, contentType = givenBDDFileBody(t, "retry.txt", "text/plain", []byte("retryable content"))
	response := world.request(t, http.MethodPost, "/api/files", contentType, body)
	thenStatus(t, response, http.StatusCreated)
	file := thenBDDFileSchema(t, response)
	var files, audits int
	if err := world.store.DB().QueryRowContext(t.Context(), `SELECT (SELECT COUNT(*) FROM files), (SELECT COUNT(*) FROM activity_log)`).Scan(&files, &audits); err != nil || files != 1 || audits != 1 {
		t.Fatalf("Then recovery creates one file/audit: files=%d audits=%d error=%v", files, audits, err)
	}
	downloaded := world.request(t, http.MethodGet, "/api/files/"+file.ID, "", nil)
	thenStatus(t, downloaded, http.StatusOK)
	if string(downloaded.body) != "retryable content" {
		t.Fatalf("Then recovered upload retains its bytes: %q", downloaded.body)
	}
}

func TestBDDInterruptedMultipartUploadLeavesNoState(t *testing.T) {
	// Given a real TCP upload large enough to spill multipart parsing to disk.
	multipartDir := t.TempDir()
	t.Setenv("TMPDIR", multipartDir)
	finished := make(chan struct{})
	world := givenBDDApp(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer close(finished)
			next.ServeHTTP(w, r)
		})
	})
	address, err := url.Parse(world.url)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(t.Context(), "tcp", address.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := connection.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err := connection.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	const boundary = "interrupted-upload-boundary"
	prefix := "--" + boundary + "\r\nContent-Disposition: form-data; name=\"file\"; filename=\"interrupted.bin\"\r\nContent-Type: application/octet-stream\r\n\r\n"
	payload := strings.Repeat("x", 2<<20)
	expectedLength := len(prefix) + len(payload) + len("\r\n--"+boundary+"--\r\n")
	if _, err := fmt.Fprintf(connection, "POST /api/files HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\nContent-Type: multipart/form-data; boundary=%s\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s%s", address.Host, world.token, boundary, expectedLength, prefix, payload); err != nil {
		t.Fatal(err)
	}
	// When the client disconnects its write side before the final boundary,
	// net/http observes a truncated body instead of an injected helper error.
	tcp, ok := connection.(*net.TCPConn)
	if !ok {
		t.Fatal("Given loopback dial must return a TCP connection")
	}
	if err := tcp.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(connection), nil)
	if err != nil {
		t.Fatal(err)
	}
	data, readErr := io.ReadAll(response.Body)
	closeErr := response.Body.Close()
	if readErr != nil || closeErr != nil || response.StatusCode != http.StatusBadRequest {
		t.Fatalf("Then interrupted upload is rejected: status=%d body=%q read=%v close=%v", response.StatusCode, data, readErr, closeErr)
	}
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("Then upload handler must return after the client closes its write side")
	}
	// Then no metadata, audit, durable blob or multipart scratch file remains.
	thenBDDNoFilesOrAudits(t, world)
	entries, err := os.ReadDir(multipartDir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("Then multipart scratch files are removed: entries=%v error=%v", entries, err)
	}
}

func thenBDDNoFilesOrAudits(t *testing.T, world *bddWorld) {
	t.Helper()
	var files, audits int
	if err := world.store.DB().QueryRowContext(t.Context(), `SELECT (SELECT COUNT(*) FROM files), (SELECT COUNT(*) FROM activity_log)`).Scan(&files, &audits); err != nil || files != 0 || audits != 0 {
		t.Fatalf("Then rejected upload leaves no metadata/audits: files=%d audits=%d error=%v", files, audits, err)
	}
	entries, err := os.ReadDir(world.store.FilesDir())
	if err != nil || len(entries) != 0 {
		t.Fatalf("Then rejected upload leaves no blobs: entries=%v error=%v", entries, err)
	}
}

func TestBDDFileDownloadRejectsInconsistentBlobWithoutAudit(t *testing.T) {
	for _, failure := range []string{"truncated", "directory"} {
		t.Run(failure, func(t *testing.T) {
			// Given a valid HTTP upload whose backing blob is later corrupted.
			world := givenBDDApp(t)
			body, contentType := givenBDDFileBody(t, "integrity.txt", "text/plain", []byte("complete content"))
			uploaded := world.request(t, http.MethodPost, "/api/files", contentType, body)
			thenStatus(t, uploaded, http.StatusCreated)
			file := thenBDDFileSchema(t, uploaded)
			path := filepath.Join(world.store.FilesDir(), file.ID)
			if failure == "truncated" {
				if err := os.Truncate(path, 1); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			}
			// When downloading, then failure precedes both delivery and its audit.
			response := world.request(t, http.MethodGet, "/api/files/"+file.ID, "", nil)
			thenStatus(t, response, http.StatusInternalServerError)
			if response.header.Get("Content-Disposition") != "" || bytes.Contains(response.body, []byte("complete content")) {
				t.Fatalf("Then inconsistent content is not served: headers=%v body=%q", response.header, response.body)
			}
			var audits int
			if err := world.store.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM activity_log WHERE action = 'download'`).Scan(&audits); err != nil || audits != 0 {
				t.Fatalf("Then inconsistent blob is not audited as a download: count=%d error=%v", audits, err)
			}
		})
	}
}

func TestBDDFileUploadNormalizesPathComponents(t *testing.T) {
	// Browsers may send a path in filename. Go's multipart parser takes the last
	// slash-delimited component before the application's filename validation.
	world := givenBDDApp(t)
	body, contentType := givenBDDFileBody(t, "../../nested/report.txt", "text/plain", []byte("local synthetic bytes"))
	response := world.request(t, http.MethodPost, "/api/files", contentType, body)
	thenStatus(t, response, http.StatusCreated)
	file := thenBDDFileSchema(t, response)
	if file.Filename != "report.txt" {
		t.Fatalf("Then the effective filename is the safe basename: %+v", file)
	}
	entries, err := os.ReadDir(world.store.FilesDir())
	if err != nil || len(entries) != 1 || entries[0].Name() != file.ID {
		t.Fatalf("Then storage uses only the server ID, never the supplied path: entries=%v error=%v", entries, err)
	}
	data, err := os.ReadFile(filepath.Join(world.store.FilesDir(), file.ID))
	if err != nil || string(data) != "local synthetic bytes" {
		t.Fatalf("Then the bytes stay in the owned store: data=%q error=%v", data, err)
	}
}
