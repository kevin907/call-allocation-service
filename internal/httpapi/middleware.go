package httpapi

import (
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"
)

// statusRecorder remembers what the handler wrote so the request log can report
// it. Handlers that write a body without calling WriteHeader still imply a 200.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *statusRecorder) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusRecorder) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}

// Unwrap keeps http.ResponseController working through the wrapper.
func (w *statusRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func logRequests(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Probes run every few seconds and would otherwise be most of the log.
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
			next.ServeHTTP(w, r)
			return
		}

		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)

		level := slog.LevelInfo
		switch {
		case rec.status >= http.StatusInternalServerError && rec.status != http.StatusServiceUnavailable:
			level = slog.LevelError
		case rec.status >= http.StatusBadRequest:
			// 503 lands here on purpose: a full or unknown region is the allocator
			// answering correctly, not the service failing.
			level = slog.LevelWarn
		}

		log.Log(r.Context(), level, "request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"bytes", rec.bytes,
			"duration_ms", float64(time.Since(start).Microseconds())/1000,
		)
	})
}

func recoverPanics(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			v := recover()
			if v == nil {
				return
			}
			log.ErrorContext(r.Context(), "panic serving request",
				"method", r.Method, "path", r.URL.Path, "panic", v, "stack", string(debug.Stack()))

			// If the handler already wrote a status, a second one only produces
			// a superfluous-WriteHeader warning and no better outcome.
			if rec, ok := w.(*statusRecorder); ok && rec.status != 0 {
				return
			}
			writeJSON(w, http.StatusInternalServerError, apiError{Code: codeInternal, Message: "internal error"})
		}()

		next.ServeHTTP(w, r)
	})
}
