package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5/middleware"

	"github.com/weave-lab/interview-public/go/internal/app"
	"github.com/weave-lab/interview-public/go/internal/seed"
	"github.com/weave-lab/interview-public/go/internal/store"
)

func main() {
	var (
		addr         = flag.String("addr", ":8080", "server address")
		dataDir      = flag.String("data", "data", "data directory")
		doSeed       = flag.Bool("seed", false, "seed database with test data")
		seedContacts = flag.Int("contacts", 10000, "number of contacts to seed")
		seedFiles    = flag.Int("files", 20, "number of files to seed")
	)
	flag.Parse()

	s, err := store.New(*dataDir)
	if err != nil {
		log.Fatalf("Failed to initialize store: %v", err)
	}
	defer s.Close()

	if *doSeed {
		count, err := s.CountContacts(context.Background())
		if err != nil {
			log.Fatalf("Failed to check database: %v", err)
		}
		if count > 0 {
			fmt.Println("Database already seeded. Use 'make reset' to reset.")
			os.Exit(0)
		}

		fmt.Println("Seeding database...")
		opts := seed.Options{
			Contacts: *seedContacts,
			Files:    *seedFiles,
		}
		if err := seed.Run(context.Background(), s, opts); err != nil {
			log.Fatalf("Failed to seed database: %v", err)
		}
		fmt.Println("Done seeding.")
		os.Exit(0)
	}

	count, err := s.CountContacts(context.Background())
	if err != nil || count == 0 {
		fmt.Println("Database is empty. Run with --seed first:")
		fmt.Println("  go run ./cmd/server --seed")
		os.Exit(1)
	}

	r := middleware.Logger(app.RateLimit(app.NewRouter(s, app.Options{})))

	srv := &http.Server{
		Addr:    *addr,
		Handler: r,
	}

	go func() {
		fmt.Printf("Server listening on %s\n", *addr)
		err := srv.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("Server error: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	fmt.Println("\nShutting down...")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
}
