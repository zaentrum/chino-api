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

// maxSettingsBody bounds what a PATCH of the settings reads: a longer body
// is refused.
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
			storeFailed(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, toSettingsJSON(s))
	}
}

// patchSettings -> PATCH /me/settings {"watchlist_by_kind": true|false|null}.
// 200 with the settings as kept. Without a database, where nothing is kept,
// a change the store takes is answered as creating, renaming or deleting a
// list is then (listError of store.ErrListNotFound).
func patchSettings(st *store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, _ := auth.SubjectFromContext(r.Context())
		if userID == "" {
			http.Error(w, "no subject", http.StatusUnauthorized)
			return
		}
		defer r.Body.Close()
		// A body past maxSettingsBody fails to read, and is no JSON object.
		body, ok := jsonObject(http.MaxBytesReader(w, r.Body, maxSettingsBody))
		if !ok {
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
		if st == nil {
			listError(w, r, store.ErrListNotFound)
			return
		}
		s, err := st.ChangeSettings(r.Context(), userID, change)
		if err != nil {
			storeFailed(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, toSettingsJSON(s))
	}
}

// jsonObject is the fields of the one JSON object body holds, by name; false
// for a body that holds anything else, a field named twice, or more after the
// object than white space.
func jsonObject(body io.Reader) (map[string]json.RawMessage, bool) {
	dec := json.NewDecoder(body)
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, false
	}
	fields := map[string]json.RawMessage{}
	for dec.More() {
		t, err := dec.Token()
		name, isName := t.(string)
		if err != nil || !isName {
			return nil, false
		}
		if _, twice := fields[name]; twice {
			return nil, false
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, false
		}
		fields[name] = value
	}
	if t, err := dec.Token(); err != nil || t != json.Delim('}') {
		return nil, false
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, false
	}
	return fields, true
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
