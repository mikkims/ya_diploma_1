package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const restartCreateDatabaseQuery = `
CREATE DATABASE %s
`
const restartDropDatabaseQuery = `
DROP DATABASE %s
`
const restartStateQuery = `
SELECT count(*)
FROM orders
WHERE (number = '00' AND status = $1)
   OR (number = '18' AND status = $2)
`
const restartBalanceQuery = `
SELECT balance
FROM users
WHERE login = 'restart-user'
`

func TestApplicationRestart(t *testing.T) {
	dsn := os.Getenv("GOPHERMART_TEST_DATABASE_URI")
	if dsn == "" {
		t.Skip("GOPHERMART_TEST_DATABASE_URI не задан: перезапуск с PostgreSQL не проверен")
	}
	u, err := url.Parse(dsn)
	if err != nil || u.Host == "" || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		t.Fatal("Нужен URI отдельного тестового PostgreSQL")
	}
	query := u.Query()
	query.Set("connect_timeout", "5")
	u.RawQuery = query.Encode()
	admin, err := sql.Open("postgres", u.String())
	if err != nil {
		t.Fatal("Ошибка подключения к тестовому PostgreSQL")
	}
	t.Cleanup(func() {
		if err := admin.Close(); err != nil {
			t.Error("admin close")
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	name := fmt.Sprintf("gophermart_restart_%d", time.Now().UnixNano())
	if _, err := admin.ExecContext(ctx, fmt.Sprintf(restartCreateDatabaseQuery, name)); err != nil {
		t.Fatal("Не удалось создать отдельную тестовую БД")
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := admin.ExecContext(ctx, fmt.Sprintf(restartDropDatabaseQuery, name)); err != nil {
			t.Error("Не удалось удалить созданную тестовую БД")
		}
	})
	u.Path = "/" + name
	db, err := sql.Open("postgres", u.String())
	if err != nil {
		t.Fatal("Не удалось открыть временную БД")
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error("test DB close")
		}
	})
	var complete atomic.Bool
	var calls atomic.Int64
	accrualServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		number := strings.TrimPrefix(r.URL.Path, "/api/orders/")
		if number != "00" && number != "18" {
			t.Error("unexpected order")
			w.WriteHeader(400)
			return
		}
		if !complete.Load() && number == "18" {
			w.WriteHeader(204)
			return
		}
		status, amount := "PROCESSING", 0
		if complete.Load() {
			status = "PROCESSED"
			amount = 1
			if number == "18" {
				amount = 2
			}
		}
		if err := json.NewEncoder(w).Encode(map[string]any{"order": number, "status": status, "accrual": amount}); err != nil {
			t.Error(err)
		}
	}))
	defer accrualServer.Close()
	binary := filepath.Join(t.TempDir(), "gophermart")
	build := exec.CommandContext(ctx, "go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build gophermart: %v\n%s", err, output)
	}
	client := &http.Client{Timeout: time.Second}
	defer client.CloseIdleConnections()
	start := func() (string, func()) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		address := listener.Addr().String()
		if err := listener.Close(); err != nil {
			t.Fatal(err)
		}
		cmd := exec.CommandContext(ctx, binary)
		for _, value := range os.Environ() {
			key, _, _ := strings.Cut(value, "=")
			switch key {
			case "RUN_ADDRESS", "DATABASE_URI", "ACCRUAL_SYSTEM_ADDRESS", "AUTH_SECRET":
				continue
			}
			cmd.Env = append(cmd.Env, value)
		}
		cmd.Env = append(cmd.Env, "RUN_ADDRESS="+address, "DATABASE_URI="+u.String(), "ACCRUAL_SYSTEM_ADDRESS="+accrualServer.URL, "AUTH_SECRET="+strings.Repeat("s", 32))
		var output bytes.Buffer
		cmd.Stdout, cmd.Stderr = &output, &output
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		stopped := false
		stop := func() {
			if stopped {
				return
			}
			stopped = true
			if err := cmd.Process.Signal(os.Interrupt); err != nil {
				t.Error("signal gophermart", err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Error("gophermart shutdown", err)
				}
			case <-time.After(10 * time.Second):
				if err := cmd.Process.Kill(); err != nil {
					t.Error(err)
				}
				<-done
				t.Error("gophermart shutdown timeout")
			}
		}
		t.Cleanup(stop)
		base := "http://" + address
		poll(t, ctx, func() bool {
			select {
			case <-done:
				stopped = true
				t.Fatal("gophermart exited before readiness")
			default:
			}
			response, err := client.Get(base + "/api/user/balance")
			if err != nil {
				return false
			}
			if err := response.Body.Close(); err != nil {
				t.Error(err)
			}
			return response.StatusCode == 401
		})
		return base, stop
	}
	request := func(base, path, token, content, body string, want int) string {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", content)
		if token != "" {
			req.Header.Set("Authorization", token)
		}
		response, err := client.Do(req)
		if err != nil {
			t.Fatal("HTTP request failed")
		}
		if _, err := io.Copy(io.Discard, response.Body); err != nil {
			t.Error(err)
		}
		if err := response.Body.Close(); err != nil {
			t.Error(err)
		}
		if response.StatusCode != want {
			t.Fatalf("%s: status %d, want %d", path, response.StatusCode, want)
		}
		return response.Header.Get("Authorization")
	}
	base, stop := start()
	token := request(base, "/api/user/register", "", "application/json", `{"login":"restart-user","password":"synthetic-password"}`, 200)
	if token == "" {
		t.Fatal("missing token")
	}
	request(base, "/api/user/orders", token, "text/plain", "00", 202)
	request(base, "/api/user/orders", token, "text/plain", "18", 202)
	state := func(first, second string) bool {
		var count int
		return db.QueryRowContext(ctx, restartStateQuery, first, second).Scan(&count) == nil && count == 2
	}
	poll(t, ctx, func() bool { return state("PROCESSING", "NEW") })
	stop()
	if !state("PROCESSING", "NEW") {
		t.Fatal("pending state lost after shutdown")
	}
	complete.Store(true)
	base, stop = start()
	poll(t, ctx, func() bool { return state("PROCESSED", "PROCESSED") })
	request(base, "/api/user/orders", token, "text/plain", "00", 200)
	stop()
	assertBalance := func() {
		var balance int64
		if err := db.QueryRowContext(ctx, restartBalanceQuery).Scan(&balance); err != nil || balance != 300 {
			t.Fatalf("expected exactly 300 cents, got %d", balance)
		}
	}
	assertBalance()
	before := calls.Load()
	_, stop = start()
	stop()
	assertBalance()
	if calls.Load() != before {
		t.Fatal("terminal orders polled after restart")
	}
}

func poll(t *testing.T, ctx context.Context, condition func() bool) {
	t.Helper()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for !condition() {
		select {
		case <-ctx.Done():
			t.Fatal("condition deadline exceeded")
		case <-ticker.C:
		}
	}
}
