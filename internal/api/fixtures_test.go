// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package api

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// update regenerates the fixtures rather than checking against them.
//
//	go test ./internal/api -run TestFixtures -update
//
// Only ever run deliberately, and the diff reviewed: a fixture changing is a change to the contract
// with the app, and if it was not intended then the code is wrong rather than the fixture.
var update = flag.Bool("update", false, "rewrite spec/fixtures from the current types")

const fixtureDir = "../../spec/fixtures"

// fixtures is every message shape on the wire, with a representative value for each.
//
// These files are the contract. The app repository holds the same directory and asserts the same
// round trip, so a divergence between the two implementations shows up as a failing test on both
// sides rather than as a reader whose sync silently stops working.
//
// Values are chosen to exercise the encodings that are easy to get wrong rather than to look
// realistic: a blob of non-UTF-8 bytes, a timestamp with a millisecond component, and an empty slice
// that must serialise as [] rather than null.
func fixtures() map[string]any {
	return map[string]any{
		"sync-request-export": SyncRequest{
			DeviceID:   "11111111-1111-1111-1111-111111111111",
			DeviceName: "Marc's phone",
			Cursor:     412,
			Limit:      500,
			Entries: []InboundEntry{
				{Blob: []byte{0x00, 0xff, 0x10, 0x80, 0x7f}},
				{Blob: []byte("plain bytes are fine too")},
			},
		},

		// An import is the same shape with no entries. Present as its own fixture because the
		// distinction is a client-side flow decision this server must remain ignorant of, and because
		// an empty slice must encode as [] and not null.
		"sync-request-import": SyncRequest{
			DeviceID:   "11111111-1111-1111-1111-111111111111",
			DeviceName: "Marc's phone",
			Cursor:     412,
			Limit:      500,
			Entries:    []InboundEntry{},
		},

		"sync-response": SyncResponse{
			Entries: []OutboundEntry{
				{
					Sequence: 413,
					DeviceID: "22222222-2222-2222-2222-222222222222",
					Blob:     []byte{0x00, 0xff, 0x10, 0x80, 0x7f},
				},
			},
			Cursor:  418,
			HasMore: false,
		},

		"sync-response-empty": SyncResponse{
			Entries: []OutboundEntry{},
			Cursor:  418,
			HasMore: false,
		},

		"devices": []DeviceInfo{
			{
				DeviceID:    "11111111-1111-1111-1111-111111111111",
				DeviceName:  "Marc's phone",
				FirstSeenAt: Timestamp(time.Unix(1_700_000_000, 0)),
				LastSeenAt:  Timestamp(time.Unix(1_700_086_400, 125_000_000)),
			},
		},

		"health": HealthResponse{Status: "ok", Version: ProtocolVersion},

		// The operational probe. Its body is pinned because a monitor matches on it as a literal —
		// blackbox_exporter's body regexp, a Kubernetes probe, a shell script with grep — so changing
		// it silently turns somebody's alerting green or red for no reason.
		"healthz":      HealthzResponse{OK: true},
		"healthz-down": HealthzResponse{OK: false},

		"error-invalid-credentials": ErrorResponse{
			Reason:  ReasonInvalidCredentials,
			Message: "That token was refused.",
		},
		"error-version-unsupported": ErrorResponse{
			Reason:  ReasonVersionUnsupported,
			Message: "This server speaks version 2 of the sync protocol.",
		},
		"error-malformed-request": ErrorResponse{
			Reason:  ReasonMalformedRequest,
			Message: "deviceID is required",
		},
		"error-server-error": ErrorResponse{
			Reason:  ReasonServerError,
			Message: "The server could not complete that request. Nothing was lost.",
		},
	}
}

// TestFixturesMatchTheTypes is the cross-implementation check.
//
// Indented rather than minified, so the files are readable and a diff is meaningful — but still
// byte-compared, so field order, base64 padding, timestamp format and null-versus-empty-array are all
// pinned. Go emits struct fields in declaration order, which is what makes this stable.
func TestFixturesMatchTheTypes(t *testing.T) {
	for name, value := range fixtures() {
		t.Run(name, func(t *testing.T) {
			encoded, err := json.MarshalIndent(value, "", "  ")
			if err != nil {
				t.Fatalf("encoding: %v", err)
			}
			encoded = append(encoded, '\n')

			path := filepath.Join(fixtureDir, name+".json")

			if *update {
				if err := os.MkdirAll(fixtureDir, 0o755); err != nil {
					t.Fatalf("creating fixture directory: %v", err)
				}
				if err := os.WriteFile(path, encoded, 0o644); err != nil {
					t.Fatalf("writing fixture: %v", err)
				}
				t.Logf("wrote %s", path)
				return
			}

			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("reading fixture (run with -update to create it): %v", err)
			}
			if !bytes.Equal(want, encoded) {
				t.Errorf("%s does not match the current types.\n--- fixture ---\n%s\n--- produced ---\n%s",
					path, want, encoded)
			}
		})
	}
}

// A fixture must also decode back into its type, or the file is documentation rather than a contract.
func TestFixturesDecodeIntoTheirTypes(t *testing.T) {
	cases := map[string]func() any{
		"sync-request-export":       func() any { return new(SyncRequest) },
		"sync-request-import":       func() any { return new(SyncRequest) },
		"sync-response":             func() any { return new(SyncResponse) },
		"sync-response-empty":       func() any { return new(SyncResponse) },
		"devices":                   func() any { return new([]DeviceInfo) },
		"health":                    func() any { return new(HealthResponse) },
		"healthz":                   func() any { return new(HealthzResponse) },
		"healthz-down":              func() any { return new(HealthzResponse) },
		"error-invalid-credentials": func() any { return new(ErrorResponse) },
	}

	for name, make := range cases {
		t.Run(name, func(t *testing.T) {
			body, err := os.ReadFile(filepath.Join(fixtureDir, name+".json"))
			if err != nil {
				t.Fatalf("reading fixture: %v", err)
			}
			into := make()
			if err := json.Unmarshal(body, into); err != nil {
				t.Fatalf("decoding fixture: %v", err)
			}

			// And re-encoding it reproduces the file, which is the round trip the app asserts too.
			again, err := json.MarshalIndent(into, "", "  ")
			if err != nil {
				t.Fatalf("re-encoding: %v", err)
			}
			if !bytes.Equal(bytes.TrimSpace(body), bytes.TrimSpace(again)) {
				t.Errorf("the round trip is not stable.\n--- fixture ---\n%s\n--- re-encoded ---\n%s", body, again)
			}
		})
	}
}

// An empty entry list must serialise as [] and never as null.
//
// A client decoding null into a non-optional array is a crash, and it is exactly the kind of
// difference that survives every test written in one language and fails in the other.
func TestEmptyCollectionsEncodeAsArrays(t *testing.T) {
	response, err := json.Marshal(SyncResponse{Entries: []OutboundEntry{}, Cursor: 1})
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}
	if !bytes.Contains(response, []byte(`"entries":[]`)) {
		t.Errorf("an empty entry list did not encode as []: %s", response)
	}

	devices, err := json.Marshal([]DeviceInfo{})
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}
	if string(devices) != "[]" {
		t.Errorf("an empty device list did not encode as []: %s", devices)
	}
}

// The timestamp format is the one interoperability detail that is easy to get wrong and invisible
// when it is.
func TestTimestampsAreISO8601InUTCWithMilliseconds(t *testing.T) {
	cases := []struct {
		want string
		in   time.Time
	}{
		{"2023-11-14T22:13:20.000Z", time.Unix(1_700_000_000, 0)},
		{"2023-11-14T22:13:20.125Z", time.Unix(1_700_000_000, 125_000_000)},
		// A non-UTC input must still be rendered in UTC. Two devices in different zones agreeing on
		// an instant is the whole reason the format is pinned.
		{"2023-11-14T22:13:20.000Z", time.Unix(1_700_000_000, 0).In(time.FixedZone("somewhere", 5*3600))},
		// Sub-millisecond precision is truncated, not rounded up into the next millisecond.
		{"2023-11-14T22:13:20.999Z", time.Unix(1_700_000_000, 999_999_999)},
	}
	for _, c := range cases {
		if got := Timestamp(c.in); got != c.want {
			t.Errorf("want %q, got %q", c.want, got)
		}
	}
}
