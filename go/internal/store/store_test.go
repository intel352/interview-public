package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := s.RegisterActors(t.Context(), []string{"local-demo", "seed"}); err != nil {
		t.Fatal(err)
	}
	return s
}

func rejectAuditWrites(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.DB().Exec(`CREATE TRIGGER reject_audit BEFORE INSERT ON activity_log BEGIN SELECT RAISE(ABORT, 'audit unavailable'); END`); err != nil {
		t.Fatal(err)
	}
}

func TestContactAuditFailureRollsBack(t *testing.T) {
	s := testStore(t)
	rejectAuditWrites(t, s)
	c := &Contact{ID: "contact-1", FirstName: "Before"}
	if err := s.CreateContact(context.Background(), "local-demo", c); err == nil {
		t.Fatal("create succeeded despite audit failure")
	}
	count, err := s.CountContacts(context.Background(), "local-demo")
	if err != nil || count != 0 {
		t.Fatalf("count=%d, err=%v; want empty store", count, err)
	}
}

func TestDuplicateFilePreservesOriginal(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.CreateFile(ctx, "local-demo", &File{ID: "file-1"}, strings.NewReader("original")); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateFile(ctx, "local-demo", &File{ID: "file-1"}, strings.NewReader("replacement")); err == nil {
		t.Fatal("duplicate file accepted")
	}
	f, _, err := s.OpenFile(ctx, "local-demo", "file-1")
	if err != nil {
		t.Fatal(err)
	}
	data, readErr := io.ReadAll(f)
	closeErr := f.Close()
	if readErr != nil || closeErr != nil || string(data) != "original" {
		t.Fatalf("original content=%q readErr=%v closeErr=%v", data, readErr, closeErr)
	}
}

func TestContactMutationsRollbackOnAuditFailure(t *testing.T) {
	for _, operation := range []string{"create", "update", "delete", "import"} {
		t.Run(operation, func(t *testing.T) {
			s := testStore(t)
			ctx := t.Context()
			if err := s.CreateContact(ctx, "local-demo", &Contact{ID: "original", FirstName: "Before"}); err != nil {
				t.Fatal(err)
			}
			rejectAuditWrites(t, s)
			var err error
			switch operation {
			case "create":
				err = s.CreateContact(ctx, "local-demo", &Contact{ID: "new"})
			case "update":
				err = s.UpdateContact(ctx, "local-demo", &Contact{ID: "original", FirstName: "After"})
			case "delete":
				err = s.DeleteContact(ctx, "local-demo", "original")
			case "import":
				var imported int
				imported, err = s.ImportContacts(ctx, "local-demo", []Contact{{ID: "new-1"}, {ID: "new-2"}})
				if imported != 0 {
					t.Fatalf("reported %d imports for rolled-back transaction", imported)
				}
			}
			if err == nil {
				t.Fatal("mutation succeeded despite audit failure")
			}
			contacts, err := s.ExportContacts(ctx, "local-demo")
			if err != nil || len(contacts) != 1 || contacts[0].ID != "original" || contacts[0].FirstName != "Before" {
				t.Fatalf("contacts=%+v err=%v; original must be unchanged", contacts, err)
			}
			if got := activityCount(t, s); got != 1 {
				t.Fatalf("audit records=%d, want 1", got)
			}
		})
	}
}

func TestImportConstraintFailureRollsBackEarlierRowsAndAllowsRetry(t *testing.T) {
	for _, conflict := range []string{"existing contact", "duplicate within batch"} {
		t.Run(conflict, func(t *testing.T) {
			s := testStore(t)
			original := &Contact{ID: "original", FirstName: "Keep me"}
			if err := s.CreateContact(t.Context(), "local-demo", original); err != nil {
				t.Fatal(err)
			}
			batch := []Contact{{ID: "first", FirstName: "First"}, {ID: "original", FirstName: "Replacement"}}
			if conflict == "duplicate within batch" {
				batch[1].ID = "first"
			}
			// The first INSERT succeeds before SQLite rejects the second one.
			if imported, err := s.ImportContacts(t.Context(), "local-demo", batch); err == nil || imported != 0 {
				t.Fatalf("partial import: count=%d error=%v", imported, err)
			}
			contacts, err := s.ExportContacts(t.Context(), "local-demo")
			if err != nil || len(contacts) != 1 || contacts[0].ID != original.ID || contacts[0].FirstName != original.FirstName || contacts[0].OwnerID != original.OwnerID || !contacts[0].CreatedAt.Equal(original.CreatedAt) || !contacts[0].UpdatedAt.Equal(original.UpdatedAt) || activityCount(t, s) != 1 {
				t.Fatalf("failed import changed durable rows: contacts=%+v error=%v", contacts, err)
			}
			batch[1] = Contact{ID: "second", FirstName: "Second"}
			if imported, err := s.ImportContacts(t.Context(), "local-demo", batch); err != nil || imported != 2 {
				t.Fatalf("retry failed after rollback: count=%d error=%v", imported, err)
			}
			for _, expected := range batch {
				actual, err := s.GetContact(t.Context(), "local-demo", expected.ID)
				if err != nil || actual.FirstName != expected.FirstName || actual.OwnerID != "local-demo" {
					t.Fatalf("retried import row=%+v error=%v; want %+v", actual, err, expected)
				}
			}
			var importAudits int
			if err := s.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM activity_log WHERE user_id='local-demo' AND action='import' AND resource_type='contacts'`).Scan(&importAudits); err != nil || importAudits != 1 || activityCount(t, s) != 2 {
				t.Fatalf("retry audit records=%d error=%v", importAudits, err)
			}
		})
	}
}

func activityCount(t *testing.T, s *Store) int {
	t.Helper()
	var count int
	if err := s.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM activity_log`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestConcurrentWritersAllPersistWithAudit(t *testing.T) {
	s := testStore(t)
	const writers = 32
	start := make(chan struct{})
	errorsCh := make(chan error, writers)
	var wg sync.WaitGroup
	for i := range writers {
		wg.Go(func() {
			<-start
			actor := []string{"local-demo", "seed"}[i%2]
			errorsCh <- s.CreateContact(t.Context(), actor, &Contact{ID: fmt.Sprintf("contact-%d", i), FirstName: fmt.Sprintf("Writer %d", i)})
		})
	}
	close(start)
	wg.Wait()
	close(errorsCh)
	for err := range errorsCh {
		if err != nil {
			t.Errorf("concurrent write: %v", err)
		}
	}
	count, err := s.CountAllContacts(t.Context())
	if err != nil || count != writers || activityCount(t, s) != writers {
		t.Fatalf("contacts=%d, err=%v; want %d contacts and audits", count, err, writers)
	}
	for i := range writers {
		actor := []string{"local-demo", "seed"}[i%2]
		id := fmt.Sprintf("contact-%d", i)
		contact, err := s.GetContact(t.Context(), actor, id)
		if err != nil || contact.FirstName != fmt.Sprintf("Writer %d", i) || contact.OwnerID != actor {
			t.Fatalf("writer %d persisted contact=%+v error=%v", i, contact, err)
		}
		var matchingAudits int
		if err := s.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM activity_log WHERE user_id=? AND action='create' AND resource_type='contact' AND resource_id=?`, actor, id).Scan(&matchingAudits); err != nil || matchingAudits != 1 {
			t.Fatalf("writer %d matching audits=%d error=%v", i, matchingAudits, err)
		}
	}
}

func TestCanceledWriterWaitingForConnection(t *testing.T) {
	s := testStore(t)
	conn, err := s.DB().Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if conn != nil {
			if err := conn.Close(); err != nil {
				t.Error(err)
			}
		}
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error, 1)
	before := s.DB().Stats().WaitCount
	go func() { result <- s.CreateContact(ctx, "local-demo", &Contact{ID: "canceled"}) }()
	deadline := time.After(5 * time.Second)
	for s.DB().Stats().WaitCount == before {
		select {
		case <-deadline:
			t.Fatal("writer never waited for the occupied connection")
		default:
			runtime.Gosched()
		}
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled writer remained blocked")
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	conn = nil
	if count, err := s.CountAllContacts(t.Context()); err != nil || count != 0 || activityCount(t, s) != 0 {
		t.Fatalf("canceled waiter persisted a mutation: contacts=%d error=%v", count, err)
	}
	if err := s.CreateContact(t.Context(), "local-demo", &Contact{ID: "recovered"}); err != nil {
		t.Fatalf("connection was not reusable after cancellation: %v", err)
	}
	if count, err := s.CountAllContacts(t.Context()); err != nil || count != 1 || activityCount(t, s) != 1 {
		t.Fatalf("recovery mutation: contacts=%d error=%v", count, err)
	}
}

func TestFileAuditFailureAndCopyFailureCleanUp(t *testing.T) {
	t.Run("audit", func(t *testing.T) {
		s := testStore(t)
		rejectAuditWrites(t, s)
		if err := s.CreateFile(t.Context(), "local-demo", &File{ID: "upload"}, strings.NewReader("content")); err == nil {
			t.Fatal("upload succeeded despite audit failure")
		}
		assertNoFiles(t, s)
	})
	t.Run("reader", func(t *testing.T) {
		s := testStore(t)
		readErr := errors.New("reader failed")
		reader := io.MultiReader(strings.NewReader("partial"), failingReader{err: readErr})
		err := s.CreateFile(t.Context(), "local-demo", &File{ID: "upload"}, reader)
		if !errors.Is(err, readErr) {
			t.Fatalf("got %v, want reader error", err)
		}
		assertNoFiles(t, s)
	})
	t.Run("cancellation", func(t *testing.T) {
		s := testStore(t)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		err := s.CreateFile(ctx, "local-demo", &File{ID: "upload"}, cancelReader{cancel: cancel})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v, want canceled copy", err)
		}
		assertNoFiles(t, s)
	})
	t.Run("cancellation on final read", func(t *testing.T) {
		s := testStore(t)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		err := s.CreateFile(ctx, "local-demo", &File{ID: "upload"}, cancelEOFReader{cancel: cancel})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v, want cancellation even when the final bytes accompany EOF", err)
		}
		assertNoFiles(t, s)
	})
	t.Run("size", func(t *testing.T) {
		s := testStore(t)
		err := s.CreateFile(t.Context(), "local-demo", &File{ID: "upload"}, io.LimitReader(zeroReader{}, (100<<20)+1))
		if !errors.Is(err, ErrFileTooLarge) {
			t.Fatalf("got %v, want size limit", err)
		}
		assertNoFiles(t, s)
	})
}

func TestFileAtExactSizeLimitPersistsAndClosesCleanly(t *testing.T) {
	// Keep the public boundary independent of MaxFileSize so changing the
	// implementation constant cannot silently change the contract tested here.
	const allowedBytes int64 = 100 << 20
	dir := filepath.Join(t.TempDir(), "exact-size-upload")
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := s.RegisterActors(t.Context(), []string{"alice"}); err != nil {
		t.Fatal(err)
	}
	upload := &File{ID: "exact-size", Filename: "boundary.bin", ContentType: "application/octet-stream"}
	// Generate bytes as the store reads them; no 100 MiB allocation is needed.
	if err := s.CreateFile(t.Context(), "alice", upload, io.LimitReader(zeroReader{}, allowedBytes)); err != nil {
		t.Fatalf("exact 100 MiB upload was rejected: %v", err)
	}
	file, metadata, err := s.OpenFile(t.Context(), "alice", upload.ID)
	if err != nil {
		t.Fatal(err)
	}
	info, statErr := file.Stat()
	readBytes, readErr := io.Copy(io.Discard, file)
	closeErr := file.Close()
	if statErr != nil || readErr != nil || closeErr != nil {
		t.Fatalf("read complete upload: stat=%v read=%v close=%v", statErr, readErr, closeErr)
	}
	if info.Size() != allowedBytes || readBytes != allowedBytes || metadata.Size != allowedBytes || upload.Size != allowedBytes || metadata.OwnerID != "alice" {
		t.Fatalf("exact-size upload changed: disk=%d read=%d metadata=%+v returned=%+v", info.Size(), readBytes, metadata, upload)
	}
	var matchingAudits int
	if err := s.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM activity_log WHERE user_id='alice' AND action='upload' AND resource_type='file' AND resource_id='exact-size'`).Scan(&matchingAudits); err != nil || matchingAudits != 1 || activityCount(t, s) != 1 {
		t.Fatalf("upload audit count=%d error=%v", matchingAudits, err)
	}
	files, err := s.ListFiles(t.Context(), "alice")
	if err != nil || len(files) != 1 || files[0].ID != upload.ID {
		t.Fatalf("persisted file records=%+v error=%v", files, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("clean test-owned large upload: %v", err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("test-owned upload directory remains after cleanup: %v", err)
	}
}

type failingReader struct{ err error }

func (r failingReader) Read([]byte) (int, error) { return 0, r.err }

type cancelReader struct{ cancel context.CancelFunc }

func (r cancelReader) Read(p []byte) (int, error) {
	r.cancel()
	return copy(p, "partial"), nil
}

type zeroReader struct{}

type cancelEOFReader struct{ cancel context.CancelFunc }

func (r cancelEOFReader) Read(p []byte) (int, error) {
	r.cancel()
	return copy(p, "final bytes"), io.EOF
}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

func assertNoFiles(t *testing.T, s *Store) {
	t.Helper()
	entries, err := os.ReadDir(s.FilesDir())
	if err != nil || len(entries) != 0 {
		t.Fatalf("remaining content files=%v err=%v", entries, err)
	}
	files, err := s.ListFiles(t.Context(), "local-demo")
	if err != nil || len(files) != 0 || activityCount(t, s) != 0 {
		t.Fatalf("remaining metadata=%v err=%v; want no files or audits", files, err)
	}
}

func TestInvalidFileIDs(t *testing.T) {
	s := testStore(t)
	for _, id := range []string{"", ".", "..", "../escape", "/absolute", `..\escape`, "nested/file", "file.txt", strings.Repeat("a", 129)} {
		if err := s.CreateFile(t.Context(), "local-demo", &File{ID: id}, strings.NewReader("content")); !errors.Is(err, ErrInvalidFileID) {
			t.Errorf("create ID %q: got %v", id, err)
		}
		if _, err := s.GetFile(t.Context(), "local-demo", id); !errors.Is(err, ErrInvalidFileID) {
			t.Errorf("get ID %q: got %v", id, err)
		}
	}
	assertNoFiles(t, s)
}

func TestConcurrentDuplicateFiles(t *testing.T) {
	s := testStore(t)
	type result struct {
		writer int
		err    error
	}
	actors := []string{"local-demo", "seed"}
	contents := []string{"Alice's original bytes", "Bob's distinct replacement bytes"}
	results := make(chan result, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range 2 {
		wg.Go(func() {
			<-start
			file := &File{ID: "same-file", Filename: fmt.Sprintf("writer-%d.txt", i), ContentType: "text/plain"}
			results <- result{i, s.CreateFile(t.Context(), actors[i], file, strings.NewReader(contents[i]))}
		})
	}
	close(start)
	wg.Wait()
	close(results)
	accepted, duplicates := 0, 0
	winner := -1
	for result := range results {
		switch {
		case result.err == nil:
			accepted++
			winner = result.writer
		case errors.Is(result.err, ErrFileExists):
			duplicates++
		default:
			t.Errorf("unexpected error: %v", result.err)
		}
	}
	if accepted != 1 || duplicates != 1 || activityCount(t, s) != 1 {
		t.Fatalf("accepted=%d duplicates=%d; want one complete upload", accepted, duplicates)
	}
	file, metadata, err := s.OpenFile(t.Context(), actors[winner], "same-file")
	if err != nil {
		t.Fatal(err)
	}
	content, readErr := io.ReadAll(file)
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || string(content) != contents[winner] || metadata.Size != int64(len(contents[winner])) || metadata.OwnerID != actors[winner] || metadata.Filename != fmt.Sprintf("writer-%d.txt", winner) {
		t.Fatalf("winner's bytes/metadata changed: content=%q metadata=%+v read=%v close=%v", content, metadata, readErr, closeErr)
	}
	if _, err := s.GetFile(t.Context(), actors[1-winner], "same-file"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("losing actor can read winning metadata: %v", err)
	}
	var auditActor, action, resource, resourceID string
	if err := s.DB().QueryRowContext(t.Context(), `SELECT user_id, action, resource_type, resource_id FROM activity_log`).Scan(&auditActor, &action, &resource, &resourceID); err != nil {
		t.Fatal(err)
	}
	if auditActor != actors[winner] || action != "upload" || resource != "file" || resourceID != "same-file" {
		t.Fatalf("audit does not describe the winning upload: %q %q %q %q", auditActor, action, resource, resourceID)
	}
}

func TestStorePermissionsAndSQLiteSettings(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private")
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := s.RegisterActors(t.Context(), []string{"local-demo"}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateFile(t.Context(), "local-demo", &File{ID: "upload"}, strings.NewReader("content")); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		for _, path := range []string{dir, s.FilesDir(), filepath.Join(dir, "app.db"), filepath.Join(s.FilesDir(), "upload")} {
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm()&0077 != 0 {
				t.Fatalf("path %s is not private: info=%v err=%v", path, info, err)
			}
		}
	}
	if s.DB().Stats().MaxOpenConnections != 1 {
		t.Fatal("SQLite pool must serialize local writes")
	}
	var mode string
	var timeout int
	if err := s.DB().QueryRowContext(t.Context(), `PRAGMA journal_mode`).Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal_mode=%s err=%v", mode, err)
	}
	if err := s.DB().QueryRowContext(t.Context(), `PRAGMA busy_timeout`).Scan(&timeout); err != nil || timeout != 5000 {
		t.Fatalf("busy_timeout=%d err=%v", timeout, err)
	}
}

func TestUpdateContactPreservesCreatedAt(t *testing.T) {
	s := testStore(t)
	original := &Contact{ID: "original", FirstName: "Before"}
	if err := s.CreateContact(t.Context(), "local-demo", original); err != nil {
		t.Fatal(err)
	}
	updated := &Contact{ID: original.ID, FirstName: "After"}
	if err := s.UpdateContact(t.Context(), "local-demo", updated); err != nil {
		t.Fatal(err)
	}
	if updated.CreatedAt.IsZero() || !updated.CreatedAt.Equal(original.CreatedAt) {
		t.Fatalf("update returned created_at=%s, want %s", updated.CreatedAt, original.CreatedAt)
	}
	stored, err := s.GetContact(t.Context(), "local-demo", original.ID)
	if err != nil || !stored.CreatedAt.Equal(updated.CreatedAt) || !stored.UpdatedAt.Equal(updated.UpdatedAt) {
		t.Fatalf("stored timestamps differ from update response: stored=%+v updated=%+v err=%v", stored, updated, err)
	}
	if activityCount(t, s) != 2 {
		t.Fatal("successful update must add one audit record")
	}
}

func TestUpdateContactFailureLeavesTimestampsAndAuditUnchanged(t *testing.T) {
	for _, operation := range []string{"missing", "audit failure"} {
		t.Run(operation, func(t *testing.T) {
			s := testStore(t)
			original := &Contact{ID: "original", FirstName: "Before"}
			if err := s.CreateContact(t.Context(), "local-demo", original); err != nil {
				t.Fatal(err)
			}
			updated := &Contact{ID: "missing", FirstName: "After"}
			if operation == "audit failure" {
				updated.ID = original.ID
				rejectAuditWrites(t, s)
			}
			err := s.UpdateContact(t.Context(), "local-demo", updated)
			if err == nil || (operation == "missing" && !errors.Is(err, ErrNotFound)) {
				t.Fatalf("unexpected update error: %v", err)
			}
			if !updated.CreatedAt.IsZero() || !updated.UpdatedAt.IsZero() {
				t.Fatalf("failed update changed caller timestamps: %+v", updated)
			}
			stored, err := s.GetContact(t.Context(), "local-demo", original.ID)
			if err != nil || stored.FirstName != original.FirstName || !stored.CreatedAt.Equal(original.CreatedAt) || !stored.UpdatedAt.Equal(original.UpdatedAt) {
				t.Fatalf("failed update changed original: %+v err=%v", stored, err)
			}
			if activityCount(t, s) != 1 {
				t.Fatal("failed update changed audit history")
			}
		})
	}
}
