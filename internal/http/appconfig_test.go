package http

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zaentrum/chino-api/internal/config"
)

// features.feedback is true exactly when bug reports can be filed: with an
// OpenProject token, as the router builds the feedback client.
func TestAppConfigReportsWhetherFeedbackIsOn(t *testing.T) {
	for _, c := range []struct {
		token string
		want  bool
	}{
		{"", false},
		{"  ", false},
		{"a-token", true},
	} {
		rec := httptest.NewRecorder()
		appConfig(config.Config{OpenProjectURL: "https://openproject.example", OpenProjectToken: c.token})(rec, httptest.NewRequest(http.MethodGet, "/api/config", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("token %q: status %d", c.token, rec.Code)
		}
		var doc struct {
			Features map[string]bool `json:"features"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
			t.Fatalf("token %q: %v", c.token, err)
		}
		got, ok := doc.Features["feedback"]
		if !ok || got != c.want {
			t.Errorf("token %q: features.feedback = %v (present %v), want %v", c.token, got, ok, c.want)
		}
	}
}

// features.downloads is what DOWNLOADS_ENABLED made of cfg.DownloadsEnabled,
// beside features.feedback, which it leaves as it is.
func TestAppConfigReportsWhetherDownloadsAreOn(t *testing.T) {
	for _, on := range []bool{true, false} {
		rec := httptest.NewRecorder()
		appConfig(config.Config{DownloadsEnabled: on, OpenProjectToken: "a-token"})(rec, httptest.NewRequest(http.MethodGet, "/api/config", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("downloads %v: status %d", on, rec.Code)
		}
		var doc struct {
			Features map[string]bool `json:"features"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
			t.Fatalf("downloads %v: %v", on, err)
		}
		if got, ok := doc.Features["downloads"]; !ok || got != on {
			t.Errorf("features.downloads = %v (present %v), want %v", got, ok, on)
		}
		if !doc.Features["feedback"] {
			t.Errorf("downloads %v: features.feedback = false, want true", on)
		}
	}
}
