package seed

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	mrand "math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/brianvoe/gofakeit/v7"
	"github.com/google/uuid"

	"github.com/weave-lab/interview-public/go/internal/auth"
	"github.com/weave-lab/interview-public/go/internal/store"
)

var logger = log.New(os.Stderr, "", 0)

func SetQuiet(quiet bool) {
	if quiet {
		logger.SetOutput(io.Discard)
	} else {
		logger.SetOutput(os.Stderr)
	}
}

type Options struct {
	Contacts int
	Files    int
	Actors   []string
}

func DefaultOptions() Options {
	return Options{Contacts: 10000, Files: 20, Actors: strings.Split(auth.DefaultActors, ",")}
}

// Run commits a complete synthetic dataset once. An explicit marker distinguishes
// completed seeding from an empty or independently populated database.
// SQLite and filesystem writes are not crash-atomic: ordinary failures remove
// this attempt's files, but an abrupt process/power loss may leave orphan blobs.
func Run(ctx context.Context, s *store.Store, opts Options) error {
	return runWithContent(ctx, s, opts, rand.Reader)
}

// The content reader is a narrow I/O seam for deterministic failure tests; all
// SQL and filesystem operations remain real. A blocked reader must cooperate
// with cancellation; no background copy goroutine is started.
func runWithContent(ctx context.Context, s *store.Store, opts Options, content io.Reader) (result error) {
	if opts.Contacts < 0 || opts.Files < 0 {
		return errors.New("seed counts must not be negative")
	}
	actors := slices.Clone(opts.Actors)
	if len(actors) == 0 {
		var err error
		actors, err = auth.ParseActors(auth.DefaultActors)
		if err != nil {
			return fmt.Errorf("default seed actors: %w", err)
		}
	}
	if err := auth.ValidateActors(actors); err != nil {
		return fmt.Errorf("seed actors: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	tx, err := s.DB().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin seed: %w", err)
	}
	var ownedFiles []string
	committed := false
	defer func() {
		if committed {
			return
		}
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			result = errors.Join(result, fmt.Errorf("rollback seed: %w", err))
		}
		for _, path := range ownedFiles {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				result = errors.Join(result, fmt.Errorf("remove failed seed file: %w", err))
			}
		}
	}()

	var completed bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM seed_state WHERE id=1)").Scan(&completed); err != nil {
		return fmt.Errorf("read seed state: %w", err)
	}
	if completed {
		return nil
	}
	for _, actor := range actors {
		if _, err := tx.ExecContext(ctx, `INSERT INTO actors(id) VALUES(?) ON CONFLICT(id) DO NOTHING`, actor); err != nil {
			return fmt.Errorf("register seed actor: %w", err)
		}
	}

	src := mrand.NewPCG(42, 42)
	rng := mrand.New(src)
	fake := gofakeit.NewFaker(src, false)
	now := time.Now().UTC()
	if err := seedContacts(ctx, tx, fake, opts.Contacts, actors, now); err != nil {
		return fmt.Errorf("seed contacts: %w", err)
	}
	if err := seedFiles(ctx, tx, s.FilesDir(), rng, opts.Files, actors, now, content, &ownedFiles); err != nil {
		return fmt.Errorf("seed files: %w", err)
	}
	if err := seedActivity(ctx, tx, fake, rng, actors, now); err != nil {
		return fmt.Errorf("seed activity: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO seed_state(id, completed_at) VALUES(1, ?)`, store.DatabaseTime(now)); err != nil {
		return fmt.Errorf("mark seed complete: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit seed: %w", err)
	}
	committed = true
	logger.Printf("Committed seed: %d contacts, %d files and 5000 synthetic activity entries", opts.Contacts, opts.Files)
	return nil
}

func seedContacts(ctx context.Context, tx *sql.Tx, fake *gofakeit.Faker, count int, actors []string, now time.Time) (result error) {
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO contacts
		(id, owner_id, first_name, last_name, email, phone, company, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer closeWithError(stmt, &result)
	at := store.DatabaseTime(now)
	for i := range count {
		if _, err := stmt.ExecContext(ctx, uuid.NewString(), actors[i%len(actors)], fake.FirstName(), fake.LastName(), fake.Email(), fake.Phone(), fake.Company(), at, at); err != nil {
			return err
		}
	}
	for _, actor := range actors[:min(count, len(actors))] {
		if err := insertActivity(ctx, tx, actor, "import", "contacts", "", now); err != nil {
			return err
		}
	}
	return nil
}

func seedFiles(ctx context.Context, tx *sql.Tx, directory string, rng *mrand.Rand, count int, actors []string, now time.Time, content io.Reader, ownedFiles *[]string) error {
	sizes := []int64{1024, 10 << 10, 100 << 10, 1 << 20, 10 << 20, 50 << 20}
	at := store.DatabaseTime(now)
	for i := range count {
		id := uuid.NewString()
		size := sizes[rng.IntN(len(sizes))]
		path := filepath.Join(directory, id)
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return fmt.Errorf("create seed file: %w", err)
		}
		// Track only files created by this attempt; existing/orphan files remain.
		*ownedFiles = append(*ownedFiles, path)
		_, copyErr := io.CopyN(file, contextReader{ctx: ctx, reader: content}, size)
		if copyErr == nil {
			copyErr = ctx.Err()
		}
		if copyErr == nil {
			copyErr = file.Sync()
		}
		if err := errors.Join(copyErr, file.Close()); err != nil {
			return fmt.Errorf("write seed file: %w", err)
		}
		actor := actors[i%len(actors)]
		if _, err := tx.ExecContext(ctx, `INSERT INTO files(id, owner_id, filename, size, content_type, created_at)
			VALUES (?, ?, ?, ?, ?, ?)`, id, actor, fmt.Sprintf("testfile-%04d.bin", i+1), size, "application/octet-stream", at); err != nil {
			return err
		}
		if err := insertActivity(ctx, tx, actor, "upload", "file", id, now); err != nil {
			return err
		}
	}
	return nil
}

func seedActivity(ctx context.Context, tx *sql.Tx, fake *gofakeit.Faker, rng *mrand.Rand, actors []string, now time.Time) (result error) {
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO activity_log(user_id, action, resource_type, resource_id, created_at) VALUES(?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer closeWithError(stmt, &result)
	actions := []string{"create", "update", "delete", "view", "export", "import"}
	resources := []string{"contact", "file", "report"}
	at := store.DatabaseTime(now)
	for range 5000 {
		actor := actors[rng.IntN(len(actors))]
		action := actions[rng.IntN(len(actions))]
		resource := resources[rng.IntN(len(resources))]
		if _, err := stmt.ExecContext(ctx, actor, action, resource, fake.UUID(), at); err != nil {
			return err
		}
	}
	return nil
}

func insertActivity(ctx context.Context, tx *sql.Tx, actor, action, resource, id string, at time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO activity_log(user_id, action, resource_type, resource_id, created_at) VALUES(?, ?, ?, ?, ?)`, actor, action, resource, id, store.DatabaseTime(at))
	return err
}

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

func closeWithError(closer io.Closer, result *error) {
	if err := closer.Close(); err != nil {
		*result = errors.Join(*result, err)
	}
}
