package http

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zaentrum/chino-api/internal/config"
	"github.com/zaentrum/chino-api/internal/eventsse"
	"github.com/zaentrum/chino-api/internal/portal"
	"github.com/zaentrum/chino-api/internal/store"
)

// The answer to a write for an account whose deletion has begun.
const accountDeletedAnswer = `{"error":"account_deleted","detail":"the account's deletion has begun: nothing more is kept for it"}` + "\n"

// noValues fails t where the JSON object body has a values key: a code that
// needs no values sends none (ADR-0023, part 4).
func noValues(t *testing.T, what string, body []byte) {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatalf("%s: not a JSON object: %s", what, body)
	}
	if _, has := fields["values"]; has {
		t.Errorf("%s: a code without values sends values: %s", what, body)
	}
}

// A write the store refuses because the account's deletion has begun is
// answered 401 account_deleted, a code without values, as JSON; the lists'
// routes, which answer the store's errors through listError, alike.
func TestAWriteOfADeletedAccountAnswersAccountDeleted(t *testing.T) {
	refused := fmt.Errorf("saving the position: %w", store.ErrAccountDeleted)
	for name, answer := range map[string]func(http.ResponseWriter, *http.Request){
		"storeFailed": func(w http.ResponseWriter, r *http.Request) { storeFailed(w, r, refused) },
		"listError":   func(w http.ResponseWriter, r *http.Request) { listError(w, r, refused) },
	} {
		w := httptest.NewRecorder()
		answer(w, httptest.NewRequest(http.MethodPost, "/api/v1/items/m1/progress", nil))
		if w.Code != http.StatusUnauthorized || w.Body.String() != accountDeletedAnswer {
			t.Errorf("%s: %d %q, want 401 %q", name, w.Code, w.Body, accountDeletedAnswer)
		}
		if got := w.Header().Get("Content-Type"); got != "application/json" {
			t.Errorf("%s: Content-Type %q", name, got)
		}
		noValues(t, name, w.Body.Bytes())
	}
}

// A write that waited for its account's deletion under way to end and gave
// up is answered 503 "account busy", with Retry-After: it may be sent again.
func TestAWriteThatWaitedTooLongIsAnswered503(t *testing.T) {
	w := httptest.NewRecorder()
	storeFailed(w, httptest.NewRequest(http.MethodPost, "/api/v1/items/m1/progress", nil),
		fmt.Errorf("saving the position: %w", store.ErrAccountBusy))
	if w.Code != http.StatusServiceUnavailable || w.Body.String() != errAccountBusy+"\n" || w.Header().Get("Retry-After") != "5" {
		t.Errorf("%d %q, Retry-After %q; want 503 %q, 5", w.Code, w.Body, w.Header().Get("Retry-After"), errAccountBusy)
	}
}

// A code that needs no values sends no values key: watchlist_by_kind_invalid;
// setting_unknown sends its values. openapi.yaml requires no values of a
// refusal of the settings.
func TestACodeWithoutValuesSendsNoValues(t *testing.T) {
	is := newIssuer(t)
	h := filesRouter(t, is, "http://katalog-api.invalid", nil)
	w := send(h, http.MethodPatch, settingsRoute, bearerWith(t, is, "viewer-1", nil), `{"watchlist_by_kind":"off"}`)
	if w.Code != http.StatusBadRequest || answer(t, w)["error"] != "watchlist_by_kind_invalid" {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	noValues(t, "watchlist_by_kind_invalid", w.Body.Bytes())
	w = send(h, http.MethodPatch, settingsRoute, bearerWith(t, is, "viewer-1", nil), `{"theme":"dark"}`)
	if values, _ := answer(t, w)["values"].(map[string]any); values["name"] != "theme" {
		t.Errorf("setting_unknown: %s", w.Body)
	}

	doc := string(openapi)
	refusal := yamlBlock(doc, "    SettingRefusal:")
	if !strings.Contains(refusal, "      required: [error, detail]\n") {
		t.Errorf("SettingRefusal does not require error and detail alone:\n%s", refusal)
	}
}

// openapi.yaml says account_deleted: its schema, a code without values, and
// the 401 of every route it documents that writes for the person, and of
// the bug reports.
func TestOpenAPISaysAccountDeleted(t *testing.T) {
	doc := string(openapi)
	if s := yamlBlock(doc, "    AccountDeleted:"); !strings.Contains(s, "enum: [account_deleted]") || !strings.Contains(s, "required: [error, detail]") {
		t.Errorf("AccountDeleted in openapi.yaml:\n%s", s)
	}
	if r := yamlBlock(yamlBlock(doc, "  responses:"), "    Unauthorized:"); !strings.Contains(r, `$ref: "#/components/schemas/AccountDeleted"`) {
		t.Errorf("the Unauthorized response in openapi.yaml:\n%s", r)
	}
	const ref = `        "401": { $ref: "#/components/responses/Unauthorized" }`
	const busy = `        "503": { $ref: "#/components/responses/AccountBusy" }`
	if r := yamlBlock(yamlBlock(doc, "  responses:"), "    AccountBusy:"); !strings.Contains(r, "Retry-After:") {
		t.Errorf("the AccountBusy response in openapi.yaml:\n%s", r)
	}
	if op := yamlBlock(yamlBlock(doc, "  /v1/admin/accounts/{sub}/deleted:"), "    delete:"); !strings.Contains(op, busy) {
		t.Error("the undo in openapi.yaml does not answer account busy")
	}
	for path, methods := range map[string][]string{
		"/v1/items/{id}/progress":                   {"post"},
		"/v1/me/settings":                           {"patch"},
		"/v1/me/watchlists":                         {"get", "post"},
		"/v1/me/watchlists/{listId}":                {"patch", "delete"},
		"/v1/me/watchlists/{listId}/items/{itemId}": {"put", "delete"},
		"/v1/feedback":                              {"post"},
		"/v1/me/watchlist":                          {"get"},
		"/v1/me/watchlist/{id}":                     {"put", "delete"},
	} {
		for _, m := range methods {
			op := yamlBlock(yamlBlock(doc, "  "+path+":"), "    "+m+":")
			if !strings.Contains(op, ref) {
				t.Errorf("%s %s in openapi.yaml does not answer account_deleted", m, path)
			}
			if path != "/v1/feedback" && !strings.Contains(op, busy) {
				t.Errorf("%s %s in openapi.yaml does not answer account busy", m, path)
			}
		}
	}
}

// Against Postgres (CHINO_API_TEST_DATABASE_URL): once the person's account
// is deleted, every write of theirs with the bearer still valid answers 401
// account_deleted, the read of the lists, which makes the default one, too,
// and nothing is kept; another person's writes go in as before.
func TestAfterTheDeletionAWriteAnswersAccountDeleted(t *testing.T) {
	st := testStore(t)
	is := newIssuer(t)
	h := deletionRouter(t, is, newFakePortal(t).URL, st)
	gone, other := bearerWith(t, is, "deleted-http", nil), bearerWith(t, is, "other-http", nil)
	if w := send(h, http.MethodPost, "/api/v1/items/m1/progress", gone, `{"position_sec":600,"duration_sec":5400}`); w.Code != http.StatusNoContent {
		t.Fatalf("before the deletion: %d %s", w.Code, w.Body)
	}
	if w := do(h, http.MethodDelete, "/api/v1/me", gone); w.Code != http.StatusOK {
		t.Fatalf("deleted: %d %s", w.Code, w.Body)
	}

	for _, c := range []struct{ method, path, body string }{
		{http.MethodPost, "/api/v1/items/m1/progress", `{"position_sec":900,"duration_sec":5400}`},
		{http.MethodPost, "/api/v1/items/m2/progress", `{"position_sec":60,"duration_sec":5400,"played_ms_ago":1000}`},
		{http.MethodPost, "/api/v1/me/items/m2/watched", ``},
		{http.MethodDelete, "/api/v1/me/items/m2/watched", ``},
		{http.MethodPut, "/api/v1/me/likes/m2", ``},
		{http.MethodDelete, "/api/v1/me/likes/m2", ``},
		{http.MethodPut, "/api/v1/me/watchlist/m2", ``},
		{http.MethodDelete, "/api/v1/me/watchlist/m2", ``},
		{http.MethodGet, "/api/v1/me/watchlist", ``},
		{http.MethodGet, "/api/v1/me/watchlists", ``},
		{http.MethodPost, "/api/v1/me/watchlists", `{"name":"Rainy days"}`},
		{http.MethodPatch, "/api/v1/me/watchlists/wl_any", `{"name":"Later"}`},
		{http.MethodDelete, "/api/v1/me/watchlists/wl_any", ``},
		{http.MethodPut, "/api/v1/me/watchlists/wl_any/items/m2", ``},
		{http.MethodDelete, "/api/v1/me/watchlists/wl_any/items/m2", ``},
		{http.MethodPatch, settingsRoute, `{"watchlist_by_kind":true}`},
		{http.MethodPatch, settingsRoute, `{}`},
		{http.MethodPost, "/api/v1/me/stream-token", ``},
	} {
		w := send(h, c.method, c.path, gone, c.body)
		if w.Code != http.StatusUnauthorized || w.Body.String() != accountDeletedAnswer {
			t.Errorf("%s %s: %d %q, want 401 %q", c.method, c.path, w.Code, w.Body, accountDeletedAnswer)
		}
		if w := send(h, c.method, c.path, other, c.body); w.Code >= 300 && w.Code != http.StatusNotFound {
			t.Errorf("another person's %s %s: %d %s", c.method, c.path, w.Code, w.Body)
		}
	}
	if pos, err := st.GetProgress(context.Background(), "deleted-http", "m1"); err != nil || pos != 0 {
		t.Errorf("the deleted person's position: %d, %v; want none", pos, err)
	}
	if pos, err := st.GetProgress(context.Background(), "other-http", "m1"); err != nil || pos != 900 {
		t.Errorf("another person's position: %d, %v; want 900", pos, err)
	}
	// A bug report: refused for the deleted account before anything else;
	// another person's reaches the check that bug reports are not set up.
	if w := send(h, http.MethodPost, "/api/v1/feedback", gone, ``); w.Code != http.StatusUnauthorized || w.Body.String() != accountDeletedAnswer {
		t.Errorf("a bug report: %d %q, want 401 %q", w.Code, w.Body, accountDeletedAnswer)
	}
	if w := send(h, http.MethodPost, "/api/v1/feedback", other, ``); w.Code != http.StatusServiceUnavailable {
		t.Errorf("another person's bug report: %d %s, want 503", w.Code, w.Body)
	}
	// Reads stay as they are.
	if w := send(h, http.MethodGet, settingsRoute, gone, ``); w.Code != http.StatusOK || w.Body.String() != settingsOff {
		t.Errorf("a read of the settings: %d %q", w.Code, w.Body)
	}
}

// Against Postgres (D1): the person deletes their account, portal-api
// deletes it, and the app goes before the answer. The deletion ends all the
// same: the rows are gone, the id is kept, and the next write with the
// bearer still valid, from another device, is refused.
func TestADeletionWhoseCallerWentEndsAllTheSame(t *testing.T) {
	st := testStore(t)
	is := newIssuer(t)
	fp := newFakePortal(t)
	h := deletionRouter(t, is, fp.URL, st)
	person := bearerWith(t, is, "deleted-caller-went", nil)
	if w := send(h, http.MethodPost, "/api/v1/items/m1/progress", person, `{"position_sec":600,"duration_sec":5400}`); w.Code != http.StatusNoContent {
		t.Fatalf("before the deletion: %d %s", w.Code, w.Body)
	}
	ctx, leave := context.WithCancel(context.Background())
	defer leave()
	fp.mu.Lock()
	fp.hook = leave // the account is deleted, and the app goes before its answer
	fp.mu.Unlock()
	r := httptest.NewRequest(http.MethodDelete, "/api/v1/me", nil).WithContext(ctx)
	for k, v := range person {
		r.Header[k] = v
	}
	h.ServeHTTP(httptest.NewRecorder(), r)
	if calls := fp.take(); len(calls) != 1 {
		t.Fatalf("%d calls to portal-api, want 1", len(calls))
	}
	if pos, err := st.GetProgress(context.Background(), "deleted-caller-went", "m1"); err != nil || pos != 0 {
		t.Errorf("the deleted person's position: %d, %v; want none", pos, err)
	}
	if w := send(h, http.MethodPost, "/api/v1/items/m2/progress", person, `{"position_sec":600,"duration_sec":5400}`); w.Code != http.StatusUnauthorized || w.Body.String() != accountDeletedAnswer {
		t.Errorf("the next write: %d %q, want 401 %q", w.Code, w.Body, accountDeletedAnswer)
	}
}

// deletionRouter is chino-api with OIDC against is, portal-api at portalURL,
// the account deletion token, the store st, and a katalog-api that has no
// title: a write is the title's alone.
func deletionRouter(t *testing.T, is *issuer, portalURL string, st *store.Store) http.Handler {
	t.Helper()
	kat := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(kat.Close)
	h, err := NewRouter(config.Config{OIDCIssuer: is.URL, OIDCAudience: "chino", OIDCEnabled: true,
		KatalogBaseURL: kat.URL, StreamBaseURL: "http://chino-stream.invalid",
		ArtworkBaseURL: "http://katalog-manager.invalid", StreamSigningKey: signingKey,
		PortalBaseURL: portalURL, AccountDeletionToken: deletionToken}, st, eventsse.NewBroker())
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// The answer to the undoing of a deletion of which no id is kept.
const deletionNotKeptAnswer = `{"error":"deletion_not_kept","detail":"no id of this deletion is kept: nothing of its own to undo"}` + "\n"

// adminDeletion is an admin's deletion of the account sub, as portal-api
// asks for it in h: the header it asks with (an admin's bearer and the
// deletion token), and the deletion's value the data's answer gave.
func adminDeletion(t *testing.T, h http.Handler, is *issuer, sub string) (http.Header, string) {
	t.Helper()
	portalAPI := http.Header{"Authorization": {"Bearer " + is.tokenWith(t, "admin-of-"+sub, "chino", realm("zaentrum-admin", "zaentrum-user"))}}
	portalAPI.Set(portal.DeletionHeader, deletionToken)
	w := do(h, http.MethodDelete, "/api/v1/admin/accounts/"+sub+"/data", portalAPI)
	deletion, _ := answer(t, w)["deletion"].(string)
	if w.Code != http.StatusOK || len(deletion) != 32 {
		t.Fatalf("the data: %d %s", w.Code, w.Body)
	}
	return portalAPI, deletion
}

// Against Postgres: an admin's deletion whose later step fails locks nobody
// out. portal-api has the person's data deleted, and then cannot delete
// their account; until it says so, the person's writes are refused, as for
// any deletion begun. Then it has chino-api forget that deletion, by the
// value the data's answer gave, and the person, who still has their
// account, writes again at once; the data deleted stays deleted. Another
// value, or none, or the token missing, forgets nothing.
func TestAFailedAdminDeletionLocksNobodyOut(t *testing.T) {
	st := testStore(t)
	is := newIssuer(t)
	h := deletionRouter(t, is, "http://portal-api.invalid", st)
	person := bearerWith(t, is, "kept-admin", nil)
	save := func(item string) *httptest.ResponseRecorder {
		return send(h, http.MethodPost, "/api/v1/items/"+item+"/progress", person, `{"position_sec":600,"duration_sec":5400}`)
	}
	if w := save("m1"); w.Code != http.StatusNoContent {
		t.Fatalf("before the deletion: %d %s", w.Code, w.Body)
	}
	portalAPI, deletion := adminDeletion(t, h, is, "kept-admin")
	if w := save("m2"); w.Code != http.StatusUnauthorized || w.Body.String() != accountDeletedAnswer {
		t.Errorf("while the account's deletion goes on: %d %q", w.Code, w.Body)
	}

	// The account's own deletion fails at portal-api.
	forget := "/api/v1/admin/accounts/kept-admin/deleted?deletion="
	noToken := portalAPI.Clone()
	noToken.Del(portal.DeletionHeader)
	for name, c := range map[string]struct {
		path   string
		header http.Header
		code   int
		body   string
	}{
		"without the token": {forget + deletion, noToken, http.StatusForbidden, ""},
		"another value":     {forget + "0123456789abcdef0123456789abcdef", portalAPI, http.StatusConflict, deletionNotKeptAnswer},
		"the value, cut":    {forget + deletion[:16], portalAPI, http.StatusConflict, deletionNotKeptAnswer},
		"another account's": {"/api/v1/admin/accounts/other-admin/deleted?deletion=" + deletion, portalAPI, http.StatusConflict, deletionNotKeptAnswer},
		"no value":          {forget, portalAPI, http.StatusBadRequest, ""},
	} {
		w := do(h, http.MethodDelete, c.path, c.header)
		if w.Code != c.code || (c.body != "" && w.Body.String() != c.body) {
			t.Errorf("%s: %d %q, want %d", name, w.Code, w.Body, c.code)
		}
		if c.body != "" {
			noValues(t, name, w.Body.Bytes())
		}
		if w := save("m2"); w.Code != http.StatusUnauthorized {
			t.Errorf("after an undo %s: %d %s", name, w.Code, w.Body)
		}
	}
	if w := do(h, http.MethodDelete, forget+deletion, portalAPI); w.Code != http.StatusNoContent {
		t.Fatalf("forgotten: %d %s", w.Code, w.Body)
	}
	if w := save("m2"); w.Code != http.StatusNoContent {
		t.Errorf("at once after: %d %s", w.Code, w.Body)
	}
	ctx := context.Background()
	if pos, err := st.GetProgress(ctx, "kept-admin", "m2"); err != nil || pos != 600 {
		t.Errorf("the write after: position %d, %v; want 600", pos, err)
	}
	if pos, err := st.GetProgress(ctx, "kept-admin", "m1"); err != nil || pos != 0 {
		t.Errorf("the data deleted: position %d, %v; want none", pos, err)
	}
	for _, w := range []*httptest.ResponseRecorder{
		send(h, http.MethodPatch, settingsRoute, person, `{"watchlist_by_kind":true}`),
		do(h, http.MethodPut, "/api/v1/me/likes/m2", person),
		do(h, http.MethodGet, "/api/v1/me/watchlists", person),
	} {
		if w.Code/100 != 2 {
			t.Errorf("a write after the deletion was forgotten: %d %s", w.Code, w.Body)
		}
	}
	// Forgotten once: nothing of its own to undo again.
	if w := do(h, http.MethodDelete, forget+deletion, portalAPI); w.Code != http.StatusConflict || w.Body.String() != deletionNotKeptAnswer {
		t.Errorf("forgotten again: %d %q", w.Code, w.Body)
	}
}

// Against Postgres: the person deletes their own account between an admin's
// deletion of their data and portal-api's undo of it. The undo names the
// admin's deletion, whose id is no longer the one kept: it answers that
// there is nothing of its own to undo, the id of the person's own deletion
// stays, and a write with their bearer still valid is still refused.
func TestAnUndoAfterThePersonsOwnDeletionUndoesNothing(t *testing.T) {
	st := testStore(t)
	is := newIssuer(t)
	fp := newFakePortal(t)
	h := deletionRouter(t, is, fp.URL, st)
	person := bearerWith(t, is, "own-after-admin", nil)
	save := func() *httptest.ResponseRecorder {
		return send(h, http.MethodPost, "/api/v1/items/m1/progress", person, `{"position_sec":600,"duration_sec":5400}`)
	}
	if w := save(); w.Code != http.StatusNoContent {
		t.Fatalf("before the deletions: %d %s", w.Code, w.Body)
	}
	portalAPI, deletion := adminDeletion(t, h, is, "own-after-admin")
	if w := do(h, http.MethodDelete, "/api/v1/me", person); w.Code != http.StatusOK {
		t.Fatalf("the person's own deletion: %d %s", w.Code, w.Body)
	}
	w := do(h, http.MethodDelete, "/api/v1/admin/accounts/own-after-admin/deleted?deletion="+deletion, portalAPI)
	if w.Code != http.StatusConflict || w.Body.String() != deletionNotKeptAnswer {
		t.Errorf("the admin's undo after the person's own deletion: %d %q, want 409 %q", w.Code, w.Body, deletionNotKeptAnswer)
	}
	noValues(t, "deletion_not_kept", w.Body.Bytes())
	if w := save(); w.Code != http.StatusUnauthorized || w.Body.String() != accountDeletedAnswer {
		t.Errorf("a write after the undo: %d %q, want 401 %q", w.Code, w.Body, accountDeletedAnswer)
	}
	if pos, err := st.GetProgress(context.Background(), "own-after-admin", "m1"); err != nil || pos != 0 {
		t.Errorf("the deleted person's position: %d, %v; want none", pos, err)
	}
}

// Against Postgres (E1): with the database down, a stream token is minted all
// the same: it needs no database, and the apps show no artwork and start no
// playback without one. Only an account whose deletion has begun, or one
// busy with it, is refused.
func TestAStreamTokenIsMintedWithTheDatabaseDown(t *testing.T) {
	st := testStore(t)
	is := newIssuer(t)
	h := deletionRouter(t, is, "http://portal-api.invalid", st)
	st.Close() // every read and write of the store fails from now on
	w := do(h, http.MethodPost, "/api/v1/me/stream-token", bearerWith(t, is, "viewer-db-down", nil))
	if token, _ := answer(t, w)["stream_token"].(string); w.Code != http.StatusOK || token == "" {
		t.Errorf("%d %s, want 200 and a token", w.Code, w.Body)
	}
}

// Against Postgres (E3): portal-api deletes the account and its answer does
// not come in time; asked once more, it says the account is gone. The
// deletion is committed: the rows are deleted, and the next write is
// refused.
func TestADeletionWhoseAnswerDidNotComeIsAskedOnceMore(t *testing.T) {
	prev := deletionAsk
	deletionAsk = 300 * time.Millisecond
	t.Cleanup(func() { deletionAsk = prev })
	st := testStore(t)
	is := newIssuer(t)
	var mu sync.Mutex
	asked := 0
	pa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked++
		n := asked
		mu.Unlock()
		if n == 1 {
			// The account is deleted; the answer does not come before
			// chino-api stops waiting for it.
			<-r.Context().Done()
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"account":"gone"}`))
	}))
	t.Cleanup(pa.Close)
	h := deletionRouter(t, is, pa.URL, st)
	person := bearerWith(t, is, "deleted-asked-twice", nil)
	if w := send(h, http.MethodPost, "/api/v1/items/m1/progress", person, `{"position_sec":600,"duration_sec":5400}`); w.Code != http.StatusNoContent {
		t.Fatalf("before the deletion: %d %s", w.Code, w.Body)
	}
	w := do(h, http.MethodDelete, "/api/v1/me", person)
	if w.Code != http.StatusOK || answer(t, w)["account"] != "gone" {
		t.Errorf("the deletion: %d %s, want 200 gone", w.Code, w.Body)
	}
	mu.Lock()
	if asked != 2 {
		t.Errorf("portal-api asked %d times, want 2", asked)
	}
	mu.Unlock()
	if pos, err := st.GetProgress(context.Background(), "deleted-asked-twice", "m1"); err != nil || pos != 0 {
		t.Errorf("the deleted person's position: %d, %v; want none", pos, err)
	}
	if w := send(h, http.MethodPost, "/api/v1/items/m2/progress", person, `{"position_sec":600,"duration_sec":5400}`); w.Code != http.StatusUnauthorized || w.Body.String() != accountDeletedAnswer {
		t.Errorf("the next write: %d %q, want 401 %q", w.Code, w.Body, accountDeletedAnswer)
	}
}

// Against Postgres (G1): a deleted account gets no stream token by closing
// its side of the connection, which ends the request's context and the
// check with it: over a real connection, the request sent and its sending
// half closed, the answer is never a token.
func TestNoStreamTokenForACallerThatClosesItsSide(t *testing.T) {
	st := testStore(t)
	is := newIssuer(t)
	srv := httptest.NewServer(deletionRouter(t, is, newFakePortal(t).URL, st))
	t.Cleanup(srv.Close)
	bearer := is.tokenWith(t, "deleted-half-closed", "chino", nil)
	if w := do(srv.Config.Handler, http.MethodDelete, "/api/v1/me", http.Header{"Authorization": {"Bearer " + bearer}}); w.Code != http.StatusOK {
		t.Fatalf("the deletion: %d %s", w.Code, w.Body)
	}
	for try := range 10 {
		conn, err := net.Dial("tcp", srv.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		_, err = fmt.Fprintf(conn, "POST /api/v1/me/stream-token HTTP/1.1\r\nHost: chino\r\nAuthorization: Bearer %s\r\nContent-Length: 0\r\n\r\n", bearer)
		if err != nil {
			t.Fatal(err)
		}
		if err := conn.(*net.TCPConn).CloseWrite(); err != nil {
			t.Fatal(err)
		}
		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK || strings.Contains(string(body), "stream_token") {
				t.Errorf("try %d: %d %s, want no token", try, resp.StatusCode, body)
			}
		}
		conn.Close()
	}
}
