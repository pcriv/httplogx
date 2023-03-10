package httplogx

import (
	"context"
	"fmt"
	"io/ioutil"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/rs/zerolog"
)

func newRequestLogger(logger zerolog.Logger, opts ...Options) *requestLogger {
	reqLogger := requestLogger{
		Logger: logger,
	}
	if len(opts) > 0 {
		reqLogger.opts = Configure(opts[0])
	}
	return &reqLogger
}

// RequestLogger is an http middleware to log http requests and responses.
//
// NOTE: for simplicity, RequestLogger automatically makes use of the chi RequestID and
// Recoverer middleware.
func RequestLogger(logger zerolog.Logger, opts ...Options) func(next http.Handler) http.Handler {
	return chi.Chain(
		middleware.RequestID,
		Handler(logger, opts...),
		middleware.Recoverer,
	).Handler
}

func Handler(logger zerolog.Logger, opts ...Options) func(next http.Handler) http.Handler {
	var f middleware.LogFormatter = newRequestLogger(logger, opts...)
	return func(next http.Handler) http.Handler {
		fn := func(w http.ResponseWriter, r *http.Request) {
			entry := f.NewLogEntry(r)
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)

			buf := newLimitBuffer(512)
			ww.Tee(buf)

			t1 := time.Now()
			defer func() {
				var respBody []byte
				if ww.Status() >= 400 {
					respBody, _ = ioutil.ReadAll(buf)
				}
				entry.Write(ww.Status(), ww.BytesWritten(), ww.Header(), time.Since(t1), respBody)
			}()

			next.ServeHTTP(ww, middleware.WithLogEntry(r, entry))
		}
		return http.HandlerFunc(fn)
	}
}

type requestLogger struct {
	Logger zerolog.Logger
	opts   Options
}

func (l *requestLogger) NewLogEntry(r *http.Request) middleware.LogEntry {
	msg := fmt.Sprintf("Request: %s %s", r.Method, r.URL.Path)
	entry := &RequestLoggerEntry{
		Logger: l.Logger.With().Fields(requestLogFields(r, l.opts.SkipHeaders)).Logger(),
	}
	if l.opts.Concise {
		entry.Logger.Info().Msgf(msg)
	}
	return entry
}

type RequestLoggerEntry struct {
	Logger zerolog.Logger
	msg    string
	opts   Options
}

func (l *RequestLoggerEntry) Write(status, bytes int, header http.Header, elapsed time.Duration, extra interface{}) {
	msg := fmt.Sprintf("Response: %d %s", status, statusLabel(status))
	if l.msg != "" {
		msg = fmt.Sprintf("%s - %s", msg, l.msg)
	}

	responseLog := map[string]interface{}{
		"status":  status,
		"bytes":   bytes,
		"elapsed": float64(elapsed.Nanoseconds()) / 1000000.0, // in milliseconds
	}

	if l.opts.Concise {
		// Include response header, as well for error status codes (>400) we include
		// the response body so we may inspect the log message sent back to the client.
		if status >= 400 {
			body, _ := extra.([]byte)
			responseLog["body"] = string(body)
		}
		if len(header) > 0 {
			responseLog["header"] = headerLogField(header, l.opts.SkipHeaders)
		}
	}

	l.Logger.WithLevel(statusLevel(status)).Fields(map[string]interface{}{
		"http_response": responseLog,
	}).Msgf(msg)
}

func (l *RequestLoggerEntry) Panic(v interface{}, stack []byte) {
	stacktrace := "#"
	if l.opts.JSON {
		stacktrace = string(stack)
	}

	l.Logger = l.Logger.With().
		Str("stacktrace", stacktrace).
		Str("panic", fmt.Sprintf("%+v", v)).
		Logger()

	l.msg = fmt.Sprintf("%+v", v)

	if !l.opts.JSON {
		middleware.PrintPrettyStack(v)
	}
}

func requestLogFields(r *http.Request, skipHeaders []string) map[string]interface{} {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	requestURL := fmt.Sprintf("%s://%s%s", scheme, r.Host, r.RequestURI)

	requestFields := map[string]interface{}{
		"request_url":    requestURL,
		"request_method": r.Method,
		"request_path":   r.URL.Path,
		"remote_ip":      r.RemoteAddr,
		"proto":          r.Proto,
	}
	if reqID := middleware.GetReqID(r.Context()); reqID != "" {
		requestFields["request_id"] = reqID
	}

	requestFields["scheme"] = scheme

	if len(r.Header) > 0 {
		requestFields["header"] = headerLogField(r.Header, skipHeaders)
	}

	return map[string]interface{}{
		"http_request": requestFields,
	}
}

func headerLogField(header http.Header, skipList []string) map[string]string {
	headerField := map[string]string{}
	for k, v := range header {
		k = strings.ToLower(k)
		switch {
		case len(v) == 0:
			continue
		case len(v) == 1:
			headerField[k] = v[0]
		default:
			headerField[k] = fmt.Sprintf("[%s]", strings.Join(v, "], ["))
		}
		if k == "authorization" || k == "cookie" || k == "set-cookie" {
			headerField[k] = "***"
		}

		for _, skip := range skipList {
			if k == skip {
				headerField[k] = "***"
				break
			}
		}
	}
	return headerField
}

func statusLevel(status int) zerolog.Level {
	switch {
	case status <= 0:
		return zerolog.WarnLevel
	case status < 400: // for codes in 100s, 200s, 300s
		return zerolog.InfoLevel
	case status >= 400 && status < 500:
		return zerolog.WarnLevel
	case status >= 500:
		return zerolog.ErrorLevel
	default:
		return zerolog.InfoLevel
	}
}

func statusLabel(status int) string {
	switch {
	case status >= 100 && status < 300:
		return "OK"
	case status >= 300 && status < 400:
		return "Redirect"
	case status >= 400 && status < 500:
		return "Client Error"
	case status >= 500:
		return "Server Error"
	default:
		return "Unknown"
	}
}

// Helper methods used by the application to get the request-scoped
// logger entry and set additional fields between handlers.
//
// This is a useful pattern to use to set state on the entry as it
// passes through the handler chain, which at any point can be logged
// with a call to .Print(), .Info(), etc.

func LogEntry(ctx context.Context) zerolog.Logger {
	entry, ok := ctx.Value(middleware.LogEntryCtxKey).(*RequestLoggerEntry)
	if !ok || entry == nil {
		return zerolog.Nop()
	} else {
		return entry.Logger
	}
}

func LogEntrySetField(ctx context.Context, key, value string) {
	if entry, ok := ctx.Value(middleware.LogEntryCtxKey).(*RequestLoggerEntry); ok {
		entry.Logger = entry.Logger.With().Str(key, value).Logger()
	}
}

func LogEntrySetFields(ctx context.Context, fields map[string]interface{}) {
	if entry, ok := ctx.Value(middleware.LogEntryCtxKey).(*RequestLoggerEntry); ok {
		entry.Logger = entry.Logger.With().Fields(fields).Logger()
	}
}
