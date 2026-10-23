// Package logger записывает HTTP-метаданные без адресов, тел, cookie и токенов.
package logger

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"time"

	"github.com/rs/zerolog"
)

// Failure добавляет только заданную операцию и безопасную категорию ошибки.
func Failure(event *zerolog.Event, operation string, err error) *zerolog.Event {
	event = event.Str("operation", operation)
	var state interface{ SQLState() string }
	var status interface{ HTTPStatus() int }
	var network net.Error
	var syntax *json.SyntaxError
	var value *json.UnmarshalTypeError
	switch {
	case errors.Is(err, context.Canceled):
		return event.Str("category", "canceled")
	case errors.Is(err, context.DeadlineExceeded):
		return event.Str("category", "timeout")
	case errors.As(err, &state):
		code := state.SQLState()
		if len(code) != 5 {
			return event.Str("category", "database")
		}
		for _, c := range code {
			if (c < '0' || c > '9') && (c < 'A' || c > 'Z') {
				return event.Str("category", "database")
			}
		}
		return event.Str("category", "database").Str("sqlstate", code)
	case errors.As(err, &status):
		return event.Str("category", "http_status").Int("http_status", status.HTTPStatus())
	case errors.As(err, &network):
		if network.Timeout() {
			return event.Str("category", "timeout")
		}
		return event.Str("category", "network")
	case errors.As(err, &syntax), errors.As(err, &value):
		return event.Str("category", "invalid_response")
	default:
		return event.Str("category", "internal")
	}
}

type responseData struct {
	status int
	size   int
}

type loggingResponseWriter struct {
	http.ResponseWriter
	responseData *responseData
}

// WriteHeader сохраняет первый отправленный HTTP-статус.
func (w *loggingResponseWriter) WriteHeader(status int) {
	if w.responseData.status != 0 {
		return
	}
	w.responseData.status = status
	w.ResponseWriter.WriteHeader(status)
}

// Write учитывает фактически отправленный размер ответа.
func (w *loggingResponseWriter) Write(data []byte) (int, error) {
	if w.responseData.status == 0 {
		w.responseData.status = http.StatusOK
	}
	size, err := w.ResponseWriter.Write(data)
	w.responseData.size += size
	return size, err
}

// Middleware записывает метод, длительность, статус и размер ответа.
func Middleware(log zerolog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			startedAt := time.Now()
			response := &responseData{}
			writer := &loggingResponseWriter{ResponseWriter: w, responseData: response}

			next.ServeHTTP(writer, r)

			if response.status == 0 {
				response.status = http.StatusOK
			}
			log.Info().
				Str("method", r.Method).
				Dur("duration", time.Since(startedAt)).
				Int("status", response.status).
				Int("size", response.size).
				Msg("request handled")
		})
	}
}
