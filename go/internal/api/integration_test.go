package api_test

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"

	"github.com/weave-lab/interview-public/go/internal/app"
	"github.com/weave-lab/interview-public/go/internal/auth"
	"github.com/weave-lab/interview-public/go/internal/store"
)

func testAPI(t *testing.T) (http.Handler, *store.Store, string) {
	t.Helper()
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := s.RegisterActors(t.Context(), []string{"alice@example.com"}); err != nil {
		t.Fatal(err)
	}
	token, err := auth.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	return app.NewRouter(s, app.Options{AuthTokens: map[string]string{token: "alice@example.com"}}), s, token
}

func request(h http.Handler, token, method, path, contentType string, body io.Reader) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, body)
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", contentType)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestContactMutationAuditIsAtomicAndNotDuplicated(t *testing.T) {
	h, s, token := testAPI(t)
	w := request(h, token, "POST", "/api/contacts", "application/json", strings.NewReader(`{"first_name":"Before","email":"before@example.com"}`))
	var contact store.Contact
	if err := json.Unmarshal(w.Body.Bytes(), &contact); err != nil || w.Code != 201 {
		t.Fatalf("create status=%d body=%s err=%v", w.Code, w.Body.String(), err)
	}
	w = request(h, token, "PUT", "/api/contacts/"+contact.ID, "application/json", strings.NewReader(`{"first_name":"After"}`))
	if w.Code != 200 {
		t.Fatalf("update status=%d body=%s", w.Code, w.Body.String())
	}
	w = request(h, token, "DELETE", "/api/contacts/"+contact.ID, "", nil)
	if w.Code != 204 {
		t.Fatalf("delete status=%d body=%s", w.Code, w.Body.String())
	}
	var logs int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM activity_log WHERE user_id = 'alice@example.com'`).Scan(&logs); err != nil || logs != 3 {
		t.Fatalf("audit rows=%d, want exactly3; err=%v", logs, err)
	}
	if _, err := s.DB().Exec(`CREATE TRIGGER reject_audit BEFORE INSERT ON activity_log BEGIN SELECT RAISE(FAIL, 'synthetic failure'); END`); err != nil {
		t.Fatal(err)
	}
	w = request(h, token, "POST", "/api/contacts", "application/json", strings.NewReader(`{"first_name":"Must roll back"}`))
	count, err := s.CountContacts(context.Background(), "alice@example.com")
	if w.Code != 500 || err != nil || count != 0 {
		t.Fatalf("failed audit: status=%d contacts=%d err=%v", w.Code, count, err)
	}
	for _, path := range []string{"/api/contacts/export", "/api/reports/activity"} {
		w = request(h, token, "GET", path, "", nil)
		if w.Code != 500 || w.Header().Get("Content-Type") != "application/json" {
			t.Fatalf("%s audit failure returned %d, headers=%v", path, w.Code, w.Header())
		}
	}
}

func TestImportValidationPrecedesAllWrites(t *testing.T) {
	h, s, token := testAPI(t)
	for _, body := range []string{`[{"first_name":"First"},{"unknown":"bad"}]`, `[{"first_name":"First"}] {}`} {
		w := request(h, token, "POST", "/api/contacts/import", "application/json", strings.NewReader(body))
		count, err := s.CountContacts(context.Background(), "alice@example.com")
		if w.Code != 400 || err != nil || count != 0 {
			t.Fatalf("status=%d count=%d err=%v body=%s", w.Code, count, err, w.Body.String())
		}
	}
	w := request(h, token, "POST", "/api/contacts/import", "application/json", strings.NewReader(`[{"first_name":"One"},{"first_name":"Two"}]`))
	count, err := s.CountContacts(context.Background(), "alice@example.com")
	if w.Code != 200 || err != nil || count != 2 {
		t.Fatalf("valid import: status=%d count=%d err=%v", w.Code, count, err)
	}
	var logs int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM activity_log WHERE action = 'import'`).Scan(&logs); err != nil || logs != 1 {
		t.Fatalf("import audit rows=%d err=%v", logs, err)
	}
}

func TestCSVExportNeutralizesFormulaWithoutChangingStoredValue(t *testing.T) {
	h, s, token := testAPI(t)
	w := request(h, token, "POST", "/api/contacts", "application/json", strings.NewReader(`{"first_name":" =1+1","last_name":"Normal","phone":"+15551234"}`))
	if w.Code != 201 {
		t.Fatal(w.Body.String())
	}
	w = request(h, token, "GET", "/api/contacts/export", "", nil)
	rows, err := csv.NewReader(w.Body).ReadAll()
	if err != nil || w.Code != 200 || len(rows) != 2 || rows[1][1] != "' =1+1" || rows[1][2] != "Normal" || rows[1][4] != "'+15551234" {
		t.Fatalf("CSV status=%d rows=%v err=%v", w.Code, rows, err)
	}
	contacts, err := s.ListContacts(context.Background(), "alice@example.com", 1, nil)
	if err != nil || len(contacts) != 1 || contacts[0].FirstName != " =1+1" {
		t.Fatalf("stored value changed: %+v %v", contacts, err)
	}
}

func uploadBody(t *testing.T, filename string, parts int) (*bytes.Buffer, string) {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for range parts {
		header := make(textproto.MIMEHeader)
		header.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{"name": "file", "filename": filename}))
		header.Set("Content-Type", "text/html; charset=utf-8")
		part, err := writer.CreatePart(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(part, "synthetic file content"); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return &body, writer.FormDataContentType()
}

func TestUploadDownloadHeadersAndAuditFailure(t *testing.T) {
	h, s, token := testAPI(t)
	filename := `report; name "quoted".html`
	body, contentType := uploadBody(t, filename, 1)
	w := request(h, token, "POST", "/api/files", contentType, body)
	var file store.File
	if err := json.Unmarshal(w.Body.Bytes(), &file); err != nil || w.Code != 201 {
		t.Fatalf("upload status=%d body=%s err=%v", w.Code, w.Body.String(), err)
	}
	w = request(h, token, "GET", "/api/files/"+file.ID, "", nil)
	disposition, params, err := mime.ParseMediaType(w.Header().Get("Content-Disposition"))
	if w.Code != 200 || err != nil || disposition != "attachment" || params["filename"] != filename || len(params) != 1 || w.Body.String() != "synthetic file content" {
		t.Fatalf("download status=%d disposition=%q params=%v err=%v body=%q", w.Code, disposition, params, err, w.Body.String())
	}
	if w.Header().Get("X-Content-Type-Options") != "nosniff" || w.Header().Get("Content-Security-Policy") != "sandbox" {
		t.Fatalf("missing download protections: %v", w.Header())
	}
	if _, err := s.DB().Exec(`CREATE TRIGGER reject_audit BEFORE INSERT ON activity_log BEGIN SELECT RAISE(FAIL, 'synthetic failure'); END`); err != nil {
		t.Fatal(err)
	}
	w = request(h, token, "GET", "/api/files/"+file.ID, "", nil)
	if w.Code != 500 || w.Header().Get("Content-Disposition") != "" || strings.Contains(w.Body.String(), "synthetic file content") {
		t.Fatalf("download did not fail before data delivery: %d %v %q", w.Code, w.Header(), w.Body.String())
	}
}

func TestUploadRejectsInvalidFormsBeforeStorage(t *testing.T) {
	h, s, token := testAPI(t)
	for _, tt := range []struct {
		filename string
		parts    int
	}{
		{"two-files.txt", 2},
		{strings.Repeat("x", 256), 1},
		{"bad\nname.txt", 1},
	} {
		body, contentType := uploadBody(t, tt.filename, tt.parts)
		w := request(h, token, "POST", "/api/files", contentType, body)
		if w.Code != 400 {
			t.Fatalf("invalid upload status=%d body=%s", w.Code, w.Body.String())
		}
	}
	files, err := s.ListFiles(context.Background(), "alice@example.com")
	if err != nil || len(files) != 0 {
		t.Fatalf("invalid uploads persisted files=%v err=%v", files, err)
	}
	r := httptest.NewRequest("POST", "/api/files", strings.NewReader("small"))
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", "multipart/form-data; boundary=test")
	r.ContentLength = store.MaxFileSize + (1 << 20) + 1
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized upload status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestQueryBounds(t *testing.T) {
	h, _, token := testAPI(t)
	for _, path := range []string{"/api/contacts?limit=0", "/api/contacts?limit=101", "/api/contacts?limit=bad", "/api/contacts?page_token=" + strings.Repeat("A", 2049), "/api/reports/activity?since=invalid"} {
		w := request(h, token, "GET", path, "", nil)
		if w.Code != 400 {
			t.Fatalf("%s returned %d: %s", path, w.Code, w.Body.String())
		}
	}
}

type failedResponse struct {
	header http.Header
	codes  []int
}

func (w *failedResponse) Header() http.Header  { return w.header }
func (w *failedResponse) WriteHeader(code int) { w.codes = append(w.codes, code) }
func (w *failedResponse) Write([]byte) (int, error) {
	if len(w.codes) == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return 0, errors.New("synthetic disconnected response")
}

func TestFailedResponsesAbortInsteadOfCompletingPartialBodies(t *testing.T) {
	h, s, token := testAPI(t)
	file := store.File{ID: "test-file", Filename: "test.txt", ContentType: "text/plain"}
	if err := s.CreateFile(context.Background(), "alice@example.com", &file, strings.NewReader("content")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/contacts", "/api/contacts/export", "/api/files/test-file"} {
		t.Run(path, func(t *testing.T) {
			w := &failedResponse{header: make(http.Header)}
			r := httptest.NewRequest(http.MethodGet, path, nil)
			r.Header.Set("Authorization", "Bearer "+token)
			defer func() {
				if got := recover(); got != http.ErrAbortHandler {
					t.Errorf("panic=%v, want http.ErrAbortHandler", got)
				}
				if len(w.codes) != 1 || w.codes[0] != 200 {
					t.Errorf("unexpected additional status write: %v", w.codes)
				}
			}()
			h.ServeHTTP(w, r)
		})
	}
}

func TestInconsistentFileMetadataFailsBeforeDownload(t *testing.T) {
	h, s, token := testAPI(t)
	file := store.File{ID: "test-file", Filename: "test.txt", ContentType: "text/plain"}
	if err := s.CreateFile(context.Background(), "alice@example.com", &file, strings.NewReader("content")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`UPDATE files SET size = size + 1 WHERE id = ?`, file.ID); err != nil {
		t.Fatal(err)
	}
	w := request(h, token, http.MethodGet, "/api/files/test-file", "", nil)
	if w.Code != 500 || w.Header().Get("Content-Disposition") != "" {
		t.Fatalf("inconsistent file served: status=%d headers=%v", w.Code, w.Header())
	}
}
