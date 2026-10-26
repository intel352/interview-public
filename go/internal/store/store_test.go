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
	count, err := s.CountContacts(context.Background())
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
	f, _, err := s.OpenFile(ctx, "file-1")
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
			if err := s.CreateContact(ctx, "seed", &Contact{ID: "original", FirstName: "Before"}); err != nil {
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
			contacts, err := s.ExportContacts(ctx)
			if err != nil || len(contacts) != 1 || contacts[0].ID != "original" || contacts[0].FirstName != "Before" {
				t.Fatalf("contacts=%+v err=%v; original must be unchanged", contacts, err)
			}
			if got := activityCount(t, s); got != 1 {
				t.Fatalf("audit records=%d, want 1", got)
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
			errorsCh <- s.CreateContact(t.Context(), "local-demo", &Contact{ID: fmt.Sprintf("contact-%d", i)})
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
	count, err := s.CountContacts(t.Context())
	if err != nil || count != writers || activityCount(t, s) != writers {
		t.Fatalf("contacts=%d, err=%v; want %d contacts and audits", count, err, writers)
	}
}

func TestCanceledWriterWaitingForConnection(t *testing.T) {
	s := testStore(t)
	conn, err := s.DB().Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := conn.Close(); err != nil {
			t.Error(err)
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
	t.Run("size", func(t *testing.T) {
		s := testStore(t)
		err := s.CreateFile(t.Context(), "local-demo", &File{ID: "upload"}, zeroReader{})
		if !errors.Is(err, ErrFileTooLarge) {
			t.Fatalf("got %v, want size limit", err)
		}
		assertNoFiles(t, s)
	})
}

type failingReader struct{ err error }

func (r failingReader) Read([]byte) (int, error) { return 0, r.err }

type cancelReader struct{ cancel context.CancelFunc }

func (r cancelReader) Read(p []byte) (int, error) {
	r.cancel()
	return copy(p, "partial"), nil
}

type zeroReader struct{}

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
	files, err := s.ListFiles(t.Context())
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
		if _, err := s.GetFile(t.Context(), id); !errors.Is(err, ErrInvalidFileID) {
			t.Errorf("get ID %q: got %v", id, err)
		}
	}
	assertNoFiles(t, s)
}

func TestConcurrentDuplicateFiles(t *testing.T) {
	s := testStore(t)
	results := make(chan error, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			<-start
			results <- s.CreateFile(t.Context(), "local-demo", &File{ID: "same-file"}, strings.NewReader("original"))
		})
	}
	close(start)
	wg.Wait()
	close(results)
	accepted, duplicates := 0, 0
	for err := range results {
		switch {
		case err == nil:
			accepted++
		case errors.Is(err, ErrFileExists):
			duplicates++
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	if accepted != 1 || duplicates != 1 || activityCount(t, s) != 1 {
		t.Fatalf("accepted=%d duplicates=%d; want one complete upload", accepted, duplicates)
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
	if err := s.CreateContact(t.Context(), "seed", original); err != nil {
		t.Fatal(err)
	}
	updated := &Contact{ID: original.ID, FirstName: "After"}
	if err := s.UpdateContact(t.Context(), "local-demo", updated); err != nil {
		t.Fatal(err)
	}
	if updated.CreatedAt.IsZero() || !updated.CreatedAt.Equal(original.CreatedAt) {
		t.Fatalf("update returned created_at=%s, want %s", updated.CreatedAt, original.CreatedAt)
	}
	stored, err := s.GetContact(t.Context(), original.ID)
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
			if err := s.CreateContact(t.Context(), "seed", original); err != nil {
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
			stored, err := s.GetContact(t.Context(), original.ID)
			if err != nil || stored.FirstName != original.FirstName || !stored.CreatedAt.Equal(original.CreatedAt) || !stored.UpdatedAt.Equal(original.UpdatedAt) {
				t.Fatalf("failed update changed original: %+v err=%v", stored, err)
			}
			if activityCount(t, s) != 1 {
				t.Fatal("failed update changed audit history")
			}
		})
	}
}
