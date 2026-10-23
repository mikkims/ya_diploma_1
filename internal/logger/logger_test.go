package logger

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/rs/zerolog"
)

type testStatusError int

func (e testStatusError) Error() string   { return "secret-response-body" }
func (e testStatusError) HTTPStatus() int { return int(e) }

func TestFailureCategories(t *testing.T) {
	for _, tt := range []struct {
		name     string
		err      error
		category string
		field    string
	}{
		{"cancel", fmt.Errorf("password=secret: %w", context.Canceled), "canceled", ""},
		{"timeout", &url.Error{Op: "GET", URL: "http://user:secret@host/path?token=secret", Err: context.DeadlineExceeded}, "timeout", ""},
		{"sql", &pgconn.PgError{Code: "23514", Message: "secret DSN password", Detail: "request-body"}, "database", `"sqlstate":"23514"`},
		{"http", testStatusError(503), "http_status", `"http_status":503`},
		{"json", &json.UnmarshalTypeError{Value: "secret", Field: "token"}, "invalid_response", ""},
		{"internal", fmt.Errorf("postgres://user:secret@host/db?token=secret request-body"), "internal", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var output bytes.Buffer
			log := zerolog.New(&output)
			Failure(log.Error(), "safe.operation", tt.err).Msg("operation failed")
			data := output.String()
			if !strings.Contains(data, `"operation":"safe.operation"`) || !strings.Contains(data, `"category":"`+tt.category+`"`) || !strings.Contains(data, tt.field) {
				t.Fatalf("missing diagnostics: %s", data)
			}
			for _, forbidden := range []string{"secret", "password", "postgres://", "http://", "request-body", "token"} {
				if strings.Contains(data, forbidden) {
					t.Fatalf("sensitive diagnostic: %s", data)
				}
			}
		})
	}
}

func TestMetadataOnly(t *testing.T) {
	var output bytes.Buffer
	h := Middleware(zerolog.New(&output))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(202)
		if _, err := w.Write([]byte("ok")); err != nil {
			t.Error(err)
		}
	}))
	r := httptest.NewRequest("POST", "/sensitive?token=secret", strings.NewReader("password-secret"))
	r.Header.Set("Authorization", "Bearer secret")
	r.Header.Set("Cookie", "secret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var entries []map[string]any
	decoder := json.NewDecoder(strings.NewReader(output.String()))
	for decoder.More() {
		var entry map[string]any
		if err := decoder.Decode(&entry); err != nil {
			t.Fatal(err)
		}
		entries = append(entries, entry)
	}
	if len(entries) != 1 {
		t.Fatalf("expected one HTTP record, got %d", len(entries))
	}
	for _, field := range []string{"method", "duration", "status", "size"} {
		if _, ok := entries[0][field]; !ok {
			t.Errorf("missing field %s", field)
		}
	}
	if strings.Contains(output.String(), "secret") || strings.Contains(output.String(), "sensitive") || !strings.Contains(output.String(), `"status":202`) || !strings.Contains(output.String(), `"size":2`) {
		t.Fatalf("unexpected metadata: %s", output.String())
	}
	output.Reset()
	h = Middleware(zerolog.New(&output))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := w.Write([]byte("ok")); err != nil {
			t.Error(err)
		}
	}))
	h.ServeHTTP(httptest.NewRecorder(), r)
	if !strings.Contains(output.String(), `"status":200`) {
		t.Fatal("implicit status")
	}
	output.Reset()
	h = Middleware(zerolog.New(&output))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	h.ServeHTTP(httptest.NewRecorder(), r)
	if !strings.Contains(output.String(), `"status":200`) {
		t.Fatal("empty response status")
	}
}
