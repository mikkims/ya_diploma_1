package gzip

import (
	"bytes"
	compressgzip "compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

func TestMiddlewareDecompressesRequest(t *testing.T) {
	const body = `{"url":"https://practicum.yandex.ru"}`
	var compressed bytes.Buffer
	writer := compressgzip.NewWriter(&compressed)
	_, _ = writer.Write([]byte(body))
	_ = writer.Close()

	handler := MiddlewareGzip(zerolog.Nop())(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request: %v", err)
		}
		if string(data) != body {
			t.Errorf("body = %q, want %q", data, body)
		}
		w.WriteHeader(http.StatusCreated)
	}))
	request := httptest.NewRequest(http.MethodPost, "/api/shorten", &compressed)
	request.Header.Set("Content-Encoding", "gzip")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusCreated {
		t.Errorf("status = %d, want %d", response.Code, http.StatusCreated)
	}
}

func TestMiddlewareCompressesSupportedResponse(t *testing.T) {
	const body = `{"result":"http://localhost:8080/abcdefgh"}`
	handler := MiddlewareGzip(zerolog.Nop())(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(body))
	}))
	request := httptest.NewRequest(http.MethodPost, "/api/shorten", nil)
	request.Header.Set("Accept-Encoding", "br, gzip")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", response.Header().Get("Content-Encoding"))
	}
	reader, err := compressgzip.NewReader(response.Body)
	if err != nil {
		t.Fatalf("create gzip reader: %v", err)
	}
	decompressed, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read compressed response: %v", err)
	}
	_ = reader.Close()
	if string(decompressed) != body {
		t.Errorf("body = %q, want %q", decompressed, body)
	}
}

func TestMiddlewareCompressesHTMLResponse(t *testing.T) {
	const body = `<!doctype html><html><body>shortener</body></html>`
	handler := MiddlewareGzip(zerolog.Nop())(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(body))
	}))
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("Accept-Encoding", "gzip")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	reader, err := compressgzip.NewReader(response.Body)
	if err != nil {
		t.Fatalf("create gzip reader: %v", err)
	}
	decompressed, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read compressed response: %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("close gzip reader: %v", err)
	}
	if response.Header().Get("Content-Encoding") != "gzip" || string(decompressed) != body {
		t.Fatalf("encoding=%q body=%q", response.Header().Get("Content-Encoding"), decompressed)
	}
}

func TestMiddlewareDoesNotCompressUnsupportedResponse(t *testing.T) {
	handler := MiddlewareGzip(zerolog.Nop())(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("plain response"))
	}))
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("Accept-Encoding", "gzip")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if encoding := response.Header().Get("Content-Encoding"); encoding != "" {
		t.Errorf("Content-Encoding = %q, want empty", encoding)
	}
	if strings.TrimSpace(response.Body.String()) != "plain response" {
		t.Errorf("body = %q, want %q", response.Body.String(), "plain response")
	}
}

func TestMiddlewareStreamsCompressedResponseBeforeHandlerReturns(t *testing.T) {
	flushed := make(chan struct{})
	release := make(chan struct{})
	handler := MiddlewareGzip(zerolog.Nop())(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":"ready"}`))
		w.(http.Flusher).Flush()
		close(flushed)
		<-release
	}))
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("Accept-Encoding", "gzip")
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(response, request)
		close(done)
	}()

	select {
	case <-flushed:
		if !response.Flushed || response.Body.Len() == 0 {
			t.Fatal("gzip response не передан клиенту до завершения handler")
		}
	case <-time.After(time.Second):
		t.Fatal("ожидание потоковой записи превысило timeout")
	}
	close(release)
	<-done
}
