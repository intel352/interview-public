package store

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestActorOwnershipAcrossContactAndFileOperations(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	if err := s.RegisterActors(ctx, []string{"alice", "bob"}); err != nil {
		t.Fatal(err)
	}
	alice := &Contact{ID: "alice-contact", OwnerID: "bob", FirstName: "Alice"}
	if err := s.CreateContact(ctx, "alice", alice); err != nil || alice.OwnerID != "alice" {
		t.Fatalf("server-derived contact owner=%q error=%v", alice.OwnerID, err)
	}
	if _, err := s.ImportContacts(ctx, "bob", []Contact{{ID: "bob-contact", OwnerID: "alice", FirstName: "Bob"}}); err != nil {
		t.Fatal(err)
	}
	for _, actor := range []string{"alice", "bob"} {
		contacts, err := s.ListContacts(ctx, actor, 100, nil)
		if err != nil || len(contacts) != 1 || contacts[0].OwnerID != actor {
			t.Fatalf("%s list=%+v error=%v", actor, contacts, err)
		}
		exported, err := s.ExportContacts(ctx, actor)
		if err != nil || len(exported) != 1 || exported[0].OwnerID != actor {
			t.Fatalf("%s export=%+v error=%v", actor, exported, err)
		}
		count, err := s.CountContacts(ctx, actor)
		if err != nil || count != 1 {
			t.Fatalf("%s count=%d error=%v", actor, count, err)
		}
	}
	if _, err := s.GetContact(ctx, "bob", alice.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-actor get=%v", err)
	}
	if err := s.UpdateContact(ctx, "bob", &Contact{ID: alice.ID, FirstName: "Stolen"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-actor update=%v", err)
	}
	if err := s.DeleteContact(ctx, "bob", alice.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-actor delete=%v", err)
	}
	alice.OwnerID = "bob"
	if err := s.UpdateContact(ctx, "alice", alice); err != nil || alice.OwnerID != "alice" {
		t.Fatalf("update attempted ownership transfer: owner=%q error=%v", alice.OwnerID, err)
	}
	if count, err := s.CountAllContacts(ctx); err != nil || count != 2 {
		t.Fatalf("admin total=%d error=%v", count, err)
	}
	file := &File{ID: "alice-file", OwnerID: "bob", Filename: "private.txt"}
	if err := s.CreateFile(ctx, "alice", file, strings.NewReader("private")); err != nil || file.OwnerID != "alice" {
		t.Fatalf("server-derived file owner=%q error=%v", file.OwnerID, err)
	}
	if _, err := s.GetFile(ctx, "bob", file.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-actor metadata=%v", err)
	}
	// Removing the content proves unauthorized lookup fails before filesystem I/O.
	if err := os.Remove(filepath.Join(s.FilesDir(), file.ID)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.OpenFile(ctx, "bob", file.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-actor download=%v", err)
	}
	files, err := s.ListFiles(ctx, "bob")
	if err != nil || files == nil || len(files) != 0 {
		t.Fatalf("empty foreign list=%+v error=%v", files, err)
	}
	for _, actor := range []string{"alice", "bob"} {
		report, err := s.GenerateActivityReport(ctx, actor, time.Time{})
		if err != nil || len(report) != 1 || report[0].UserID != actor {
			t.Fatalf("%s report=%+v error=%v", actor, report, err)
		}
	}
	var aliceOwner, bobOwner, fileOwner string
	if err := s.DB().QueryRowContext(ctx, `SELECT owner_id FROM contacts WHERE id=?`, alice.ID).Scan(&aliceOwner); err != nil {
		t.Fatal(err)
	}
	if err := s.DB().QueryRowContext(ctx, `SELECT owner_id FROM contacts WHERE id='bob-contact'`).Scan(&bobOwner); err != nil {
		t.Fatal(err)
	}
	if err := s.DB().QueryRowContext(ctx, `SELECT owner_id FROM files WHERE id=?`, file.ID).Scan(&fileOwner); err != nil {
		t.Fatal(err)
	}
	if aliceOwner != "alice" || bobOwner != "bob" || fileOwner != "alice" {
		t.Fatalf("persisted owners=%q,%q,%q", aliceOwner, bobOwner, fileOwner)
	}
}

func TestScopedDatabaseReopensWithoutChangingOwners(t *testing.T) {
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
	if err := s.RegisterActors(t.Context(), []string{"alice", "bob"}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateContact(t.Context(), "alice", &Contact{ID: "persisted"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := reopened.Close(); err != nil {
			t.Error(err)
		}
	})
	contact, err := reopened.GetContact(t.Context(), "alice", "persisted")
	if err != nil || contact.OwnerID != "alice" {
		t.Fatalf("reopened contact=%+v error=%v", contact, err)
	}
	if _, err := reopened.GetContact(t.Context(), "bob", "persisted"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("reopened foreign contact=%v", err)
	}
}

func TestActorForeignKeysAndRegistration(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	for _, actors := range [][]string{{""}, {" leading"}, {"trailing "}, {"bad\nactor"}, {strings.Repeat("a", 255)}} {
		if err := s.RegisterActors(ctx, actors); err == nil {
			t.Fatalf("invalid actors accepted: %q", actors)
		}
	}
	if err := s.RegisterActors(ctx, []string{"alice", "alice"}); err != nil {
		t.Fatal(err)
	}
	var foreignKeys int
	if err := s.DB().QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&foreignKeys); err != nil || foreignKeys != 1 {
		t.Fatalf("foreign_keys=%d error=%v", foreignKeys, err)
	}
	// A replacement physical connection must receive the same foreign-key policy.
	s.DB().SetMaxIdleConns(0)
	if err := s.DB().QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&foreignKeys); err != nil || foreignKeys != 1 {
		t.Fatalf("replacement foreign_keys=%d error=%v", foreignKeys, err)
	}
	s.DB().SetMaxIdleConns(1)
	if err := s.CreateContact(ctx, "unregistered", &Contact{ID: "unknown-owner"}); err == nil {
		t.Fatal("unknown actor contact accepted")
	}
	if err := s.CreateFile(ctx, "unregistered", &File{ID: "unknown-owner"}, strings.NewReader("content")); err == nil {
		t.Fatal("unknown actor file accepted")
	}
	if err := s.LogActivity(ctx, "unregistered", "create", "contact", "none"); err == nil {
		t.Fatal("unknown audit actor accepted")
	}
	for _, query := range []string{
		`INSERT INTO contacts(id,owner_id,first_name,last_name,email,phone,company) VALUES('x',NULL,'','','','','')`,
		`INSERT INTO contacts(id,owner_id,first_name,last_name,email,phone,company) VALUES('x','unknown','','','','','')`,
		`INSERT INTO files(id,owner_id,filename,size,content_type) VALUES('x','unknown','',0,'')`,
		`INSERT INTO activity_log(user_id,action,resource_type) VALUES('unknown','create','contact')`,
	} {
		if _, err := s.DB().ExecContext(ctx, query); err == nil {
			t.Fatalf("SQLite accepted invalid ownership: %s", query)
		}
	}
	assertNoFiles(t, s)
}

func TestActorCursorScopeSurvivesDeletedAnchor(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	if err := s.RegisterActors(ctx, []string{"alice", "bob"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ImportContacts(ctx, "alice", []Contact{{ID: "a"}, {ID: "b"}, {ID: "c"}}); err != nil {
		t.Fatal(err)
	}
	first, err := s.ListContacts(ctx, "alice", 1, nil)
	if err != nil || len(first) != 1 || first[0].ID != "c" {
		t.Fatalf("first page=%+v error=%v", first, err)
	}
	cursor := &PageToken{OwnerID: "alice", ID: first[0].ID, CreatedAt: first[0].CreatedAt}
	if err := s.DeleteContact(ctx, "alice", first[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateContact(ctx, "alice", &Contact{ID: "newer"}); err != nil {
		t.Fatal(err)
	}
	next, err := s.ListContacts(ctx, "alice", 100, cursor)
	if err != nil || len(next) != 2 || next[0].ID != "b" || next[1].ID != "a" {
		t.Fatalf("deleted-anchor continuation=%+v error=%v", next, err)
	}
	if _, err := s.ListContacts(ctx, "bob", 100, cursor); !errors.Is(err, ErrInvalidPageToken) {
		t.Fatalf("foreign cursor error=%v", err)
	}
}

func TestLegacySharedDatabaseIsRejectedWithoutSchemaChanges(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE contacts(id TEXT PRIMARY KEY, first_name TEXT); INSERT INTO contacts VALUES('legacy','Unassigned')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if s, err := New(dir); !errors.Is(err, ErrLegacySchema) {
		if s != nil {
			if closeErr := s.Close(); closeErr != nil {
				t.Error(closeErr)
			}
		}
		t.Fatalf("legacy open error=%v, want explicit migration refusal", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(before) {
		t.Fatalf("legacy database changed: error=%v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "files")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy rejection created files directory: %v", err)
	}
}
