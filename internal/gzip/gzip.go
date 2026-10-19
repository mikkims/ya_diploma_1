// Package gzip адаптирует сжатие HTTP-запросов и ответов по образцу shortener.
package gzip

import (
	compressgzip "compress/gzip"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/rs/zerolog"
)

type responseWriter struct {
	http.ResponseWriter
	status             int
	acceptsCompression bool
	wroteHeader        bool
	gzipWriter         *compressgzip.Writer
	logger             zerolog.Logger
}

// WriteHeader откладывает отправку статуса до выбора сжатия.
func (w *responseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

// Write отправляет данные через выбранный способ сжатия.
func (w *responseWriter) Write(data []byte) (int, error) {
	if !w.wroteHeader {
		w.commitHeader(data)
	}
	if w.gzipWriter != nil {
		return w.gzipWriter.Write(data)
	}
	return w.ResponseWriter.Write(data)
}

// MiddlewareGzip распаковывает запросы и согласует gzip для JSON и HTML.
func MiddlewareGzip(logger zerolog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if encoding := strings.TrimSpace(r.Header.Get("Content-Encoding")); encoding != "" && !strings.EqualFold(encoding, "gzip") {
				http.Error(w, "Unsupported encoding", http.StatusBadRequest)
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, 16384)
			if strings.EqualFold(strings.TrimSpace(r.Header.Get("Content-Encoding")), "gzip") {
				reader, err := compressgzip.NewReader(r.Body)
				if err != nil {
					http.Error(w, "Bad request", http.StatusBadRequest)
					return
				}
				r.Body = http.MaxBytesReader(w, reader, 16384)
				r.ContentLength = -1
				r.Header.Del("Content-Encoding")
				r.Header.Del("Content-Length")
				defer func() {
					if err := reader.Close(); err != nil {
						logger.Error().Err(err).Msg("failed to close gzip request body")
					}
				}()
			}

			writer := &responseWriter{
				ResponseWriter:     w,
				acceptsCompression: acceptsGzip(r.Header.Get("Accept-Encoding")),
				logger:             logger,
			}
			next.ServeHTTP(writer, r)
			if err := writer.finish(); err != nil {
				logger.Error().Err(err).Msg("failed to finish gzip response")
			}
		})
	}
}

func (w *responseWriter) commitHeader(firstChunk []byte) {
	if w.wroteHeader {
		return
	}
	if w.status == 0 {
		w.status = http.StatusOK
	}
	contentType := w.Header().Get("Content-Type")
	if contentType == "" && len(firstChunk) > 0 {
		contentType = http.DetectContentType(firstChunk)
		w.Header().Set("Content-Type", contentType)
	}
	if compressible(contentType) {
		w.Header().Add("Vary", "Accept-Encoding")
	}
	if w.acceptsCompression && compressible(contentType) && w.status != http.StatusNoContent && w.status != http.StatusNotModified {
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Del("Content-Length")
		w.gzipWriter = compressgzip.NewWriter(w.ResponseWriter)
	}
	w.ResponseWriter.WriteHeader(w.status)
	w.wroteHeader = true
}

func (w *responseWriter) finish() error {
	if !w.wroteHeader {
		w.commitHeader(nil)
	}
	if w.gzipWriter != nil {
		return w.gzipWriter.Close()
	}
	return nil
}

// Flush отправляет накопленные сжатые данные writer.
func (w *responseWriter) Flush() {
	if !w.wroteHeader {
		w.commitHeader(nil)
	}
	if w.gzipWriter != nil {
		if err := w.gzipWriter.Flush(); err != nil {
			w.logger.Error().Err(err).Msg("failed to flush gzip response")
		}
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func compressible(contentType string) bool {
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	return mediaType == "application/json" || mediaType == "text/html"
}

func acceptsGzip(header string) bool {
	for _, value := range strings.Split(header, ",") {
		parts := strings.Split(value, ";")
		if !strings.EqualFold(strings.TrimSpace(parts[0]), "gzip") {
			continue
		}
		for _, parameter := range parts[1:] {
			name, rawValue, ok := strings.Cut(strings.TrimSpace(parameter), "=")
			if ok && strings.EqualFold(name, "q") {
				quality, err := strconv.ParseFloat(rawValue, 64)
				return err == nil && quality > 0
			}
		}
		return true
	}
	return false
}
