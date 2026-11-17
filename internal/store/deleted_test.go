package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// The tests below but the first run against Postgres
// (CHINO_API_TEST_DATABASE_URL). An account's lock is the database's,
// whatever the schema: each test's people are its own, so that no other test
// waits on them. Each deletion a test opens is rolled back when it ends
// (rollbackOf), so that a test that fails with one open ends too.

// The id of a deleted account is kept an hour: the figure the comment beside
// DeletedAccountKept gives its reasons for, in the statements too.
func TestTheIDIsKeptAnHour(t *testing.T) {
	if DeletedAccountKept != time.Hour || keptSeconds != 3600 {
		t.Errorf("kept %v (%d s), want an hour", DeletedAccountKept, keptSeconds)
	}
}

// rollbackOf rolls d back, for a test's cleanup: after its commit or
// rollback, it does nothing.
func rollbackOf(d *Deletion) func() {
	return func() { _ = d.Rollback(context.Background()) }
}

// deleting opens the deletion of userID's account, rolled back when t ends
// where it is still open.
func deleting(t *testing.T, st *Store, userID string) *Deletion {
	t.Helper()
	d, err := st.DeleteUserData(context.Background(), userID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rollbackOf(d))
	return d
}

// accountWrite is one of the writes of a person's: of userID, for the title
// item.
type accountWrite struct {
	name  string
	write func(ctx context.Context, st *Store, userID, item string) error
}

// accountWrites are every write of a person's - those that add a row, those
// that remove one or rename a list, and a change of no setting - each after
// the write it undoes, and the default list before the writes to it.
func accountWrites() []accountWrite {
	on := true
	return []accountWrite{
		{"progress", func(ctx context.Context, st *Store, u, item string) error {
			return st.SaveProgress(ctx, u, []string{item}, 600, 5400)
		}},
		{"progress played earlier", func(ctx context.Context, st *Store, u, item string) error {
			_, err := st.SaveProgressPlayed(ctx, u, []string{item}, 700, 5400, time.Millisecond)
			return err
		}},
		{"watched", func(ctx context.Context, st *Store, u, item string) error {
			return st.MarkWatched(ctx, u, item)
		}},
		{"watched earlier", func(ctx context.Context, st *Store, u, item string) error {
			return st.MarkWatchedPlayed(ctx, u, time.Minute, item)
		}},
		{"unwatched", func(ctx context.Context, st *Store, u, item string) error {
			return st.UnmarkWatched(ctx, u, item)
		}},
		{"a like", func(ctx context.Context, st *Store, u, item string) error {
			return st.SetFlag(ctx, LikesTable, u, item, true)
		}},
		{"a like taken back", func(ctx context.Context, st *Store, u, item string) error {
			return st.SetFlag(ctx, LikesTable, u, item, false)
		}},
		{"the legacy watchlist", func(ctx context.Context, st *Store, u, item string) error {
			return st.SetFlag(ctx, WatchlistTable, u, item, true)
		}},
		{"off the legacy watchlist", func(ctx context.Context, st *Store, u, item string) error {
			return st.SetFlag(ctx, WatchlistTable, u, item, false)
		}},
		{"a new list", func(ctx context.Context, st *Store, u, item string) error {
			_, err := st.CreateWatchlist(ctx, u, "Rainy days "+item)
			return err
		}},
		{"the default list", func(ctx context.Context, st *Store, u, _ string) error {
			_, err := st.EnsureDefaultList(ctx, u)
			return err
		}},
		{"the lists, read, which makes the default one", func(ctx context.Context, st *Store, u, _ string) error {
			_, err := st.ListWatchlists(ctx, u)
			return err
		}},
		{"a title saved to the default list", func(ctx context.Context, st *Store, u, item string) error {
			return st.AddWatchlistItem(ctx, u, defaultListID(u), item)
		}},
		{"a title taken off the default list", func(ctx context.Context, st *Store, u, item string) error {
			return st.RemoveWatchlistItem(ctx, u, defaultListID(u), item)
		}},
		{"the default list renamed", func(ctx context.Context, st *Store, u, _ string) error {
			_, err := st.RenameWatchlist(ctx, u, defaultListID(u), defaultListName)
			return err
		}},
		{"the new list deleted", func(ctx context.Context, st *Store, u, item string) error {
			// Its id read as it is, outside the writes; a list that is not
			// there is one of no id.
			id := "wl_none"
			_ = st.p.QueryRow(ctx, `SELECT id FROM watchlists WHERE user_id = $1 AND name = $2`,
				u, "Rainy days "+item).Scan(&id)
			return st.DeleteWatchlist(ctx, u, id)
		}},
		{"the settings", func(ctx context.Context, st *Store, u, _ string) error {
			_, err := st.ChangeSettings(ctx, u, SettingsChange{WatchlistByKind: &on})
			return err
		}},
		{"a change of no setting", func(ctx context.Context, st *Store, u, _ string) error {
			_, err := st.ChangeSettings(ctx, u, SettingsChange{})
			return err
		}},
	}
}

// writeDuring starts w for userID and item, and answers what it returns.
func writeDuring(st *Store, w accountWrite, userID, item string) <-chan error {
	done := make(chan error, 1)
	go func() { done <- w.write(context.Background(), st, userID, item) }()
	return done
}

// blockedBy counts the connections that wait on d's transaction.
func blockedBy(t *testing.T, st *Store, d *Deletion) int {
	t.Helper()
	return blockedByPID(t, st, d.tx.Conn().PgConn().PID())
}

// blockedByPID counts the connections that wait on the backend pid.
func blockedByPID(t *testing.T, st *Store, pid uint32) int {
	t.Helper()
	var n int
	if err := st.p.QueryRow(context.Background(),
		`SELECT count(*) FROM pg_stat_activity WHERE $1::int = ANY (pg_blocking_pids(pid))`,
		int32(pid)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// waitsOn waits until a connection waits on d's transaction - a write sent
// while the deletion is open - and fails t where none does in 10 s, or where
// the write of done has returned already.
func waitsOn(t *testing.T, st *Store, d *Deletion, done <-chan error) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; {
		select {
		case err := <-done:
			t.Fatalf("the write returned while the deletion is open: %v", err)
		default:
		}
		if blockedBy(t, st, d) > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("no write waits on the deletion")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// returned is what the write of done returned; t fails where it has not in
// 10 s.
func returned(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("the write did not return")
		return nil
	}
}

// kept counts userID's rows in every table chino keeps of a person.
func kept(t *testing.T, st *Store, userID string) int {
	t.Helper()
	n := 0
	for _, c := range rowsOf(t, st, userID) {
		n += c
	}
	return n
}

// keptIDs counts the ids of userID in deleted_accounts: 1 while its
// deletion's id is kept, else 0.
func keptIDs(t *testing.T, st *Store, userID string) int {
	t.Helper()
	var n int
	if err := st.p.QueryRow(context.Background(),
		`SELECT count(*) FROM deleted_accounts WHERE user_id = $1`, userID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// (a) A write that finds no row of its own while the person's deletion is
// open - one that would go in at once, beside the deletes - waits for the
// deletion's end, and after its commit is refused and adds nothing: each of
// the writes of a person's.
func TestAWriteWithNoRowDuringTheDeletionIsRefused(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	for i, w := range accountWrites() {
		user := fmt.Sprintf("deleted-a-%d", i)
		d := deleting(t, st, user)
		done := writeDuring(st, w, user, "m9")
		waitsOn(t, st, d, done)
		if err := d.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if err := returned(t, done); !errors.Is(err, ErrAccountDeleted) {
			t.Errorf("%s: %v, want ErrAccountDeleted", w.name, err)
		}
		if n := kept(t, st, user); n != 0 {
			t.Errorf("%s: %d rows of the deleted person kept", w.name, n)
		}
	}
}

// (b) A write of a row the open deletion holds - one of the person's titles,
// lists or settings, a removal or a rename of it too - waits, and after the
// commit is refused and puts nothing back.
func TestAWriteOfARowTheDeletionHoldsIsRefusedAfterTheCommit(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	for i, w := range accountWrites() {
		user := fmt.Sprintf("deleted-b-%d", i)
		seedUser(t, st, user)
		d := deleting(t, st, user)
		done := writeDuring(st, w, user, "m1")
		waitsOn(t, st, d, done)
		if err := d.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if err := returned(t, done); !errors.Is(err, ErrAccountDeleted) {
			t.Errorf("%s: %v, want ErrAccountDeleted", w.name, err)
		}
		if n := kept(t, st, user); n != 0 {
			t.Errorf("%s: %d rows of the deleted person kept", w.name, n)
		}
	}
}

// (c) After the deletion, a write with a bearer still valid is refused at
// once and adds nothing, as are a bug report's and a stream token's check;
// another person's writes go in as before.
func TestAWriteAfterTheDeletionIsRefused(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	seedUser(t, st, "deleted-c")
	if err := deleting(t, st, "deleted-c").Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if n := keptIDs(t, st, "deleted-c"); n != 1 {
		t.Fatalf("%d ids kept of the deleted account, want 1", n)
	}
	for _, w := range accountWrites() {
		if err := w.write(ctx, st, "deleted-c", "m3"); !errors.Is(err, ErrAccountDeleted) {
			t.Errorf("%s: %v, want ErrAccountDeleted", w.name, err)
		}
		if err := w.write(ctx, st, "other-c", "m3"); err != nil {
			t.Errorf("another person's %s: %v", w.name, err)
		}
	}
	if err := st.CheckAccount(ctx, "deleted-c"); !errors.Is(err, ErrAccountDeleted) {
		t.Errorf("the account's check: %v, want ErrAccountDeleted", err)
	}
	if err := st.CheckAccount(ctx, "other-c"); err != nil {
		t.Errorf("another account's check: %v", err)
	}
	if n := kept(t, st, "deleted-c"); n != 0 {
		t.Errorf("%d rows of the deleted person kept", n)
	}
	if n := kept(t, st, "other-c"); n == 0 {
		t.Error("another person's writes kept nothing")
	}
}

// (d) A deletion that fails, or is rolled back as when portal-api refuses or
// does not answer, keeps no id: a write that waited on it is written, and
// the person's writes work after it.
func TestADeletionRolledBackLocksNobodyOut(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	seedUser(t, st, "deleted-d")
	before := kept(t, st, "deleted-d")

	// A step of the deletion fails: its table is not there.
	if _, err := st.p.Exec(ctx, `ALTER TABLE account_settings RENAME TO account_settings_away`); err != nil {
		t.Fatal(err)
	}
	failed, err := st.DeleteUserData(ctx, "deleted-d")
	if failed != nil {
		t.Cleanup(rollbackOf(failed))
	}
	if _, err := st.p.Exec(ctx, `ALTER TABLE account_settings_away RENAME TO account_settings`); err != nil {
		t.Fatal(err)
	}
	if err == nil {
		t.Fatal("a deletion with a step that fails did not fail")
	}
	if n := keptIDs(t, st, "deleted-d"); n != 0 {
		t.Errorf("a failed deletion keeps %d ids", n)
	}
	if n := kept(t, st, "deleted-d"); n != before {
		t.Errorf("a failed deletion: %d rows, want %d", n, before)
	}

	// Rolled back while a write waits on it.
	d := deleting(t, st, "deleted-d")
	done := writeDuring(st, accountWrites()[0], "deleted-d", "m9")
	waitsOn(t, st, d, done)
	if err := d.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := returned(t, done); err != nil {
		t.Errorf("the write that waited on a deletion rolled back: %v", err)
	}
	if n := keptIDs(t, st, "deleted-d"); n != 0 {
		t.Errorf("a deletion rolled back keeps %d ids", n)
	}
	if pos, err := st.GetProgress(ctx, "deleted-d", "m9"); err != nil || pos != 600 {
		t.Errorf("the waiting write: position %d, %v; want 600", pos, err)
	}
	for _, w := range accountWrites() {
		if err := w.write(ctx, st, "deleted-d", "m8"); err != nil {
			t.Errorf("after a deletion rolled back, %s: %v", w.name, err)
		}
	}
	if n := kept(t, st, "deleted-d"); n <= before {
		t.Errorf("after a deletion rolled back: %d rows, want more than %d", n, before)
	}
}

// deletedAgo sets the deletion of userID back to ago before now.
func deletedAgo(t *testing.T, st *Store, userID string, ago time.Duration) {
	t.Helper()
	if _, err := st.p.Exec(context.Background(),
		`UPDATE deleted_accounts SET deleted_at = now() - $2::bigint * interval '1 second' WHERE user_id = $1`,
		userID, int64(ago/time.Second)); err != nil {
		t.Fatal(err)
	}
}

// (e) The id is kept DeletedAccountKept: a minute before its end a write is
// still refused; after it the id writes again, as a new account, and the
// id is removed - another account's past its hour with it.
func TestAfterTheHourTheIDWritesAgain(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	for _, user := range []string{"deleted-e", "deleted-e-other"} {
		seedUser(t, st, user)
		if err := deleting(t, st, user).Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	deletedAgo(t, st, "deleted-e", DeletedAccountKept-time.Minute)
	if err := st.SaveProgress(ctx, "deleted-e", []string{"m1"}, 600, 5400); !errors.Is(err, ErrAccountDeleted) {
		t.Errorf("a minute before the hour's end: %v, want ErrAccountDeleted", err)
	}

	deletedAgo(t, st, "deleted-e", DeletedAccountKept+time.Minute)
	deletedAgo(t, st, "deleted-e-other", DeletedAccountKept+time.Minute)
	for _, w := range accountWrites() {
		if err := w.write(ctx, st, "deleted-e", "m1"); err != nil {
			t.Errorf("after the hour, %s: %v", w.name, err)
		}
	}
	if n := keptIDs(t, st, "deleted-e"); n != 0 {
		t.Errorf("after the hour, %d ids kept of the account", n)
	}
	if n := keptIDs(t, st, "deleted-e-other"); n != 0 {
		t.Errorf("after the hour, %d ids kept of another account", n)
	}
	// A new account: what it writes now, and nothing of before.
	if s, err := st.GetSettings(ctx, "deleted-e"); err != nil || !s.WatchlistByKind {
		t.Errorf("the new account's settings: %+v, %v", s, err)
	}
	if lists, err := st.ListWatchlists(ctx, "deleted-e"); err != nil || len(lists) != 1 || !lists[0].IsDefault {
		t.Errorf("the new account's lists: %+v, %v; want the default alone, the new one deleted", lists, err)
	}
}

// (D5) The writes waiting on an open deletion hold the pool's connections
// only accountLockWait: with a pool of 5 holding the deletion and three of
// the account's writes waiting, another account's read and write are served
// at once; the waiting writes give up with ErrAccountBusy while the deletion
// is still open, and write nothing.
func TestWritesWaitingOnADeletionLeaveThePoolToOthers(t *testing.T) {
	st := testStoreSized(t, 5)
	ctx := context.Background()
	d := deleting(t, st, "deleted-busy")
	var waiting []<-chan error
	for i := range 3 {
		waiting = append(waiting, writeDuring(st, accountWrites()[0], "deleted-busy", fmt.Sprintf("m%d", i)))
	}
	for deadline := time.Now().Add(10 * time.Second); blockedBy(t, st, d) < 3; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%d writes wait on the deletion, want 3", blockedBy(t, st, d))
		}
	}
	start := time.Now()
	served := make(chan error, 1)
	go func() {
		_, err := st.GetSettings(ctx, "other-busy")
		if err == nil {
			err = st.SaveProgress(ctx, "other-busy", []string{"m1"}, 60, 5400)
		}
		served <- err
	}()
	select {
	case err := <-served:
		if err != nil {
			t.Errorf("another account: %v", err)
		}
		if took := time.Since(start); took > time.Second {
			t.Errorf("another account was served after %v", took)
		}
	case <-time.After(accountLockWait):
		t.Fatal("another account was not served while the deletion's writes waited")
	}
	for _, done := range waiting {
		if err := returned(t, done); !errors.Is(err, ErrAccountBusy) {
			t.Errorf("a write waiting on the deletion: %v, want ErrAccountBusy", err)
		}
	}
	if blockedBy(t, st, d) != 0 {
		t.Error("writes still wait on the deletion")
	}
	if err := d.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if n := kept(t, st, "deleted-busy"); n != 0 {
		t.Errorf("%d rows of the deleted person kept", n)
	}
}

// (E6) A deletion is forgotten by its own value alone: not by another
// deletion's, not once a later deletion of the account has kept its own,
// not past its hour (swept or not); behind an open deletion of the account
// the undo waits as a write does, and gives up past accountLockWait.
func TestADeletionIsForgottenByItsValueWithinItsHour(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	first := deleting(t, st, "forget-me")
	if err := first.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.ForgetDeletion(ctx, "forget-me", newDeletion()); !errors.Is(err, ErrDeletionNotKept) {
		t.Errorf("another value: %v, want ErrDeletionNotKept", err)
	}
	second := deleting(t, st, "forget-me")
	if err := second.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.ForgetDeletion(ctx, "forget-me", first.ID); !errors.Is(err, ErrDeletionNotKept) {
		t.Errorf("the value of a deletion another has followed: %v, want ErrDeletionNotKept", err)
	}
	if n := keptIDs(t, st, "forget-me"); n != 1 {
		t.Errorf("%d ids kept after the undos that did not match, want 1", n)
	}

	// Past its hour, not swept yet: not kept, and nothing changes.
	deletedAgo(t, st, "forget-me", DeletedAccountKept+time.Minute)
	if err := st.ForgetDeletion(ctx, "forget-me", second.ID); !errors.Is(err, ErrDeletionNotKept) {
		t.Errorf("past its hour: %v, want ErrDeletionNotKept", err)
	}
	if n := keptIDs(t, st, "forget-me"); n != 1 {
		t.Errorf("past its hour, %d ids, want the one not swept yet", n)
	}

	// Within its hour, by its value: forgotten.
	third := deleting(t, st, "forget-me")
	if err := third.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.ForgetDeletion(ctx, "forget-me", third.ID); err != nil {
		t.Errorf("by its value: %v", err)
	}
	if n := keptIDs(t, st, "forget-me"); n != 0 {
		t.Errorf("forgotten, %d ids kept", n)
	}

	// Behind an open deletion: it waits as a write does, then gives up.
	open := deleting(t, st, "forget-me")
	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- st.ForgetDeletion(context.Background(), "forget-me", open.ID) }()
	select {
	case err := <-done:
		if !errors.Is(err, ErrAccountBusy) {
			t.Errorf("behind an open deletion: %v, want ErrAccountBusy", err)
		}
		if took := time.Since(start); took < accountLockWait-time.Second {
			t.Errorf("gave up after %v, want about %v", took, accountLockWait)
		}
	case <-time.After(accountLockWait + 5*time.Second):
		t.Fatal("the undo waited on the open deletion without a bound")
	}
}

// accountLockWait bounds the wait for the account's lock alone: a write that
// waits longer than it on a row another transaction of the same person
// holds, with no deletion open, ends as that one does, written.
func TestAWriteWaitingOnTheirOwnRowIsNotCutShort(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := st.SaveProgress(ctx, "own-row", []string{"m1"}, 60, 5400); err != nil {
		t.Fatal(err)
	}
	other, err := st.p.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Rollback(context.Background()) })
	if _, err := other.Exec(ctx, `UPDATE playback_progress SET position_sec = 120 WHERE user_id = $1 AND item_id = 'm1'`, "own-row"); err != nil {
		t.Fatal(err)
	}
	done := writeDuring(st, accountWrites()[0], "own-row", "m1")
	pid := other.Conn().PgConn().PID()
	for deadline := time.Now().Add(10 * time.Second); blockedByPID(t, st, pid) == 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the write does not wait on the row")
		}
	}
	select {
	case err := <-done:
		t.Fatalf("the write returned while the row is held: %v", err)
	case <-time.After(accountLockWait + time.Second):
	}
	if err := other.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := returned(t, done); err != nil {
		t.Errorf("the write that waited on its own row: %v, want it written", err)
	}
	if pos, err := st.GetProgress(ctx, "own-row", "m1"); err != nil || pos != 600 {
		t.Errorf("position %d, %v; want 600, the waiting write's", pos, err)
	}
}

// A subject longer than any row's user_id can hold (64) is deleted too: its
// id is kept, and its writes are refused as any other's.
func TestALongSubjectIsDeletedToo(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	for _, n := range []int{65, 255} {
		user := strings.Repeat("s", n)
		if err := deleting(t, st, user).Commit(ctx); err != nil {
			t.Fatalf("a subject of %d characters: %v", n, err)
		}
		if got := keptIDs(t, st, user); got != 1 {
			t.Errorf("a subject of %d characters: %d ids kept, want 1", n, got)
		}
		if err := st.SaveProgress(ctx, user, []string{"m1"}, 600, 5400); !errors.Is(err, ErrAccountDeleted) {
			t.Errorf("a subject of %d characters, a write: %v, want ErrAccountDeleted", n, err)
		}
	}
}
