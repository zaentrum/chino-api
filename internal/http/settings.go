package http

import (
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"slices"

	"github.com/zaentrum/chino-api/internal/auth"
	"github.com/zaentrum/chino-api/internal/store"
)

// The settings an account keeps on the server, the same on every device it
// signs in on, in one store: GET and PATCH /api/v1/me/settings, by the
// person's subject, deleted with the rest of their data (DELETE /api/v1/me).
// It answers the fields it keeps: watchlist_by_kind, whether every list shows
// its movies and its shows apart (the lists by kind, watchlists.go).
//
// A PATCH sets each field its body names. A field the store does not keep,
// or a value a field does not take, refuses the request in the form of
// ADR-0023, part 4 (codeAnswer), and the request changes nothing: the fields
// beside the refused one neither.

// maxSettingsBody bounds what a PATCH of the settings reads.
const maxSettingsBody = 4096

// settingsJSON is the settings as the store answers them.
type settingsJSON struct {
	WatchlistByKind bool `json:"watchlist_by_kind"`
}

func toSettingsJSON(s store.Settings) settingsJSON {
	return settingsJSON{WatchlistByKind: s.WatchlistByKind}
}

// codeAnswer is a refusal in the form of ADR-0023, part 4: the case's code,
// which an app has its own sentence for; the values that sentence needs,
// numbers and names; and a line of English for the log and an app's
// details, never shown as the message.
type codeAnswer struct {
	Error  string         `json:"error"`
	Values map[string]any `json:"values,omitempty"`
	Detail string         `json:"detail"`
}

// getSettings -> GET /me/settings.
func getSettings(st *store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, _ := auth.SubjectFromContext(r.Context())
		if userID == "" {
			http.Error(w, "no subject", http.StatusUnauthorized)
			return
		}
		s, err := st.GetSettings(r.Context(), userID)
		if err != nil {
			http.Error(w, "db: "+err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, toSettingsJSON(s))
	}
}

// patchSettings -> PATCH /me/settings {"watchlist_by_kind": true|false|null}.
// 200 with the settings as kept.
func patchSettings(st *store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, _ := auth.SubjectFromContext(r.Context())
		if userID == "" {
			http.Error(w, "no subject", http.StatusUnauthorized)
			return
		}
		defer r.Body.Close()
		var body map[string]json.RawMessage
		if err := json.NewDecoder(io.LimitReader(r.Body, maxSettingsBody)).Decode(&body); err != nil || body == nil {
			// A body that is no JSON object, answered as the lists' routes
			// answer one.
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		change, refused := settingsChange(body)
		if refused != nil {
			writeJSON(w, http.StatusBadRequest, refused)
			return
		}
		s, err := st.ChangeSettings(r.Context(), userID, change)
		if err != nil {
			http.Error(w, "db: "+err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, toSettingsJSON(s))
	}
}

// settingsChange is what body changes; or, where the store refuses a field
// of it, the refusal of the first such field in the order of their names.
// Every field is checked before anything is written.
func settingsChange(body map[string]json.RawMessage) (store.SettingsChange, *codeAnswer) {
	var change store.SettingsChange
	for _, name := range slices.Sorted(maps.Keys(body)) {
		switch name {
		case "watchlist_by_kind":
			var on *bool
			if err := json.Unmarshal(body[name], &on); err != nil {
				return store.SettingsChange{}, &codeAnswer{Error: "watchlist_by_kind_invalid",
					Detail: "watchlist_by_kind takes true, false or null"}
			}
			// null puts it back to its first value, false.
			v := on != nil && *on
			change.WatchlistByKind = &v
		default:
			return store.SettingsChange{}, &codeAnswer{Error: "setting_unknown",
				Values: map[string]any{"name": name}, Detail: "the settings store keeps no such field"}
		}
	}
	return change, nil
}
