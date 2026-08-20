// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package api

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/marc2912/cmdv-sync-server/internal/auth"
	"github.com/marc2912/cmdv-sync-server/internal/store"
)

// Tokens as `openssl rand -base64 32` produces them.
const (
	tokenA = "kO0Zq2XjP1sVn8mWcR4tYuI6oPaSdF7gHjKlZxCvBnM="
	tokenB = "9aB8cD7eF6gH5iJ4kL3mN2oP1qR0sT9uV8wX7yZ6aB4="

	phone  = "11111111-1111-1111-1111-111111111111"
	tablet = "22222222-2222-2222-2222-222222222222"
	laptop = "33333333-3333-3333-3333-333333333333"
)

// instant is fixed so the device-list timestamps are assertable.
var instant = time.Unix(1_700_000_000, 0).UTC()

// harness drives the real routes over a real socket.
//
// httptest.NewServer, not a hand-rolled responder: it exercises the actual HTTP parsing, the actual
// middleware chain and the actual status codes, so there is nothing mocked on either side.
type harness struct {
	t      *testing.T
	server *httptest.Server
	store  *store.Store
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.sqlite"))
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	// Discard logs: these tests assert on responses, and a passing run should be quiet.
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := New(st, auth.New([]string{tokenA, tokenB}), logger,
		WithClock(func() time.Time { return instant })).Handler()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return &harness{t: t, server: server, store: st}
}

// do sends a request with the protocol version header set, as a real client does.
func (h *harness) do(method, path, token string, body any) *http.Response {
	h.t.Helper()

	var reader io.Reader
	if body != nil {
		switch v := body.(type) {
		case string:
			reader = strings.NewReader(v)
		default:
			encoded, err := json.Marshal(v)
			if err != nil {
				h.t.Fatalf("encoding request: %v", err)
			}
			reader = bytes.NewReader(encoded)
		}
	}

	req, err := http.NewRequest(method, h.server.URL+path, reader)
	if err != nil {
		h.t.Fatalf("building request: %v", err)
	}
	req.Header.Set(VersionHeader, strconv.Itoa(ProtocolVersion))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := h.server.Client().Do(req)
	if err != nil {
		h.t.Fatalf("sending request: %v", err)
	}
	h.t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// sync performs an exchange and requires it to succeed.
func (h *harness) sync(token, device string, cursor int64, blobs ...string) SyncResponse {
	h.t.Helper()
	entries := make([]InboundEntry, 0, len(blobs))
	for _, b := range blobs {
		entries = append(entries, InboundEntry{Blob: []byte(b)})
	}
	resp := h.do(http.MethodPost, "/api/v1/sync", token, SyncRequest{
		DeviceID:   device,
		DeviceName: "Device " + device[:4],
		Cursor:     cursor,
		Entries:    entries,
	})
	if resp.StatusCode != http.StatusOK {
		h.t.Fatalf("sync failed: %s — %s", resp.Status, readBody(h.t, resp))
	}
	var out SyncResponse
	decode(h.t, resp, &out)
	return out
}

func decode(t *testing.T, resp *http.Response, into any) {
	t.Helper()
	if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	return string(b)
}

// reason decodes a described failure. Asserted on throughout rather than the status alone, because
// the reason is what a client branches on.
func reason(t *testing.T, resp *http.Response) Reason {
	t.Helper()
	var e ErrorResponse
	decode(t, resp, &e)
	return e.Reason
}

// MARK: The exchange

func TestAnEntryTravelsFromOneDeviceToAnother(t *testing.T) {
	h := newHarness(t)

	pushed := h.sync(tokenA, phone, 0, "the phone's opaque bytes")
	if len(pushed.Entries) != 0 {
		t.Errorf("a device was sent its own entry: %+v", pushed.Entries)
	}

	pulled := h.sync(tokenA, tablet, 0)
	if len(pulled.Entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(pulled.Entries))
	}
	if got := string(pulled.Entries[0].Blob); got != "the phone's opaque bytes" {
		t.Errorf("want the blob unchanged, got %q", got)
	}
	if pulled.Entries[0].DeviceID != phone {
		t.Errorf("want the author to be the phone, got %q", pulled.Entries[0].DeviceID)
	}
	if pulled.HasMore {
		t.Error("a complete page reported more to come")
	}
}

// The payload is opaque, and this asserts it over the wire rather than only in the store: arbitrary
// bytes survive base64, JSON, HTTP and SQLite unchanged. If they did not, an encrypted payload would
// fail to open and nothing here would know why.
func TestArbitraryBytesSurviveTheRoundTrip(t *testing.T) {
	h := newHarness(t)

	blob := make([]byte, 256)
	for i := range blob {
		blob[i] = byte(i)
	}

	resp := h.do(http.MethodPost, "/api/v1/sync", tokenA, SyncRequest{
		DeviceID: phone, DeviceName: "Phone", Cursor: 0,
		Entries: []InboundEntry{{Blob: blob}},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("push failed: %s", resp.Status)
	}

	pulled := h.sync(tokenA, tablet, 0)
	if len(pulled.Entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(pulled.Entries))
	}
	if !bytes.Equal(pulled.Entries[0].Blob, blob) {
		t.Error("the blob did not survive the round trip byte for byte")
	}
}

// An import is the same endpoint with no entries. The server neither knows nor cares that a client
// may call it twice per sync, which is what lets the app's import-resolve-export flow change without
// a server release.
func TestAnImportIsAnExchangeWithNoEntries(t *testing.T) {
	h := newHarness(t)
	h.sync(tokenA, phone, 0, "one", "two")

	imported := h.sync(tokenA, tablet, 0)
	if len(imported.Entries) != 2 {
		t.Fatalf("an import returned %d entries, want 2", len(imported.Entries))
	}

	// And nothing was stored on the tablet's behalf by importing.
	back := h.sync(tokenA, phone, 0)
	if len(back.Entries) != 0 {
		t.Errorf("importing created entries: %+v", back.Entries)
	}
}

func TestASecondSyncWithTheReturnedCursorIsEmpty(t *testing.T) {
	h := newHarness(t)
	h.sync(tokenA, phone, 0, "one")

	first := h.sync(tokenA, tablet, 0)
	if len(first.Entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(first.Entries))
	}

	second := h.sync(tokenA, tablet, first.Cursor)
	if len(second.Entries) != 0 {
		t.Errorf("the cursor did not cover what was delivered: %+v", second.Entries)
	}
	if second.Cursor != first.Cursor {
		t.Errorf("the cursor moved with nothing to deliver: %d then %d", first.Cursor, second.Cursor)
	}
}

// A device whose page is empty because everything above its cursor is its own must still have its
// cursor advanced, or it asks for those rows forever.
func TestTheCursorAdvancesPastADevicesOwnEntries(t *testing.T) {
	h := newHarness(t)
	pushed := h.sync(tokenA, phone, 0, "one", "two", "three")
	if pushed.Cursor == 0 {
		t.Fatal("the cursor did not advance past the device's own entries")
	}

	again := h.sync(tokenA, phone, pushed.Cursor)
	if len(again.Entries) != 0 || again.Cursor != pushed.Cursor {
		t.Errorf("want a stable empty page, got %+v", again)
	}
}

func TestALongHistoryArrivesInPages(t *testing.T) {
	h := newHarness(t)
	blobs := []string{"a", "b", "c", "d", "e", "f", "g"}
	h.sync(tokenA, phone, 0, blobs...)

	var collected []string
	cursor := int64(0)
	pages := 0
	for {
		resp := h.do(http.MethodPost, "/api/v1/sync", tokenA, SyncRequest{
			DeviceID: tablet, DeviceName: "Tablet", Cursor: cursor, Limit: 3,
			Entries: []InboundEntry{},
		})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("page failed: %s", resp.Status)
		}
		var page SyncResponse
		decode(t, resp, &page)
		for _, e := range page.Entries {
			collected = append(collected, string(e.Blob))
		}
		cursor = page.Cursor
		pages++
		if !page.HasMore {
			break
		}
		if pages > 10 {
			t.Fatal("paging did not terminate")
		}
	}

	if pages != 3 {
		t.Errorf("want 3 pages, got %d", pages)
	}
	if strings.Join(collected, "") != strings.Join(blobs, "") {
		t.Errorf("want %v in order, got %v", blobs, collected)
	}
}

func TestAnAbsurdLimitIsClampedRatherThanRefused(t *testing.T) {
	h := newHarness(t)
	resp := h.do(http.MethodPost, "/api/v1/sync", tokenA, SyncRequest{
		DeviceID: phone, DeviceName: "Phone", Limit: 100_000_000,
		Entries: []InboundEntry{},
	})
	if resp.StatusCode != http.StatusOK {
		t.Errorf("an absurd limit was refused rather than clamped: %s", resp.Status)
	}
}

// Nothing arbitrates, so two devices' versions of one logical record both survive. This is the
// property that removes every app-domain concept from the server.
func TestTwoDevicesVersionsBothSurvive(t *testing.T) {
	h := newHarness(t)
	h.sync(tokenA, phone, 0, "phone's version")
	h.sync(tokenA, tablet, 0, "tablet's version")

	pulled := h.sync(tokenA, laptop, 0)
	if len(pulled.Entries) != 2 {
		t.Fatalf("want both versions, got %d entries", len(pulled.Entries))
	}
	// In arrival order, which is the only ordering in the system.
	if string(pulled.Entries[0].Blob) != "phone's version" || string(pulled.Entries[1].Blob) != "tablet's version" {
		t.Errorf("entries are not in arrival order: %q, %q",
			pulled.Entries[0].Blob, pulled.Entries[1].Blob)
	}
}

// MARK: Dataset isolation

// The requirement a hosted offering would rest on.
func TestOneTokenNeverSeesAnothersEntries(t *testing.T) {
	h := newHarness(t)
	h.sync(tokenA, phone, 0, "dataset A's data")
	h.sync(tokenB, phone, 0, "dataset B's data")

	// The same device identifier under both tokens, which is the harshest version of the test.
	a := h.sync(tokenA, tablet, 0)
	if len(a.Entries) != 1 || string(a.Entries[0].Blob) != "dataset A's data" {
		t.Errorf("token A saw the wrong entries: %+v", a.Entries)
	}
	b := h.sync(tokenB, tablet, 0)
	if len(b.Entries) != 1 || string(b.Entries[0].Blob) != "dataset B's data" {
		t.Errorf("token B saw the wrong entries: %+v", b.Entries)
	}
}

func TestOneTokenNeverSeesAnothersDevices(t *testing.T) {
	h := newHarness(t)
	h.sync(tokenA, phone, 0)
	h.sync(tokenB, tablet, 0)

	resp := h.do(http.MethodGet, "/api/v1/devices", tokenA, nil)
	var devices []DeviceInfo
	decode(t, resp, &devices)
	if len(devices) != 1 || devices[0].DeviceID != phone {
		t.Errorf("token A's device list leaked: %+v", devices)
	}
}

// MARK: Authentication

func TestARequestWithNoTokenIsRefused(t *testing.T) {
	h := newHarness(t)
	for _, path := range []string{"/api/v1/sync", "/api/v1/devices"} {
		method := http.MethodGet
		var body any
		if strings.HasSuffix(path, "sync") {
			method, body = http.MethodPost, SyncRequest{DeviceID: phone, DeviceName: "P"}
		}
		resp := h.do(method, path, "", body)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: want 401, got %s", path, resp.Status)
		}
		if got := reason(t, resp); got != ReasonInvalidCredentials {
			t.Errorf("%s: want invalidCredentials, got %q", path, got)
		}
	}
}

func TestAnUnknownTokenIsRefused(t *testing.T) {
	h := newHarness(t)
	resp := h.do(http.MethodGet, "/api/v1/devices", "not-a-real-token-but-long-enough-to-pass=", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("want 401, got %s", resp.Status)
	}
	if got := reason(t, resp); got != ReasonInvalidCredentials {
		t.Errorf("want invalidCredentials, got %q", got)
	}
}

func TestANonBearerAuthorizationSchemeIsNotAccepted(t *testing.T) {
	h := newHarness(t)
	req, _ := http.NewRequest(http.MethodGet, h.server.URL+"/api/v1/devices", nil)
	req.Header.Set("Authorization", "Basic "+tokenA)
	req.Header.Set(VersionHeader, strconv.Itoa(ProtocolVersion))

	resp, err := h.server.Client().Do(req)
	if err != nil {
		t.Fatalf("sending: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("want 401, got %s", resp.Status)
	}
}

// MARK: Versioning

// A version interpreted optimistically is how a newer client silently corrupts an older server's
// data. Refused, and said plainly.
func TestAVersionThisServerDoesNotSpeakIsRefused(t *testing.T) {
	h := newHarness(t)
	for _, version := range []string{"99", "1", "two", "-1"} {
		req, _ := http.NewRequest(http.MethodGet, h.server.URL+"/api/v1/health", nil)
		req.Header.Set(VersionHeader, version)

		resp, err := h.server.Client().Do(req)
		if err != nil {
			t.Fatalf("sending: %v", err)
		}
		if resp.StatusCode != http.StatusConflict {
			t.Errorf("version %q: want 409, got %s", version, resp.Status)
		}
		if got := reason(t, resp); got != ReasonVersionUnsupported {
			t.Errorf("version %q: want versionUnsupported, got %q", version, got)
		}
		resp.Body.Close()
	}
}

func TestResponsesCarryTheProtocolVersion(t *testing.T) {
	h := newHarness(t)
	resp := h.do(http.MethodGet, "/api/v1/health", "", nil)
	if got := resp.Header.Get(VersionHeader); got != strconv.Itoa(ProtocolVersion) {
		t.Errorf("want the version header %d, got %q", ProtocolVersion, got)
	}
}

// MARK: Health and routing

// The first thing anyone does when a server will not connect is curl it, and a health check refused
// for a missing header would send them looking in the wrong place.
func TestHealthNeedsNeitherTokenNorVersionHeader(t *testing.T) {
	h := newHarness(t)
	resp, err := h.server.Client().Get(h.server.URL + "/api/v1/health")
	if err != nil {
		t.Fatalf("getting health: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %s", resp.Status)
	}
	var health HealthResponse
	decode(t, resp, &health)
	if health.Status != "ok" || health.Version != ProtocolVersion {
		t.Errorf("unexpected health body: %+v", health)
	}
}

// A path a reverse proxy failed to strip is a misconfiguration, and the operator needs to be told
// that rather than shown a server error.
func TestAnUnknownPathIsANotFoundWithAReason(t *testing.T) {
	h := newHarness(t)
	resp := h.do(http.MethodGet, "/cmdv-sync/api/v1/devices", tokenA, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("want 404, got %s", resp.Status)
	}
	var e ErrorResponse
	decode(t, resp, &e)
	if e.Reason != ReasonMalformedRequest {
		t.Errorf("want malformedRequest, got %q", e.Reason)
	}
	// The message should point at the actual cause, since this is nearly always a proxy prefix.
	if !strings.Contains(e.Message, "prefix") {
		t.Errorf("the message does not mention a proxy prefix: %q", e.Message)
	}
}

func TestTheWrongMethodIsNotAServerError(t *testing.T) {
	h := newHarness(t)
	resp := h.do(http.MethodGet, "/api/v1/sync", tokenA, nil)
	if resp.StatusCode != http.StatusMethodNotAllowed && resp.StatusCode != http.StatusNotFound {
		t.Errorf("want 405 or 404, got %s", resp.Status)
	}
}

// MARK: Devices

func TestDevicesAreListedWithTheNamesTheirReadersGave(t *testing.T) {
	h := newHarness(t)
	h.do(http.MethodPost, "/api/v1/sync", tokenA, SyncRequest{
		DeviceID: phone, DeviceName: "Marc's phone", Entries: []InboundEntry{},
	})
	h.do(http.MethodPost, "/api/v1/sync", tokenA, SyncRequest{
		DeviceID: tablet, DeviceName: "Kitchen tablet", Entries: []InboundEntry{},
	})

	resp := h.do(http.MethodGet, "/api/v1/devices", tokenA, nil)
	var devices []DeviceInfo
	decode(t, resp, &devices)
	if len(devices) != 2 {
		t.Fatalf("want 2 devices, got %d", len(devices))
	}
	names := []string{devices[0].DeviceName, devices[1].DeviceName}
	if names[0] != "Marc's phone" || names[1] != "Kitchen tablet" {
		t.Errorf("want the given names in order, got %v", names)
	}
	if devices[0].LastSeenAt != Timestamp(instant) {
		t.Errorf("want last-seen %q, got %q", Timestamp(instant), devices[0].LastSeenAt)
	}
}

// A device is known by presenting a valid token. There is no registration step to get wrong.
func TestADeviceBecomesKnownByFirstContact(t *testing.T) {
	h := newHarness(t)
	resp := h.do(http.MethodGet, "/api/v1/devices", tokenA, nil)
	var before []DeviceInfo
	decode(t, resp, &before)
	if len(before) != 0 {
		t.Fatalf("want no devices before first contact, got %d", len(before))
	}

	h.sync(tokenA, phone, 0)

	resp = h.do(http.MethodGet, "/api/v1/devices", tokenA, nil)
	var after []DeviceInfo
	decode(t, resp, &after)
	if len(after) != 1 || after[0].DeviceID != phone {
		t.Errorf("want the device listed after one sync, got %+v", after)
	}
}

func TestRemovingADeviceDelistsItButKeepsItsEntries(t *testing.T) {
	h := newHarness(t)
	h.sync(tokenA, phone, 0, "written before removal")

	resp := h.do(http.MethodDelete, "/api/v1/devices/"+phone, tokenA, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("want 204, got %s — %s", resp.Status, readBody(t, resp))
	}

	resp = h.do(http.MethodGet, "/api/v1/devices", tokenA, nil)
	var devices []DeviceInfo
	decode(t, resp, &devices)
	if len(devices) != 0 {
		t.Errorf("want an empty device list, got %+v", devices)
	}

	// The history survives, because another device may not have replayed it.
	pulled := h.sync(tokenA, tablet, 0)
	if len(pulled.Entries) != 1 {
		t.Errorf("a removed device's entries were lost: got %d", len(pulled.Entries))
	}
}

func TestRemovingAnUnlistedDeviceIsANotFound(t *testing.T) {
	h := newHarness(t)
	resp := h.do(http.MethodDelete, "/api/v1/devices/"+laptop, tokenA, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("want 404, got %s", resp.Status)
	}
}

func TestRemovingADeviceIsScopedToTheToken(t *testing.T) {
	h := newHarness(t)
	h.sync(tokenB, phone, 0)

	// Token A trying to remove a device that belongs to token B's dataset.
	resp := h.do(http.MethodDelete, "/api/v1/devices/"+phone, tokenA, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("want 404, got %s", resp.Status)
	}

	resp = h.do(http.MethodGet, "/api/v1/devices", tokenB, nil)
	var devices []DeviceInfo
	decode(t, resp, &devices)
	if len(devices) != 1 {
		t.Error("a device was removed from another dataset")
	}
}

// MARK: Malformed input

func TestABodyThatIsNotTheProtocolIsADescribedRefusal(t *testing.T) {
	h := newHarness(t)
	for _, body := range []string{
		`{"deviceID":`,    // truncated
		`not json at all`, // not JSON
		``,                // empty
		`{"a":1}{"b":2}`,  // trailing content
	} {
		resp := h.do(http.MethodPost, "/api/v1/sync", tokenA, body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("body %q: want 400, got %s", body, resp.Status)
			continue
		}
		if got := reason(t, resp); got != ReasonMalformedRequest {
			t.Errorf("body %q: want malformedRequest, got %q", body, got)
		}
	}
}

// A newer client sending a field this server has never heard of must be served, not refused. That is
// the same forward compatibility that lets the app evolve without a server release.
func TestAnUnknownFieldIsIgnoredRatherThanRefused(t *testing.T) {
	h := newHarness(t)
	body := `{"deviceID":"` + phone + `","deviceName":"Phone","cursor":0,"entries":[],` +
		`"somethingFromTheFuture":{"nested":true}}`

	resp := h.do(http.MethodPost, "/api/v1/sync", tokenA, body)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("an unknown field was refused: %s — %s", resp.Status, readBody(t, resp))
	}
}

func TestMissingOrMalformedIdentifiersAreRefused(t *testing.T) {
	h := newHarness(t)
	cases := map[string]SyncRequest{
		"no device id":      {DeviceName: "Phone"},
		"blank device id":   {DeviceID: "   ", DeviceName: "Phone"},
		"no device name":    {DeviceID: phone},
		"control character": {DeviceID: phone, DeviceName: "Ph\x00one"},
		"oversized name":    {DeviceID: phone, DeviceName: strings.Repeat("n", MaxIdentifierBytes+1)},
		"negative cursor":   {DeviceID: phone, DeviceName: "Phone", Cursor: -1},
	}
	for name, request := range cases {
		resp := h.do(http.MethodPost, "/api/v1/sync", tokenA, request)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: want 400, got %s", name, resp.Status)
			continue
		}
		if got := reason(t, resp); got != ReasonMalformedRequest {
			t.Errorf("%s: want malformedRequest, got %q", name, got)
		}
	}
}

func TestAnEmptyBlobIsRefused(t *testing.T) {
	h := newHarness(t)
	resp := h.do(http.MethodPost, "/api/v1/sync", tokenA, SyncRequest{
		DeviceID: phone, DeviceName: "Phone",
		Entries: []InboundEntry{{Blob: []byte{}}},
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("want 400, got %s", resp.Status)
	}
}

func TestTooManyEntriesInOneRequestIsRefused(t *testing.T) {
	h := newHarness(t)
	entries := make([]InboundEntry, MaxEntriesPerRequest+1)
	for i := range entries {
		entries[i] = InboundEntry{Blob: []byte("x")}
	}
	resp := h.do(http.MethodPost, "/api/v1/sync", tokenA, SyncRequest{
		DeviceID: phone, DeviceName: "Phone", Entries: entries,
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("want 400, got %s", resp.Status)
	}
}

// The server must refuse an oversized body rather than buffer it, and must still be standing
// afterwards — which is the property actually being defended.
func TestAnOversizedBodyIsRefusedAndTheServerSurvives(t *testing.T) {
	h := newHarness(t)

	oversized := strings.Repeat("a", MaxBodyBytes+1024)
	resp := h.do(http.MethodPost, "/api/v1/sync", tokenA, `{"deviceID":"`+phone+`","deviceName":"`+oversized+`"}`)
	if resp.StatusCode < 400 {
		t.Errorf("an oversized body was accepted: %s", resp.Status)
	}

	// Still answering, which a server that fell over would not be.
	health := h.do(http.MethodGet, "/api/v1/health", "", nil)
	if health.StatusCode != http.StatusOK {
		t.Errorf("the server did not survive an oversized body: %s", health.Status)
	}
}

func TestAnOversizedSingleBlobIsRefused(t *testing.T) {
	h := newHarness(t)
	resp := h.do(http.MethodPost, "/api/v1/sync", tokenA, SyncRequest{
		DeviceID: phone, DeviceName: "Phone",
		Entries: []InboundEntry{{Blob: bytes.Repeat([]byte("x"), MaxBlobBytes+1)}},
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("want 400, got %s", resp.Status)
	}
}

// MARK: /healthz

// The minimum a probe needs: 200, a body it can match on a fixed string, and no authentication.
func TestHealthzAnswersWithoutAuthentication(t *testing.T) {
	h := newHarness(t)

	resp, err := h.server.Client().Get(h.server.URL + "/healthz")
	if err != nil {
		t.Fatalf("getting healthz: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %s", resp.Status)
	}
	// Asserted as an exact string, not decoded, because a monitor matches bytes. blackbox_exporter's
	// fail_if_body_not_matches_regexp is looking at this literal.
	body := readBody(t, resp)
	if body != `{"ok":true}` {
		t.Errorf("want `{\"ok\":true}`, got %q", body)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("want application/json, got %q", got)
	}
}

// `curl -sI` sends HEAD, and a probe configured that way must not get a 404 or a 405. net/http serves
// HEAD from a GET pattern and suppresses the body itself, so this costs no extra route — but it is
// worth pinning, because it is invisible until somebody's monitor is silently red.
func TestHealthzAnswersHead(t *testing.T) {
	h := newHarness(t)

	resp, err := h.server.Client().Head(h.server.URL + "/healthz")
	if err != nil {
		t.Fatalf("HEAD healthz: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200 for HEAD, got %s", resp.Status)
	}
	if body := readBody(t, resp); body != "" {
		t.Errorf("HEAD returned a body: %q", body)
	}
	// The headers still describe the response that a GET would have produced.
	if got := resp.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("want application/json on HEAD, got %q", got)
	}
}

// The reason /healthz is on its own mux rather than exempted by path inside the middleware. A monitor
// has no stake in the sync protocol, and must not be capable of going red because of a version header.
func TestHealthzIgnoresTheProtocolVersion(t *testing.T) {
	h := newHarness(t)

	for _, version := range []string{"1", "99", "not-a-number", ""} {
		req, _ := http.NewRequest(http.MethodGet, h.server.URL+"/healthz", nil)
		if version != "" {
			req.Header.Set(VersionHeader, version)
		}

		resp, err := h.server.Client().Do(req)
		if err != nil {
			t.Fatalf("version %q: %v", version, err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Errorf("version %q: want 200, got %s", version, resp.Status)
		}
		resp.Body.Close()
	}

	// While the protocol's own endpoint still refuses a version it cannot speak, which is what makes
	// the two behaviours a deliberate split rather than an inconsistency.
	req, _ := http.NewRequest(http.MethodGet, h.server.URL+"/api/v1/health", nil)
	req.Header.Set(VersionHeader, "99")
	resp, err := h.server.Client().Do(req)
	if err != nil {
		t.Fatalf("getting versioned health: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("want the protocol endpoint to still refuse version 99, got %s", resp.Status)
	}
}

// A token, valid or not, changes nothing. A probe should not need one and must not be refused for
// carrying a bad one — a monitor pointed at the wrong URL should not look like a healthy server.
func TestHealthzIsUnaffectedByATokenEitherWay(t *testing.T) {
	h := newHarness(t)

	for _, token := range []string{"", tokenA, "not-a-real-token-but-long-enough-to-pass="} {
		resp := h.do(http.MethodGet, "/healthz", token, nil)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("token %q: want 200, got %s", token, resp.Status)
		}
	}
}

// This is the whole reason the endpoint touches the database. A closed store stands in for the failure
// that actually matters — a wedged or lock-starved database on replicated storage — and the probe has
// to report it rather than answering 200 because the process is still running.
func TestHealthzReportsAFailedDatabase(t *testing.T) {
	h := newHarness(t)

	// Healthy first, so the assertion below is about the database and not about the route.
	if resp := h.do(http.MethodGet, "/healthz", "", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200 before closing the store, got %s", resp.Status)
	}

	if err := h.store.Close(); err != nil {
		t.Fatalf("closing the store: %v", err)
	}

	resp := h.do(http.MethodGet, "/healthz", "", nil)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("want 500 once the database cannot answer, got %s", resp.Status)
	}
	if body := readBody(t, resp); body != `{"ok":false}` {
		t.Errorf("want `{\"ok\":false}`, got %q", body)
	}
}

// A probe answering every fifteen seconds must not fill the log. Successful probes go to debug so
// `info` stays worth reading; a failure is logged normally, because that is the line an operator will
// come looking for.
func TestSuccessfulProbesDoNotFillTheLogButFailuresDo(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.sqlite"))
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	var logged bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelInfo}))
	server := httptest.NewServer(New(st, auth.New([]string{tokenA}), logger).Handler())
	t.Cleanup(server.Close)

	for _, path := range []string{"/healthz", "/api/v1/health"} {
		resp, err := server.Client().Get(server.URL + path)
		if err != nil {
			t.Fatalf("getting %s: %v", path, err)
		}
		resp.Body.Close()
	}
	if strings.Contains(logged.String(), "healthz") {
		t.Errorf("a successful probe was logged at info:\n%s", logged.String())
	}

	// A real request is still logged, so this is quietening probes rather than quietening the log.
	resp, err := server.Client().Get(server.URL + "/api/v1/devices")
	if err != nil {
		t.Fatalf("getting devices: %v", err)
	}
	resp.Body.Close()
	if !strings.Contains(logged.String(), "/api/v1/devices") {
		t.Errorf("an ordinary request was not logged at info:\n%s", logged.String())
	}

	// And a failing probe is.
	st.Close()
	resp, err = server.Client().Get(server.URL + "/healthz")
	if err != nil {
		t.Fatalf("getting healthz: %v", err)
	}
	resp.Body.Close()
	if !strings.Contains(logged.String(), "healthz") {
		t.Errorf("a failing probe was not logged at info:\n%s", logged.String())
	}
}

// No side effects. A probe running every fifteen seconds must not create a device, write an entry, or
// move a cursor — otherwise monitoring would slowly accumulate state.
func TestHealthzHasNoSideEffects(t *testing.T) {
	h := newHarness(t)
	h.sync(tokenA, phone, 0, "one")

	before := h.sync(tokenA, tablet, 0)
	for range 5 {
		if resp := h.do(http.MethodGet, "/healthz", "", nil); resp.StatusCode != http.StatusOK {
			t.Fatalf("probe failed: %s", resp.Status)
		}
	}
	after := h.sync(tokenA, tablet, 0)

	if before.Cursor != after.Cursor {
		t.Errorf("probing moved the cursor: %d then %d", before.Cursor, after.Cursor)
	}

	resp := h.do(http.MethodGet, "/api/v1/devices", tokenA, nil)
	var devices []DeviceInfo
	decode(t, resp, &devices)
	for _, d := range devices {
		if d.DeviceID != phone && d.DeviceID != tablet {
			t.Errorf("probing registered a device: %+v", d)
		}
	}
}
