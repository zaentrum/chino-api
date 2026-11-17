package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Once an account's deletion has begun, nothing more is kept for it. The
// deletion (DeleteUserData) takes the account's lock exclusive and writes the
// account's id to deleted_accounts (migration 007), both in its own
// transaction. Every write of a person's runs in asAccount (one that adds a
// row, removes one, renames a list, or changes no setting), which takes the
// same lock shared and then reads deleted_accounts:
//
//   - a write under way when the deletion begins ends first, and the deletion
//     takes its rows with the rest;
//   - a write that comes while the deletion is open waits for its end: after
//     a commit it is refused (ErrAccountDeleted) and adds nothing, after a
//     rollback it is written;
//   - a write after the deletion, with a bearer still valid, is refused;
//   - a write that waits longer than accountLockWait gives up
//     (ErrAccountBusy), so that a deletion held open does not hold the
//     pool's connections from every other account.
//
// A deletion rolled back leaves no id behind, and locks nobody out. An
// admin's deletion is committed before portal-api deletes the account
// itself; where that later step fails, the person still has their account,
// and portal-api has the id forgotten (ForgetDeletion), naming the deletion by
// its value: an id another deletion has kept since, the person's own among
// them, stays. Reads run without the lock. A bug report and a stream token
// are refused alike (CheckAccount).

// ErrAccountDeleted refuses a write for an account whose deletion has begun:
// one under way, or one done less than DeletedAccountKept ago.
var ErrAccountDeleted = errors.New("account deleted")

// ErrAccountBusy: a write waited accountLockWait for its account's lock,
// which a deletion under way holds, and gave up; nothing is written, and the
// write may be sent again.
var ErrAccountBusy = errors.New("account busy")

// accountLockWait is how long a write waits for its account's lock: a few
// seconds. A deletion holds the lock while portal-api deletes the account,
// which portal-api is given up to 30 s for; each write waiting for it holds
// one of the pool's connections the while, which every other account's
// reads and writes need too. It bounds that wait alone: once the lock is
// held, lock_timeout is 0 again, and a wait on a row another write of the
// person's holds is as long as that write.
const accountLockWait = 3 * time.Second

// lockWait is accountLockWait as lock_timeout takes it, in milliseconds.
var lockWait = strconv.FormatInt(accountLockWait.Milliseconds(), 10)

// ErrDeletionNotKept: ForgetDeletion found no id of that deletion to remove.
// It was forgotten already or removed past its hour, or another deletion of
// the account has kept its own id since; that one stays.
var ErrDeletionNotKept = errors.New("deletion not kept")

// DeletedAccountKept is how long the id of a deleted account is kept, to
// refuse what a device still signed in to it sends: an hour. It outlives
// every token a row can be written with: a sign-in's access token lives
// 300 s, 900 s where an app signs in by the implicit flow, and none is issued
// for an account that is gone; chino-api's stream token lives 6 h but writes
// no row (it is taken by the media routes, the catalog's events and the
// players' telemetry alone). After the hour the id is ignored and removed,
// and a new sign-in of it is a new account.
const DeletedAccountKept = time.Hour

// keptSeconds is DeletedAccountKept in seconds, as the statements take it.
var keptSeconds = int64(DeletedAccountKept / time.Second)

// accountLock is the key of userID's account lock, a Postgres advisory lock
// of one 64-bit key: the first 8 bytes of the SHA-256 of "chino-api account",
// a zero byte and the id, as a signed integer. The deletion and
// ForgetDeletion take it exclusive (pg_advisory_xact_lock), a write shared
// (pg_advisory_xact_lock_shared), each until its transaction ends. Two ids
// whose keys are the same only wait for each other's deletion.
//
// The key space is this service's own. An advisory lock is a database's,
// and the platform gives chino-api a database of its own (the operator's
// chart names it databases.chino, apart from the catalog's, the portal's and
// the identity provider's); nothing else in chino-api's database takes
// advisory locks: no migration does, and no other package of chino-api.
// The prefix "chino-api account\x00" names the kind of lock in its key: a
// lock of another kind would hash a prefix of its own.
func accountLock(userID string) int64 {
	sum := sha256.Sum256([]byte("chino-api account\x00" + userID))
	return int64(binary.BigEndian.Uint64(sum[:8]))
}

// sweepDeleted removes the ids kept longer than $1 seconds, passing over a
// row another transaction holds: it never waits.
const sweepDeleted = `DELETE FROM deleted_accounts
	WHERE user_id IN (
	  SELECT user_id FROM deleted_accounts
	  WHERE deleted_at <= now() - $1::bigint * interval '1 second'
	  FOR UPDATE SKIP LOCKED)`

// checkDeleted is whether the deletion of the account $2 began less than $1
// seconds ago, the older ids removed in the same statement.
const checkDeleted = `WITH swept AS (` + sweepDeleted + `)
	SELECT EXISTS (
	  SELECT 1 FROM deleted_accounts
	  WHERE user_id = $2 AND deleted_at > now() - $1::bigint * interval '1 second')`

// querier is what a statement runs on: the pool, or a transaction.
type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// asAccount runs write for userID in a transaction of its own, under the
// account's lock held shared, unless the account's deletion has begun:
// ErrAccountDeleted then, and nothing is written. The lock waits for a
// deletion under way to end, up to accountLockWait (ErrAccountBusy past
// it), and deleted_accounts is read in a statement after it, which sees what
// that deletion committed: at read committed, each statement reads what was
// committed before it began.
func (s *Store) asAccount(ctx context.Context, userID string, write func(pgx.Tx) error) error {
	tx, err := s.p.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }() // after a commit, nothing to roll back
	if err := waitShort(ctx, tx, `SELECT pg_advisory_xact_lock_shared($1::bigint)`, accountLock(userID)); err != nil {
		return err
	}
	var deleted bool
	if err := tx.QueryRow(ctx, checkDeleted, keptSeconds, userID).Scan(&deleted); err != nil {
		return err
	}
	if deleted {
		return ErrAccountDeleted
	}
	if err := write(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// CheckAccount is nil where userID's account may still be written for, and
// ErrAccountDeleted where its deletion has begun, as a write would find it
// (asAccount: it waits for a deletion under way, ErrAccountBusy past
// accountLockWait), for what keeps none of the person's rows here: a bug
// report, a stream token. Nil with no database.
func (s *Store) CheckAccount(ctx context.Context, userID string) error {
	if s == nil || s.p == nil || userID == "" {
		return nil
	}
	return s.asAccount(ctx, userID, func(pgx.Tx) error { return nil })
}

// waitShort runs the lock statement sql in tx waiting at most
// accountLockWait (ErrAccountBusy past it), and lock_timeout is 0 again
// once the lock is held.
func waitShort(ctx context.Context, tx pgx.Tx, sql string, args ...any) error {
	if _, err := tx.Exec(ctx, `SELECT set_config('lock_timeout', $1, true)`, lockWait); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, sql, args...); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "55P03" { // lock_not_available
			return ErrAccountBusy
		}
		return err
	}
	_, err := tx.Exec(ctx, `SELECT set_config('lock_timeout', '0', true)`)
	return err
}

// lockAccount takes userID's account lock exclusive in tx: a write under way
// ends first, and one that comes waits for tx to end.
func lockAccount(ctx context.Context, tx pgx.Tx, userID string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1::bigint)`, accountLock(userID))
	return err
}

// newDeletion is a deletion's own value: 16 random bytes, as hex. Nobody can
// make another deletion's from what they know of it.
func newDeletion() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// markDeleted begins the deletion of userID's account in tx: it takes the
// account's lock exclusive (lockAccount); removes the ids kept longer than
// DeletedAccountKept; and writes userID's with the deletion's value, or makes
// it now's and this deletion's where it is there already.
func markDeleted(ctx context.Context, tx pgx.Tx, userID, deletion string) error {
	if err := lockAccount(ctx, tx, userID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, sweepDeleted, keptSeconds); err != nil {
		return err
	}
	_, err := tx.Exec(ctx,
		`INSERT INTO deleted_accounts (user_id, deleted_at, deletion) VALUES ($1, now(), $2)
		 ON CONFLICT (user_id) DO UPDATE
		   SET deleted_at = EXCLUDED.deleted_at, deletion = EXCLUDED.deletion`,
		userID, deletion)
	return err
}

// ForgetDeletion stops keeping the id of userID's deletion whose value is
// deletion (Deletion.ID), so that their writes go in again at once: for an
// admin's deletion whose later step, the deletion of the account itself by
// portal-api, failed after the data was deleted, so that the person still
// has their account. It takes the account's lock exclusive, as a deletion
// does: a deletion under way ends first, waited for as a write waits
// (ErrAccountBusy past accountLockWait). Where the id kept is another
// deletion's, is past its hour (swept or not), or none is kept, it is
// ErrDeletionNotKept, and nothing changes. The rows deleted stay deleted. A
// nil Store keeps no id.
func (s *Store) ForgetDeletion(ctx context.Context, userID, deletion string) error {
	if userID == "" {
		return errors.New("no subject")
	}
	if s == nil || s.p == nil {
		return nil
	}
	tx, err := s.p.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }() // after a commit, nothing to roll back
	if err := waitShort(ctx, tx, `SELECT pg_advisory_xact_lock($1::bigint)`, accountLock(userID)); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx,
		`DELETE FROM deleted_accounts
		 WHERE user_id = $1 AND deletion = $2 AND deleted_at > now() - $3::bigint * interval '1 second'`,
		userID, deletion, keptSeconds)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrDeletionNotKept
	}
	return tx.Commit(ctx)
}
