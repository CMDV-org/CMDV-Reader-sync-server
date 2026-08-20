// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package api

import (
	"fmt"
	"strings"
	"time"
	"unicode"
)

// ProtocolVersion is the wire format this server speaks.
//
// Sent and checked on every request so a version mismatch is refused rather than misinterpreted. A
// mismatch interpreted optimistically is how a newer client silently corrupts an older server's
// data.
const ProtocolVersion = 2

// VersionHeader carries ProtocolVersion.
const VersionHeader = "X-CMDV-Sync-Version"

// The shapes below are the entire contract between the app and this server. They are also the whole
// of what `spec/PROTOCOL.md` describes, and `spec/fixtures/` pins them byte for byte.
//
// Every field is either transport bookkeeping or an opaque identifier. There is no kind, no
// document identity, no record timestamp, and nothing whose meaning depends on the app's data
// model — which is what makes it possible to add a synced data type to the app without touching
// this file.
//
// Field order matters: Go's encoding/json emits struct fields in declaration order, which is what
// makes the fixtures byte-stable and therefore usable as a cross-implementation check.

// SyncRequest pushes entries and pulls whatever came from elsewhere.
//
// An import is this same request with an empty Entries. The server does not know or care that a
// client may call it twice per sync, once to import and once to export — that flow is the app's
// business, and keeping the server ignorant of it means the app can change it without a server
// release.
type SyncRequest struct {
	DeviceID   string         `json:"deviceID"`
	DeviceName string         `json:"deviceName"`
	Cursor     int64          `json:"cursor"`
	Limit      int            `json:"limit"`
	Entries    []InboundEntry `json:"entries"`
}

// InboundEntry is one opaque record being pushed.
type InboundEntry struct {
	// Blob is base64 on the wire. Its contents are none of this server's business: it may be
	// encrypted, it may be compressed, its internal schema may change every release, and none of
	// that is visible or relevant here.
	Blob []byte `json:"blob"`
}

// SyncResponse is what the caller did not already have.
type SyncResponse struct {
	Entries []OutboundEntry `json:"entries"`

	// Cursor is where to resume. Opaque to the client, which stores it and sends it back.
	//
	// Advanced past everything in Entries — and past the caller's *own* entries too, even though
	// those are never returned, or the caller would ask for them forever.
	Cursor int64 `json:"cursor"`

	// HasMore says whether anything remains beyond Limit, so a client knows to go again rather than
	// waiting for the next sync to deliver the rest.
	HasMore bool `json:"hasMore"`
}

// OutboundEntry is one opaque record being delivered.
type OutboundEntry struct {
	// Sequence is the server's arrival order, and the only ordering in the system. Every device
	// replays in this order and therefore reaches the same state.
	Sequence int64 `json:"sequence"`

	// DeviceID is which device wrote it. Needed so a client can tell where a change came from, and
	// so the app can bind a payload's authenticated encryption to its author.
	DeviceID string `json:"deviceID"`

	Blob []byte `json:"blob"`
}

// DeviceInfo is one device with access to a dataset.
type DeviceInfo struct {
	DeviceID    string `json:"deviceID"`
	DeviceName  string `json:"deviceName"`
	FirstSeenAt string `json:"firstSeenAt"`
	LastSeenAt  string `json:"lastSeenAt"`
}

// HealthResponse is what a monitor sees.
//
// Unauthenticated on purpose: this is what someone points a monitor at, and what an app can use to
// say "the address is right but your token is not" rather than "cannot connect".
type HealthResponse struct {
	Status  string `json:"status"`
	Version int    `json:"version"`
}

// HealthzResponse is what an operational probe sees at /healthz.
//
// Deliberately not HealthResponse. This is a different endpoint for a different audience: /healthz is
// for whatever watches the process — blackbox_exporter, a Kubernetes probe, a Docker HEALTHCHECK —
// and its body is the smallest thing such a tool can match on. `{"ok":true}` is 11 bytes and a
// regex-matchable literal.
//
// It carries no version, deliberately. A monitor must not have to be updated when the sync protocol
// moves, and an alert must not fire because it was.
type HealthzResponse struct {
	OK bool `json:"ok"`
}

// HealthzTimeout bounds the database touch behind /healthz.
//
// The touch is what makes the probe worth anything: without it, /healthz proves a process is running
// and accepting connections, which is the least interesting thing that could be true. With it, the
// probe answers "can this server actually serve", and a wedged or lock-starved database — the failure
// that matters on replicated storage — shows up as a 500 rather than as a green check and a queue of
// failing syncs.
//
// Bounded because a probe that hangs is worse than one that lies: the monitor's own timeout fires,
// the check goes red with no reason attached, and on some orchestrators the container is killed. Two
// seconds is far longer than any query here takes — page reads are capped at 2000 rows and appends at
// 2000 entries, both milliseconds — and shorter than the store's five-second busy timeout, so a
// database held by an external lock reports unhealthy rather than waiting on SQLite to give up.
const HealthzTimeout = 2 * time.Second

// ErrorResponse is a failure the server chose to describe.
//
// A machine-readable reason alongside the prose, because a client's response to "your token is not
// valid" is to ask the reader to sign in again and to "that request was malformed" is not, and
// neither can be told from an HTTP status alone.
type ErrorResponse struct {
	Reason  Reason `json:"reason"`
	Message string `json:"message"`
}

// Reason is the closed set of machine-readable failure reasons.
type Reason string

const (
	ReasonInvalidCredentials Reason = "invalidCredentials"
	ReasonVersionUnsupported Reason = "versionUnsupported"
	ReasonMalformedRequest   Reason = "malformedRequest"
	ReasonServerError        Reason = "serverError"
)

// Timestamp formats an instant for the wire.
//
// ISO 8601 in UTC with milliseconds, written down rather than left to a default because the wire
// format is a contract with implementations that are not this one. Milliseconds because that is the
// resolution the app's own format uses.
func Timestamp(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

// Limits on what a request may contain.
//
// Every one of these exists so that a malicious or broken client cannot exhaust a small server's
// memory. Self-hosted servers are usually small.
const (
	// MaxBodyBytes is the largest request body accepted. Several times the largest plausible sync
	// page, and small enough that streaming an endless body at the server achieves nothing.
	MaxBodyBytes = 16 << 20

	// MaxBlobBytes bounds one entry. A single synced record is a few hundred bytes in practice; a
	// megabyte is generous enough to never be hit by a correct client.
	MaxBlobBytes = 1 << 20

	// MaxEntriesPerRequest bounds a batch, independently of the body limit, so a request of a
	// million empty entries is refused before anything is allocated per entry.
	MaxEntriesPerRequest = 2000

	// MaxIdentifierBytes bounds a device identifier and name. The app uses UUIDs and human-chosen
	// names; the server only requires that they be short and printable, so that it stays free of any
	// assumption about their format.
	MaxIdentifierBytes = 128

	// DefaultLimit is the page size when a client does not ask for one. Large enough that an
	// ordinary sync is one round trip, small enough that a first sync of a long history stays within
	// a few megabytes per response.
	DefaultLimit = 500
)

// Validate checks a request is well formed, and says precisely what is wrong when it is not.
//
// Deliberately shallow. It bounds sizes and rejects unprintable identifiers, and it does not look
// inside a blob or form any view about what the request means.
func (r *SyncRequest) Validate() error {
	if err := identifier("deviceID", r.DeviceID); err != nil {
		return err
	}
	if err := identifier("deviceName", r.DeviceName); err != nil {
		return err
	}
	if r.Cursor < 0 {
		return fmt.Errorf("cursor must not be negative, but was %d", r.Cursor)
	}
	if len(r.Entries) > MaxEntriesPerRequest {
		return fmt.Errorf("a request may carry at most %d entries, but carried %d", MaxEntriesPerRequest, len(r.Entries))
	}
	for i, e := range r.Entries {
		if len(e.Blob) == 0 {
			return fmt.Errorf("entry %d has an empty blob", i)
		}
		if len(e.Blob) > MaxBlobBytes {
			return fmt.Errorf("entry %d is %d bytes, above the %d-byte limit for one entry", i, len(e.Blob), MaxBlobBytes)
		}
	}
	return nil
}

// EffectiveLimit is the page size to use, clamped rather than refused.
//
// A smaller page than asked for is still a correct answer, and refusing an absurd number would fail
// a sync over something the server can simply decide for itself.
func (r *SyncRequest) EffectiveLimit() int {
	if r.Limit <= 0 {
		return DefaultLimit
	}
	return r.Limit
}

// Blobs is the entries' payloads, in the order given.
func (r *SyncRequest) Blobs() [][]byte {
	if len(r.Entries) == 0 {
		return nil
	}
	blobs := make([][]byte, 0, len(r.Entries))
	for _, e := range r.Entries {
		blobs = append(blobs, e.Blob)
	}
	return blobs
}

func identifier(field, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s is required", field)
	}
	if len(value) > MaxIdentifierBytes {
		return fmt.Errorf("%s is %d bytes, above the %d-byte limit", field, len(value), MaxIdentifierBytes)
	}
	for _, r := range value {
		// Control characters in an identifier are either a bug or an attempt to forge a log line.
		if unicode.IsControl(r) {
			return fmt.Errorf("%s contains a control character", field)
		}
	}
	return nil
}
