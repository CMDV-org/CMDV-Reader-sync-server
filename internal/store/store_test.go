// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package store

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"
)

const (
	datasetA = "aaaaaaaaaaaaaaaa"
	datasetB = "bbbbbbbbbbbbbbbb"
	phone    = "device-phone"
	tablet   = "device-tablet"
)

func open(t *testing.T) *Store {
	t.Helper()
	// A file rather than :memory:, because :memory: with SetMaxOpenConns(1) is a different code path
	// from what ships and would not exercise WAL at all.
	s, err := Open(filepath.Join(t.TempDir(), "test.sqlite"))
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func append1(t *testing.T, s *Store, dataset, device, blob string) int64 {
	t.Helper()
	seq, err := s.Append(context.Background(), dataset, device, [][]byte{[]byte(blob)})
	if err != nil {
		t.Fatalf("appending: %v", err)
	}
	return seq
}

func page(t *testing.T, s *Store, dataset, exclude string, after int64, limit int) Page {
	t.Helper()
	p, err := s.Page(context.Background(), dataset, exclude, after, limit)
	if err != nil {
		t.Fatalf("paging: %v", err)
	}
	return p
}

// The property everything else rests on: a blob comes back exactly as it went in. The server is
// meant to be unable to understand a payload, and re-encoding one would break a signature or a seal
// without the server ever knowing it had.
func TestBlobIsReturnedByteForByte(t *testing.T) {
	s := open(t)
	// Deliberately not valid UTF-8 and not valid JSON: this is ciphertext as far as the server knows.
	original := []byte{0x00, 0xff, 0x10, 0x80, 0x7f, 0xc3, 0x28, 0x00}

	if _, err := s.Append(context.Background(), datasetA, phone, [][]byte{original}); err != nil {
		t.Fatalf("appending: %v", err)
	}

	got := page(t, s, datasetA, tablet, 0, 10)
	if len(got.Entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(got.Entries))
	}
	if !bytes.Equal(got.Entries[0].Blob, original) {
		t.Errorf("blob was altered:\n want %x\n  got %x", original, got.Entries[0].Blob)
	}
}

func TestEmptyBlobSliceIsNotAnError(t *testing.T) {
	s := open(t)
	seq, err := s.Append(context.Background(), datasetA, phone, nil)
	if err != nil {
		t.Fatalf("appending nothing: %v", err)
	}
	if seq != 0 {
		t.Errorf("want sequence 0 for an empty append, got %d", seq)
	}
}

// Returning a device its own entries would make every sync echo.
func TestOwnEntriesAreNotReturned(t *testing.T) {
	s := open(t)
	append1(t, s, datasetA, phone, "from the phone")

	toPhone := page(t, s, datasetA, phone, 0, 10)
	if len(toPhone.Entries) != 0 {
		t.Errorf("a device was sent its own entry: %+v", toPhone.Entries)
	}

	toTablet := page(t, s, datasetA, tablet, 0, 10)
	if len(toTablet.Entries) != 1 {
		t.Errorf("want the other device to receive it, got %d entries", len(toTablet.Entries))
	}
}

// The subtle one. A device whose pull is empty *because everything above its cursor is its own* must
// still have its cursor advanced, or it asks for those rows on every sync forever.
func TestCursorAdvancesPastOwnEntries(t *testing.T) {
	s := open(t)
	last := append1(t, s, datasetA, phone, "one")
	append1(t, s, datasetA, phone, "two")
	high := append1(t, s, datasetA, phone, "three")
	_ = last

	got := page(t, s, datasetA, phone, 0, 10)
	if len(got.Entries) != 0 {
		t.Fatalf("want an empty page, got %d entries", len(got.Entries))
	}
	if got.Cursor != high {
		t.Errorf("cursor did not advance to the high-water mark: want %d, got %d", high, got.Cursor)
	}
	if got.HasMore {
		t.Error("an empty page reported more to come")
	}
}

// A cursor must never move backwards, whatever the caller sends.
func TestCursorNeverMovesBackwards(t *testing.T) {
	s := open(t)
	append1(t, s, datasetA, phone, "one")

	// A cursor far beyond anything stored, which a client should never send but might after a restore.
	got := page(t, s, datasetA, tablet, 9999, 10)
	if got.Cursor != 9999 {
		t.Errorf("cursor moved backwards: want 9999, got %d", got.Cursor)
	}
	if len(got.Entries) != 0 {
		t.Errorf("want no entries above an absurd cursor, got %d", len(got.Entries))
	}
}

func TestEmptyDatasetPagesCleanly(t *testing.T) {
	s := open(t)
	got := page(t, s, datasetA, phone, 0, 10)
	if len(got.Entries) != 0 || got.Cursor != 0 || got.HasMore {
		t.Errorf("want an empty page at cursor 0, got %+v", got)
	}
}

// Absolute isolation. This is the requirement a hosted offering would depend on, so it is asserted
// from both directions rather than once.
func TestDatasetsAreIsolated(t *testing.T) {
	s := open(t)
	append1(t, s, datasetA, phone, "mine")
	append1(t, s, datasetB, phone, "theirs")

	a := page(t, s, datasetA, tablet, 0, 10)
	if len(a.Entries) != 1 || string(a.Entries[0].Blob) != "mine" {
		t.Errorf("dataset A saw the wrong entries: %+v", a.Entries)
	}

	b := page(t, s, datasetB, tablet, 0, 10)
	if len(b.Entries) != 1 || string(b.Entries[0].Blob) != "theirs" {
		t.Errorf("dataset B saw the wrong entries: %+v", b.Entries)
	}
}

// Two datasets share one sequence counter, which is fine — a read filters by dataset, so the gaps are
// invisible. What must hold is that the ordering stays globally monotonic, so one dataset's entries
// can never be numbered below a cursor the same dataset has already been given.
func TestSequenceIsGloballyMonotonicAcrossDatasets(t *testing.T) {
	s := open(t)
	first := append1(t, s, datasetA, phone, "a1")
	middle := append1(t, s, datasetB, phone, "b1")
	last := append1(t, s, datasetA, phone, "a2")

	if !(first < middle && middle < last) {
		t.Errorf("sequences are not monotonic: %d, %d, %d", first, middle, last)
	}

	a := page(t, s, datasetA, tablet, 0, 10)
	if len(a.Entries) != 2 {
		t.Fatalf("want 2 entries in dataset A, got %d", len(a.Entries))
	}
	if a.Entries[0].Sequence != first || a.Entries[1].Sequence != last {
		t.Errorf("dataset A's sequences are wrong: %d, %d (want %d, %d)",
			a.Entries[0].Sequence, a.Entries[1].Sequence, first, last)
	}
	// The gap where dataset B's entry sits is expected and harmless.
	if a.Cursor != last {
		t.Errorf("want cursor %d, got %d", last, a.Cursor)
	}
}

func TestEntriesComeBackInSequenceOrderOnceEach(t *testing.T) {
	s := open(t)
	for _, blob := range []string{"one", "two", "three", "four"} {
		append1(t, s, datasetA, phone, blob)
	}

	got := page(t, s, datasetA, tablet, 0, 10)
	var order []string
	var previous int64
	for _, e := range got.Entries {
		if e.Sequence <= previous {
			t.Errorf("entries are out of order at sequence %d", e.Sequence)
		}
		previous = e.Sequence
		order = append(order, string(e.Blob))
	}
	want := []string{"one", "two", "three", "four"}
	if len(order) != len(want) {
		t.Fatalf("want %v, got %v", want, order)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("want %v, got %v", want, order)
		}
	}
}

// A first sync of a long history has to arrive in pages, and paging through must deliver everything
// exactly once.
func TestPagingDeliversEverythingOnce(t *testing.T) {
	s := open(t)
	const total = 7
	for i := 0; i < total; i++ {
		append1(t, s, datasetA, phone, string(rune('a'+i)))
	}

	var collected []string
	cursor := int64(0)
	pages := 0
	for {
		got := page(t, s, datasetA, tablet, cursor, 3)
		for _, e := range got.Entries {
			collected = append(collected, string(e.Blob))
		}
		cursor = got.Cursor
		pages++
		if !got.HasMore {
			break
		}
		if pages > 10 {
			t.Fatal("paging did not terminate")
		}
	}

	if pages != 3 {
		t.Errorf("want 3 pages for %d entries at 3 per page, got %d", total, pages)
	}
	if len(collected) != total {
		t.Errorf("want %d entries, got %d: %v", total, len(collected), collected)
	}
	// And a further read at the final cursor is empty.
	if final := page(t, s, datasetA, tablet, cursor, 3); len(final.Entries) != 0 {
		t.Errorf("want nothing after the final cursor, got %d entries", len(final.Entries))
	}
}

func TestLimitIsClampedRatherThanRefused(t *testing.T) {
	s := open(t)
	append1(t, s, datasetA, phone, "one")

	for _, limit := range []int{0, -5, MaxLimit + 1_000_000} {
		got := page(t, s, datasetA, tablet, 0, limit)
		if len(got.Entries) != 1 {
			t.Errorf("limit %d: want 1 entry, got %d", limit, len(got.Entries))
		}
	}
}

// Nothing is ever overwritten. Two devices asserting different versions of the same logical record
// both survive, which is the whole reason the server needs no arbitration.
func TestEntriesAreNeverOverwritten(t *testing.T) {
	s := open(t)
	append1(t, s, datasetA, phone, "phone's version")
	append1(t, s, datasetA, tablet, "tablet's version")

	third := "device-laptop"
	got := page(t, s, datasetA, third, 0, 10)
	if len(got.Entries) != 2 {
		t.Fatalf("want both versions to survive, got %d entries", len(got.Entries))
	}
}

// MARK: Devices

func TestDeviceIsRecordedAndUpdated(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	first := time.Unix(1_700_000_000, 0).UTC()

	if err := s.TouchDevice(ctx, datasetA, phone, "Marc's phone", first); err != nil {
		t.Fatalf("touching device: %v", err)
	}

	devices, err := s.Devices(ctx, datasetA)
	if err != nil {
		t.Fatalf("listing devices: %v", err)
	}
	if len(devices) != 1 {
		t.Fatalf("want 1 device, got %d", len(devices))
	}
	if devices[0].DeviceName != "Marc's phone" {
		t.Errorf("want the given name, got %q", devices[0].DeviceName)
	}
	if !devices[0].FirstSeenAt.Equal(first) || !devices[0].LastSeenAt.Equal(first) {
		t.Errorf("timestamps are wrong on first contact: %+v", devices[0])
	}

	// A second contact updates last-seen and the name, and leaves first-seen alone — which is what
	// lets a reader tell which device in the list is the one they stopped using.
	later := first.Add(48 * time.Hour)
	if err := s.TouchDevice(ctx, datasetA, phone, "Renamed phone", later); err != nil {
		t.Fatalf("touching device again: %v", err)
	}
	devices, _ = s.Devices(ctx, datasetA)
	if len(devices) != 1 {
		t.Fatalf("a second contact created a second device: %d", len(devices))
	}
	if devices[0].DeviceName != "Renamed phone" {
		t.Errorf("want the updated name, got %q", devices[0].DeviceName)
	}
	if !devices[0].FirstSeenAt.Equal(first) {
		t.Errorf("first-seen changed: want %v, got %v", first, devices[0].FirstSeenAt)
	}
	if !devices[0].LastSeenAt.Equal(later) {
		t.Errorf("last-seen did not advance: want %v, got %v", later, devices[0].LastSeenAt)
	}
}

func TestDevicesAreIsolatedByDataset(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	at := time.Unix(1_700_000_000, 0).UTC()
	_ = s.TouchDevice(ctx, datasetA, phone, "Mine", at)
	_ = s.TouchDevice(ctx, datasetB, tablet, "Theirs", at)

	a, _ := s.Devices(ctx, datasetA)
	if len(a) != 1 || a[0].DeviceName != "Mine" {
		t.Errorf("dataset A's device list leaked: %+v", a)
	}
	b, _ := s.Devices(ctx, datasetB)
	if len(b) != 1 || b[0].DeviceName != "Theirs" {
		t.Errorf("dataset B's device list leaked: %+v", b)
	}
}

// Removing a device removes its access and its listing, not its history. Other devices may not have
// replayed its entries, and a reinstalled device has a new identity and a zero cursor.
func TestRemovingADeviceKeepsItsEntries(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	at := time.Unix(1_700_000_000, 0).UTC()
	_ = s.TouchDevice(ctx, datasetA, phone, "Phone", at)
	append1(t, s, datasetA, phone, "written before removal")

	removed, err := s.RemoveDevice(ctx, datasetA, phone)
	if err != nil {
		t.Fatalf("removing device: %v", err)
	}
	if !removed {
		t.Fatal("want the device to have been removed")
	}

	if devices, _ := s.Devices(ctx, datasetA); len(devices) != 0 {
		t.Errorf("want an empty device list, got %+v", devices)
	}
	got := page(t, s, datasetA, tablet, 0, 10)
	if len(got.Entries) != 1 {
		t.Errorf("a removed device's entries were lost: got %d", len(got.Entries))
	}
}

func TestRemovingAnUnknownDeviceReportsIt(t *testing.T) {
	s := open(t)
	removed, err := s.RemoveDevice(context.Background(), datasetA, "never-existed")
	if err != nil {
		t.Fatalf("removing an unknown device: %v", err)
	}
	if removed {
		t.Error("want removal of an unknown device to report false rather than succeed silently")
	}
}

func TestRemovingADeviceIsScopedToItsDataset(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	at := time.Unix(1_700_000_000, 0).UTC()
	_ = s.TouchDevice(ctx, datasetB, phone, "Theirs", at)

	// The same device identifier, but from the wrong dataset.
	removed, err := s.RemoveDevice(ctx, datasetA, phone)
	if err != nil {
		t.Fatalf("removing device: %v", err)
	}
	if removed {
		t.Error("a device was removed from another dataset")
	}
	if devices, _ := s.Devices(ctx, datasetB); len(devices) != 1 {
		t.Error("the other dataset's device was removed")
	}
}

// MARK: Durability

// The schema is created on every open, so opening an existing database must be safe and must not
// lose anything.
func TestReopeningPreservesEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reopen.sqlite")

	first, err := Open(path)
	if err != nil {
		t.Fatalf("opening: %v", err)
	}
	if _, err := first.Append(context.Background(), datasetA, phone, [][]byte{[]byte("survives")}); err != nil {
		t.Fatalf("appending: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("closing: %v", err)
	}

	second, err := Open(path)
	if err != nil {
		t.Fatalf("reopening: %v", err)
	}
	defer second.Close()

	got := page(t, second, datasetA, tablet, 0, 10)
	if len(got.Entries) != 1 || string(got.Entries[0].Blob) != "survives" {
		t.Errorf("entries did not survive a reopen: %+v", got.Entries)
	}
}

// AUTOINCREMENT rather than a plain integer primary key, so a sequence is never reused. A reused
// number would land below a cursor already handed out, and that entry would be silently never
// delivered — the failure mode that loses a reader's highlight without anything reporting it.
func TestSequencesAreNeverReused(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	highest := append1(t, s, datasetA, phone, "one")
	highest = append1(t, s, datasetA, phone, "two")

	// Delete everything, the way a future retention feature might, then append again.
	if _, err := s.db.ExecContext(ctx, "DELETE FROM entries"); err != nil {
		t.Fatalf("clearing entries: %v", err)
	}
	next := append1(t, s, datasetA, phone, "three")

	if next <= highest {
		t.Errorf("a sequence was reused after a delete: previous high %d, next %d", highest, next)
	}
}

func TestPingReportsAHealthyDatabase(t *testing.T) {
	s := open(t)

	// Empty is healthy. Ping reads the highest sequence, and no rows is an answer rather than a fault —
	// a fresh install must not report itself unhealthy.
	if err := s.Ping(context.Background()); err != nil {
		t.Errorf("ping failed on an empty database: %v", err)
	}

	append1(t, s, datasetA, phone, "something")
	if err := s.Ping(context.Background()); err != nil {
		t.Errorf("ping failed on a populated database: %v", err)
	}
}

// The failure the probe exists to catch. A closed store stands in for a database that cannot be read —
// the wedged or lock-starved case on replicated storage — and Ping has to report it rather than
// succeeding because the process is still alive.
func TestPingFailsWhenTheDatabaseCannotBeRead(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.sqlite"))
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	if err := s.Ping(context.Background()); err != nil {
		t.Fatalf("ping failed while open: %v", err)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("closing: %v", err)
	}
	if err := s.Ping(context.Background()); err == nil {
		t.Error("ping succeeded against a closed database")
	}
}

// A cancelled context must abort the touch rather than run it. This is what bounds the health probe:
// with one connection, a touch can queue behind an in-flight request, and database/sql honours the
// deadline while waiting for a connection as well as while running the query.
func TestPingRespectsACancelledContext(t *testing.T) {
	s := open(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := s.Ping(ctx); err == nil {
		t.Error("ping ignored a cancelled context")
	}
}
