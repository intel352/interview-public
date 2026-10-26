package seed

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"log"
	mrand "math/rand/v2"
	"os"

	"github.com/brianvoe/gofakeit/v7"

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
}

func DefaultOptions() Options {
	return Options{
		Contacts: 10000,
		Files:    20,
	}
}

func Run(ctx context.Context, s *store.Store, opts Options) error {
	src := mrand.NewPCG(42, 42)
	rng := mrand.New(src)
	fake := gofakeit.NewFaker(src, false)

	if err := seedContacts(ctx, s, fake, opts.Contacts); err != nil {
		return fmt.Errorf("seed contacts: %w", err)
	}

	if err := seedFiles(ctx, s, rng, opts.Files); err != nil {
		return fmt.Errorf("seed files: %w", err)
	}

	if err := seedActivity(ctx, s, fake, rng); err != nil {
		return fmt.Errorf("seed activity: %w", err)
	}

	return nil
}

func seedContacts(ctx context.Context, s *store.Store, fake *gofakeit.Faker, count int) error {
	const batchSize = 1000

	for i := 0; i < count; i += batchSize {
		batch := make([]store.Contact, 0, batchSize)
		for j := 0; j < batchSize && i+j < count; j++ {
			batch = append(batch, store.Contact{
				ID:        fake.UUID(),
				FirstName: fake.FirstName(),
				LastName:  fake.LastName(),
				Email:     fake.Email(),
				Phone:     fake.Phone(),
				Company:   fake.Company(),
			})
		}
		if _, err := s.ImportContacts(ctx, batch); err != nil {
			return err
		}
		logger.Printf("Seeded %d/%d contacts", min(i+batchSize, count), count)
	}
	return nil
}

func seedFiles(ctx context.Context, s *store.Store, rng *mrand.Rand, count int) error {
	sizes := []int64{
		1024,             // 1KB
		10 * 1024,        // 10KB
		100 * 1024,       // 100KB
		1024 * 1024,      // 1MB
		10 * 1024 * 1024, // 10MB
		50 * 1024 * 1024, // 50MB
	}

	for i := range count {
		size := sizes[rng.IntN(len(sizes))]
		f := &store.File{
			ID:          fmt.Sprintf("file-%04d", i+1),
			Filename:    fmt.Sprintf("testfile-%04d.bin", i+1),
			ContentType: "application/octet-stream",
		}

		content := io.LimitReader(rand.Reader, size)
		if err := s.CreateFile(ctx, f, content); err != nil {
			return err
		}
		logger.Printf("Seeded file %d/%d (%d bytes)", i+1, count, size)
	}
	return nil
}

func seedActivity(ctx context.Context, s *store.Store, fake *gofakeit.Faker, rng *mrand.Rand) error {
	users := []string{
		"alice@example.com",
		"bob@example.com",
		"carol@example.com",
		"dave@example.com",
		"eve@example.com",
	}
	actions := []string{"create", "update", "delete", "view", "export", "import"}
	resources := []string{"contact", "file", "report"}

	for range 5000 {
		user := users[rng.IntN(len(users))]
		action := actions[rng.IntN(len(actions))]
		resource := resources[rng.IntN(len(resources))]
		resourceID := fake.UUID()

		if err := s.LogActivity(ctx, user, action, resource, resourceID); err != nil {
			return err
		}
	}
	logger.Println("Seeded 5000 activity log entries")
	return nil
}
