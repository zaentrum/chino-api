package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/zaentrum/chino-api/internal/redact"
)

// The instance's subtitle languages: the languages every title of it is to
// have a subtitle in, as katalog-manager's settings name them, BCP 47 tags in
// the setting's order. They are the instance's, not a viewer's, so one answer
// serves every viewer.
//
//	GET /api/v1/subtitles/languages   {"languages": ["en", "de", "zh-Hans"]}; [] when none is set
//
// chino-api asks katalog-manager's GET /api/library/subtitles/languages with
// the bearer of the viewer whose request finds no fresh answer kept, and
// passes on its languages alone, as it gives them (not the setting they come
// from). An answer is fresh for languagesFresh; then the next request asks
// again, and when katalog-manager does not answer, the last answer is served
// while it is younger than languagesStale. A failure is never kept: with no
// answer that young the request is answered 502 and no list, and the next
// one asks again.

// languagesFresh is how long an answer of katalog-manager's is served before
// it is asked again: an admin's change of the setting reaches the clients
// within it.
const languagesFresh = 5 * time.Minute

// languagesStale is how old the last answer may be and still be served while
// katalog-manager does not answer.
const languagesStale = time.Hour

// languagesTimeout is how long katalog-manager has to answer.
const languagesTimeout = 5 * time.Second

// languagesPath is katalog-manager's route of the subtitle languages.
const languagesPath = "/api/library/subtitles/languages"

// subtitleLanguages serves GET /api/v1/subtitles/languages with the answers
// of katalog-manager at base, keeping the last one.
type subtitleLanguages struct {
	base    string
	fresh   time.Duration
	stale   time.Duration
	timeout time.Duration
	now     func() time.Time

	mu    sync.Mutex
	langs []string  // the last answer
	asked time.Time // when it was asked for; zero while there is none
}

// newSubtitleLanguages is the route's handler for katalog-manager at base,
// with nothing kept yet.
func newSubtitleLanguages(base string) *subtitleLanguages {
	return &subtitleLanguages{base: base, fresh: languagesFresh, stale: languagesStale,
		timeout: languagesTimeout, now: time.Now}
}

func (s *subtitleLanguages) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	langs, err := s.languages(r)
	if err != nil {
		slog.Warn("subtitle languages: katalog-manager did not answer, and there is no answer young enough to serve",
			"err", redact.Text(err.Error()))
		http.Error(w, "subtitle languages unavailable", http.StatusBadGateway)
		return
	}
	writeJSON(w, http.StatusOK, map[string][]string{"languages": langs})
}

// languages is the last answer while it is fresh; else katalog-manager's,
// asked with r's bearer, which is kept; else, when katalog-manager does not
// answer, the last answer while it is not stale.
func (s *subtitleLanguages) languages(r *http.Request) ([]string, error) {
	if langs, _, ok := s.kept(s.fresh); ok {
		return langs, nil
	}
	asked := s.now()
	langs, err := s.ask(r)
	if err == nil {
		s.keep(langs, asked)
		return langs, nil
	}
	if kept, age, ok := s.kept(s.stale); ok {
		slog.Warn("subtitle languages: katalog-manager did not answer, the last answer is served",
			"age", age.Round(time.Second).String(), "err", redact.Text(err.Error()))
		return kept, nil
	}
	return nil, err
}

// kept is the last answer and its age, when it was asked for less than
// maxAge ago.
func (s *subtitleLanguages) kept(maxAge time.Duration) ([]string, time.Duration, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.asked.IsZero() {
		return nil, 0, false
	}
	age := s.now().Sub(s.asked)
	if age >= maxAge {
		return nil, 0, false
	}
	return s.langs, age, true
}

// keep keeps langs, asked for at asked, in place of the last answer - unless
// that one was asked for later.
func (s *subtitleLanguages) keep(langs []string, asked time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if asked.Before(s.asked) {
		return
	}
	s.langs, s.asked = langs, asked
}

// ask is the languages katalog-manager answers r's bearer, as it gives them,
// in its order. It is an error when there is no katalog-manager, or it does
// not answer within the timeout, answers a status other than 2xx, or a body
// that is not its answer: no JSON object, or one whose languages are missing,
// null or not a list of strings.
func (s *subtitleLanguages) ask(r *http.Request) ([]string, error) {
	if s.base == "" {
		return nil, errors.New("katalog-manager is not configured (KATALOG_MANAGER_URL)")
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.timeout)
	defer cancel()
	req, err := katalogManagerRequest(ctx, r, s.base, http.MethodGet, languagesPath)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("katalog-manager answered %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var answer struct {
		Languages *[]string `json:"languages"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&answer); err != nil {
		return nil, fmt.Errorf("katalog-manager's answer: %w", err)
	}
	if answer.Languages == nil {
		return nil, errors.New("katalog-manager's answer has no languages")
	}
	return *answer.Languages, nil
}
