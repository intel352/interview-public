# CRM API Server

A simple CRM-style API server for managing contacts and files. Available in both Go and Java implementations with the same core API routes.

## Implementations

- **Go**: Located in `go/` directory
- **Java**: Located in `java/` directory (Spring Boot)

Both implementations provide the core CRM routes. Go now has actor-owned data, a separate health listener and stricter validation; its database schema is no longer interchangeable with Java. See [Go hardening](go/HARDENING.md), [API testing](go/API_TESTING.md) and [compatibility review](go/COMPATIBILITY.md). Java is unchanged.

The original prework requested the global leaky bucket. This branch also includes
later requested work; [scope and rationale](go/SCOPE.md) separates the assignment,
its supporting tests, and the expanded hardening, ownership and health features.

## Quick Start

### Go

Requires Go **1.27.3** or newer (`go version`).

```bash
cd go
make seed   # Optional: seed actor-owned synthetic data
make run    # Start the server
```

Or without make:

```bash
cd go
go run ./cmd/server --seed
go run ./cmd/server
```

### Java

```bash
cd java
make seed   # Seed the database with test data
make run    # Start the server
```

Or without make:

```bash
cd java
./mvnw spring-boot:run -Dspring-boot.run.arguments="--app.seed=true"
./mvnw spring-boot:run
```

Go serves the business API at `http://127.0.0.1:8080` and public liveness at `http://127.0.0.1:8081/health`. Both accept only numeric loopback addresses. A fresh empty database is usable without seeding. Java retains its original configuration.

## Authentication

### Go: configured local actors

At startup, the server prints an independent random bearer token for each actor
**after both listeners bind**. The defaults are `alice@example.com` and
`bob@example.com`. Configure explicit labels with, for example:

```bash
go run ./cmd/server --actors=alice@example.com,bob@example.com
```

Labels are case-sensitive local configuration, not verified email identities.
Copy one actor's token from the private startup terminal:

```bash
read -r -s TOKEN   # Paste that actor's token, then press Enter
curl -H "Authorization: Bearer $TOKEN" http://127.0.0.1:8080/api/contacts
```

Each credential carries its actor into contact/file ownership and audit rows.
Lists, individual resources, imports, exports, files and reports are restricted
to that actor. Another actor's resource ID returns the same `404` as a missing ID.
Tokens stay in memory, change on restart, and are absent from data/access logs.
`POST /api/auth/token` returns the authenticated `user_id`; it does not issue tokens.
Keep startup output private and run `unset TOKEN` when finished. An email label
alone never authenticates. The local operator can see every startup credential;
this is a local demonstration, not an external identity provider.

Legacy shared databases are rejected without automatic ownership assignment.
Keep the old directory intact and use a fresh `--data` directory. See the
[compatibility and migration policy](go/COMPATIBILITY.md).

### Java

Java is unchanged: it accepts a valid email address as its demo bearer token,
for example `Authorization: Bearer user@example.com`.

## Request limiting (Go)

The Go server applies one **process-local leaky bucket** shared by every client
on the business listener, including unauthenticated, unknown-route and failed requests. The separate health listener bypasses this limiter.
The defaults remain **10 requests per minute, capacity one**: one admission every
six seconds. The Java implementation is unchanged.

Runtime settings (command-line flags only; restart to change them):

| Flag | Default | Meaning |
|---|---|---|
| `--rate-limit-requests` | `10` | Positive whole requests drained per period |
| `--rate-limit-period` | `1m` | Positive Go duration, such as `1s`, `500ms` or `2m` |
| `--rate-limit-capacity` | `1` | Positive whole requests the bucket can hold; maximum immediate burst when empty |

For example, from `go/`:

```bash
go run ./cmd/server --rate-limit-requests=3 --rate-limit-period=1s --rate-limit-capacity=2
```

This allows two immediate admissions when empty, then drains at three requests
per second. The interval is derived from period/count, not a separate setting.
Fractional intervals such as one third of a second retain their rational drain
rate; admission is evaluated at nanosecond clock resolution. Capacity greater
than one permits bursts, so the configured count is not a strict request ceiling
for each fixed or rolling period.

Invalid explicit settings fail startup before opening data/listeners or printing
credentials. Count/capacity must fit signed 64-bit integers; the period must fit a Go
duration. Period text that would lose fractional nanoseconds (such as `1.5ns`)
is rejected; exact fractional durations such as `1.5s` work. Rates faster than
one request per nanosecond and configurations whose
`capacity × period-in-nanoseconds` exceeds `9223372036854775807` are rejected.
Zero, negative, malformed and overflowing values never silently use defaults.
There are no limiter environment variables, config files or hot reload.

Assumptions and behavior:

- **Default capacity: one request.** The first request passes immediately. Another
  can pass exactly six seconds later. With larger capacity, idle time can empty
  the bucket but never allow more than the configured capacity in an immediate burst.
- **Overflow: reject immediately.** A full bucket returns `429 Too Many Requests`,
  `Content-Type: application/json`, and `{"error":"rate limit exceeded"}`. No
  requests are queued or delayed. This is leaky-bucket policing, not a queued
  traffic shaper.
- `Retry-After` is the time until one more request fits, rounded up to whole seconds. It is
  advisory: another client may take the next available slot. Rejected requests
  add no work and do not postpone the next admission.
- Limiting runs before routing and authentication, so `429` takes precedence
  while the bucket is full. Health probes on the separate health listener remain available and never consume the API allowance.

The bucket stores scaled integer work: an admission adds `period-in-nanoseconds`
units, and each elapsed nanosecond drains `requests` units, down to zero. This
avoids floating-point error and rounding each interval. A mutex makes draining
and admission atomic across clients. The clock is read under that lock, and
handlers execute after it is released. Production uses Go's monotonic clock
through `time.Now`; tests inject time so boundaries do not need sleeps. No
background worker or new dependency is needed.

This limits admissions, not simultaneous in-flight work: slow handlers can
overlap. There is no per-client fairness, state resets on restart, and separate
server processes have independent limits. A larger burst capacity changes
the default strict spacing policy; per-client or distributed limits would require a
different state scope. The server creates the wrapper once in `cmd/server`;
`NewRouter` remains reusable without limiting so the existing benchmarks continue
to measure CRM handlers.

Run the tests from `go/`:

```bash
go test ./...
go test -race ./...
go vet ./...
```

Coverage includes default six-second boundaries, varied rates and capacities,
fractional drain and `Retry-After` rounding, invalid configuration, idle and
backward-clock behavior, concurrent authenticated actors sharing one bucket,
health isolation, and rejected writes leaving the real SQLite store unchanged.

## Endpoints

### Health

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/health` | Public liveness on the health listener only; no database readiness claim |

### Authentication (Go business listener)

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/api/auth/token` | Verify the current actor credential |

### Contacts

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/api/contacts` | List contacts (paginated) |
| `GET` | `/api/contacts/:id` | Get a contact |
| `POST` | `/api/contacts` | Create a contact |
| `PUT` | `/api/contacts/:id` | Update a contact |
| `DELETE` | `/api/contacts/:id` | Delete a contact |
| `POST` | `/api/contacts/import` | Bulk import contacts (JSON array, max 10k) |
| `GET` | `/api/contacts/export` | Export this actor's contacts as spreadsheet-safe CSV |

### Files

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/api/files` | List files |
| `POST` | `/api/files` | Upload a file (multipart form, max 100 MiB file) |
| `GET` | `/api/files/:id` | Download a file |

### Reports

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/api/reports/activity` | This actor's report (last 30 UTC days by default; optional `since=YYYY-MM-DD`) |

## Pagination

The `/api/contacts` endpoint uses cursor-based pagination. The response includes a `next_page_token` field when more results are available:

```json
{
  "contacts": [...],
  "next_page_token": "eyJjcmVhdGVkX2F0Ijoi..."
}
```

Pass the token as a query parameter to fetch the next page:

```bash
curl -H "Authorization: Bearer $TOKEN" \
  "http://127.0.0.1:8080/api/contacts?page_token=eyJjcmVhdGVkX2F0Ijoi..."
```

Pagination is a live descending `(created_at, id)` walk, not a snapshot. A cursor
is scoped to its actor, still works if its anchor is deleted, and omits new rows
inserted before that position. Do not reuse another actor's cursor. Empty lists
are arrays; the final page omits `next_page_token`.

## Examples (Go)

Use the startup `TOKEN` above. With default limiter settings, leave at least six seconds between requests, or honor `Retry-After`. JSON mutations require `Content-Type: application/json`. Send only the five writable contact fields shown below; `PUT` replaces omitted fields with empty strings. Metadata such as `id`, `owner_id` and timestamps is read-only. [API testing](go/API_TESTING.md) includes a safe read-modify-write example.

```bash
# List contacts (first page)
curl -H "Authorization: Bearer $TOKEN" \
  "http://127.0.0.1:8080/api/contacts?limit=10"

# Create a contact
curl -X POST -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"first_name":"Jane","last_name":"Doe","email":"jane@example.com","phone":"555-1234","company":"Acme"}' \
  http://127.0.0.1:8080/api/contacts

# Upload a file
curl -X POST -H "Authorization: Bearer $TOKEN" \
  -F "file=@/path/to/file.pdf" \
  http://127.0.0.1:8080/api/files

# Bulk import
curl -X POST -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '[{"first_name":"A","last_name":"B","email":"a@b.com","phone":"555","company":"X"}]' \
  http://127.0.0.1:8080/api/contacts/import

# Export contacts
curl -H "Authorization: Bearer $TOKEN" \
  http://127.0.0.1:8080/api/contacts/export > contacts.csv
```

CSV protects spreadsheet cells and may prefix values such as `+15551234` with an apostrophe. It is not a lossless backup. Use paginated JSON to obtain exact stored text and full timestamp precision.

## Seeding Options

### Go

```bash
cd go
make seed                                      # Default totals: 10k contacts, 20 files across configured actors
go run ./cmd/server --seed --contacts=50000    # Custom contact count
go run ./cmd/server --seed --files=100         # Custom file count
```

Seeding commits contacts, file metadata, activity and a completion marker together. A completed seed is a no-op on repetition; contact count is not an initialization marker. Zero-contact seeds and empty databases can serve normally. Ordinary failure/cancellation rolls back seed rows and cleans up owned files; retry is safe. Power-loss filesystem recovery is a separate limitation.

### Java

```bash
cd java
make seed                                                                              # Default: 10k contacts, 20 files
./mvnw spring-boot:run -Dspring-boot.run.arguments="--app.seed=true --contacts=50000"  # Custom contact count
```

To reset the database:

```bash
cd go && make reset   # Go
cd java && make reset # Java
```

## Benchmarks (Go only)

```bash
cd go
make bench
```

## Configuration

### Go

| Flag | Default | Description |
|------|---------|-------------|
| `--addr` | `127.0.0.1:8080` | Numeric loopback address only (IPv4 or IPv6) |
| `--health-addr` | API port + 1 | Separate numeric loopback liveness address; API port `0` uses a second ephemeral port |
| `--actors` | `alice@example.com,bob@example.com` | 1–64 case-sensitive actor labels; independent ephemeral credentials |
| `--data` | `data` | Actor-owned database and files; legacy shared schemas rejected |
| `--seed` | `false` | Seed database with test data |
| `--contacts` | `10000` | Number of contacts to seed |
| `--files` | `20` | Number of files to seed |

API port `65535` requires an explicit health address; colliding listeners fail startup before credential output. IPv6 loopback is supported, for example `--addr='[::1]:8080'`. Both listeners drain and close together on shutdown or server failure.

### Java

Configuration is in `src/main/resources/application.properties`:

| Property | Default | Description |
|----------|---------|-------------|
| `server.port` | `8080` | Server listen port |
| `app.data-dir` | `data` | Data directory for database and files |
| `app.max-upload-size` | `104857600` | Max file upload size (100MB) |

Run `make test-api`, `make test-bdd` or `make test-race` from `go/` for isolated native API testing; no manually managed server or credentials are needed. The [coverage matrix](go/API_TESTING.md#coverage-matrix) names the scenarios and exclusions.
