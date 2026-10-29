package store

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schema string

var ErrLegacySchema = errors.New("unsupported database schema; keep a backup and use a new data directory, or explicitly migrate ownership and timestamps offline")

type Store struct {
	db      *sql.DB
	dataDir string
}

func New(dataDir string) (*Store, error) {
	dbPath, err := filepath.Abs(filepath.Join(dataDir, "app.db"))
	if err != nil {
		return nil, fmt.Errorf("database path: %w", err)
	}
	// Inspect existing data read-only before creating directories, enabling WAL,
	// or applying schema. Never silently assign shared records to a caller.
	if err := checkExistingSchema(dbPath); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}

	filesDir := filepath.Join(dataDir, "files")
	if err := os.MkdirAll(filesDir, 0700); err != nil {
		return nil, fmt.Errorf("create files dir: %w", err)
	}

	// Restrict newly created databases without changing existing permissions.
	f, err := os.OpenFile(dbPath, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return nil, fmt.Errorf("create database: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("close new database: %w", err)
	}
	// SQLite has one writer. Serialize local operations; the busy timeout handles
	// other processes, and WAL allows their readers to coexist with our writes.
	// DSN pragmas also apply when database/sql replaces a connection.
	query := url.Values{"_pragma": {"busy_timeout(5000)", "journal_mode(WAL)", "foreign_keys(1)"}}
	dsn := (&url.URL{Scheme: "file", Path: filepath.ToSlash(dbPath), RawQuery: query.Encode()}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if _, err := db.Exec(schema); err != nil {
		return nil, errors.Join(fmt.Errorf("init schema: %w", err), db.Close())
	}

	return &Store{db: db, dataDir: dataDir}, nil
}

func checkExistingSchema(path string) (err error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect database: %w", err)
	}
	if info.Size() == 0 {
		return nil
	}
	query := url.Values{"mode": {"ro"}}
	dsn := (&url.URL{Scheme: "file", Path: filepath.ToSlash(path), RawQuery: query.Encode()}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return fmt.Errorf("inspect schema: %w", err)
	}
	defer closeWithError(db, &err)
	var tables, version int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'`).Scan(&tables); err != nil {
		return fmt.Errorf("inspect schema: %w", err)
	}
	if tables == 0 {
		return nil
	}
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("inspect schema version: %w", err)
	}
	if version != 2 {
		return ErrLegacySchema
	}
	var required int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name IN ('actors','contacts','files','activity_log')`).Scan(&required); err != nil {
		return fmt.Errorf("inspect required tables: %w", err)
	}
	if required != 4 {
		return ErrLegacySchema
	}
	for _, ownership := range []struct{ table, column string }{
		{"contacts", "owner_id"}, {"files", "owner_id"}, {"activity_log", "user_id"},
	} {
		var columns, foreignKeys int
		if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name=? AND "notnull"=1`, ownership.table, ownership.column).Scan(&columns); err != nil {
			return fmt.Errorf("inspect ownership column: %w", err)
		}
		if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_foreign_key_list(?) WHERE "table"='actors' AND "from"=? AND "to"='id'`, ownership.table, ownership.column).Scan(&foreignKeys); err != nil {
			return fmt.Errorf("inspect ownership constraint: %w", err)
		}
		if columns != 1 || foreignKeys != 1 {
			return ErrLegacySchema
		}
	}
	var orphaned int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_foreign_key_check`).Scan(&orphaned); err != nil {
		return fmt.Errorf("inspect ownership integrity: %w", err)
	}
	if orphaned != 0 {
		return ErrLegacySchema
	}
	return nil
}

// RegisterActors persists identity labels only. Credentials remain in memory in
// the server; registration does not transfer ownership or grant API access.
func (s *Store) RegisterActors(ctx context.Context, actors []string) error {
	for _, actor := range actors {
		if actor == "" || len(actor) > 254 || strings.TrimSpace(actor) != actor || !utf8.ValidString(actor) || strings.ContainsFunc(actor, unicode.IsControl) {
			return errors.New("actor must be a nonempty label of at most 254 bytes without surrounding whitespace or control characters")
		}
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		for _, actor := range actors {
			if _, err := tx.ExecContext(ctx, `INSERT INTO actors(id) VALUES(?) ON CONFLICT(id) DO NOTHING`, actor); err != nil {
				return fmt.Errorf("register actor: %w", err)
			}
		}
		return nil
	})
}

// withTx keeps mutations and their audit record on the same connection. The
// callback must use tx rather than s.db, because the pool has one connection.
func (s *Store) withTx(ctx context.Context, change func(*sql.Tx) error) (err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			err = errors.Join(err, fmt.Errorf("rollback transaction: %w", rollbackErr))
		}
	}()
	if err := change(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

func closeWithError(closer io.Closer, err *error) {
	if closeErr := closer.Close(); closeErr != nil {
		*err = errors.Join(*err, closeErr)
	}
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) DB() *sql.DB {
	return s.db
}

func (s *Store) FilesDir() string {
	return filepath.Join(s.dataDir, "files")
}
