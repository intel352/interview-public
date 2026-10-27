package app

import (
	"bytes"
	"encoding/json"
	"maps"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/weave-lab/interview-public/go/internal/store"
)

func givenBDDFileBody(t *testing.T, filename, contentType string, content []byte) (*bytes.Buffer, string) {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{"name": "file", "filename": filename}))
	header.Set("Content-Type", contentType)
	part, err := writer.CreatePart(header)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return &body, writer.FormDataContentType()
}

func thenBDDFileSchema(t *testing.T, response bddResponse) store.File {
	t.Helper()
	if response.header.Get("Content-Type") != "application/json" {
		t.Errorf("Then file metadata must be served as JSON: %v", response.header)
	}
	fields := decodeBDD[map[string]json.RawMessage](t, response)
	for _, field := range []string{"id", "owner_id", "filename", "size", "content_type", "created_at"} {
		if _, ok := fields[field]; !ok {
			t.Errorf("Then file response must contain %q", field)
		}
	}
	if len(fields) != 6 {
		t.Errorf("Then file response must have six documented fields; got %d", len(fields))
	}
	file := decodeBDD[store.File](t, response)
	if file.ID == "" || file.OwnerID == "" || file.CreatedAt.IsZero() {
		t.Fatalf("Then the server must assign an ID, owner and creation timestamp: %+v", file)
	}
	return file
}

func TestBDDFilesRoundTrip(t *testing.T) {
	for _, scenario := range []struct {
		name, filename, contentType, storedType string
		content                                 []byte
	}{
		{"binary_content_with_quoted_filename", `report; "quoted".bin`, "application/octet-stream", "application/octet-stream", []byte{0, 1, 2, '\n', 127, 128, 255}},
		{"empty_file", "empty.txt", "text/plain; charset=utf-8", "text/plain; charset=utf-8", nil},
		{"invalid_media_type_falls_back_to_binary", "unknown.bin", "not a media type", "application/octet-stream", []byte("synthetic")},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// Given an authenticated consumer and an empty private file store.
			world := givenBDDApp(t)
			empty := world.request(t, http.MethodGet, "/api/files", "", nil)
			thenStatus(t, empty, http.StatusOK)
			if files := decodeBDD[[]store.File](t, empty); len(files) != 0 {
				t.Fatalf("Given store should be empty: %+v", files)
			}
			if string(bytes.TrimSpace(empty.body)) != "[]" {
				t.Fatalf("Then an empty file list must be []: %s", empty.body)
			}
			body, contentType := givenBDDFileBody(t, scenario.filename, scenario.contentType, scenario.content)

			// When the consumer uploads, lists and downloads that file over HTTP.
			upload := world.request(t, http.MethodPost, "/api/files", contentType, body)
			thenStatus(t, upload, http.StatusCreated)
			file := thenBDDFileSchema(t, upload)
			listed := world.request(t, http.MethodGet, "/api/files", "", nil)
			thenStatus(t, listed, http.StatusOK)
			download := world.request(t, http.MethodGet, "/api/files/"+file.ID, "", nil)
			thenStatus(t, download, http.StatusOK)

			// Then metadata, payload and attachment protections survive the round trip.
			if file.OwnerID != world.actor || file.Filename != scenario.filename || file.ContentType != scenario.storedType || file.Size != int64(len(scenario.content)) {
				t.Errorf("Then upload metadata must match: %+v", file)
			}
			files := decodeBDD[[]store.File](t, listed)
			if len(files) != 1 || files[0] != file {
				t.Errorf("Then list must contain exactly the uploaded metadata: %+v", files)
			}
			if !bytes.Equal(download.body, scenario.content) {
				t.Errorf("Then downloaded bytes must match: got %v, want %v", download.body, scenario.content)
			}
			disposition, params, err := mime.ParseMediaType(download.header.Get("Content-Disposition"))
			if err != nil || disposition != "attachment" || len(params) != 1 || params["filename"] != scenario.filename {
				t.Errorf("Then filename must be safely encoded: %q, %v", download.header.Get("Content-Disposition"), err)
			}
			for header, want := range map[string]string{
				"Content-Type": scenario.storedType, "Content-Length": strconv.Itoa(len(scenario.content)),
				"X-Content-Type-Options": "nosniff", "Content-Security-Policy": "sandbox", "Cache-Control": "no-store",
			} {
				if got := download.header.Get(header); got != want {
					t.Errorf("Then %s = %q, want %q", header, got, want)
				}
			}
			var audits int
			if err := world.store.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM activity_log WHERE user_id = ? AND resource_id = ? AND action IN ('upload', 'download')`, world.actor, file.ID).Scan(&audits); err != nil {
				t.Fatal(err)
			}
			if audits != 2 {
				t.Errorf("Then upload and download each record one owner-specific audit: %d", audits)
			}
		})
	}
}

func TestBDDFileRequestErrors(t *testing.T) {
	// Given an empty file store and a valid actor credential.
	world := givenBDDApp(t)
	for _, scenario := range []struct {
		name, method, path, contentType, body string
		status                                int
	}{
		{"missing_file", http.MethodGet, "/api/files/missing-file", "", "", http.StatusNotFound},
		{"invalid_storage_id", http.MethodGet, "/api/files/not.a.valid.id", "", "", http.StatusBadRequest},
		{"unsupported_upload_media", http.MethodPost, "/api/files", "application/json", `{}`, http.StatusUnsupportedMediaType},
		{"malformed_multipart", http.MethodPost, "/api/files", "multipart/form-data; boundary=expected", "not multipart", http.StatusBadRequest},
		{"missing_file_part", http.MethodPost, "/api/files", "multipart/form-data; boundary=empty", "--empty--\r\n", http.StatusBadRequest},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// When an invalid or missing resource is requested.
			response := world.request(t, scenario.method, scenario.path, scenario.contentType, strings.NewReader(scenario.body))
			// Then the documented error status and JSON envelope are returned.
			thenStatus(t, response, scenario.status)
			problem := decodeBDD[map[string]string](t, response)
			if len(problem) != 1 || problem["error"] == "" || response.header.Get("Content-Type") != "application/json" {
				t.Errorf("Then error must use the JSON error contract: headers=%v body=%s", response.header, response.body)
			}
		})
	}
	// Then rejected requests create neither file records nor successful audit entries.
	for _, table := range []string{"files", "activity_log"} {
		var count int
		if err := world.store.DB().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM "+table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Errorf("Then rejected requests must leave %s empty, got %d", table, count)
		}
	}
	entries, err := os.ReadDir(world.store.FilesDir())
	if err != nil || len(entries) != 0 {
		t.Fatalf("Then rejected uploads leave no blobs: entries=%d err=%v", len(entries), err)
	}
}

func TestBDDUnavailableFileFailsBeforeDelivery(t *testing.T) {
	// Given an uploaded file whose task-owned blob becomes unavailable.
	world := givenBDDApp(t)
	body, contentType := givenBDDFileBody(t, "missing.txt", "text/plain", []byte("private synthetic content"))
	upload := world.request(t, http.MethodPost, "/api/files", contentType, body)
	thenStatus(t, upload, http.StatusCreated)
	file := thenBDDFileSchema(t, upload)
	if err := os.Remove(filepath.Join(world.store.FilesDir(), file.ID)); err != nil {
		t.Fatal(err)
	}
	// When the consumer asks to download the missing blob.
	download := world.request(t, http.MethodGet, "/api/files/"+file.ID, "", nil)
	// Then no attachment or content is delivered, and no delivery attempt is recorded.
	thenStatus(t, download, http.StatusInternalServerError)
	problem := decodeBDD[map[string]string](t, download)
	if problem["error"] == "" || download.header.Get("Content-Disposition") != "" || bytes.Contains(download.body, []byte("private synthetic content")) {
		t.Errorf("Then unavailable content must not be delivered: headers=%v body=%s", download.header, download.body)
	}
	var count int
	if err := world.store.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM activity_log WHERE action = 'download'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("Then failed open must precede download audit: count=%d err=%v", count, err)
	}
}

func givenBDDActivity(t *testing.T, world *bddWorld, action, resource string, at time.Time) {
	t.Helper()
	// Fixed synthetic timestamps make report filtering deterministic. This fixture
	// uses the registered actor attached to this authenticated consumer.
	if _, err := world.store.DB().ExecContext(t.Context(), `INSERT INTO activity_log (user_id, action, resource_type, resource_id, created_at) VALUES (?, ?, ?, ?, ?)`, world.actor, action, resource, "fixture", store.DatabaseTime(at)); err != nil {
		t.Fatal(err)
	}
}

func thenBDDReport(t *testing.T, response bddResponse, actor string, actions, resources map[string]int) {
	t.Helper()
	thenStatus(t, response, http.StatusOK)
	if response.header.Get("Content-Type") != "application/json" || response.header.Get("Cache-Control") != "no-store" {
		t.Errorf("Then reports must be uncached JSON: %v", response.header)
	}
	reports := decodeBDD[[]store.ActivityReport](t, response)
	if len(reports) != 1 {
		t.Fatalf("Then report must aggregate only the requesting actor: %+v", reports)
	}
	total := 0
	for _, count := range actions {
		total += count
	}
	report := reports[0]
	if report.UserID != actor || report.TotalActions != total || !maps.Equal(report.ByAction, actions) || !maps.Equal(report.ByResource, resources) {
		t.Errorf("Then report must have exact aggregate counts: got %+v, want total=%d actions=%v resources=%v", report, total, actions, resources)
	}
	fields := decodeBDD[[]map[string]json.RawMessage](t, response)
	for _, key := range []string{"user_id", "total_actions", "by_action", "by_resource"} {
		if _, ok := fields[0][key]; !ok {
			t.Errorf("Then report schema must contain %q", key)
		}
	}
	if len(fields[0]) != 4 {
		t.Errorf("Then report schema must have four fields: %v", fields[0])
	}
}

func TestBDDActivityReportIncludesSinceBoundary(t *testing.T) {
	// Given records just before, exactly at and after an explicit UTC date boundary.
	world := givenBDDApp(t)
	boundary := time.Now().UTC().AddDate(0, 0, -2).Truncate(24 * time.Hour)
	path := "/api/reports/activity?since=" + boundary.Format("2006-01-02")
	givenBDDActivity(t, world, "excluded", "contact", boundary.Add(-time.Second))
	givenBDDActivity(t, world, "create", "contact", boundary)
	givenBDDActivity(t, world, "create", "contact", boundary.Add(time.Hour))
	givenBDDActivity(t, world, "upload", "file", boundary.Add(2*time.Hour))
	// When the consumer requests a report since that date.
	response := world.request(t, http.MethodGet, path, "", nil)
	// Then the boundary is inclusive and grouping counts only included records.
	thenBDDReport(t, response, world.actor, map[string]int{"create": 2, "upload": 1}, map[string]int{"contact": 2, "file": 1})
	// The report's own audit occurs after its snapshot; the next report includes it.
	next := world.request(t, http.MethodGet, path, "", nil)
	thenBDDReport(t, next, world.actor, map[string]int{"create": 2, "upload": 1, "view": 1}, map[string]int{"contact": 2, "file": 1, "report": 1})
}

func TestBDDActivityReportDefaultAndEmptyWindows(t *testing.T) {
	t.Run("default_last_thirty_days", func(t *testing.T) {
		// Given old and recent activity safely away from the moving 30-day cutoff.
		world := givenBDDApp(t)
		now := time.Now().UTC()
		givenBDDActivity(t, world, "old", "file", now.AddDate(0, 0, -31))
		givenBDDActivity(t, world, "recent", "contact", now.AddDate(0, 0, -1))
		// When since is omitted, then only recent activity is aggregated.
		response := world.request(t, http.MethodGet, "/api/reports/activity", "", nil)
		thenBDDReport(t, response, world.actor, map[string]int{"recent": 1}, map[string]int{"contact": 1})
	})
	t.Run("empty_window_is_an_array_and_still_audited", func(t *testing.T) {
		// Given no events after a future boundary.
		world := givenBDDApp(t)
		future := time.Now().UTC().AddDate(1, 0, 0).Format("2006-01-02")
		// When the consumer requests that window, then it gets an empty JSON array.
		response := world.request(t, http.MethodGet, "/api/reports/activity?since="+future, "", nil)
		thenStatus(t, response, http.StatusOK)
		if string(bytes.TrimSpace(response.body)) != "[]" {
			t.Errorf("Then an empty report must be []: %s", response.body)
		}
		var count int
		if err := world.store.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM activity_log WHERE user_id = ? AND action = 'view' AND resource_type = 'report'`, world.actor).Scan(&count); err != nil || count != 1 {
			t.Fatalf("Then even empty report delivery is audited: count=%d err=%v", count, err)
		}
	})
}

func TestBDDActivityReportRejectsInvalidDateWithoutAudit(t *testing.T) {
	// Given a valid consumer and no audit history.
	world := givenBDDApp(t)
	for _, date := range []string{"yesterday", "2026-02-30", "2026-01-01T12:00:00Z"} {
		t.Run(date, func(t *testing.T) {
			// When a malformed date is submitted, then validation returns a JSON error.
			response := world.request(t, http.MethodGet, "/api/reports/activity?since="+date, "", nil)
			thenStatus(t, response, http.StatusBadRequest)
			if problem := decodeBDD[map[string]string](t, response); problem["error"] == "" {
				t.Errorf("Then invalid date must explain the error: %s", response.body)
			}
		})
	}
	var count int
	if err := world.store.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM activity_log`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("Then validation must not record a report attempt: count=%d err=%v", count, err)
	}
}

func TestBDDFilesArePrivateToTheirOwner(t *testing.T) {
	// Given two registered actors and a file uploaded using Alice's credential.
	alice := givenBDDApp(t)
	bob := alice.forActor(t, "bob@example.com")
	body, contentType := givenBDDFileBody(t, "alice-only.txt", "text/plain", []byte("Alice's synthetic content"))
	upload := alice.request(t, http.MethodPost, "/api/files", contentType, body)
	thenStatus(t, upload, http.StatusCreated)
	file := thenBDDFileSchema(t, upload)
	if file.OwnerID != alice.actor {
		t.Fatalf("Then the upload owner is derived from authentication: %+v", file)
	}
	owned := alice.request(t, http.MethodGet, "/api/files/"+file.ID, "", nil)
	thenStatus(t, owned, http.StatusOK)
	if string(owned.body) != "Alice's synthetic content" {
		t.Fatalf("Given owner download must work: %q", owned.body)
	}

	// When Bob lists files and requests both a foreign ID and a nonexistent ID.
	listed := bob.request(t, http.MethodGet, "/api/files", "", nil)
	foreign := bob.request(t, http.MethodGet, "/api/files/"+file.ID, "", nil)
	missing := bob.request(t, http.MethodGet, "/api/files/not-present", "", nil)
	// Then the list is empty and the two lookups are indistinguishable 404s.
	thenStatus(t, listed, http.StatusOK)
	if string(bytes.TrimSpace(listed.body)) != "[]" {
		t.Fatalf("Then Bob must not see Alice's metadata: %s", listed.body)
	}
	thenStatus(t, foreign, http.StatusNotFound)
	thenStatus(t, missing, http.StatusNotFound)
	if !bytes.Equal(foreign.body, missing.body) || foreign.header.Get("Content-Disposition") != "" {
		t.Errorf("Then foreign and absent IDs must not disclose existence: foreign=%s missing=%s", foreign.body, missing.body)
	}

	// Given the owned blob is then removed, Bob still gets 404 before file access.
	if err := os.Remove(filepath.Join(alice.store.FilesDir(), file.ID)); err != nil {
		t.Fatal(err)
	}
	foreign = bob.request(t, http.MethodGet, "/api/files/"+file.ID, "", nil)
	thenStatus(t, foreign, http.StatusNotFound)
	if !bytes.Equal(foreign.body, missing.body) {
		t.Errorf("Then inaccessible blob state must remain private: %s", foreign.body)
	}
	var audits int
	if err := alice.store.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM activity_log WHERE user_id = ?`, bob.actor).Scan(&audits); err != nil || audits != 0 {
		t.Fatalf("Then denied/missing downloads do not create Bob audit records: count=%d err=%v", audits, err)
	}
}

func TestBDDFileOwnerCannotBeSuppliedByUpload(t *testing.T) {
	// Given Alice's credential and a multipart upload claiming Bob as owner.
	alice := givenBDDApp(t)
	_ = alice.forActor(t, "bob@example.com")
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", "forged-owner.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte("synthetic")); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteField("owner_id", "bob@example.com"); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	// When Alice submits that payload, then the extra ownership field is rejected.
	response := alice.request(t, http.MethodPost, "/api/files", writer.FormDataContentType(), &body)
	thenStatus(t, response, http.StatusBadRequest)
	var count int
	if err := alice.store.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM files`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("Then rejected ownership input creates no file records: count=%d err=%v", count, err)
	}
}

func TestBDDActivityReportsArePrivateToActor(t *testing.T) {
	// Given different activity for two registered actors in the same SQLite store.
	alice := givenBDDApp(t)
	bob := alice.forActor(t, "bob@example.com")
	at := time.Now().UTC().Add(-time.Hour)
	givenBDDActivity(t, alice, "create", "contact", at)
	givenBDDActivity(t, alice, "upload", "file", at)
	for range 3 {
		givenBDDActivity(t, bob, "delete", "contact", at)
	}
	// When each actor requests a report, even with another actor in a query field.
	aliceReport := alice.request(t, http.MethodGet, "/api/reports/activity", "", nil)
	bobReport := bob.request(t, http.MethodGet, "/api/reports/activity?user_id=alice@example.com", "", nil)
	// Then credentials alone scope the report; aggregate counts cannot leak across actors.
	thenBDDReport(t, aliceReport, alice.actor, map[string]int{"create": 1, "upload": 1}, map[string]int{"contact": 1, "file": 1})
	thenBDDReport(t, bobReport, bob.actor, map[string]int{"delete": 3}, map[string]int{"contact": 3})
	// Alice's next report includes her own attempt, but never Bob's attempt.
	next := alice.request(t, http.MethodGet, "/api/reports/activity", "", nil)
	thenBDDReport(t, next, alice.actor, map[string]int{"create": 1, "upload": 1, "view": 1}, map[string]int{"contact": 1, "file": 1, "report": 1})
}
