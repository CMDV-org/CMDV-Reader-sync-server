# CMDV Sync Protocol, version 2

**This document is normative.** It is the contract between a client and a server, and it is the only
thing either side may rely on. The Go in this repository is *an* implementation of it, not the
definition of it.

Alongside it, [`fixtures/`](fixtures/) holds one JSON file per message shape. Both this repository
and the CMDV Reader app assert that decoding a fixture and re-encoding it reproduces the file byte
for byte. That is what keeps two implementations in two languages honest without either importing
the other's code.

Requirements use **MUST**, **MUST NOT**, **SHOULD** and **MAY** in the usual sense.

---

## 1. What the server is

A server stores opaque blobs and hands them back in arrival order. That is the whole of it.

A server **MUST NOT** interpret, validate, parse, decrypt, transform or re-encode a blob. A server
**MUST NOT** compare two blobs, and **MUST NOT** discard one in favour of another. There is no
concept of a record type, a record identity, a record timestamp, or which version of something is
newer — none of that appears in this protocol, and a server that grew an opinion about it would be a
server the client has to be kept in step with.

The consequence, which is the design's whole purpose: a client can change what it stores, how it
structures it, how it versions it and whether it encrypts it, and **no server change is required**.

## 2. Datasets and authentication

A server holds one or more **datasets**. A dataset is an isolated set of entries and devices.

Authentication is a pre-shared bearer token:

```
Authorization: Bearer <token>
```

- One token selects exactly one dataset. Two tokens **MUST** select different datasets.
- A server **MUST** reject an unknown token with `401` and reason `invalidCredentials`, and **MUST
  NOT** distinguish "no token" from "unknown token" in a way that reveals which tokens exist.
- A server **SHOULD** compare tokens in constant time.
- A server **MUST NOT** store a token in a form from which it can be recovered. This implementation
  keeps only `SHA-256("cmdv-sync-dataset-v2\x00" + token)`, truncated to 16 hex characters, as the
  dataset identifier.
- A server **MUST NOT** write a token to a log.

There are no accounts, no usernames, no passwords, and no registration. A device becomes known to a
dataset by presenting a valid token; a server **MUST** accept a device it has not seen before without
any prior step.

Because every device in a dataset holds the same token, a server **cannot** authenticate a device *as
itself*. Anything holding the token can claim to be any device. Implementations and clients **MUST
NOT** present per-device access control as a security boundary. Cutting off a device that is no
longer under the owner's control is done by rotating the token.

## 3. Versioning

Every request **SHOULD** carry:

```
X-CMDV-Sync-Version: 2
```

- A server **MUST** reject a version it does not implement with `409` and reason
  `versionUnsupported`.
- A request with **no** version header **MUST** be treated as the server's current version, so that a
  plain `curl` against the health endpoint is not refused for a missing header.
- A server **MUST** set the header on every response it generates.

## 4. Ordering — the only ordering there is

When a server accepts an entry it assigns it a **sequence**: a positive integer, unique within the
server, and strictly greater than every sequence it has assigned before.

- A sequence **MUST** be monotonically increasing in the order entries are accepted.
- A sequence **MUST NOT** ever be reused, including after any deletion.
- Sequences **MAY** have gaps, and a client **MUST NOT** assume they are contiguous. A server holding
  several datasets will naturally interleave them in one sequence space.
- A sequence is assigned *on arrival*, not derived from anything in the entry. A server **MUST NOT**
  use a clock for ordering.

This is what makes clocks irrelevant to the whole protocol. Every client replays the same sequence in
the same order and therefore reaches the same state, so there is no clock skew to reconcile, no
logical clock to maintain, and no tie to break. The semantics are "the last device to *sync* wins",
not "the last device to *edit* wins" — a client that cares about the difference must handle it itself
(see §9).

## 5. Entries

An entry is immutable. A server **MUST NOT** modify or replace a stored entry, and **MUST NOT** delete
one except on an explicit, authenticated request. Two entries that a client considers to be versions
of the same logical record are simply two entries, and both **MUST** survive.

Each entry carries:

| Field | Set by | Meaning |
|---|---|---|
| `sequence` | Server | Arrival order. §4. |
| `deviceID` | Client | Which device wrote it. Opaque to the server. |
| `blob` | Client | The payload, base64 in JSON. Opaque to the server. |

`deviceID` and `blob` **MUST** be returned exactly as they were submitted. A blob is arbitrary
binary: it may be encrypted, compressed, or in a format that changes every client release.

## 6. The exchange

### `POST /api/v1/sync`

One request pushes and pulls. Authenticated.

**Request** — [`sync-request-export.json`](fixtures/sync-request-export.json):

```json
{
  "deviceID": "11111111-1111-1111-1111-111111111111",
  "deviceName": "Marc's phone",
  "cursor": 412,
  "limit": 500,
  "entries": [ { "blob": "AP8QgH8=" } ]
}
```

**Response** — [`sync-response.json`](fixtures/sync-response.json):

```json
{
  "entries": [
    { "sequence": 413, "deviceID": "22222222-…", "blob": "AP8QgH8=" }
  ],
  "cursor": 418,
  "hasMore": false
}
```

Server behaviour, in order:

1. **Store** every entry in `entries`, in the order given, each with a fresh sequence, attributed to
   `deviceID`. An empty `entries` **MUST** be accepted and stores nothing — that is how a client
   performs a pull-only *import*.
2. **Select** entries in this dataset where `sequence > cursor` **and** `deviceID` is not the calling
   device's, oldest first, at most `limit` of them. A device's own entries **MUST NOT** be returned;
   it already has them, and returning them would make every sync echo.
3. **Record** the device's identity and name, so it appears in the device list. The name **MUST** be
   updated on each contact, so renaming a device needs no separate call.

`cursor` in the response is where the client resumes, and its rules are the subtle part:

- If entries were returned, it **MUST** be the highest sequence among them.
- If none were returned, it **MUST** advance to the dataset's highest sequence. This matters: a page
  can be empty *because everything above the cursor is the caller's own*, and a cursor that did not
  advance would make the client ask for those entries on every sync forever.
- It **MUST NOT** ever move backwards, whatever the client sends.

`hasMore` is true when entries remain beyond `limit`, so a client knows to go again rather than wait
for the next sync.

A client **MUST** treat `cursor` as opaque: store what it was given and send it back. It **MUST NOT**
compute one.

Limits, and a server **MAY** choose its own:

| Field | Rule |
|---|---|
| `limit` | Absent or ≤ 0 means the server's default (500 here). A server **MUST** clamp an excessive value rather than refuse the request — a smaller page is still a correct answer. Maximum here: 2000. |
| `cursor` | **MUST** be ≥ 0. |
| `deviceID`, `deviceName` | **MUST** be non-empty, at most 128 bytes, and free of control characters. A server **MUST NOT** require any particular format; this implementation's client uses UUIDs, and that is the client's choice. |
| `blob` | **MUST** be non-empty. Maximum here: 1 MiB per entry. |
| `entries` | Maximum here: 2000 per request. |
| Body | Maximum here: 16 MiB. Exceeding it is `413`. |

A server **MUST** ignore fields it does not recognise rather than refuse the request. This is the same
forward compatibility that lets a client evolve without a server release.

### `GET /api/v1/devices`

Authenticated. Returns the devices in the dataset, oldest first —
[`devices.json`](fixtures/devices.json):

```json
[
  {
    "deviceID": "11111111-…",
    "deviceName": "Marc's phone",
    "firstSeenAt": "2023-11-14T22:13:20.000Z",
    "lastSeenAt": "2023-11-15T22:13:20.125Z"
  }
]
```

### `DELETE /api/v1/devices/{deviceID}`

Authenticated. Removes a device's **listing**. `204` on success, `404` when no such device is listed.

The device's entries **MUST** be kept. Other devices may not have replayed them, and the history is
legitimate whether or not the device that wrote it still exists — a reinstalled device has a new
identity and a cursor of 0, and needs every other device's entries.

This is list management, not access control. See §2.

### `GET /api/v1/health`

**Unauthenticated**, and **MUST NOT** require a version header —
[`health.json`](fixtures/health.json):

```json
{ "status": "ok", "version": 2 }
```

This is the *client-facing* health endpoint: it reports the protocol version, which is what lets a
client tell "the address is right but the token is not" from "cannot connect", and lets it discover a
version mismatch before authenticating.

### `GET /healthz` — operational, and outside this protocol

Not part of the sync protocol, and deliberately not under `/api/v1/`. It is documented here only
because a server implementation should provide it and a monitor will look for it.

Unauthenticated. It **MUST NOT** require a version header, and **MUST NOT** be refused for carrying
one — a monitor has no stake in the protocol version, and an alert that fires because the version
moved is a false alarm. **MUST** answer `HEAD` as well as `GET`, since `curl -sI` and several probes
use it. **MUST** have no side effects; it is polled continuously.

Exactly two responses, and the bodies are stable literals so a probe can match on them —
[`healthz.json`](fixtures/healthz.json), [`healthz-down.json`](fixtures/healthz-down.json):

| | Body |
|---|---|
| `200` | `{"ok":true}` |
| `500` | `{"ok":false}` |

It **SHOULD** touch the database — one trivial query, nothing more — and answer `500` when that fails.
Without it the endpoint proves only that a process is accepting connections, which is the least
interesting thing that could be true; with it, the probe answers "can this server actually serve", and
a wedged or lock-starved database shows up as a failure rather than as a green check above a queue of
failing syncs.

That touch **MUST** be bounded by a short timeout. A probe that hangs is worse than one that lies: the
monitor's own timeout fires with no reason attached, and some orchestrators will kill the container.
This implementation uses two seconds, which is far longer than any query it makes and shorter than its
own database busy timeout, so an externally locked database reports unhealthy rather than waiting.

No version number appears in the body, deliberately: a monitor must not have to be updated when this
protocol moves.

## 7. Errors

Every failure a server generates **MUST** carry a body naming a machine-readable reason —
[`error-invalid-credentials.json`](fixtures/error-invalid-credentials.json):

```json
{ "reason": "invalidCredentials", "message": "That token was refused." }
```

`reason` is a closed set:

| Reason | Typical status | Meaning to a client |
|---|---|---|
| `invalidCredentials` | 401 | The token is missing or not accepted. Ask the reader to pair again. |
| `versionUnsupported` | 409 | The protocol versions differ. Update one side. |
| `malformedRequest` | 400, 404, 413 | This request was wrong. Includes an unserved path. |
| `serverError` | 500 | The server failed. Nothing was lost; retry later. |

A status alone is not sufficient, because a client's next action differs between these and cannot be
derived from the number.

A server **MUST NOT** include internal detail — file paths, queries, fragments of stored data — in a
`serverError` message. Such detail belongs in the server's log, where only the operator sees it. A
`malformedRequest` message **MAY** describe the fault precisely, since it describes the caller's own
request.

A path the server does not serve **MUST** be `404` with `malformedRequest`, not a bare status: behind
a reverse proxy under a sub-path this is the most common misconfiguration, and the operator needs to
be told which it is.

## 8. Encodings

| | Rule |
|---|---|
| Content type | `application/json`, UTF-8 |
| Binary | base64, standard alphabet with padding (RFC 4648 §4) |
| Timestamps | ISO 8601, UTC, exactly three fractional digits: `2023-11-14T22:13:20.000Z`. Sub-millisecond precision is truncated. |
| Integers | JSON numbers. `sequence` and `cursor` **MUST** be representable as a signed 64-bit integer. |
| Empty collections | **MUST** encode as `[]`, never `null`. A client decoding `null` into a non-optional array is a crash, and it is precisely the difference that passes every test in one language and fails in the other. |

## 9. What a client must do

Not binding on a server, and recorded here because the protocol only makes sense alongside it.

- Apply every entry received, **in sequence order**, with nothing skipped or reordered. This is what
  makes two clients converge.
- Store the cursor only after a page has been durably applied, so a crash re-delivers rather than
  skips.
- Retain, rather than discard, an entry it cannot yet make sense of — one from a future payload
  version, or one referring to something not present locally.
- Because ordering is "last to sync wins", detect the case where an incoming change would overwrite a
  local change not yet pushed, and resolve it deliberately rather than silently. The client this
  protocol was designed for syncs in three steps for exactly this reason: **import**, then **resolve**
  with the reader, then **export**. A server cannot see the difference between that and a combined
  exchange, and **MUST NOT** try to.

## 10. Reserved

- `sequence` 0 is never assigned; a cursor of 0 means "I have seen nothing".
- `/healthz` is allocated for the operational probe in §6. Other paths outside `/api/v1/` are unallocated.
- A future version will increment `X-CMDV-Sync-Version` and **MAY** change anything in this document.
  A server **MUST** refuse a version it does not implement rather than guess.
