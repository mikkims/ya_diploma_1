package main

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

func TestStartupSafeDiagnostics(t *testing.T) {
	t.Setenv("AUTH_SECRET", strings.Repeat("s", 32))
	t.Setenv("DATABASE_URI", "postgres://user:secret@host/db")
	t.Setenv("ACCRUAL_SYSTEM_ADDRESS", "http://user:secret@host?token=secret")
	var output bytes.Buffer
	if err := run(context.Background(), nil, zerolog.New(&output)); err == nil {
		t.Fatal("invalid URL accepted")
	}
	if !strings.Contains(output.String(), `"operation":"accrual.new"`) || strings.Contains(output.String(), "secret") || strings.Contains(output.String(), "postgres://") {
		t.Fatalf("unsafe or missing startup diagnostics: %s", output.String())
	}
}

func TestServeCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	stopped := make(chan struct{})
	done := make(chan error, 1)
	server := &http.Server{Addr: "127.0.0.1:0", Handler: http.NotFoundHandler()}
	go func() {
		done <- serve(ctx, server, func(ctx context.Context) { close(started); <-ctx.Done(); close(stopped) })
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("worker not started")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown timeout")
	}
	select {
	case <-stopped:
	default:
		t.Fatal("worker leaked")
	}
}

func TestListenFailure(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := l.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err := serve(context.Background(), &http.Server{Addr: l.Addr().String()}, func(context.Context) { t.Error("worker started after listen error") }); err == nil {
		t.Fatal("listen error hidden")
	}
}

func TestRunInvalidConfiguration(t *testing.T) {
	t.Setenv("DATABASE_URI", "")
	t.Setenv("AUTH_SECRET", "")
	if err := run(context.Background(), nil, zerolog.Nop()); err == nil {
		t.Fatal("missing DB accepted")
	}
	t.Setenv("DATABASE_URI", "postgres://localhost/test")
	t.Setenv("AUTH_SECRET", "short")
	if err := run(context.Background(), nil, zerolog.Nop()); err == nil {
		t.Fatal("short secret accepted")
	}
}
