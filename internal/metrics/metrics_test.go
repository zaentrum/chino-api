package metrics

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
)

// A request whose handler panics is counted, and the panic goes on: an
// answer cut off (http.ErrAbortHandler) with the status it went out with,
// one with no status out yet as 500; a whole answer as before.
func TestAPanickingHandlerIsCounted(t *testing.T) {
	boom := errors.New("boom")
	mux := chi.NewRouter()
	mux.Use(Middleware)
	mux.Get("/cut", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("hel"))
		panic(http.ErrAbortHandler)
	})
	mux.Get("/failed", func(http.ResponseWriter, *http.Request) { panic(boom) })
	mux.Get("/whole", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusPartialContent) })

	for _, c := range []struct {
		route, status string
		panics        any
	}{
		{"/cut", "200", http.ErrAbortHandler},
		{"/failed", "500", boom},
		{"/whole", "206", nil},
	} {
		labels := map[string]string{"route": c.route, "status": c.status}
		before := read(t, HTTPRequests, labels).value
		timed := read(t, HTTPDuration, labels).count
		func() {
			defer func() {
				if v := recover(); v != c.panics {
					t.Errorf("%s: the panic went on as %v, want %v", c.route, v, c.panics)
				}
			}()
			mux.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, c.route, nil))
		}()
		if got := read(t, HTTPRequests, labels).value; got != before+1 {
			t.Errorf("%s: counted %v times as %s, want once", c.route, got-before, c.status)
		}
		if got := read(t, HTTPDuration, labels).count; got != timed+1 {
			t.Errorf("%s: timed %d times as %s, want once", c.route, got-timed, c.status)
		}
	}
}
