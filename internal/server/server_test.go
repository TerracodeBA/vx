package server

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// Stopping the server logs why it stopped and that it shut down, instead of
// exiting silently.
func TestListenAndServeLogsShutdownReason(t *testing.T) {
	var logs syncBuffer
	s := &Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }),
		log:     slog.New(slog.NewTextHandler(&logs, nil)),
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()

	ctx, cancel := context.WithCancelCause(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.ListenAndServe(ctx, addr) }()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(logs.String(), "vx server listening") {
		if time.Now().After(deadline) {
			t.Fatalf("server did not start: %s", logs.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	// signal.NotifyContext cancels with this cause on SIGTERM.
	cancel(errors.New("terminated signal received"))
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ListenAndServe = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not stop")
	}
	out := logs.String()
	for _, want := range []string{`msg="vx server shutting down" reason="terminated signal received"`, `msg="vx server stopped"`} {
		if !strings.Contains(out, want) {
			t.Errorf("logs missing %s:\n%s", want, out)
		}
	}
}
