# CRM API Server

A simple CRM-style API server for managing contacts and files. Available in both Go and Java implementations with identical API interfaces.

## Implementations

- **Go**: Located in `go/` directory
- **Java**: Located in `java/` directory (Spring Boot)

Both implementations share the same SQLite database schema and provide identical REST API endpoints.

## Quick Start

### Go

Requires Go **1.27.3** or newer (`go version`).

```bash
cd go
make seed   # Seed the database with test data
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

The server runs on `http://localhost:8080` by default.

## Authentication

All `/api/*` endpoints require a bearer token in the `Authorization` header:

```bash
curl -H "Authorization: Bearer user@example.com" http://localhost:8080/api/contacts
```

The token should be a valid email address. It serves as the user identifier.

## Request limiting (Go)

The Go server applies one **process-local leaky bucket** shared by every client
and route, including `/health`, unauthenticated requests, and requests that fail.
It admits at most **10 requests per minute** by draining one request every six
seconds. The Java implementation is unchanged.

Assumptions and behavior:

- **Capacity: one request.** The first request passes immediately. Another can
  pass exactly six seconds later. Idle time does not accumulate burst capacity.
- **Overflow: reject immediately.** A full bucket returns `429 Too Many Requests`,
  `Content-Type: application/json`, and `{"error":"rate limit exceeded"}`. No
  requests are queued or delayed. This is leaky-bucket policing, not a queued
  traffic shaper.
- `Retry-After` is the remaining drain time rounded up to whole seconds. It is
  advisory: another client may take the next available slot. Rejected requests
  add no work and do not postpone the next admission.
- Limiting runs before routing and authentication, so `429` takes precedence
  while the bucket is full. Even a health check consumes the shared allowance.

The bucket stores pending work as a duration. Each admitted request adds six
seconds; elapsed time drains it down to zero. A mutex makes draining and admission
atomic across clients. The clock is read under that lock, and handlers execute
after it is released. Production uses Go's monotonic clock through `time.Now`;
tests inject time so boundaries do not need sleeps. No background worker or new
dependency is needed.

This limits admissions, not simultaneous in-flight work: slow handlers can
overlap. There is no per-client fairness, state resets on restart, and separate
server processes have independent limits. A larger burst capacity would change
the strict spacing policy; per-client or distributed limits would require a
different state scope. The server creates the wrapper once in `cmd/server`;
`NewRouter` remains reusable without limiting so the existing benchmarks continue
to measure CRM handlers.

Run the tests from `go/`:

```bash
go test ./...
go test -race ./...
go vet ./...
```

Coverage includes exact six-second boundaries, fractional `Retry-After` rounding,
idle and backward-clock behavior, concurrent clients sharing one bucket, all-route
coverage, and rejected writes leaving the real SQLite store unchanged.

## Endpoints

### Health

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/health` | Health check (no auth required) |

### Contacts

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/api/contacts` | List contacts (paginated) |
| `GET` | `/api/contacts/:id` | Get a contact |
| `POST` | `/api/contacts` | Create a contact |
| `PUT` | `/api/contacts/:id` | Update a contact |
| `DELETE` | `/api/contacts/:id` | Delete a contact |
| `POST` | `/api/contacts/import` | Bulk import contacts (JSON array, max 10k) |
| `GET` | `/api/contacts/export` | Export all contacts as CSV |

### Files

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/api/files` | List files |
| `POST` | `/api/files` | Upload a file (multipart form, max 100MB) |
| `GET` | `/api/files/:id` | Download a file |

### Reports

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/api/reports/activity` | Activity report (last 30 days by default) |

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
curl -H "Authorization: Bearer user@example.com" \
  "http://localhost:8080/api/contacts?page_token=eyJjcmVhdGVkX2F0Ijoi..."
```

## Examples

```bash
# List contacts (first page)
curl -H "Authorization: Bearer user@example.com" \
  http://localhost:8080/api/contacts?limit=10

# Create a contact
curl -X POST -H "Authorization: Bearer user@example.com" \
  -H "Content-Type: application/json" \
  -d '{"first_name":"Jane","last_name":"Doe","email":"jane@example.com","phone":"555-1234","company":"Acme"}' \
  http://localhost:8080/api/contacts

# Upload a file
curl -X POST -H "Authorization: Bearer user@example.com" \
  -F "file=@/path/to/file.pdf" \
  http://localhost:8080/api/files

# Bulk import
curl -X POST -H "Authorization: Bearer user@example.com" \
  -H "Content-Type: application/json" \
  -d '[{"first_name":"A","last_name":"B","email":"a@b.com","phone":"555","company":"X"}]' \
  http://localhost:8080/api/contacts/import

# Export contacts
curl -H "Authorization: Bearer user@example.com" \
  http://localhost:8080/api/contacts/export > contacts.csv
```

## Seeding Options

### Go

```bash
cd go
make seed                                      # Default: 10k contacts, 20 files
go run ./cmd/server --seed --contacts=50000    # Custom contact count
go run ./cmd/server --seed --files=100         # Custom file count
```

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
| `--addr` | `:8080` | Server listen address |
| `--data` | `data` | Data directory for database and files |
| `--seed` | `false` | Seed database with test data |
| `--contacts` | `10000` | Number of contacts to seed |
| `--files` | `20` | Number of files to seed |

### Java

Configuration is in `src/main/resources/application.properties`:

| Property | Default | Description |
|----------|---------|-------------|
| `server.port` | `8080` | Server listen port |
| `app.data-dir` | `data` | Data directory for database and files |
| `app.max-upload-size` | `104857600` | Max file upload size (100MB) |
