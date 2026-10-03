package http

import (
	"io"
	"log"
	"net/http"
	"runtime"

	"github.com/go-chi/chi/v5/middleware"

	"github.com/zaentrum/chino-api/internal/redact"
)

// redactingLogFormatter is chi's DefaultLogFormatter logging the request URI
// through redact.Text, so ?token= / ?stream= values never reach the log.
// Which one a client used stays visible: ?token=REDACTED is a client still
// sending its bearer in a URL.
type redactingLogFormatter struct {
	middleware.DefaultLogFormatter
}

func (f *redactingLogFormatter) NewLogEntry(r *http.Request) middleware.LogEntry {
	if red := redact.Text(r.RequestURI); red != r.RequestURI {
		// A copy for the log line only: the handlers keep the real URI.
		r = r.WithContext(r.Context())
		r.RequestURI = red
	}
	return f.DefaultLogFormatter.NewLogEntry(r)
}

// requestLogger is middleware.Logger (same line, same colour rule) writing
// to out, minus the credentials.
func requestLogger(out io.Writer) func(http.Handler) http.Handler {
	return middleware.RequestLogger(&redactingLogFormatter{middleware.DefaultLogFormatter{
		Logger:  log.New(out, "", log.LstdFlags),
		NoColor: runtime.GOOS == "windows",
	}})
}
