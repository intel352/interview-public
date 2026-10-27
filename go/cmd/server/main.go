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
	"strings"
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
	healthAddr := flags.String("health-addr", "", "health loopback address (default API port + 1; API port 0 uses a separate ephemeral port)")
	actorList := flags.String("actors", auth.DefaultActors, "comma-separated local actor identifiers")
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
	address, healthAddress, err := listenerAddresses(*addr, *healthAddr)
	if err != nil {
		return err
	}
	actors, err := auth.ParseActors(*actorList)
	if err != nil {
		return fmt.Errorf("actors: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s, err := store.New(*dataDir)
	if err != nil {
		return fmt.Errorf("initialize store: %w", err)
	}
	defer func() { result = errors.Join(result, s.Close()) }()

	if *doSeed {
		if err := seed.Run(ctx, s, seed.Options{Contacts: *contacts, Files: *files, Actors: actors}); err != nil {
			return fmt.Errorf("seed database: %w", err)
		}
		_, err := fmt.Fprintln(output, "Done seeding.")
		return err
	}
	// Seeding registers actors inside its own atomic transaction. Ordinary
	// startup registers only the identities configured for this server run.
	if err := s.RegisterActors(ctx, actors); err != nil {
		return fmt.Errorf("register local actors: %w", err)
	}

	listener, healthListener, err := listenPair(address, healthAddress)
	if err != nil {
		return err
	}
	defer func() {
		for _, bound := range []net.Listener{listener, healthListener} {
			if err := bound.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
				result = errors.Join(result, err)
			}
		}
	}()
	if err := ctx.Err(); err != nil {
		return err
	}
	credentials, err := auth.NewCredentials(actors)
	if err != nil {
		return err
	}
	// Startup-only credentials are shown in configured actor order after both
	// binds. Never include them in access logs or persist them in the data dir.
	byActor := make(map[string]string, len(credentials))
	for token, actor := range credentials {
		byActor[actor] = token
	}
	lines := []string{
		fmt.Sprintf("Local demo API listening on http://%s", listener.Addr()),
		fmt.Sprintf("Local demo health listening on http://%s", healthListener.Addr()),
	}
	for _, actor := range actors {
		lines = append(lines, fmt.Sprintf("Local demo bearer token for %s (expires on restart): %s", actor, byActor[actor]))
	}
	if _, err := fmt.Fprintln(output, strings.Join(lines, "\n")); err != nil {
		return fmt.Errorf("write startup information: %w", err)
	}
	handler := middleware.Logger(app.RateLimit(app.NewRouter(s, app.Options{AuthTokens: credentials})))
	return serveBoth(ctx, listener, healthListener, handler, app.NewHealthRouter(), 5*time.Second)
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

func listenerAddresses(apiAddress, healthAddress string) (string, string, error) {
	api, err := loopbackAddress(apiAddress)
	if err != nil {
		return "", "", fmt.Errorf("API address: %w", err)
	}
	if healthAddress == "" {
		host, portText, err := net.SplitHostPort(api)
		if err != nil {
			return "", "", err
		}
		port, err := strconv.Atoi(portText)
		if err != nil {
			return "", "", err
		}
		if port == 65535 {
			return "", "", errors.New("API port 65535 has no next port; set --health-addr explicitly")
		}
		// Port zero requests independent OS allocation for both listeners. Using
		// port one or a guessed neighbor of an ephemeral port would be unsafe.
		if port != 0 {
			port++
		}
		healthAddress = net.JoinHostPort(host, strconv.Itoa(port))
	}
	health, err := loopbackAddress(healthAddress)
	if err != nil {
		return "", "", fmt.Errorf("health address: %w", err)
	}
	if api == health && !strings.HasSuffix(api, ":0") {
		return "", "", errors.New("API and health must use separate listen addresses")
	}
	return api, health, nil
}

func listenPair(apiAddress, healthAddress string) (net.Listener, net.Listener, error) {
	api, err := net.Listen("tcp", apiAddress)
	if err != nil {
		return nil, nil, fmt.Errorf("listen API: %w", err)
	}
	health, err := net.Listen("tcp", healthAddress)
	if err != nil {
		return nil, nil, errors.Join(fmt.Errorf("listen health: %w", err), api.Close())
	}
	return api, health, nil
}

func serveBoth(ctx context.Context, apiListener, healthListener net.Listener, apiHandler, healthHandler http.Handler, grace time.Duration) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan error, 2)
	for _, endpoint := range []struct {
		name     string
		listener net.Listener
		handler  http.Handler
	}{
		{"API", apiListener, apiHandler},
		{"health", healthListener, healthHandler},
	} {
		go func() {
			err := serve(ctx, endpoint.listener, endpoint.handler, grace)
			if err != nil {
				err = fmt.Errorf("%s server: %w", endpoint.name, err)
			}
			results <- err
		}()
	}
	first := <-results
	// Any server exit stops the peer. Join both bounded server loops before
	// releasing shared resources; active handlers must honor cancellation.
	cancel()
	second := <-results
	return errors.Join(first, second)
}
