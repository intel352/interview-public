package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

const MaxFileSize int64 = 100 << 20

var (
	ErrInvalidFileID = errors.New("invalid storage file ID")
	ErrFileExists    = errors.New("storage file already exists")
	ErrFileTooLarge  = errors.New("file exceeds size limit")
	fileIDPattern    = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
)

type File struct {
	ID          string    `json:"id"`
	OwnerID     string    `json:"owner_id"`
	Filename    string    `json:"filename"`
	Size        int64     `json:"size"`
	ContentType string    `json:"content_type"`
	CreatedAt   time.Time `json:"created_at"`
}

// CreateFile publishes complete content before committing metadata and audit.
// Ordinary failures remove this call's file. Filesystem changes and SQLite are
// not one crash-atomic transaction: a crash may still leave orphaned files or
// metadata whose directory entry/content needs recovery from backup.
func (s *Store) CreateFile(ctx context.Context, actor string, f *File, content io.Reader) (err error) {
	if !fileIDPattern.MatchString(f.ID) {
		return ErrInvalidFileID
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	f.CreatedAt = time.Now().UTC()

	path := filepath.Join(s.FilesDir(), f.ID)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return ErrFileExists
		}
		return fmt.Errorf("create file: %w", err)
	}
	// Only remove files created by this call; never remove a duplicate's data.
	defer func() {
		if err != nil {
			if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				err = errors.Join(err, fmt.Errorf("remove failed upload: %w", removeErr))
			}
		}
	}()

	n, copyErr := io.Copy(file, io.LimitReader(contextReader{ctx: ctx, reader: content}, MaxFileSize+1))
	if copyErr == nil && n > MaxFileSize {
		copyErr = ErrFileTooLarge
	}
	if copyErr == nil {
		copyErr = ctx.Err()
	}
	if copyErr == nil {
		copyErr = file.Sync()
	}
	if err := errors.Join(copyErr, file.Close()); err != nil {
		return fmt.Errorf("write file: %w", err)
	}
	f.Size = n

	err = s.withTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
		INSERT INTO files (id, owner_id, filename, size, content_type, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, f.ID, actor, f.Filename, f.Size, f.ContentType, DatabaseTime(f.CreatedAt))
		if err != nil {
			return fmt.Errorf("insert file record: %w", err)
		}
		return logActivity(ctx, tx, actor, "upload", "file", f.ID)
	})
	if err == nil {
		f.OwnerID = actor
	}
	return err
}

// Cancellation is checked between reads; an arbitrary reader's already-blocked
// Read cannot be interrupted safely without cooperation from that reader.
type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func (s *Store) GetFile(ctx context.Context, actor, id string) (File, error) {
	if !fileIDPattern.MatchString(id) {
		return File{}, ErrInvalidFileID
	}
	var f File
	err := s.db.QueryRowContext(ctx, `
		SELECT id, owner_id, filename, size, content_type, created_at
		FROM files WHERE owner_id = ? AND id = ?
	`, actor, id).Scan(&f.ID, &f.OwnerID, &f.Filename, &f.Size, &f.ContentType, &f.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return File{}, ErrNotFound
	}
	if err != nil {
		return File{}, fmt.Errorf("query file: %w", err)
	}
	return f, nil
}

func (s *Store) OpenFile(ctx context.Context, actor, id string) (*os.File, File, error) {
	f, err := s.GetFile(ctx, actor, id)
	if err != nil {
		return nil, File{}, err
	}

	path := filepath.Join(s.FilesDir(), id)
	file, err := os.Open(path)
	if err != nil {
		return nil, File{}, fmt.Errorf("open file: %w", err)
	}
	return file, f, nil
}

func (s *Store) ListFiles(ctx context.Context, actor string) (files []File, err error) {
	files = make([]File, 0)
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, owner_id, filename, size, content_type, created_at
		FROM files
		WHERE owner_id = ?
		ORDER BY created_at DESC, id DESC
	`, actor)
	if err != nil {
		return nil, fmt.Errorf("query files: %w", err)
	}
	defer closeWithError(rows, &err)

	for rows.Next() {
		var f File
		if err := rows.Scan(&f.ID, &f.OwnerID, &f.Filename, &f.Size, &f.ContentType, &f.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan file: %w", err)
		}
		files = append(files, f)
	}
	return files, rows.Err()
}
