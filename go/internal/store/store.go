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

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schema string

type Store struct {
	db      *sql.DB
	dataDir string
}

func New(dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}

	filesDir := filepath.Join(dataDir, "files")
	if err := os.MkdirAll(filesDir, 0700); err != nil {
		return nil, fmt.Errorf("create files dir: %w", err)
	}

	dbPath, err := filepath.Abs(filepath.Join(dataDir, "app.db"))
	if err != nil {
		return nil, fmt.Errorf("database path: %w", err)
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
	query := url.Values{"_pragma": {"busy_timeout(5000)", "journal_mode(WAL)"}}
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
