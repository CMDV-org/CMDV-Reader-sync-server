// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// Package store is the whole of persistence: two tables, an append, and a page.
//
// What is absent is the point. There is no kind, no document identity, no record timestamp, no
// unique constraint beyond the primary keys, and no upsert — so there is nothing here that can hold
// an opinion about what the app's data means, and adding a new kind of synced data to the app
// requires no change to this file.
//
// Entries are immutable and nothing is ever deleted. That is what removes contention: two devices
// asserting different versions of one record both survive, and the app folds them in sequence order.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite, which is what makes CGO_ENABLED=0 and FROM scratch work
)

// Store owns the database.
type Store struct {
	db *sql.DB
}

// Entry is one immutable, opaque record as it is handed back.
type Entry struct {
	Sequence int64
	DeviceID string
	Blob     []byte
}

// Page is one page of entries and where to resume.
//
// A named type rather than three return values: (entries, cursor, hasMore) is exactly the shape
// where a caller silently transposes two results, and the cursor being wrong is the failure that
// loses a reader's highlights.
type Page struct {
	Entries []Entry
	Cursor  int64
	HasMore bool
}

// Device is a device that has contacted the server.
type Device struct {
	DeviceID    string
	DeviceName  string
	FirstSeenAt time.Time
	LastSeenAt  time.Time
}

// MaxLimit is the largest page a client may ask for.
//
// A client asking for a million entries in one response would exhaust a small server's memory, and
// self-hosted servers are usually small. Clamped rather than refused, since a smaller page is still
// a correct answer.
const MaxLimit = 2000

// Open opens or creates the database and brings the schema up to date.
func Open(path string) (*Store, error) {
	// _txlock=immediate so a write transaction takes its lock when it begins rather than on first
	// write, which is what avoids SQLITE_BUSY on upgrade under concurrency.
	db, err := sql.Open("sqlite", "file:"+path+"?_txlock=immediate")
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}

	// One connection, deliberately. This server's traffic is a few short bursts a day, and a single
	// connection makes "the sequence is assigned atomically and is a total order" true by
	// construction rather than by argument about SQLite's locking.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	pragmas := []string{
		// Write-ahead logging: a long read by one device does not block another's write.
		"PRAGMA journal_mode = WAL",
		// NORMAL rather than FULL. A sync lost in the seconds before a power cut costs one
		// exchange, which every device simply sends again; FULL would fsync per entry for a
		// durability guarantee the protocol does not need.
		"PRAGMA synchronous = NORMAL",
		// Wait rather than fail if the file is momentarily locked. With one connection the lock this
		// guards against comes from outside the process — a backup, a volume snapshot — and without a
		// timeout SQLite returns BUSY immediately, surfacing to a reader as a failed sync during a
		// routine backup.
		"PRAGMA busy_timeout = 5000",
		"PRAGMA foreign_keys = ON",
		// Temporary tables and indexes in memory rather than in files. The runtime image is minimal
		// and has no writable temp directory to speak of, so a query that needed one would fail in
		// production and nowhere else.
		"PRAGMA temp_store = MEMORY",
	}
	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			db.Close()
			return nil, fmt.Errorf("%s: %w", p, err)
		}
	}

	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// Ping checks the database is answering. For the health check and for startup.
//
// It reads from a real table rather than running `SELECT 1`, and the difference is the whole point.
// `SELECT 1` is a constant expression: SQLite answers it without opening a b-tree or reading a page,
// so it proves the driver replies and a connection was obtainable — worth something, since with a
// single connection an obtainable one means the server is not wedged behind an in-flight request —
// but it says nothing about whether the storage underneath can still be read. On a replicated or
// network volume, storage is exactly what fails.
//
// So this reads the highest sequence, which walks the primary key index and touches at least the
// b-tree root. Still O(1) and still sub-millisecond, but it goes through the pager.
//
// Honest about its limits: a root page already in SQLite's cache is answered from memory, so this is
// not proof that a disk read would succeed. It is strictly more evidence than a constant, at the same
// cost, and combined with a bounded context it turns a stalled or lock-starved database into a
// failed probe rather than a hanging one.
//
// An empty database returns no rows, which is health rather than failure.
func (s *Store) Ping(ctx context.Context) error {
	var sequence sql.NullInt64
	err := s.db.QueryRowContext(ctx, "SELECT sequence FROM entries ORDER BY sequence DESC LIMIT 1").
		Scan(&sequence)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	return err
}

// Backup writes a consistent copy of the database to a new file.
//
// `VACUUM INTO` rather than copying the file, and that distinction is the whole reason this method
// exists. The database runs in WAL mode, so at any moment the committed state is spread across the
// database file and its write-ahead log — a plain `cp` can capture a torn combination that looks fine
// and restores broken. `VACUUM INTO` goes through SQLite, which knows how to produce a single file
// that is consistent as of one point in time, and it is safe to run against a server that is serving.
//
// Built into the binary rather than documented as a `sqlite3` incantation because the runtime image is
// minimal and has no shell and no `sqlite3` in it. A backup procedure an operator cannot actually run
// is not a backup procedure.
//
// The destination must not already exist; SQLite refuses to overwrite, which is the behaviour worth
// having when the argument is a path somebody typed.
func (s *Store) Backup(ctx context.Context, destination string) error {
	if _, err := os.Stat(destination); err == nil {
		return fmt.Errorf("%s already exists; refusing to overwrite it", destination)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("checking %s: %w", destination, err)
	}

	// A bound parameter is not permitted in VACUUM INTO, so the path is quoted as a SQL string
	// literal by doubling any single quotes. This is the only place in the package where a value
	// reaches SQL outside a bound parameter, and the value is an operator's command-line argument
	// rather than anything a client can influence.
	quoted := "'" + strings.ReplaceAll(destination, "'", "''") + "'"
	if _, err := s.db.ExecContext(ctx, "VACUUM INTO "+quoted); err != nil {
		return fmt.Errorf("backing up to %s: %w", destination, err)
	}
	return nil
}

// Append stores blobs as immutable entries, in the order given.
//
// One transaction for the whole batch. Without it, a partly-applied push would leave some entries
// visible below the cursor the client is about to be given, and the rest would never be delivered.
//
// Returns the sequence assigned to the last entry, or 0 when there were none.
func (s *Store) Append(ctx context.Context, dataset, deviceID string, blobs [][]byte) (int64, error) {
	if len(blobs) == 0 {
		return 0, nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("beginning append: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once committed; rolls back on any early return

	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO entries (dataset, device_id, blob) VALUES (?, ?, ?)`)
	if err != nil {
		return 0, fmt.Errorf("preparing append: %w", err)
	}
	defer stmt.Close()

	var last int64
	for _, blob := range blobs {
		res, err := stmt.ExecContext(ctx, dataset, deviceID, blob)
		if err != nil {
			return 0, fmt.Errorf("appending entry: %w", err)
		}
		if last, err = res.LastInsertId(); err != nil {
			return 0, fmt.Errorf("reading assigned sequence: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("committing append: %w", err)
	}
	return last, nil
}

// Page returns entries above a cursor, oldest first, excluding those the asking device wrote.
//
// A device's own entries are omitted because it already has them, and returning them would make
// every sync echo.
func (s *Store) Page(ctx context.Context, dataset, excludeDeviceID string, after int64, limit int) (Page, error) {
	if limit < 1 {
		limit = 1
	}
	if limit > MaxLimit {
		limit = MaxLimit
	}

	// One more row than asked for, so "is there more" comes from the same query rather than a second
	// count that could disagree with it.
	rows, err := s.db.QueryContext(ctx,
		`SELECT sequence, device_id, blob
		   FROM entries
		  WHERE dataset = ? AND sequence > ? AND device_id != ?
		  ORDER BY sequence ASC
		  LIMIT ?`,
		dataset, after, excludeDeviceID, limit+1)
	if err != nil {
		return Page{}, fmt.Errorf("reading entries: %w", err)
	}
	defer rows.Close()

	page := Page{Entries: make([]Entry, 0, limit), Cursor: after}
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.Sequence, &e.DeviceID, &e.Blob); err != nil {
			return Page{}, fmt.Errorf("scanning entry: %w", err)
		}
		page.Entries = append(page.Entries, e)
	}
	if err := rows.Err(); err != nil {
		return Page{}, fmt.Errorf("reading entries: %w", err)
	}

	if len(page.Entries) > limit {
		page.HasMore = true
		page.Entries = page.Entries[:limit]
	}

	if n := len(page.Entries); n > 0 {
		page.Cursor = page.Entries[n-1].Sequence
		return page, nil
	}

	// An empty page means everything above the cursor in this dataset came from this device. The
	// cursor still has to advance past those rows, or the device asks for them forever. Advancing to
	// the dataset's high-water mark is correct: it has now been offered everything up to it.
	high, err := s.highWater(ctx, dataset)
	if err != nil {
		return Page{}, err
	}
	if high > page.Cursor {
		page.Cursor = high
	}
	return page, nil
}

// highWater is the largest sequence in a dataset, or 0 when it is empty.
func (s *Store) highWater(ctx context.Context, dataset string) (int64, error) {
	var high sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT MAX(sequence) FROM entries WHERE dataset = ?`, dataset).Scan(&high)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("reading high-water sequence: %w", err)
	}
	if !high.Valid {
		return 0, nil
	}
	return high.Int64, nil
}

// TouchDevice records that a device exists and has just been seen.
//
// There is no registration step, so this *is* registration: a device becomes known by presenting a
// valid token. The name is updated on every contact, so renaming a device in the app is reflected
// without any separate call.
func (s *Store) TouchDevice(ctx context.Context, dataset, deviceID, name string, at time.Time) error {
	seconds := float64(at.UnixNano()) / float64(time.Second)
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO devices (dataset, device_id, name, first_seen_at, last_seen_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(dataset, device_id) DO UPDATE SET
		     name = excluded.name,
		     last_seen_at = excluded.last_seen_at`,
		dataset, deviceID, name, seconds, seconds)
	if err != nil {
		return fmt.Errorf("recording device: %w", err)
	}
	return nil
}

// Devices lists the devices in a dataset, oldest first.
func (s *Store) Devices(ctx context.Context, dataset string) ([]Device, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT device_id, name, first_seen_at, last_seen_at
		   FROM devices WHERE dataset = ? ORDER BY first_seen_at ASC, device_id ASC`, dataset)
	if err != nil {
		return nil, fmt.Errorf("reading devices: %w", err)
	}
	defer rows.Close()

	devices := make([]Device, 0, 4)
	for rows.Next() {
		var d Device
		var first, last float64
		if err := rows.Scan(&d.DeviceID, &d.DeviceName, &first, &last); err != nil {
			return nil, fmt.Errorf("scanning device: %w", err)
		}
		d.FirstSeenAt = fromSeconds(first)
		d.LastSeenAt = fromSeconds(last)
		devices = append(devices, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading devices: %w", err)
	}
	return devices, nil
}

// RemoveDevice removes a device's listing.
//
// Its entries are deliberately left alone. Other devices may not have replayed them, and the
// history is legitimate regardless of whether the device that wrote it still exists — a reinstalled
// device has a new identity and a zero cursor, and needs every other device's entries.
//
// Returns whether a listing was removed, so removing something that does not exist can be reported
// as not-found rather than silently succeeding.
func (s *Store) RemoveDevice(ctx context.Context, dataset, deviceID string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM devices WHERE dataset = ? AND device_id = ?`, dataset, deviceID)
	if err != nil {
		return false, fmt.Errorf("removing device: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("removing device: %w", err)
	}
	return n > 0, nil
}

func fromSeconds(s float64) time.Time {
	return time.Unix(0, int64(s*float64(time.Second))).UTC()
}
