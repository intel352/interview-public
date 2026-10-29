package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestLoopbackAddress(t *testing.T) {
	for _, address := range []string{"127.0.0.1:0", "127.2.3.4:8080", "[::1]:8080"} {
		if _, err := loopbackAddress(address); err != nil {
			t.Errorf("reject loopback %q: %v", address, err)
		}
	}
	for _, address := range []string{":8080", "0.0.0.0:8080", "[::]:8080", "192.0.2.1:8080", "localhost:8080", "example.com:8080", "127.0.0.1:-1", "127.0.0.1:65536", "127.0.0.1:http"} {
		if _, err := loopbackAddress(address); err == nil {
			t.Errorf("accepted invalid/nonloopback address %q", address)
		}
	}
}

func TestRunRejectsNonLoopbackBeforeOpeningResources(t *testing.T) {
	data := filepath.Join(t.TempDir(), "must-not-create")
	var output bytes.Buffer
	err := run(context.Background(), []string{"--addr=0.0.0.0:8080", "--data=" + data}, &output)
	if err == nil {
		t.Fatal("nonloopback bind accepted")
	}
	if _, err := os.Stat(data); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("created data before validating address: %v", err)
	}
	if output.Len() != 0 {
		t.Fatal("startup output appeared before successful validation/bind")
	}
}

type observedListener struct {
	net.Listener
	closed chan struct{}
	once   sync.Once
}

func (l *observedListener) Close() error {
	err := l.Listener.Close()
	l.once.Do(func() { close(l.closed) })
	return err
}

func testListener(t *testing.T) *observedListener {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	wrapped := &observedListener{Listener: l, closed: make(chan struct{})}
	t.Cleanup(func() {
		if err := wrapped.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Error(err)
		}
	})
	return wrapped
}

func await[T any](t *testing.T, channel <-chan T) T {
	t.Helper()
	select {
	case value := <-channel:
		return value
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for lifecycle event")
		var zero T
		return zero
	}
}

func requestHealth(address string) error {
	client := &http.Client{Timeout: 3 * time.Second}
	defer client.CloseIdleConnections()
	response, err := client.Get("http://" + address + "/health")
	if err != nil {
		return err
	}
	_, readErr := io.Copy(io.Discard, response.Body)
	return errors.Join(readErr, response.Body.Close())
}

func TestServeGracefulShutdown(t *testing.T) {
	l := testListener(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	entered := make(chan context.Context, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseHandler := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseHandler)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- r.Context()
		<-release
		w.WriteHeader(http.StatusNoContent)
	})
	served := make(chan error, 1)
	go func() { served <- serve(ctx, l, handler, time.Second) }()
	clientDone := make(chan error, 1)
	go func() { clientDone <- requestHealth(l.Addr().String()) }()
	requestCtx := await(t, entered)
	deadline, ok := requestCtx.Deadline()
	if !ok || time.Until(deadline) > requestTimeout {
		t.Fatal("request has no bounded deadline")
	}
	cancel()
	await(t, l.closed) // Shutdown has stopped accepting before we inspect context.
	if err := requestCtx.Err(); err != nil {
		t.Fatalf("graceful shutdown canceled active request: %v", err)
	}
	releaseHandler()
	if err := await(t, clientDone); err != nil {
		t.Fatal(err)
	}
	if err := await(t, served); err != nil {
		t.Fatal(err)
	}
}

func TestServeForcedShutdownCancelsRequests(t *testing.T) {
	l := testListener(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	entered := make(chan struct{})
	finished := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
		close(finished)
	})
	served := make(chan error, 1)
	go func() { served <- serve(ctx, l, handler, 20*time.Millisecond) }()
	clientDone := make(chan error, 1)
	go func() { clientDone <- requestHealth(l.Addr().String()) }()
	await(t, entered)
	cancel()
	if err := await(t, served); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown error = %v, want deadline exceeded", err)
	}
	await(t, finished)
	// The transport may see EOF or a response during forced close; either way it
	// must finish instead of leaving an unbounded client/server goroutine.
	if err := await(t, clientDone); err != nil {
		var networkError net.Error
		if errors.As(err, &networkError) && networkError.Timeout() {
			t.Fatalf("client timed out instead of observing forced close: %v", err)
		}
		t.Logf("client observed forced shutdown: %v", err)
	}
}

var errAcceptFailed = errors.New("synthetic listener failure")

type failingListener struct {
	net.Listener
	fail  <-chan struct{}
	first bool
}

func (l *failingListener) Accept() (net.Conn, error) {
	if l.first {
		l.first = false
		return l.Listener.Accept()
	}
	<-l.fail
	return nil, errAcceptFailed
}

func TestServeClosesAcceptedConnectionsOnListenerFailure(t *testing.T) {
	l := testListener(t)
	fail := make(chan struct{})
	wrapped := &failingListener{Listener: l, fail: fail, first: true}
	served := make(chan error, 1)
	go func() { served <- serve(context.Background(), wrapped, http.NotFoundHandler(), time.Second) }()
	connection, err := net.DialTimeout("tcp", l.Addr().String(), time.Second)
	if err != nil {
		close(fail)
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := connection.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := connection.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		close(fail)
		t.Fatal(err)
	}
	if _, err := io.WriteString(connection, "GET /health HTTP/1.1\r\nHost: localhost\r\n"); err != nil {
		close(fail)
		t.Fatal(err)
	}
	close(fail)
	if err := await(t, served); !errors.Is(err, errAcceptFailed) {
		t.Fatalf("serve error = %v, want listener failure", err)
	}
	var response [1]byte
	// Closing a connection with unread input can produce EOF or TCP reset.
	if _, err := connection.Read(response[:]); !errors.Is(err, io.EOF) && !errors.Is(err, syscall.ECONNRESET) {
		t.Fatalf("accepted connection survived listener failure: %v", err)
	}
}

func TestServeReturnsListenerError(t *testing.T) {
	l := testListener(t)
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if err := serve(context.Background(), l, http.NotFoundHandler(), time.Second); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("serve closed listener: %v", err)
	}
}

func TestServeClosesSlowHeaders(t *testing.T) {
	l := testListener(t)
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- serve(ctx, l, http.NotFoundHandler(), time.Second) }()
	t.Cleanup(func() {
		cancel()
		if err := await(t, served); err != nil {
			t.Error(err)
		}
	})
	connection, err := net.DialTimeout("tcp", l.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := connection.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := connection.SetDeadline(time.Now().Add(8 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(connection, "GET /health HTTP/1.1\r\nHost: localhost\r\n"); err != nil {
		t.Fatal(err)
	}
	var response [1]byte
	if _, err := connection.Read(response[:]); !errors.Is(err, io.EOF) {
		t.Fatalf("slow header connection was not closed by server: %v", err)
	}
}
