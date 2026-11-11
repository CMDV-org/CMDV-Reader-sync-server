# cmdv-sync-server

A sync server for [CMDV Reader](https://github.com/CMDV-org/CMDV-Reader). It keeps reading positions,
highlights, notes, bookmarks, reading sessions and settings in step between a reader's devices.

**It cannot read what it stores.** The server holds opaque blobs and hands them back in the order they
arrived. It has no idea what a highlight is, and adding a new kind of synced data to the app requires
no change here and no redeploy. That is not a side effect — it is the design, and everything else in
this README follows from it.

```bash
cp .env.example .env
openssl rand -base64 32          # put this in .env as CMDV_SYNC_TOKENS
docker compose up -d
```

That is the whole installation. Everything below is detail.

---

## Contents

- [What it stores](#what-it-stores)
- [Quick start](#quick-start)
- [Configuration](#configuration)
- [Ports and networking](#ports-and-networking)
- [TLS and reverse proxies](#tls-and-reverse-proxies)
- [Pointing the app at it](#pointing-the-app-at-it)
- [Tokens, datasets and devices](#tokens-datasets-and-devices)
- [Your data: backup and restore](#your-data-backup-and-restore)
- [Monitoring](#monitoring)
- [Upgrading](#upgrading)
- [Security](#security)
- [Troubleshooting](#troubleshooting)
- [The protocol](#the-protocol)
- [Developing on it](#developing-on-it)
- [Licence](#licence)

---

## What it stores

Per dataset: a table of immutable entries, each one a device identifier and a blob of bytes. And a
list of devices, so a reader can see which of their devices have been in touch.

That is genuinely all. There is no column for what an entry *is*, when the thing it describes changed,
or which version of anything is newer. Books are never uploaded.

**Nothing is ever overwritten and nothing is ever deleted.** Two devices that edit the same highlight
both get an entry, both survive, and the app decides what to do with them. That is what lets the
server stay this simple: it never has to arbitrate, so it never needs to understand.

## Quick start

### With Docker Compose

Needs Docker with the Compose plugin. Nothing else — no Go toolchain, no database to set up.

```bash
git clone https://github.com/CMDV-org/CMDV-Reader-sync-server.git
cd CMDV-Reader-sync-server

cp .env.example .env
openssl rand -base64 32          # one token per person who should have their own dataset
$EDITOR .env                     # put it in CMDV_SYNC_TOKENS

docker compose up -d
```

Check it:

```bash
curl http://127.0.0.1:8080/healthz
```

```json
{"ok":true}
```

Watch it:

```bash
docker compose logs -f
```

Stop it, keeping the data:

```bash
docker compose down
```

`docker compose down -v` also deletes the volume, and with it every reader's history. There is no
confirmation prompt.

By default the port is published on `127.0.0.1` only, so a fresh install is reachable from that
machine and nowhere else. This server does not terminate TLS, and the default configuration should not
be the one exposed to the internet. See [Ports and networking](#ports-and-networking).

### With Docker, without Compose

```bash
docker run -d \
  --name cmdv-sync-server \
  --restart unless-stopped \
  -p 127.0.0.1:8080:8080 \
  -v cmdv-sync-data:/data \
  -e CMDV_SYNC_TOKENS="$(openssl rand -base64 32)" \
  ghcr.io/marc2912/cmdv-reader-sync-server:main
```

Save that token somewhere before you lose the shell — the app needs it, and the server keeps only a
hash.

### Published images

| Tag | What it is | Platforms |
|---|---|---|
| `:main` | The newest commit on `main`, published only after the image has been built, started, and exercised in CI. | amd64 |
| `:sha-<12 chars>` | An exact commit. Immutable, so a deployment can pin one and roll back to it. | amd64 |
| `:latest`, `:X.Y.Z`, `:X.Y` | Tagged releases. | amd64, arm64 |

`:latest` is deliberately *not* moved by a push to `main` — pulling it gets you a release, not whatever
landed an hour ago. Until the first release is tagged, use `:main`.

arm64 images are built by the release workflow only, because a multi-platform build cannot be loaded
into the local daemon and therefore cannot be smoke-tested in the same job that builds it. For a
Raspberry Pi before the first release, build locally with `docker compose up -d --build`.

### As a binary, without Docker

Prebuilt binaries for macOS, Windows and Linux on amd64 and arm64 are attached to each
[release](https://github.com/CMDV-org/CMDV-Reader-sync-server/releases). They are single static
executables with no runtime to install and no dependencies.

```bash
export CMDV_SYNC_TOKENS="$(openssl rand -base64 32)"
export CMDV_SYNC_DATABASE=./cmdv-sync.sqlite
./cmdv-sync-server
```

On Windows, in PowerShell:

```powershell
$env:CMDV_SYNC_TOKENS = "paste-a-generated-token-here"
$env:CMDV_SYNC_DATABASE = ".\cmdv-sync.sqlite"
.\cmdv-sync-server.exe
```

### From source

Needs Go 1.26 or newer. Nothing else — the SQLite engine is compiled in, so there is no C toolchain
and no library to install.

```bash
go build ./cmd/cmdv-sync-server
CMDV_SYNC_TOKENS="$(openssl rand -base64 32)" CMDV_SYNC_DATABASE=./sync.sqlite ./cmdv-sync-server
```

## Configuration

Environment variables and no configuration file, deliberately: every setting is visible in the command
or compose file that started it, and there is nothing to mount, template, or forget to update.

| Variable | Default | What it does |
|---|---|---|
| `CMDV_SYNC_TOKENS` | *(required)* | Comma-separated bearer tokens. **Each token is an independent dataset.** Minimum 32 characters. |
| `CMDV_SYNC_TOKENS_FILE` | — | Path to a file with one token per line, which is what a Docker secret mounts. Use this *or* the above, never both. |
| `CMDV_SYNC_HOST` | `0.0.0.0` | Interface to bind. All interfaces by default, because inside a container binding to `localhost` makes the server unreachable from outside it. |
| `CMDV_SYNC_PORT` | `8080` | The only port used. |
| `CMDV_SYNC_DATABASE` | `/data/cmdv-sync.sqlite` | Where the database lives. **Back this file up.** |
| `CMDV_SYNC_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error`. One line per request at `info`: method, path, status and duration. Never a header, never a body. |

**Starting without a token fails.** There is no other authentication, so a server with no tokens would
be an open server. Exit code `78`.

Bad configuration is refused at startup with the offending variable named, rather than defaulted
around. A variable beginning `CMDV_SYNC_` that this server does not read produces a warning, so a typo
says so instead of silently doing nothing:

```
cmdv-sync-server: warning: CMDV_SYNC_DATABSE is set but is not read by this server.
```

At startup it prints exactly what it resolved. Tokens never appear — only the dataset identifier each
one grants access to, which is a hash:

```
cmdv-sync-server v2.0.0
listening on:  0.0.0.0:8080
database:      /data/cmdv-sync.sqlite
log level:     info
datasets:      2
               3f8a1c9d2e5b7a04
               b1c4d7e0a3f6928c

TLS is not terminated here. Put this behind a reverse proxy; do not expose it directly.
```

### Command-line flags

| Flag | What it does |
|---|---|
| `-version` | Print the version and exit. |
| `-health-check` | Probe `/healthz` on the configured port and exit `0` or `1`. This is what the container's health check runs, so it follows `CMDV_SYNC_PORT` without being told. |
| `-backup <path>` | Write a consistent copy of the database and exit. Safe while the server is running. See [backup](#your-data-backup-and-restore). |

## Ports and networking

| Port | Protocol | Purpose |
|---|---|---|
| `8080` | HTTP | The whole API. The only port this server uses. |

No admin interface, no metrics port, no second listener. The container declares `EXPOSE 8080`, which
documents the port and publishes nothing on its own.

The supplied compose file publishes it as `127.0.0.1:8080:8080`. Three reasonable ways to change that:

```yaml
# Reachable only from the Docker host. The default, and correct until a proxy is in front.
ports: ["127.0.0.1:8080:8080"]

# Reachable from your home network or VPN, over plain HTTP.
ports: ["8080:8080"]

# Not published at all: the proxy is another container on a shared Docker network and reaches
# this one by service name. The best arrangement if you already run a proxy in Docker.
# (Omit `ports` entirely and put both services on the same network.)
```

Do not publish it to the internet directly. The app refuses plain HTTP to a public address, so it
would not work anyway.

**One process per database.** Do not scale this to several replicas over one SQLite file. One container
is the supported arrangement and comfortably enough: a household's sync traffic is a few short bursts
a day.

## TLS and reverse proxies

**This server does not terminate TLS, and should not be exposed directly.**

Anyone self-hosting already runs a reverse proxy that handles certificates better than this ever
would, and half-doing it here would invite somebody to put it on the internet with a certificate
nobody renews.

### Caddy

Certificates are automatic, which makes this the shortest correct answer:

```caddyfile
sync.example.com {
	reverse_proxy 127.0.0.1:8080
}
```

### nginx

```nginx
server {
    listen 443 ssl;
    http2 on;
    server_name sync.example.com;

    ssl_certificate     /etc/letsencrypt/live/sync.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/sync.example.com/privkey.pem;

    # nginx defaults to 1 MB, and a first sync of a long reading history is larger than that.
    # Without this, a new device syncs for the first time and gets a 413 it cannot explain.
    # This server accepts bodies up to 16 MB.
    client_max_body_size 16m;

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Host              $host;
        proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_read_timeout 120s;
    }
}
```

### Traefik

As labels on the service in `docker-compose.yml`, with the port unpublished and both containers on a
shared network:

```yaml
labels:
  - "traefik.enable=true"
  - "traefik.http.routers.cmdv-sync.rule=Host(`sync.example.com`)"
  - "traefik.http.routers.cmdv-sync.entrypoints=websecure"
  - "traefik.http.routers.cmdv-sync.tls.certresolver=letsencrypt"
  - "traefik.http.services.cmdv-sync.loadbalancer.server.port=8080"
```

### Hosting it under a path

This server serves absolute paths under `/api/v1/`. If you host it at `example.com/cmdv-sync`, the
proxy must strip that prefix — `handle_path` in Caddy, a trailing slash on `proxy_pass` in nginx, a
`stripprefix` middleware in Traefik.

A prefix that is *not* stripped produces an honest `404` with a `malformedRequest` reason and a message
saying so, rather than a server error.

## Pointing the app at it

On the first device, enter three things: the server address, the token, and a passphrase.

- `https://sync.example.com` — through your proxy, from anywhere.
- `http://192.168.1.20:8080` — straight to the server on a home network or VPN.
- `http://sync.example.com` — **refused by the app.** Plain HTTP to a public address is not allowed.

On every device after that, scan the pairing code the first device shows. It carries the address, the
token and the passphrase, so nothing long is retyped.

The passphrase never reaches the server. It stays on the device and derives the key the app's payloads
are sealed with — which is why the server cannot read them. **Write it down.** Losing every device
without it means losing the synced history.

## Tokens, datasets and devices

**One token is one dataset.** Two readers sharing a server get one token each and are completely
isolated: neither can see the other's entries or even the other's device list.

There is no sign-up, no username and no password. A device becomes known by presenting a valid token,
so there is no registration step to go wrong. The name a reader gives a device appears in the device
list on their other devices, and updates whenever it syncs.

### Removing a device

Two situations, two answers, and the difference is worth understanding.

**A device you still hold.** Remove it from sync in the app. It clears its own copy of the token and
key, and delists itself from the server. Its entries stay, because your other devices may not have
replayed them yet.

**A device you have lost.** Rotate the token: change it in `.env`, `docker compose up -d`, and re-pair
the devices you still have. Every device using the old token is cut off.

Rotation is blunt on purpose. Every device in a dataset holds the same token, so the server *cannot*
tell one from another — anything holding the token can claim to be any device. A per-device revocation
button would look like security and would not be. Rotation is the mechanism that actually works.

> **Rotating a token starts a fresh dataset.** The app derives its encryption key from the token, so
> after a rotation the entries written under the old one can no longer be read, and the new token maps
> to a different dataset anyway. Devices re-upload their local state, so no *reader* data is lost — but
> treat rotation as a reset rather than as changing a password. Take a backup first.

## Your data: backup and restore

Everything is in one SQLite file — the one at `CMDV_SYNC_DATABASE`. There is no other state: no cache
to warm, no uploads directory, no secret generated at first run.

**Do not back it up by copying the file while the server is running.** The database uses write-ahead
logging, so at any moment the committed state is spread across the database and its log; a plain copy
can capture a torn combination that looks fine and restores broken.

The binary does it properly:

```bash
docker compose exec sync /cmdv-sync-server -backup /data/backup.sqlite
docker compose cp sync:/data/backup.sqlite ./cmdv-sync-$(date +%F).sqlite
docker compose exec sync rm /data/backup.sqlite
```

Without Docker:

```bash
./cmdv-sync-server -backup ./cmdv-sync-$(date +%F).sqlite
```

That produces a single consistent file, and it is safe against a server that is serving. It refuses to
overwrite an existing path.

> The runtime image has no shell and no `sqlite3`, which is exactly why backup is a subcommand. A
> backup procedure you cannot actually run is not a backup procedure.

### Restoring

```bash
docker compose down
docker run --rm -v cmdv-sync_sync-data:/data -v "$PWD":/restore alpine \
  sh -c 'rm -f /data/cmdv-sync.sqlite*; cp /restore/cmdv-sync-2026-08-04.sqlite /data/cmdv-sync.sqlite'
docker compose up -d
```

Deleting the `-wal` and `-shm` files alongside it is the part that matters: left over from the old
database, they would be replayed over the restored one.

After a restore, devices whose cursor is *ahead* of the restored history re-send what they hold, so
your own devices repopulate most of what the backup missed.

### Looking inside

You can see how much there is, and nothing about what it says:

```bash
docker compose cp sync:/data/cmdv-sync.sqlite ./peek.sqlite
sqlite3 ./peek.sqlite "SELECT dataset, count(*), sum(length(blob)) FROM entries GROUP BY dataset;"
sqlite3 ./peek.sqlite "SELECT dataset, device_id, name FROM devices;"
```

The blobs are sealed by the app. Neither the server nor `sqlite3` nor you can read them from here —
that is the point, and it is the one real cost of this design. Sync problems are diagnosed in the app,
where the data can actually be decrypted.

## Monitoring

Point whatever you use at `/healthz`:

```bash
curl -s http://127.0.0.1:8080/healthz     # {"ok":true}
curl -sI http://127.0.0.1:8080/healthz    # HEAD works too
```

| | |
|---|---|
| `200` | `{"ok":true}` |
| `500` | `{"ok":false}` |

No authentication, no version header, no redirect, and no side effects — it is safe to poll as often
as you like. It is **not** under `/api/v1/`, and that is deliberate: a monitor has no stake in the sync
protocol, so it cannot be made to go red by a protocol change or a stale version header.

**It touches the database.** One trivial query, bounded at two seconds. That is the difference between
"a process is accepting connections" and "this server can actually serve" — a wedged or lock-starved
database gives you a `500` instead of a green check above a queue of failing syncs, which is the
failure worth catching on replicated or network storage. Two seconds is far longer than any query this
server makes, and shorter than its own five-second database busy timeout, so an externally locked
database reports unhealthy rather than leaving your monitor to time out with nothing to show.

Successful probes are logged at `debug`, not `info`. A check every fifteen seconds is thousands of
identical lines a day, and a log nobody reads is the same as no log. A *failing* probe is logged
normally, with the database error attached, because that is the line you will come looking for.

With Prometheus blackbox_exporter:

```yaml
modules:
  cmdv_sync:
    prober: http
    http:
      valid_status_codes: [200]
      fail_if_body_not_matches_regexp:
        - '"ok"\s*:\s*true'
```

The body is a stable literal and is pinned by a fixture, so that regexp will not quietly stop matching
after an upgrade.

`/api/v1/health` still exists and also touches the database. It is the client-facing one: it reports
the protocol version, which is what lets the app distinguish "wrong address" from "bad token". Use
`/healthz` for monitoring and leave that one to the app.

## Upgrading

With Compose, tracking the published image:

```bash
docker compose pull && docker compose up -d
```

Building from source:

```bash
git pull && docker compose up -d --build
```

The schema migrates itself on startup, so there is no migration step and no downtime beyond the
restart. Take a backup first anyway.

The protocol is versioned. A version this server does not speak is refused with an explanation rather
than guessed at, so a newer app against an older server says so plainly instead of corrupting
anything — but keep them in step, because a refused version means no sync at all.

## Security

What is done, and what is deliberately not:

- **Tokens are pre-shared and never stored.** The server keeps only a hash, as the dataset identifier,
  so a stolen database yields no access. Comparison is constant-time over SHA-256 digests, so neither
  the token's length nor its position in the configured list is observable through response timing.
- **No password hashing, and none needed.** A 256-bit pre-shared token has no dictionary to attack.
  That is why there is no key derivation, no lockout and no rate limiting: there is no unauthenticated
  endpoint expensive enough to be worth abusing.
- **Datasets are isolated absolutely.** Every query is scoped by dataset. Asserted from both
  directions in the tests, and again against a running container in CI.
- **The server cannot read payloads.** Not "does not" — cannot. They are sealed by the app with a key
  derived from a passphrase that never leaves the device.
- **No SQL injection by construction.** Every value that reaches SQL is a bound parameter. The one
  exception is the backup destination in `VACUUM INTO`, which SQLite does not allow to be bound; it is
  an operator's command-line argument, quoted, and no client can influence it.
- **Logs hold nothing sensitive.** Method, path, status and duration. Never the `Authorization` header,
  never a body, never a token. That is not configurable.
- **Runs unprivileged**, on a read-only root filesystem, with `no-new-privileges`, in an image with no
  shell and no package manager.
- **Bounded input.** 16 MB per body, 1 MB per entry, 2000 entries per request, 128 bytes per
  identifier, and request timeouts, so a slow or malicious client cannot hold resources open.
- **No TLS here** — on purpose. See [TLS and reverse proxies](#tls-and-reverse-proxies).
- **No account recovery, because there are no accounts.** Lose the token and the passphrase and the
  data is unreachable. There is nowhere for a reset link to go, and inventing somewhere would mean
  running a mail path for a server whose whole appeal is having neither.

Found something? Open an issue, or mail the address on the repository owner's profile for anything
you would rather not file in public.

## Troubleshooting

**The container exits immediately.** Read the logs first: `docker compose logs`. Exit code `78` means
the configuration was refused and the message names the variable — most often no token at all. Exit
code `74` means the database could not be opened.

```
CMDV_SYNC_DATABASE names a directory '/data' that is not writable by this process
```

A bind mount whose host directory the container's user cannot write to. Either use a named volume (the
default in the supplied compose file, which does not have this problem), or match the ownership:

```bash
stat -c '%u:%g' /path/on/host   # e.g. 1000:1000
```

```yaml
user: "1000:1000"
```

**The app says it cannot connect.** Work outwards. On the server:
`curl http://127.0.0.1:8080/api/v1/health`. If that answers, the server is fine and the problem is the
proxy or the network. If the app reaches the address but refuses to sync, check it is `https://` for a
public address.

**`401` with `invalidCredentials`.** The token in the app does not match one in `CMDV_SYNC_TOKENS`.
Check for a trailing newline or a truncated paste — a base64 token from `openssl rand -base64 32` is 44
characters and ends in `=`.

**A device syncs but finds nothing.** Almost always a different token, which means a different dataset.
Compare the dataset identifiers in the startup banner against which token each device holds.

**`409` with `versionUnsupported`.** The app and the server disagree about the protocol version. Update
whichever is behind.

**`404` on every endpoint through the proxy.** The proxy is passing a path prefix through. See
[Hosting it under a path](#hosting-it-under-a-path).

**A first sync fails with `413` through nginx.** `client_max_body_size`. See [nginx](#nginx).

**A new device takes a while on its first sync.** Expected. It replays the whole history, because
nothing is ever compacted away. Ten seconds for years of reading is the trade, and it is a one-time
cost during setup.

## The protocol

Specified in [`spec/PROTOCOL.md`](spec/PROTOCOL.md), with one JSON fixture per message shape in
[`spec/fixtures/`](spec/fixtures/). Both this repository and the app assert that decoding a fixture and
re-encoding it reproduces the file byte for byte, which is how two implementations in two languages are
kept in step without either importing the other's code.

That spec is deliberately complete enough to implement against. **If you would rather write your own
server, you can** — the app will talk to it, and nothing in the protocol assumes this implementation.

Three properties carry the whole design:

- **The server does not understand the payloads.** An entry is a device identifier and opaque bytes. A
  newer app can introduce anything it likes and this server will store and relay it unchanged.
- **The sequence number is the only ordering, and no clock is involved.** Every device replays the same
  sequence in the same order and reaches the same state, so clock skew is not a problem to be solved
  here — it is not a concept that exists.
- **Nothing is overwritten.** Two devices' versions of one record both survive, so the server never
  arbitrates, so it never needs to know what it is holding.

### Endpoints

| Method | Path | What it does |
|---|---|---|
| `POST` | `/api/v1/sync` | Push entries and pull what came from elsewhere. An empty `entries` is a pull. |
| `GET` | `/api/v1/devices` | List the devices in this dataset. |
| `DELETE` | `/api/v1/devices/{deviceID}` | Delist a device. Its entries are kept. |
| `GET` | `/api/v1/health` | Unauthenticated. Reports the protocol version, so a client can tell a version mismatch from a bad token. |
| `GET` | `/healthz` | Unauthenticated, unversioned. `{"ok":true}` or `{"ok":false}`. What to point a monitor at. |

Every request carries `X-CMDV-Sync-Version: 2`; a mismatch is refused with `409`. A request with no
version header is treated as the current version, so a plain `curl` against health is not refused.

Every failure names a machine-readable reason alongside its prose, because a client's next step differs
between them and cannot be told from a status code: `invalidCredentials`, `versionUnsupported`,
`malformedRequest`, `serverError`.

Trying it by hand:

```bash
TOKEN='paste-your-token'

curl -sS -X POST http://127.0.0.1:8080/api/v1/sync \
  -H 'Content-Type: application/json' \
  -H 'X-CMDV-Sync-Version: 2' \
  -H "Authorization: Bearer $TOKEN" \
  -d '{"deviceID":"test-device","deviceName":"curl","cursor":0,
       "entries":[{"blob":"aGVsbG8gd29ybGQ="}]}'
```

## Developing on it

```bash
go test ./...          # the whole suite, in about a second
go vet ./...
gofmt -l .             # silence is success
```

Nothing needs starting by hand. Store tests use a temporary database, and the HTTP tests run the real
routes over a real socket via `httptest`, so there are no mocks on either side.

```bash
go test ./internal/api -run TestFixtures -update    # after a deliberate wire-format change
```

CI runs the suite on Linux, macOS and Windows with `-race`, cross-compiles all six release targets,
builds the image and then actually exercises it — registering a device, checking dataset isolation,
running the backup subcommand, and timing the graceful stop.

### Layout

```
cmd/cmdv-sync-server/   Read the environment, open the store, listen, shut down cleanly
internal/config/        Six variables, validated, with refusals that name the variable
internal/auth/          Constant-time lookup, token → dataset
internal/store/         Two tables; append and page. Bound parameters only
internal/api/           Five routes, and the wire types
spec/                   The normative protocol and its fixtures
```

`internal/` because nothing here is a library for anyone else to import, and the compiler should
enforce that rather than a convention. The one thing that *is* public is `spec/`.

**One dependency:** [`modernc.org/sqlite`](https://gitlab.com/cznic/sqlite), a pure-Go SQLite. That is
what makes `CGO_ENABLED=0` work, which is what makes both the six-target cross-compile and the
near-empty container image possible. Everything else — HTTP routing, JSON, hashing, constant-time
comparison, structured logging — is the standard library.

## Licence

[Mozilla Public License 2.0](LICENSE), like the app.
