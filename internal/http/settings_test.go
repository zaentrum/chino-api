package http

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zaentrum/chino-api/internal/config"
	"github.com/zaentrum/chino-api/internal/eventsse"
)

const settingsRoute = "/api/v1/me/settings"

// The refusals of the settings store, as ADR-0023, part 4 has them.
const (
	unknownTheme   = `{"error":"setting_unknown","values":{"name":"theme"},"detail":"the settings store keeps no such field"}` + "\n"
	byKindInvalid  = `{"error":"watchlist_by_kind_invalid","detail":"watchlist_by_kind takes true, false or null"}` + "\n"
	settingsOff    = `{"watchlist_by_kind":false}` + "\n"
	settingsByKind = `{"watchlist_by_kind":true}` + "\n"
)

// settingsCase is a request of the settings store and the answer it gets.
type settingsCase struct {
	name   string
	method string
	body   string
	code   int
	want   string
}

// checkSettings sends each case's request for the viewer of header, in turn,
// and compares the answer: its status, its body and its content type.
func checkSettings(t *testing.T, h http.Handler, header http.Header, cases []settingsCase) {
	t.Helper()
	for _, c := range cases {
		w := send(h, c.method, settingsRoute, header, c.body)
		if w.Code != c.code || w.Body.String() != c.want {
			t.Errorf("%s: %d %q, want %d %q", c.name, w.Code, w.Body, c.code, c.want)
		}
		wantType := "application/json"
		if !strings.HasPrefix(c.want, "{") {
			wantType = "text/plain; charset=utf-8"
		}
		if got := w.Header().Get("Content-Type"); got != wantType {
			t.Errorf("%s: Content-Type %q, want %q", c.name, got, wantType)
		}
	}
}

// What the store refuses, it refuses whether or not a database keeps
// anything: a field it does not keep, by its name, the first by name of
// several; a value of watchlist_by_kind that is no boolean and no null; and
// a body that is no JSON object - one that names a field twice, or has more
// after the object, among them, past the bound of what is read too - as
// plain text. Without a database the settings are the first values, and a
// change the store takes is answered as creating, renaming or deleting a
// list is then: 404 "list not found".
func Test16C25TheSettingsStoreRefusesWhatItDoesNotKeep(t *testing.T) {
	is := newIssuer(t)
	h := filesRouter(t, is, "http://katalog-api.invalid", nil)
	bearer := bearerWith(t, is, "viewer-1", nil)
	checkSettings(t, h, bearer, []settingsCase{
		{"a field the store does not keep", http.MethodPatch, `{"theme":"dark"}`, http.StatusBadRequest, unknownTheme},
		{"the app's language, not kept yet", http.MethodPatch, `{"language":"de"}`, http.StatusBadRequest,
			`{"error":"setting_unknown","values":{"name":"language"},"detail":"the settings store keeps no such field"}` + "\n"},
		{"two it does not keep: the first by name", http.MethodPatch, `{"zoom":2,"autoplay":true}`, http.StatusBadRequest,
			`{"error":"setting_unknown","values":{"name":"autoplay"},"detail":"the settings store keeps no such field"}` + "\n"},
		{"one it does not keep, before a value refused by name", http.MethodPatch, `{"watchlist_by_kind":"off","theme":"dark"}`,
			http.StatusBadRequest, unknownTheme},
		{"a value refused, before one it does not keep by name", http.MethodPatch, `{"zoom":2,"watchlist_by_kind":"off"}`,
			http.StatusBadRequest, byKindInvalid},
		{"a string", http.MethodPatch, `{"watchlist_by_kind":"true"}`, http.StatusBadRequest, byKindInvalid},
		{"a number", http.MethodPatch, `{"watchlist_by_kind":1}`, http.StatusBadRequest, byKindInvalid},
		{"an object", http.MethodPatch, `{"watchlist_by_kind":{}}`, http.StatusBadRequest, byKindInvalid},
		{"a list", http.MethodPatch, `{"watchlist_by_kind":[true]}`, http.StatusBadRequest, byKindInvalid},
		{"no object: a list", http.MethodPatch, `[{"watchlist_by_kind":true}]`, http.StatusBadRequest, "bad json\n"},
		{"no object: null", http.MethodPatch, `null`, http.StatusBadRequest, "bad json\n"},
		{"no object: a boolean", http.MethodPatch, `true`, http.StatusBadRequest, "bad json\n"},
		{"no object: a string", http.MethodPatch, `"watchlist_by_kind"`, http.StatusBadRequest, "bad json\n"},
		{"no object: nothing", http.MethodPatch, ``, http.StatusBadRequest, "bad json\n"},
		{"no object: cut short", http.MethodPatch, `{"watchlist_by_kind":`, http.StatusBadRequest, "bad json\n"},
		{"a field named twice", http.MethodPatch, `{"watchlist_by_kind":"x","watchlist_by_kind":true}`, http.StatusBadRequest, "bad json\n"},
		{"a field named twice, both values taken", http.MethodPatch, `{"watchlist_by_kind":true,"watchlist_by_kind":false}`,
			http.StatusBadRequest, "bad json\n"},
		{"a field named twice, once with its underscore escaped", http.MethodPatch,
			`{"watchlist_by_kind":true,"watchlist` + "\\" + `u005fby_kind":true}`, http.StatusBadRequest, "bad json\n"},
		{"text after the object", http.MethodPatch, `{"watchlist_by_kind":true} and more`, http.StatusBadRequest, "bad json\n"},
		{"a second object", http.MethodPatch, `{"watchlist_by_kind":true}{"watchlist_by_kind":false}`, http.StatusBadRequest, "bad json\n"},
		{"text after the object, past the bound", http.MethodPatch, `{"watchlist_by_kind":true}` + strings.Repeat(" ", maxSettingsBody) + `and more`,
			http.StatusBadRequest, "bad json\n"},
		{"a second object naming the field again, past the bound", http.MethodPatch,
			`{"watchlist_by_kind":true}` + strings.Repeat(" ", maxSettingsBody) + `{"watchlist_by_kind":false}`, http.StatusBadRequest, "bad json\n"},
		{"an object longer than the bound", http.MethodPatch, `{"watchlist_by_kind":true` + strings.Repeat(" ", maxSettingsBody) + `}`,
			http.StatusBadRequest, "bad json\n"},
		{"without a database, at first", http.MethodGet, ``, http.StatusOK, settingsOff},
		{"without a database, nothing is kept", http.MethodPatch, `{"watchlist_by_kind":true}`, http.StatusNotFound, "list not found\n"},
		{"without a database, white space after the object", http.MethodPatch, "{\"watchlist_by_kind\":false}\n\t ",
			http.StatusNotFound, "list not found\n"},
		{"without a database, a change of nothing", http.MethodPatch, `{}`, http.StatusNotFound, "list not found\n"},
		{"without a database, still at first", http.MethodGet, ``, http.StatusOK, settingsOff},
	})
	// As creating a list is answered without a database.
	if w := send(h, http.MethodPost, "/api/v1/me/watchlists", bearer, `{"name":"Rainy days"}`); w.Code != http.StatusNotFound || w.Body.String() != "list not found\n" {
		t.Errorf("a list's change without a database: %d %q", w.Code, w.Body)
	}
	for _, method := range []string{http.MethodGet, http.MethodPatch} {
		if w := send(h, method, settingsRoute, nil, `{"watchlist_by_kind":true}`); w.Code != http.StatusUnauthorized {
			t.Errorf("%s without a bearer: %d", method, w.Code)
		}
	}
}

// Against Postgres (CHINO_API_TEST_DATABASE_URL): the account keeps
// watchlist_by_kind, false at first; null puts it back to false; a refused
// request changes nothing, the field beside the refused one neither;
// another person's settings are their own; and /api/config says
// features.watchlist_kinds.
func Test16C25TheAccountKeepsWhetherListsShowMoviesAndShowsApart(t *testing.T) {
	st := testStore(t)
	is := newIssuer(t)
	h := filesRouter(t, is, "http://katalog-api.invalid", st)
	checkSettings(t, h, bearerWith(t, is, "viewer-1", nil), []settingsCase{
		{"at first", http.MethodGet, ``, http.StatusOK, settingsOff},
		{"on", http.MethodPatch, `{"watchlist_by_kind":true}`, http.StatusOK, settingsByKind},
		{"kept", http.MethodGet, ``, http.StatusOK, settingsByKind},
		{"a change of nothing", http.MethodPatch, `{}`, http.StatusOK, settingsByKind},
		{"off beside a field refused before it", http.MethodPatch, `{"watchlist_by_kind":false,"theme":"dark"}`,
			http.StatusBadRequest, unknownTheme},
		{"off beside a field refused after it", http.MethodPatch, `{"watchlist_by_kind":false,"zoom":2}`, http.StatusBadRequest,
			`{"error":"setting_unknown","values":{"name":"zoom"},"detail":"the settings store keeps no such field"}` + "\n"},
		{"a value refused", http.MethodPatch, `{"watchlist_by_kind":"off"}`, http.StatusBadRequest, byKindInvalid},
		{"no object", http.MethodPatch, `[false]`, http.StatusBadRequest, "bad json\n"},
		{"off, named twice", http.MethodPatch, `{"watchlist_by_kind":false,"watchlist_by_kind":false}`, http.StatusBadRequest, "bad json\n"},
		{"off, with text after it", http.MethodPatch, `{"watchlist_by_kind":false},`, http.StatusBadRequest, "bad json\n"},
		{"still on", http.MethodGet, ``, http.StatusOK, settingsByKind},
		{"on, with white space after it", http.MethodPatch, "{\"watchlist_by_kind\":true}\r\n", http.StatusOK, settingsByKind},
		{"off", http.MethodPatch, `{"watchlist_by_kind":false}`, http.StatusOK, settingsOff},
		{"kept off", http.MethodGet, ``, http.StatusOK, settingsOff},
		{"on again", http.MethodPatch, `{"watchlist_by_kind":true}`, http.StatusOK, settingsByKind},
		{"null puts it back", http.MethodPatch, `{"watchlist_by_kind":null}`, http.StatusOK, settingsOff},
		{"kept back", http.MethodGet, ``, http.StatusOK, settingsOff},
		{"on once more", http.MethodPatch, `{"watchlist_by_kind":true}`, http.StatusOK, settingsByKind},
	})
	checkSettings(t, h, bearerWith(t, is, "viewer-2", nil), []settingsCase{
		{"another person's", http.MethodGet, ``, http.StatusOK, settingsOff},
	})
	// A server that keeps the setting says so; with sign-in off it keeps it
	// for nobody, and says not.
	if f := featuresOf(t, do(h, http.MethodGet, "/api/config", nil)); !f["watchlist_kinds"] {
		t.Error("with a database: features.watchlist_kinds = false")
	}
	off, err := NewRouter(config.Config{OIDCEnabled: false, KatalogBaseURL: "http://katalog-api.invalid",
		StreamSigningKey: signingKey}, st, eventsse.NewBroker())
	if err != nil {
		t.Fatal(err)
	}
	if f := featuresOf(t, do(off, http.MethodGet, "/api/config", nil)); f["watchlist_kinds"] {
		t.Error("with a database and sign-in off: features.watchlist_kinds = true")
	}
	if w := send(off, http.MethodPatch, settingsRoute, nil, `{"watchlist_by_kind":true}`); w.Code != http.StatusUnauthorized {
		t.Errorf("with sign-in off, a change: %d %q", w.Code, w.Body)
	}
}

// Against Postgres: the settings go with the account's data. A refused
// deletion keeps them; DELETE /api/v1/me deletes the row, counts it, and
// leaves the person at the first values, and another person's as they are.
func Test16C25TheSettingsGoWithTheAccount(t *testing.T) {
	st := testStore(t)
	is := newIssuer(t)
	fp := newFakePortal(t)
	h := accountRouter(t, is, fp.URL, deletionToken, st)
	kid := http.Header{"Authorization": {"Bearer " + is.tokenWith(t, "kid-3", "chino", realm("zaentrum-user"))}}
	parent := http.Header{"Authorization": {"Bearer " + is.tokenWith(t, "parent-3", "chino", realm("zaentrum-user"))}}
	for _, header := range []http.Header{kid, parent} {
		checkSettings(t, h, header, []settingsCase{
			{"on", http.MethodPatch, `{"watchlist_by_kind":true}`, http.StatusOK, settingsByKind},
		})
	}

	fp.status, fp.body = http.StatusConflict, "You are the last admin: make someone else an admin first."
	if w := do(h, http.MethodDelete, "/api/v1/me", kid); w.Code != http.StatusConflict {
		t.Fatalf("refused: %d %s", w.Code, w.Body)
	}
	checkSettings(t, h, kid, []settingsCase{{"a refused deletion keeps them", http.MethodGet, ``, http.StatusOK, settingsByKind}})

	fp.status, fp.body = http.StatusNoContent, ""
	w := do(h, http.MethodDelete, "/api/v1/me", kid)
	if w.Code != http.StatusOK {
		t.Fatalf("deleted: %d %s", w.Code, w.Body)
	}
	if deleted, _ := answer(t, w)["deleted"].(map[string]any); deleted["settings"] != float64(1) {
		t.Errorf("counted %v, want settings 1", deleted)
	}
	checkSettings(t, h, kid, []settingsCase{{"deleted", http.MethodGet, ``, http.StatusOK, settingsOff}})
	checkSettings(t, h, parent, []settingsCase{{"another person's", http.MethodGet, ``, http.StatusOK, settingsByKind}})
}

// Against Postgres: a read of the database that fails answers 500 with a
// fixed line, and the error itself is logged on the server, with the route,
// not sent to the caller.
func Test16C25ADatabaseErrorKeepsItsDetailToTheServer(t *testing.T) {
	st := testStore(t)
	is := newIssuer(t)
	h := filesRouter(t, is, "http://katalog-api.invalid", st)
	var logged bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(prev) })
	st.Close() // every read and write of the store fails from now on

	w := send(h, http.MethodGet, settingsRoute, bearerWith(t, is, "viewer-1", nil), "")
	if w.Code != http.StatusInternalServerError || w.Body.String() != errDatabase+"\n" {
		t.Errorf("%d %q, want 500 %q", w.Code, w.Body, errDatabase+"\n")
	}
	line := ""
	for _, l := range strings.Split(logged.String(), "\n") {
		if strings.Contains(l, "the database failed") {
			line = l
		}
	}
	if !strings.Contains(line, "GET "+settingsRoute+": the database failed: ") ||
		strings.HasSuffix(line, "the database failed: ") {
		t.Errorf("logged %q, want the route and the error", line)
	}
}

// openapi.yaml documents GET /api/config with every field it answers, the
// features among them.
func Test16C25OpenAPIDocumentsTheConfig(t *testing.T) {
	doc := string(openapi)
	if !strings.Contains(yamlBlock(doc, "  /config:"), "#/components/schemas/AppConfig") {
		t.Fatal("/config in openapi.yaml has no AppConfig")
	}
	schema := yamlBlock(doc, "    AppConfig:")
	var answer map[string]json.RawMessage
	w := httptest.NewRecorder()
	appConfig(config.Config{}, false)(w, httptest.NewRequest(http.MethodGet, "/api/config", nil))
	if err := json.Unmarshal(w.Body.Bytes(), &answer); err != nil {
		t.Fatal(err)
	}
	for name, value := range answer {
		if !strings.Contains(schema, "        "+name+":") {
			t.Errorf("AppConfig in openapi.yaml has no %s", name)
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal(value, &fields) != nil {
			continue
		}
		for field := range fields {
			if !strings.Contains(yamlBlock(schema, "        "+name+":"), "            "+field+":") {
				t.Errorf("AppConfig in openapi.yaml has no %s.%s", name, field)
			}
		}
	}
}

// features.watchlist_kinds is true exactly where the account keeps its
// settings: on a server with a database and sign-in on (the Postgres test
// above says the other half).
func Test8C18AppConfigSaysWhetherListsComeByKind(t *testing.T) {
	for _, keeps := range []bool{true, false} {
		for _, signIn := range []bool{true, false} {
			w := httptest.NewRecorder()
			appConfig(config.Config{OpenProjectToken: "a-token", OIDCEnabled: signIn}, keeps)(w, httptest.NewRequest(http.MethodGet, "/api/config", nil))
			f := featuresOf(t, w)
			if got, ok := f["watchlist_kinds"]; !ok || got != (keeps && signIn) {
				t.Errorf("keeps settings %v, sign-in %v: features.watchlist_kinds = %v (present %v)", keeps, signIn, got, ok)
			}
			if !f["feedback"] {
				t.Errorf("keeps settings %v, sign-in %v: features.feedback = false, want true", keeps, signIn)
			}
		}
	}

	h := filesRouter(t, newIssuer(t), "http://katalog-api.invalid", nil)
	if f := featuresOf(t, do(h, http.MethodGet, "/api/config", nil)); f["watchlist_kinds"] {
		t.Error("without a database: features.watchlist_kinds = true")
	}
}

// featuresOf is the features /api/config answers in w.
func featuresOf(t *testing.T, w *httptest.ResponseRecorder) map[string]bool {
	t.Helper()
	var doc struct {
		Features map[string]bool `json:"features"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil || w.Code != http.StatusOK {
		t.Fatalf("%d %s: %v", w.Code, w.Body, err)
	}
	return doc.Features
}

// openapi.yaml lists the settings routes with both codes of their refusal,
// and the lists' kind and include parameters.
func Test16C25OpenAPIListsTheSettingsCodes(t *testing.T) {
	doc := string(openapi)
	settings := yamlBlock(doc, "  /v1/me/settings:")
	for _, want := range []string{"    get:", "    patch:", "setting_unknown", "watchlist_by_kind_invalid", "#/components/schemas/SettingRefusal"} {
		if !strings.Contains(settings, want) {
			t.Errorf("/v1/me/settings in openapi.yaml does not say %q", want)
		}
	}
	if refusal := yamlBlock(doc, "    SettingRefusal:"); !strings.Contains(refusal, "enum: [setting_unknown, watchlist_by_kind_invalid]") {
		t.Errorf("SettingRefusal does not list both codes:\n%s", refusal)
	}
	if list := yamlBlock(doc, "  /v1/me/watchlists/{listId}:"); !strings.Contains(list, "name: kind") || !strings.Contains(list, "enum: [movie, series]") {
		t.Error("/v1/me/watchlists/{listId} in openapi.yaml has no kind")
	}
	if lists := yamlBlock(doc, "  /v1/me/watchlists:"); !strings.Contains(lists, "name: include") {
		t.Error("/v1/me/watchlists in openapi.yaml has no include")
	}
}

// yamlBlock is the line of doc that is header and the lines under it, up to
// the next line indented no deeper than it; "" where doc has no such line.
func yamlBlock(doc, header string) string {
	lines := strings.Split(doc, "\n")
	depth := len(header) - len(strings.TrimLeft(header, " "))
	for i, l := range lines {
		if l != header {
			continue
		}
		end := i + 1
		for end < len(lines) {
			next := lines[end]
			if strings.TrimSpace(next) != "" && len(next)-len(strings.TrimLeft(next, " ")) <= depth {
				break
			}
			end++
		}
		return strings.Join(lines[i:end], "\n")
	}
	return ""
}
