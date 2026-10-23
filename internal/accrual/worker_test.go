package accrual

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mikkims/ya_diploma_1/internal/model"
	"github.com/rs/zerolog"
)

type countedBody struct {
	io.Reader
	closed *atomic.Int64
}

func (b countedBody) Close() error { b.closed.Add(1); return nil }

func TestFetchRetryPolicy(t *testing.T) {
	for _, tt := range []struct {
		name       string
		codes      []int
		body       string
		wantStatus string
		wantErr    bool
	}{
		{name: "recovery", codes: []int{503, 502, 200}, body: `{"order":"00","status":"PROCESSED"}`, wantStatus: model.Processed},
		{name: "exhausted", codes: []int{599, 599, 599}, wantErr: true},
		{name: "no retry on 400", codes: []int{400}, wantErr: true},
		{name: "stop on 429", codes: []int{500, 429}, wantErr: true},
		{name: "stop on 204", codes: []int{500, 204}},
		{name: "stop on invalid JSON", codes: []int{500, 200}, body: "invalid", wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := New("http://example.test", zerolog.Nop())
			var closed atomic.Int64
			var times []time.Time
			c.http.Transport = testTransport(func(*http.Request) (*http.Response, error) {
				if closed.Load() != int64(len(times)) {
					t.Error("response body not closed before retry")
				}
				times = append(times, time.Now())
				if len(times) > len(tt.codes) {
					t.Error("too many attempts")
					return nil, errors.New("unexpected attempt")
				}
				return &http.Response{StatusCode: tt.codes[len(times)-1], Header: http.Header{"Retry-After": []string{"1"}}, Body: countedBody{strings.NewReader(tt.body), &closed}}, nil
			})
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			got, err := c.Fetch(ctx, "00")
			if (err != nil) != tt.wantErr || got.Status != tt.wantStatus {
				t.Fatalf("result %+v, error %v", got, err)
			}
			if len(times) != len(tt.codes) || closed.Load() != int64(len(times)) {
				t.Fatal("attempt or close count")
			}
			for i := 1; i < len(times); i++ {
				minimum := time.Duration(1<<(i-1)) * 100 * time.Millisecond
				if times[i].Sub(times[i-1]) < minimum {
					t.Error("backoff too short")
				}
			}
			if tt.codes[len(tt.codes)-1] == 429 {
				var limited *RateLimitError
				if !errors.As(err, &limited) {
					t.Fatal("rate limit type lost")
				}
			}
		})
	}
}

func TestFetchCancellationDuringBackoff(t *testing.T) {
	c, _ := New("http://example.test", zerolog.Nop())
	first := make(chan struct{}, 1)
	var calls atomic.Int64
	c.http.Transport = testTransport(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		first <- struct{}{}
		return &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := c.Fetch(ctx, "00"); done <- err }()
	select {
	case <-first:
	case <-time.After(time.Second):
		t.Fatal("no attempt")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || calls.Load() != 1 {
			t.Fatalf("cancellation: %v, calls %d", err, calls.Load())
		}
	case <-time.After(time.Second):
		t.Fatal("backoff did not stop")
	}
}

func TestRetryAfterDiagnostics(t *testing.T) {
	var output strings.Builder
	c, _ := New("http://example.test", zerolog.New(&output))
	raw := "unexpected\\nformat"
	c.http.Transport = testTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": []string{raw}}, Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	_, err := c.Fetch(context.Background(), "00")
	var limited *RateLimitError
	if !errors.As(err, &limited) || limited.RetryAfter != time.Second {
		t.Fatal("fallback changed")
	}
	var record map[string]any
	if err := json.Unmarshal([]byte(output.String()), &record); err != nil {
		t.Fatal(err)
	}
	if record["retry_after"] != raw || record["level"] != "warn" || record["error"] == nil {
		t.Fatalf("missing parse diagnostics: %v", record)
	}
	now := time.Now().UTC().Truncate(time.Second)
	for _, tt := range []struct {
		value   string
		want    time.Duration
		invalid bool
	}{
		{"", time.Second, true}, {"broken", time.Second, true}, {"-1", time.Second, true},
		{"0", 0, false}, {"2", 2 * time.Second, false},
		{fmt.Sprint(int64(math.MaxInt64)), time.Duration(math.MaxInt64), false},
		{now.Add(time.Minute).Format(http.TimeFormat), time.Minute, false},
		{now.Add(-time.Minute).Format(http.TimeFormat), time.Second, false},
	} {
		got, parseErr := retryAfter(tt.value, now)
		if got != tt.want || (parseErr != nil) != tt.invalid {
			t.Errorf("Retry-After %q: %v, %v", tt.value, got, parseErr)
		}
	}
}

func TestRateLimitPausesServerErrorRetry(t *testing.T) {
	c, _ := New("http://example.test", zerolog.Nop())
	first := make(chan struct{}, 1)
	retried := make(chan time.Time, 1)
	var attempts atomic.Int64
	c.http.Transport = testTransport(func(r *http.Request) (*http.Response, error) {
		code := 204
		if strings.HasSuffix(r.URL.Path, "/18") {
			code = 429
		} else if attempts.Add(1) == 1 {
			code = 503
			first <- struct{}{}
		} else {
			retried <- time.Now()
		}
		return &http.Response{StatusCode: code, Header: http.Header{"Retry-After": []string{"1"}}, Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := c.Fetch(ctx, "00"); done <- err }()
	select {
	case <-first:
	case <-ctx.Done():
		t.Fatal("first attempt missing")
	}
	if _, err := c.Fetch(ctx, "18"); err == nil {
		t.Fatal("429 missing")
	}
	c.mu.Lock()
	until := c.pausedUntil
	c.mu.Unlock()
	select {
	case started := <-retried:
		if started.Before(until) {
			t.Fatal("5xx retry ignored shared pause")
		}
	case <-ctx.Done():
		t.Fatal("retry never resumed")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentRateLimitsNeverShortenPause(t *testing.T) {
	c, _ := New("http://example.test", zerolog.Nop())
	started := make(chan struct{}, 3)
	gates := map[string]chan struct{}{"00": make(chan struct{}), "18": make(chan struct{}), "26": make(chan struct{})}
	delays := map[string]string{"00": "1", "18": "2", "26": "0"}
	c.http.Transport = testTransport(func(r *http.Request) (*http.Response, error) {
		n := strings.TrimPrefix(r.URL.Path, "/api/orders/")
		started <- struct{}{}
		select {
		case <-gates[n]:
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
		return &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": []string{delays[n]}}, Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 3)
	for n := range gates {
		go func() { _, err := c.Fetch(ctx, n); done <- err }()
	}
	for range 3 {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("requests did not overlap")
		}
	}
	var longest time.Time
	for _, n := range []string{"00", "18", "26"} {
		close(gates[n])
		var limited *RateLimitError
		if err := <-done; !errors.As(err, &limited) {
			t.Fatal(err)
		}
		c.mu.Lock()
		until := c.pausedUntil
		c.mu.Unlock()
		if !longest.IsZero() && until.Before(longest) {
			t.Fatal("pause shortened")
		}
		if n == "18" && !until.After(longest) {
			t.Fatal("later longer rate limit did not extend pause")
		}
		longest = until
	}
}

func TestWorkerPoolBoundedParallelism(t *testing.T) {
	c, _ := New("http://example.test", zerolog.Nop())
	entered := make(chan struct{}, 8)
	release := make(chan struct{})
	var active, peak, pendingCalls atomic.Int64
	c.http.Transport = testTransport(func(r *http.Request) (*http.Response, error) {
		n := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); n > old; old = peak.Load() {
			if peak.CompareAndSwap(old, n) {
				break
			}
		}
		entered <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
		return &http.Response{StatusCode: 204, Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var mu sync.Mutex
	applied := map[string]int{}
	store := workerStore{
		pending: func(context.Context) ([]string, error) {
			pendingCalls.Add(1)
			return []string{"00", "18", "26", "34", "42", "59", "67", "75"}, nil
		},
		apply: func(_ context.Context, number, status string, amount *model.Money) error {
			mu.Lock()
			defer mu.Unlock()
			applied[number]++
			if status != "" || amount != nil {
				t.Error("unexpected 204 result")
			}
			if len(applied) == 8 {
				cancel()
			}
			return nil
		},
	}
	done := make(chan struct{})
	go func() { defer close(done); Run(ctx, store, c, zerolog.Nop()) }()
	for range 4 {
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("four concurrent workers missing")
		}
	}
	select {
	case <-entered:
		t.Fatal("worker bound exceeded")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("pool did not finish")
	}
	if peak.Load() != 4 || pendingCalls.Load() != 1 {
		t.Fatal("parallelism or batch ownership")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(applied) != 8 {
		t.Fatalf("applied %d orders", len(applied))
	}
	for _, count := range applied {
		if count != 1 {
			t.Error("duplicate application")
		}
	}
}

func TestWorkerCancellationDuringHTTP(t *testing.T) {
	entered := make(chan struct{}, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		<-r.Context().Done()
	}))
	defer server.Close()
	c, _ := New(server.URL, zerolog.Nop())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := workerStore{pending: func(context.Context) ([]string, error) { return []string{"00", "18", "26", "34", "42"}, nil }, apply: func(context.Context, string, string, *model.Money) error {
		t.Error("canceled HTTP applied")
		return nil
	}}
	done := make(chan struct{})
	go func() { defer close(done); Run(ctx, store, c, zerolog.Nop()) }()
	for range 4 {
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatal("HTTP workers missing")
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("HTTP or blocked dispatch did not stop")
	}
}
