package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5/middleware"

	"github.com/weave-lab/interview-public/go/internal/app"
	"github.com/weave-lab/interview-public/go/internal/auth"
	"github.com/weave-lab/interview-public/go/internal/seed"
	"github.com/weave-lab/interview-public/go/internal/store"
)

const requestTimeout = 30 * time.Second

func main() {
	os.Exit(mainExit())
}

func mainExit() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout); err != nil && !errors.Is(err, flag.ErrHelp) {
		log.Printf("Server: %v", err)
		return 1
	}
	return 0
}

func run(ctx context.Context, args []string, output io.Writer) (result error) {
	flags := flag.NewFlagSet("server", flag.ContinueOnError)
	flags.SetOutput(output)
	addr := flags.String("addr", "127.0.0.1:8080", "numeric loopback listen address")
	dataDir := flags.String("data", "data", "data directory")
	doSeed := flags.Bool("seed", false, "seed database with test data")
	contacts := flags.Int("contacts", 10000, "number of contacts to seed")
	files := flags.Int("files", 20, "number of files to seed")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *contacts < 0 || *files < 0 {
		return errors.New("unexpected arguments or negative seed counts")
	}
	address, err := loopbackAddress(*addr)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s, err := store.New(*dataDir)
	if err != nil {
		return fmt.Errorf("initialize store: %w", err)
	}
	defer func() { result = errors.Join(result, s.Close()) }()

	count, err := s.CountContacts(ctx)
	if err != nil {
		return fmt.Errorf("check database: %w", err)
	}
	if *doSeed {
		if count > 0 {
			_, err := fmt.Fprintln(output, "Database already seeded. Use 'make reset' to reset.")
			return err
		}
		if err := seed.Run(ctx, s, seed.Options{Contacts: *contacts, Files: *files}); err != nil {
			return fmt.Errorf("seed database: %w", err)
		}
		_, err := fmt.Fprintln(output, "Done seeding.")
		return err
	}
	if count == 0 {
		return errors.New("database is empty; run with --seed first")
	}

	listener, err := net.Listen("tcp", address)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	defer func() {
		if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			result = errors.Join(result, err)
		}
	}()
	token, err := auth.NewToken()
	if err != nil {
		return err
	}
	// This startup-only credential is shown to the local operator after binding.
	// Never include it in access logs or persist it in the data directory.
	if _, err := fmt.Fprintf(output, "Local demo listening on http://%s\nLocal demo bearer token (expires on restart): %s\n", listener.Addr(), token); err != nil {
		return fmt.Errorf("write startup information: %w", err)
	}
	handler := middleware.Logger(app.RateLimit(app.NewRouter(s, app.Options{AuthToken: token})))
	return serve(ctx, listener, handler, 5*time.Second)
}

func loopbackAddress(address string) (string, error) {
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return "", fmt.Errorf("address must be a numeric loopback IP and port: %w", err)
	}
	ip := net.ParseIP(host)
	port, portErr := strconv.Atoi(portText)
	if ip == nil || !ip.IsLoopback() || portErr != nil || port < 0 || port > 65535 {
		return "", errors.New("only numeric loopback addresses (127.0.0.1 or ::1) and ports 0–65535 are allowed")
	}
	return net.JoinHostPort(ip.String(), strconv.Itoa(port)), nil
}

func serve(ctx context.Context, listener net.Listener, handler http.Handler, grace time.Duration) (result error) {
	requests, cancelRequests := context.WithCancel(context.Background())
	defer cancelRequests()
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requestCtx, cancel := context.WithTimeout(r.Context(), requestTimeout)
			defer cancel()
			handler.ServeHTTP(w, r.WithContext(requestCtx))
		}),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       requestTimeout,
		WriteTimeout:      requestTimeout,
		IdleTimeout:       time.Minute,
		MaxHeaderBytes:    16 << 10,
		BaseContext:       func(net.Listener) context.Context { return requests },
	}
	// Serve can fail after accepting connections; close them on every exit path.
	defer func() { result = errors.Join(result, srv.Close()) }()
	served := make(chan error, 1)
	go func() { served <- srv.Serve(listener) }()
	select {
	case err := <-served:
		return fmt.Errorf("serve HTTP: %w", err)
	case <-ctx.Done():
		// First allow active handlers to finish; only cancel them if grace expires.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), grace)
		defer cancel()
		shutdownErr := srv.Shutdown(shutdownCtx)
		if shutdownErr != nil {
			cancelRequests()
			shutdownErr = errors.Join(shutdownErr, srv.Close())
		}
		serveErr := <-served
		if errors.Is(serveErr, http.ErrServerClosed) {
			serveErr = nil
		}
		return errors.Join(shutdownErr, serveErr)
	}
}
