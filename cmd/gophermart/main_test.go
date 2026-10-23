package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
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

func TestRunDatabaseTLS(t *testing.T) {
	cases := []struct {
		name, query, env, service                     string
		keyword, unsetEnv, wantInvalid, wantPlaintext bool
	}{
		{name: "default", unsetEnv: true},
		{name: "empty environment"},
		{name: "URI overrides environment", query: "sslmode=require", env: "disable"},
		{name: "empty mode", query: "sslmode=", wantInvalid: true},
		{name: "empty overrides environment", query: "sslmode=", env: "require", wantInvalid: true},
		{name: "duplicate modes", query: "sslmode=prefer&sslmode=require", wantInvalid: true},
		{name: "duplicate encoded key", query: "sslmode=require&%73slmode=disable", wantInvalid: true},
		{name: "keyword DSN", keyword: true, wantInvalid: true},
		{name: "URI service", query: "service=synthetic", wantInvalid: true},
		{name: "URI servicefile", query: "servicefile=absent", wantInvalid: true},
		{name: "environment service", service: "synthetic", wantInvalid: true},
		{name: "SSL alias", query: "ssl=true", wantInvalid: true},
	}
	for _, mode := range []string{"disable", "allow", "prefer", "require", "verify-ca", "verify-full"} {
		plain := mode == "disable" || mode == "allow" || mode == "prefer"
		cases = append(cases, struct {
			name, query, env, service                     string
			keyword, unsetEnv, wantInvalid, wantPlaintext bool
		}{name: "URI " + mode, query: "sslmode=" + mode, wantPlaintext: plain})
		cases = append(cases, struct {
			name, query, env, service                     string
			keyword, unsetEnv, wantInvalid, wantPlaintext bool
		}{name: "environment " + mode, env: mode, wantPlaintext: plain})
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			for _, key := range []string{"PGSSLMODE", "PGSERVICE", "PGSSLCERT", "PGSSLKEY", "PGSSLROOTCERT", "PGSSLNEGOTIATION", "PGOPTIONS"} {
				t.Setenv(key, "")
			}
			t.Setenv("PGSSLMODE", tt.env)
			t.Setenv("PGSERVICE", tt.service)
			if tt.unsetEnv {
				if err := os.Unsetenv("PGSSLMODE"); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("PGPASSFILE", filepath.Join(home, "absent-pass"))
			t.Setenv("PGSERVICEFILE", filepath.Join(home, "absent-service"))
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			seen := make(chan bool, 8)
			done := make(chan struct{})
			go func() {
				defer close(done)
				for {
					conn, err := listener.Accept()
					if err != nil {
						return
					}
					if err := conn.SetDeadline(time.Now().Add(time.Second)); err != nil {
						t.Error(err)
					}
					var packet [8]byte
					if _, err := io.ReadFull(conn, packet[:]); err == nil {
						ssl := binary.BigEndian.Uint32(packet[:4]) == 8 && binary.BigEndian.Uint32(packet[4:]) == 80877103
						seen <- !ssl
						if ssl {
							if _, err := conn.Write([]byte{'N'}); err != nil {
								t.Error(err)
							}
						}
					}
					if err := conn.Close(); err != nil {
						t.Error(err)
					}
				}
			}()
			defer func() { _ = listener.Close(); <-done }()
			dsn := "postgres://diploma_test@" + listener.Addr().String() + "/test"
			if tt.keyword {
				host, port, err := net.SplitHostPort(listener.Addr().String())
				if err != nil {
					t.Fatal(err)
				}
				dsn = "host=" + host + " port=" + port + " user=diploma_test dbname=test"
			} else if tt.query != "" {
				dsn += "?" + tt.query
			}
			t.Setenv("DATABASE_URI", dsn)
			t.Setenv("RUN_ADDRESS", "127.0.0.1:0")
			t.Setenv("ACCRUAL_SYSTEM_ADDRESS", "http://127.0.0.1:1")
			t.Setenv("AUTH_SECRET", strings.Repeat("s", 32))
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			err = run(ctx, nil, zerolog.Nop())
			if err == nil {
				t.Fatal("test server cannot authenticate a connection")
			}
			if tt.wantInvalid && !strings.Contains(err.Error(), "database TLS configuration:") {
				t.Fatalf("expected configuration error, got %v", err)
			}
			if err := listener.Close(); err != nil {
				t.Fatal(err)
			}
			<-done
			close(seen)
			attempts := 0
			plaintext := false
			for plain := range seen {
				attempts++
				plaintext = plaintext || plain
			}
			if tt.wantInvalid {
				if attempts != 0 {
					t.Fatalf("invalid configuration reached network: %d attempts", attempts)
				}
				return
			}
			if attempts == 0 || plaintext != tt.wantPlaintext {
				t.Fatalf("attempts=%d plaintext=%v, want plaintext=%v", attempts, plaintext, tt.wantPlaintext)
			}
		})
	}
}
