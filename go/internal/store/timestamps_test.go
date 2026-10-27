package store

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDefaultTimestampPaginationDoesNotRepeatAnchor(t *testing.T) {
	s := testStore(t)
	// Given two rows using SQLite's own timestamp defaults in one statement.
	if _, err := s.DB().ExecContext(t.Context(), `
		INSERT INTO contacts(id,owner_id,first_name,last_name,email,phone,company)
		VALUES('a','local-demo','','','','',''),('b','local-demo','','','','','')`); err != nil {
		t.Fatal(err)
	}
	var cursor *PageToken
	for _, expected := range []string{"b", "a", ""} {
		// When those rows are paged through a timestamp decoded back into Go.
		page, err := s.ListContacts(t.Context(), "local-demo", 1, cursor)
		if err != nil {
			t.Fatal(err)
		}
		// Then each row appears once and the final page is empty.
		if expected == "" {
			if len(page) != 0 {
				t.Fatalf("terminal page repeats a row: %+v", page)
			}
			continue
		}
		if len(page) != 1 || page[0].ID != expected {
			t.Fatalf("page=%+v, want only %s", page, expected)
		}
		cursor = &PageToken{OwnerID: "local-demo", ID: page[0].ID, CreatedAt: page[0].CreatedAt}
	}
}

func TestCanonicalTimestampPaginationPreservesNanoseconds(t *testing.T) {
	s := testStore(t)
	base := time.Date(2026, 1, 2, 3, 4, 5, 999999998, time.UTC)
	for _, row := range []struct {
		id string
		at time.Time
	}{
		{"a", base}, {"b", base.In(time.FixedZone("offset", 5*60*60+30*60))}, {"c", base.Add(time.Nanosecond)},
	} {
		if _, err := s.DB().ExecContext(t.Context(), `
			INSERT INTO contacts(id,owner_id,first_name,last_name,email,phone,company,created_at,updated_at)
			VALUES(?,'local-demo','','','','','',?,?)`, row.id, DatabaseTime(row.at), DatabaseTime(row.at)); err != nil {
			t.Fatal(err)
		}
	}
	var cursor *PageToken
	for _, id := range []string{"c", "b", "a"} {
		page, err := s.ListContacts(t.Context(), "local-demo", 1, cursor)
		if err != nil || len(page) != 1 || page[0].ID != id {
			t.Fatalf("nanosecond page=%+v error=%v; want %s", page, err, id)
		}
		want := base
		if id == "c" {
			want = base.Add(time.Nanosecond)
		}
		if !page[0].CreatedAt.Equal(want) {
			t.Fatalf("timestamp lost precision: got %s, want %s", page[0].CreatedAt, want)
		}
		cursor = &PageToken{OwnerID: "local-demo", ID: page[0].ID, CreatedAt: page[0].CreatedAt}
	}
	// An old SQLite/Go timestamp encoding cannot reintroduce mixed ordering.
	for _, invalid := range []any{base, "2026-01-02 03:04:05", "2026-01-02T03:04:05Z", "2026-01-02T03:04:05.99999999XZ"} {
		if _, err := s.DB().ExecContext(t.Context(), `UPDATE contacts SET created_at=? WHERE id='a'`, invalid); err == nil {
			t.Fatalf("schema accepted noncanonical timestamp %v", invalid)
		}
	}
}

func TestDefaultActivityTimestampIncludesExactReportBoundary(t *testing.T) {
	s := testStore(t)
	if _, err := s.DB().ExecContext(t.Context(), `INSERT INTO activity_log(user_id,action,resource_type) VALUES('local-demo','view','report')`); err != nil {
		t.Fatal(err)
	}
	var created time.Time
	if err := s.DB().QueryRowContext(t.Context(), `SELECT created_at FROM activity_log`).Scan(&created); err != nil {
		t.Fatal(err)
	}
	report, err := s.GenerateActivityReport(t.Context(), "local-demo", created)
	if err != nil || len(report) != 1 || report[0].TotalActions != 1 {
		t.Fatalf("inclusive default timestamp cutoff=%+v error=%v", report, err)
	}
	report, err = s.GenerateActivityReport(t.Context(), "local-demo", created.Add(time.Nanosecond))
	if err != nil || len(report) != 0 {
		t.Fatalf("one-nanosecond-later cutoff=%+v error=%v", report, err)
	}
}

func TestPriorOwnedTimestampSchemaFailsClosed(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, err := s.DB().ExecContext(t.Context(), `PRAGMA user_version=1`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "app.db")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if opened, err := New(dir); !errors.Is(err, ErrLegacySchema) {
		if opened != nil {
			if closeErr := opened.Close(); closeErr != nil {
				t.Error(closeErr)
			}
		}
		t.Fatalf("old timestamp schema error=%v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(before) != string(after) {
		t.Fatalf("old timestamp schema was modified: %v", err)
	}
}
