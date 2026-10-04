package store

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// testDatabase names the PostgreSQL the store's tests use, as chino-api's
// other tests of its lists do; skipped without it.
const testDatabase = "CHINO_API_TEST_DATABASE_URL"

// testStore is chino-api's store, migrated, in a schema of its own on the
// database testDatabase names, dropped when the test ends.
func testStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv(testDatabase)
	if dsn == "" {
		t.Skipf("set %s to run the store's tests", testDatabase)
	}
	ctx := context.Background()
	schema := fmt.Sprintf("chino_store_test_%d", time.Now().UnixNano())
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), dsn)
		if err != nil {
			t.Error(err)
			return
		}
		defer c.Close(context.Background())
		if _, err := c.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Error(err)
		}
	})
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	st, err := New(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return st
}

// seedUser gives userID a row in every table chino keeps of a person.
func seedUser(t *testing.T, st *Store, userID string) {
	t.Helper()
	ctx := context.Background()
	for _, item := range []string{"m1", "m2"} {
		if _, err := st.p.Exec(ctx, `INSERT INTO playback_progress (user_id, item_id, position_sec, duration_sec) VALUES ($1, $2, 600, 5400)`, userID, item); err != nil {
			t.Fatal(err)
		}
		if err := st.MarkWatched(ctx, userID, item); err != nil {
			t.Fatal(err)
		}
		if err := st.SetFlag(ctx, LikesTable, userID, item, true); err != nil {
			t.Fatal(err)
		}
		if err := st.SetFlag(ctx, WatchlistTable, userID, item, true); err != nil {
			t.Fatal(err)
		}
	}
	list, err := st.EnsureDefaultList(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.p.Exec(ctx, `INSERT INTO watchlist_items (list_id, item_id) VALUES ($1, 'm1'), ($1, 'm2')`, list); err != nil {
		t.Fatal(err)
	}
}

// rowsOf counts userID's rows in each table.
func rowsOf(t *testing.T, st *Store, userID string) map[string]int {
	t.Helper()
	out := map[string]int{}
	for table, q := range map[string]string{
		"playback_progress": `SELECT count(*) FROM playback_progress WHERE user_id = $1`,
		"watched_history":   `SELECT count(*) FROM watched_history WHERE user_id = $1`,
		"likes":             `SELECT count(*) FROM likes WHERE user_id = $1`,
		"watchlist":         `SELECT count(*) FROM watchlist WHERE user_id = $1`,
		"watchlists":        `SELECT count(*) FROM watchlists WHERE user_id = $1`,
		"watchlist_items":   `SELECT count(*) FROM watchlist_items i JOIN watchlists l ON l.id = i.list_id WHERE l.user_id = $1`,
	} {
		var n int
		if err := st.p.QueryRow(context.Background(), q, userID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		out[table] = n
	}
	return out
}

// A person's data goes, all of it and only theirs, when the deletion is
// committed — and none of it when it is rolled back.
func TestDeleteUserData(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	seedUser(t, st, "kid-1")
	seedUser(t, st, "parent-1")
	before := rowsOf(t, st, "kid-1")

	d, err := st.DeleteUserData(ctx, "kid-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if got := rowsOf(t, st, "kid-1"); !equal(got, before) {
		t.Errorf("rolled back, rows %v, want %v", got, before)
	}

	d, err = st.DeleteUserData(ctx, "kid-1")
	if err != nil {
		t.Fatal(err)
	}
	want := Deleted{Progress: 2, Watched: 2, Watchlists: 1, WatchlistItems: 2, Likes: 2, LegacyWatchlist: 2}
	if d.Deleted != want {
		t.Errorf("counted %+v, want %+v", d.Deleted, want)
	}
	if err := d.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	for table, n := range rowsOf(t, st, "kid-1") {
		if n != 0 {
			t.Errorf("%s keeps %d rows of the deleted person", table, n)
		}
	}
	if got := rowsOf(t, st, "parent-1"); !equal(got, before) {
		t.Errorf("another person's rows: %v, want %v", got, before)
	}

	// Nothing left, nothing to delete: no error.
	d, err = st.DeleteUserData(ctx, "kid-1")
	if err != nil || d.Deleted != (Deleted{}) || d.Commit(ctx) != nil {
		t.Errorf("again: %+v, %v", d, err)
	}
	if _, err := st.DeleteUserData(ctx, ""); err == nil {
		t.Error("a deletion of no subject")
	}
	// No database: nothing to delete, and a commit that does nothing.
	var none *Store
	if d, err := none.DeleteUserData(ctx, "kid-1"); err != nil || d.Commit(ctx) != nil {
		t.Errorf("no store: %v", err)
	}
}

func equal(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
