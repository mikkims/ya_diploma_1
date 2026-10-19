package accrual

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lib/pq"
	"github.com/mikkims/ya_diploma_1/internal/model"
	"github.com/rs/zerolog"
)

type testTransport func(*http.Request) (*http.Response, error)

func (f testTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type closeFailureBody struct{ io.Reader }

func (b closeFailureBody) Close() error { return errors.New("secret close failure") }

func TestFetchCloseFailure(t *testing.T) {
	c, err := New("http://example.test")
	if err != nil {
		t.Fatal(err)
	}
	c.http.Transport = testTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: closeFailureBody{Reader: strings.NewReader(`{"order":"00","status":"PROCESSED","accrual":1}`)}}, nil
	})
	result, err := c.Fetch(context.Background(), "00")
	if err == nil || result.Status != "" {
		t.Fatal("close failure must leave order pending")
	}
}

func TestWorkerSafeDiagnostics(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c, err := New("http://example.test")
	if err != nil {
		t.Fatal(err)
	}
	c.http.Transport = testTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader("secret-response-body"))}, nil
	})
	store := workerStore{
		pending: func(context.Context) ([]string, error) {
			return []string{"00"}, &pq.Error{Code: "08006", Message: "postgres://user:secret@host/db"}
		},
		apply: func(context.Context, string, string, *model.Money) error {
			cancel()
			return fmt.Errorf("password=secret: %w", context.DeadlineExceeded)
		},
	}
	var output bytes.Buffer
	Run(ctx, store, c, zerolog.New(&output))
	for _, want := range []string{`"operation":"orders.pending"`, `"sqlstate":"08006"`, `"operation":"accrual.fetch"`, `"http_status":503`, `"operation":"accrual.apply"`, `"category":"timeout"`} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("missing diagnostics %s", want)
		}
	}
	for _, bad := range []string{"secret", "postgres://", "example.test", "response-body", "password"} {
		if strings.Contains(output.String(), bad) {
			t.Fatal("sensitive log")
		}
	}
}

func TestFetch(t *testing.T) {
	for _, tt := range []struct {
		name         string
		code         int
		body, status string
		amount       *model.Money
		wantErr      bool
	}{
		{"processed", 200, `{"order":"00","status":"PROCESSED","accrual":1.25}`, model.Processed, money(125), false},
		{"zero", 200, `{"order":"00","status":"PROCESSED","accrual":0}`, model.Processed, money(0), false},
		{"missing", 200, `{"order":"00","status":"PROCESSED"}`, model.Processed, nil, false},
		{"registered", 200, `{"order":"00","status":"REGISTERED"}`, model.Processing, nil, false},
		{"processing", 200, `{"order":"00","status":"PROCESSING"}`, model.Processing, nil, false},
		{"invalid", 200, `{"order":"00","status":"INVALID"}`, model.Invalid, nil, false},
		{"unregistered", 204, "", "", nil, false},
		{"failure", 500, "", "", nil, true},
		{"wrong-order", 200, `{"order":"18","status":"PROCESSED","accrual":10}`, "", nil, true},
		{"unknown", 200, `{"order":"00","status":"NEW"}`, "", nil, true},
		{"negative", 200, `{"order":"00","status":"PROCESSED","accrual":-1}`, "", nil, true},
		{"fraction", 200, `{"order":"00","status":"PROCESSED","accrual":0.001}`, "", nil, true},
		{"trailing", 200, `{"order":"00","status":"PROCESSED"} {}`, "", nil, true},
		{"large", 200, strings.Repeat("x", 65537), "", nil, true},
		{"redirect", 302, "", "", nil, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/orders/00" {
					t.Error("wrong path")
				}
				w.WriteHeader(tt.code)
				if _, err := fmt.Fprint(w, tt.body); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			c, err := New(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			got, err := c.Fetch(context.Background(), "00")
			if (err != nil) != tt.wantErr {
				t.Fatalf("Fetch error %v", err)
			}
			if err == nil {
				if got.Status != tt.status || (got.Accrual == nil) != (tt.amount == nil) {
					t.Fatalf("result %+v", got)
				}
				if tt.amount != nil && *got.Accrual != *tt.amount {
					t.Fatal("amount")
				}
			}
		})
	}
	if _, err := New("file:///x"); err == nil {
		t.Fatal("invalid URL")
	}
	c, _ := New("http://localhost")
	if _, err := c.Fetch(context.Background(), "no/path"); !errors.Is(err, model.ErrInvalidOrder) {
		t.Fatal(err)
	}
}

func money(m model.Money) *model.Money { return &m }

func TestNewRejectsInvalidURL(t *testing.T) {
	for _, bad := range []string{"", "file:///tmp/test", "http://a?key=secret", "http://u:p@host", "http://a/#fragment", "://"} {
		if _, err := New(bad); err == nil {
			t.Errorf("accepted invalid URL")
		}
	}
}

func TestRateLimitAndCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Header().Set("Retry-After", "3"); w.WriteHeader(429) }))
	defer server.Close()
	c, _ := New(server.URL)
	_, err := c.Fetch(context.Background(), "00")
	var limited *RateLimitError
	if !errors.As(err, &limited) || limited.RetryAfter != 3*time.Second || limited.Error() == "" {
		t.Fatal("retry after")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Fetch(ctx, "00"); err == nil {
		t.Fatal("cancel ignored")
	}
}

func TestRetryAfter(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	for _, tt := range []struct {
		value string
		want  time.Duration
	}{{"", time.Second}, {"invalid", time.Second}, {"-1", time.Second}, {"2", 2 * time.Second}, {now.Add(time.Minute).Format(http.TimeFormat), time.Minute}} {
		if got := retryAfter(tt.value, now); got != tt.want {
			t.Errorf("retry %q=%v", tt.value, got)
		}
	}
}

func TestWaitCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if wait(ctx, time.Hour) {
		t.Fatal("cancelled wait succeeded")
	}
	if !wait(context.Background(), time.Nanosecond) {
		t.Fatal("timer did not fire")
	}
}

type workerStore struct {
	pending func(context.Context) ([]string, error)
	apply   func(context.Context, string, string, *model.Money) error
}

func (s workerStore) Pending(ctx context.Context) ([]string, error) { return s.pending(ctx) }
func (s workerStore) Apply(ctx context.Context, n, status string, m *model.Money) error {
	return s.apply(ctx, n, status, m)
}

func TestWorkerResumesAndApplies(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := fmt.Fprint(w, `{"order":"00","status":"PROCESSED","accrual":5.5}`); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	c, _ := New(server.URL)
	called := false
	store := workerStore{pending: func(context.Context) ([]string, error) { return []string{"00"}, nil }, apply: func(_ context.Context, n, status string, m *model.Money) error {
		if n != "00" || status != model.Processed || m == nil || *m != 550 {
			t.Error("result")
		}
		called = true
		cancel()
		return nil
	}}
	Run(ctx, store, c, zerolog.Nop())
	if !called {
		t.Fatal("stored order not processed")
	}
}

func TestWorkerGlobalPause(t *testing.T) {
	var calls atomic.Int64
	first := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		first <- struct{}{}
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(429)
	}))
	defer server.Close()
	c, _ := New(server.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	store := workerStore{pending: func(context.Context) ([]string, error) { return []string{"00", "18"}, nil }, apply: func(context.Context, string, string, *model.Money) error { t.Error("429 applied"); return nil }}
	go func() { defer close(done); Run(ctx, store, c, zerolog.Nop()) }()
	select {
	case <-first:
	case <-time.After(2 * time.Second):
		t.Fatal("no request")
	}
	time.Sleep(250 * time.Millisecond)
	if calls.Load() != 1 {
		t.Fatal("global pause ignored")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker leaked")
	}
}

func TestWorkerTemporaryResultsAndErrors(t *testing.T) {
	for _, code := range []int{204, 500} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(code) }))
			defer server.Close()
			c, _ := New(server.URL)
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()
			applied := false
			store := workerStore{pending: func(context.Context) ([]string, error) { return []string{"00"}, nil }, apply: func(_ context.Context, _ string, status string, _ *model.Money) error {
				if status != "" {
					t.Error("204 finalized")
				}
				applied = true
				cancel()
				return errors.New("storage unavailable")
			}}
			Run(ctx, store, c, zerolog.New(io.Discard))
			if !applied {
				t.Fatal("temporary result")
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	c, _ := New("http://localhost")
	Run(ctx, workerStore{pending: func(context.Context) ([]string, error) { cancel(); return nil, errors.New("DB unavailable") }}, c, zerolog.Nop())
}
