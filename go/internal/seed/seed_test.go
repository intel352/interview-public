package seed

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/weave-lab/interview-public/go/internal/auth"
	"github.com/weave-lab/interview-public/go/internal/store"
)

func scenarioStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close seed store: %v", err)
		}
	})
	return s
}

func rowCount(t *testing.T, s *store.Store, query string, args ...any) int {
	t.Helper()
	var count int
	if err := s.DB().QueryRowContext(t.Context(), query, args...).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func seedSnapshot(t *testing.T, s *store.Store) []int {
	t.Helper()
	return []int{
		rowCount(t, s, "SELECT COUNT(*) FROM actors"),
		rowCount(t, s, "SELECT COUNT(*) FROM contacts"),
		rowCount(t, s, "SELECT COUNT(*) FROM files"),
		rowCount(t, s, "SELECT COUNT(*) FROM activity_log"),
		rowCount(t, s, "SELECT COUNT(*) FROM seed_state"),
	}
}

func TestBDDSeedDoesNotInferCompletionFromContactCount(t *testing.T) {
	// Given a populated database without a completed seed marker.
	s := scenarioStore(t)
	if err := s.RegisterActors(t.Context(), []string{"existing-owner"}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateContact(t.Context(), "existing-owner", &store.Contact{ID: "existing", FirstName: "Keep"}); err != nil {
		t.Fatal(err)
	}

	// When the configured actors receive their synthetic seed data.
	if err := Run(t.Context(), s, Options{Contacts: 4, Actors: []string{"alpha", "beta"}}); err != nil {
		t.Fatal(err)
	}

	// Then both actors own their own data and the unrelated contact is preserved.
	for _, actor := range []string{"alpha", "beta"} {
		if got := rowCount(t, s, "SELECT COUNT(*) FROM contacts WHERE owner_id = ?", actor); got != 2 {
			t.Errorf("actor %q owns %d contacts, want 2", actor, got)
		}
		if got := rowCount(t, s, "SELECT COUNT(*) FROM activity_log WHERE user_id = ?", actor); got == 0 {
			t.Errorf("actor %q has no seeded activity", actor)
		}
	}
	contact, err := s.GetContact(t.Context(), "existing-owner", "existing")
	if err != nil || contact.FirstName != "Keep" {
		t.Fatalf("unrelated contact changed: %+v, %v", contact, err)
	}
	if got := rowCount(t, s, "SELECT COUNT(*) FROM seed_state WHERE id = 1"); got != 1 {
		t.Fatalf("completion markers = %d, want 1", got)
	}
}

func TestBDDZeroContactSeedCompletesAndRepeatsWithoutMutation(t *testing.T) {
	// Given a new database and a deliberately empty contact/file dataset.
	s := scenarioStore(t)
	opts := Options{Contacts: 0, Files: 0, Actors: []string{"alpha", "beta"}}
	if err := Run(t.Context(), s, opts); err != nil {
		t.Fatal(err)
	}
	before := seedSnapshot(t, s)
	if want := []int{2, 0, 0, 5000, 1}; !reflect.DeepEqual(before, want) {
		t.Fatalf("initial rows = %v, want %v", before, want)
	}
	var completedAt string
	if err := s.DB().QueryRowContext(t.Context(), "SELECT CAST(completed_at AS TEXT) FROM seed_state WHERE id=1").Scan(&completedAt); err != nil {
		t.Fatal(err)
	}

	// When seeding is requested again with different counts and actor labels.
	if err := Run(t.Context(), s, Options{Contacts: 5, Files: 1, Actors: []string{"new-actor"}}); err != nil {
		t.Fatal(err)
	}

	// Then the completion marker, data and registered actors are unchanged.
	if after := seedSnapshot(t, s); !reflect.DeepEqual(after, before) {
		t.Fatalf("repeat seed changed rows: before=%v after=%v", before, after)
	}
	var repeatedAt string
	if err := s.DB().QueryRowContext(t.Context(), "SELECT CAST(completed_at AS TEXT) FROM seed_state WHERE id=1").Scan(&repeatedAt); err != nil {
		t.Fatal(err)
	}
	if repeatedAt != completedAt {
		t.Fatal("repeat seed changed completion time")
	}
}

func TestBDDSeedDefaultsAndFileOwnership(t *testing.T) {
	// Given the default actor configuration and a small seed with one file each.
	s := scenarioStore(t)
	opts := DefaultOptions()
	opts.Contacts, opts.Files = 2, 2
	actors, err := auth.ParseActors(auth.DefaultActors)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(opts.Actors, actors) {
		t.Fatalf("default seed actors = %v, want %v", opts.Actors, actors)
	}
	// An omitted actor slice follows the same defaults as DefaultOptions.
	opts.Actors = nil
	if err := Run(t.Context(), s, opts); err != nil {
		t.Fatal(err)
	}

	// Then actor-scoped reads expose only the assigned file and contact.
	for i, actor := range actors {
		files, err := s.ListFiles(t.Context(), actor)
		if err != nil || len(files) != 1 || files[0].OwnerID != actor {
			t.Fatalf("actor %q files = %+v, %v", actor, files, err)
		}
		contacts, err := s.ListContacts(t.Context(), actor, 10, nil)
		if err != nil || len(contacts) != 1 || contacts[0].OwnerID != actor {
			t.Fatalf("actor %q contacts = %+v, %v", actor, contacts, err)
		}
		other := actors[(i+1)%len(actors)]
		if _, err := s.GetFile(t.Context(), other, files[0].ID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("actor %q read %q's file: %v", other, actor, err)
		}
	}
}

type interruptedContent struct {
	read   bool
	err    error
	cancel context.CancelFunc
}

func (r *interruptedContent) Read(p []byte) (int, error) {
	if r.read {
		return 0, r.err
	}
	r.read = true
	if r.cancel != nil {
		r.cancel()
	}
	return copy(p, "partial file content"), nil
}

func TestBDDInterruptedSeedRollsBackAndCanRetry(t *testing.T) {
	for _, failure := range []string{"file read", "cancellation"} {
		t.Run(failure, func(t *testing.T) {
			// Given unrelated existing data and an interrupted seed file reader.
			s := scenarioStore(t)
			if err := s.RegisterActors(t.Context(), []string{"existing-owner"}); err != nil {
				t.Fatal(err)
			}
			if err := s.CreateContact(t.Context(), "existing-owner", &store.Contact{ID: "existing", FirstName: "Keep"}); err != nil {
				t.Fatal(err)
			}
			preserved := filepath.Join(s.FilesDir(), "unrelated-file")
			if err := os.WriteFile(preserved, []byte("preserve me"), 0600); err != nil {
				t.Fatal(err)
			}
			before := seedSnapshot(t, s)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			wantErr := errors.New("synthetic content failure")
			source := &interruptedContent{err: wantErr}
			if failure == "cancellation" {
				source.cancel = cancel
				wantErr = context.Canceled
			}
			opts := Options{Contacts: 4, Files: 1, Actors: []string{"alpha", "beta"}}

			// When contacts were inserted but file generation fails before commit.
			if err := runWithContent(ctx, s, opts, source); !errors.Is(err, wantErr) {
				t.Fatalf("seed error = %v, want %v", err, wantErr)
			}

			// Then no seed rows, actors, marker or partial file survive.
			if after := seedSnapshot(t, s); !reflect.DeepEqual(after, before) {
				t.Fatalf("failed seed changed rows: before=%v after=%v", before, after)
			}
			entries, err := os.ReadDir(s.FilesDir())
			if err != nil || len(entries) != 1 || entries[0].Name() != "unrelated-file" {
				t.Fatalf("failed seed left files: %v, %v", entries, err)
			}

			// When the same request is retried, it completes without duplicates.
			if err := Run(t.Context(), s, opts); err != nil {
				t.Fatal(err)
			}
			if got := rowCount(t, s, "SELECT COUNT(*) FROM contacts"); got != 5 {
				t.Fatalf("contacts after retry = %d, want 5", got)
			}
			if got := rowCount(t, s, "SELECT COUNT(*) FROM files WHERE owner_id='alpha'"); got != 1 {
				t.Fatalf("alpha files after retry = %d, want 1", got)
			}
			if got := rowCount(t, s, "SELECT COUNT(*) FROM seed_state"); got != 1 {
				t.Fatalf("markers after retry = %d, want 1", got)
			}
			data, err := os.ReadFile(preserved)
			if err != nil || string(data) != "preserve me" {
				t.Fatalf("unrelated file changed: %q, %v", data, err)
			}
		})
	}
}

func TestBDDSeedCompletionFailureRollsBackAllWork(t *testing.T) {
	// Given a database that refuses the final completion marker.
	s := scenarioStore(t)
	if _, err := s.DB().ExecContext(t.Context(), `CREATE TRIGGER reject_completion BEFORE INSERT ON seed_state BEGIN SELECT RAISE(ABORT, 'synthetic completion failure'); END`); err != nil {
		t.Fatal(err)
	}
	opts := Options{Contacts: 2, Files: 1, Actors: []string{"alpha", "beta"}}

	// When every earlier seed step finishes but marking completion fails.
	if err := Run(t.Context(), s, opts); err == nil || !strings.Contains(err.Error(), "synthetic completion failure") {
		t.Fatalf("seed completion error = %v", err)
	}

	// Then all rows and owned files roll back, and a later retry succeeds.
	if got := seedSnapshot(t, s); !reflect.DeepEqual(got, []int{0, 0, 0, 0, 0}) {
		t.Fatalf("failed completion left rows: %v", got)
	}
	entries, err := os.ReadDir(s.FilesDir())
	if err != nil || len(entries) != 0 {
		t.Fatalf("failed completion left files: %v, %v", entries, err)
	}
	if _, err := s.DB().ExecContext(t.Context(), "DROP TRIGGER reject_completion"); err != nil {
		t.Fatal(err)
	}
	if err := Run(t.Context(), s, opts); err != nil {
		t.Fatal(err)
	}
}

func TestBDDConcurrentSeedRequestsCompleteOnce(t *testing.T) {
	// Given two callers trying to initialize the same store.
	s := scenarioStore(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			<-start
			results <- Run(ctx, s, Options{Contacts: 2, Actors: []string{"alpha", "beta"}})
		})
	}

	// When both calls run concurrently through the single SQLite connection.
	close(start)
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Error(err)
		}
	}

	// Then only one dataset and completion marker are committed.
	if got := seedSnapshot(t, s); !reflect.DeepEqual(got, []int{2, 2, 0, 5002, 1}) {
		t.Fatalf("concurrent seed rows = %v, want one dataset", got)
	}
}

func TestBDDInvalidSeedOptionsDoNotMutate(t *testing.T) {
	for _, opts := range []Options{
		{Contacts: -1},
		{Files: -1},
		{Actors: []string{"alpha", "alpha"}},
		{Actors: []string{"alpha,beta"}},
		{Actors: []string{"bad actor"}},
	} {
		s := scenarioStore(t)
		if err := Run(t.Context(), s, opts); err == nil {
			t.Fatalf("invalid options accepted: %+v", opts)
		}
		if got := seedSnapshot(t, s); !reflect.DeepEqual(got, []int{0, 0, 0, 0, 0}) {
			t.Fatalf("invalid options changed rows: %v", got)
		}
	}
}
