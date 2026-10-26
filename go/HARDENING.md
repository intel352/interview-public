# Go hardening

The request limiter remains a small, independent middleware. This document covers
the additional security and reliability work, including changes to starter
behavior. Java is unchanged; no identity provider or external service is required.

## Local-demo boundary and access policy

- The server binds only numeric loopback IP addresses (`127.0.0.0/8` or `::1`).
  Wildcard, nonloopback, and hostname addresses are rejected before opening data.
- After a successful bind, startup creates a 256-bit random bearer token and
  prints it once to the local operator. The token stays in memory and changes
  after restart. There is no fixed password, token file, or persistent credential.
- Every client holding that token acts as `local-demo` with full access to one
  shared workspace. Email-shaped bearer strings no longer authenticate. Empty
  authentication configuration denies protected routes. Duplicate authorization
  headers are rejected, and token comparison uses constant-time comparison.
- `/health` remains public. The global leaky bucket still counts every client,
  route, and authentication outcome. Its capacity, rate, and rejection policy are
  unchanged. Access logging wraps it, including `429` responses.
- Startup output and the operating-system account must be trusted. This boundary
  does not protect against a compromised host, a local administrator, or direct
  filesystem access. Do not expose the service through a proxy or tunnel and call
  it production authentication. A multi-user service needs a real identity and
  authorization design; adding one is not simulated here.

## Input and response rules

| Input | Go policy |
|---|---|
| Create/update contact | `application/json`, at most 1 MiB, one non-null object |
| Contact import | `application/json`, at most 8 MiB, array decoded incrementally, at most 10,000 contacts |
| Writable contact fields | `first_name`, `last_name`, `email`, `phone`, `company`; IDs/timestamps and unknown fields rejected |
| Names/company | At most 256 Unicode code points per field |
| Phone | At most 64 Unicode code points |
| Email | Optional; when present, a plain parsed address of at most 254 bytes |
| Contact controls | Control characters rejected; other fields may remain empty |
| Upload | Exactly one `file` part, at most 100 MiB content; total multipart body at most 101 MiB |
| Filename | Nonempty, at most 255 bytes, no path separators or control characters |

Malformed, unknown-field, trailing, or invalid-shape JSON returns `400`.
Unsupported media types return `415`; byte limits return `413`. Import validation
finishes before any database mutation. Pagination and date queries are bounded
and invalid values are rejected rather than silently treated as defaults.

CSV export prefixes potentially formula-interpreted cells with an apostrophe,
including formula markers following whitespace/control prefixes. Stored values
remain unchanged. This is an explicit spreadsheet-safe export policy, so exported
text may differ from stored text. Consumers requiring exact values can use JSON.

Downloads use a safely formatted attachment filename, validated content type,
`nosniff`, and a sandbox content-security policy. Stored file size and regular-file
status are checked before sending headers, and `Content-Length` is set. Failed
JSON/CSV/file transmission aborts the response rather than appending a second
status or presenting a partial chunked body as complete. Error logs omit request
bodies and credentials. Responses are marked `Cache-Control: no-store`.

## Storage, concurrency, and audit

SQLite has a single local connection, including idle connections. This serializes
local database work and lets callers waiting for the connection honor context
cancellation. WAL and a 5-second busy timeout are configured on each connection,
including replacements. The timeout is bounded mitigation for other processes,
not a guarantee that arbitrary external writers cannot cause contention.

Contact creation, update, deletion, import, and file metadata insertion commit
their mutation and audit entry in the same transaction. An audit failure rolls
back the mutation. Transaction callbacks use the transaction connection rather
than reacquiring the single pool connection. Updates return the original creation
timestamp. Activity reports have stable ordering.

Downloads, exports, and reports record `download_attempt`, `export_attempt`, and
`report_attempt` before sending output. Audit failure prevents delivery. These
events record attempts, not proof that a client received the entire response.

New data directories use `0700`; new database and upload files use `0600`.
Existing permissions are preserved, so inspect them when reusing old data. Stored
file IDs are restricted to safe characters and files are created exclusively:
duplicate IDs cannot overwrite an existing blob. Copy, cancellation, size,
sync, close, metadata, and audit failures are checked. Failed attempts remove
only files created by that attempt, and cleanup failures are surfaced.

Filesystem content and SQLite are not one crash-atomic transaction. Ordinary
failures are cleaned up, but a crash or power loss can still require recovery of
orphaned content or inconsistent metadata/directory entries. Backups and recovery
tooling would be a separate production requirement. Cancellation is checked
between file reads; an arbitrary reader already blocked inside `Read` must
cooperate to be interrupted. No leaking background goroutine is used to work
around that limitation.

## Server and test lifecycle

The server sets a 5-second header deadline, 30-second read/write deadlines,
60-second idle timeout, 16 KiB header limit, and a 30-second request context.
The write deadline includes request-body time; uploads do not get a second
30-second response budget. These are deliberate local-demo limits, not tuned
production upload settings.

Shutdown first stops acceptance and allows five seconds for active handlers to
finish. If that expires, request contexts are canceled, connections are forcibly
closed, and the error is reported. Unexpected listener failures also close
accepted connections. Startup, serving, shutdown, and store-close errors reach
the process exit status. Cleanup runs before `os.Exit`.

Tests and benchmarks use generated tokens and private temporary stores. Benchmark
requests assert expected HTTP status and check request construction, body writes,
drains, and closes. Their unwrapped router deliberately measures CRM handlers;
separate tests and the launched-server smoke exercise the global limiter.

## Verification

From `go/`, using Go 1.27.3:

```bash
go test -count=1 ./...
go test -race -count=1 ./...
go test -run '^$' -bench . -benchtime=1x -count=1 .
go vet ./...
go build -o bin/server ./cmd/server
go fix -diff ./...
golangci-lint run --no-config ./...
govulncheck -test ./...
```

Use golangci-lint 2.15.0 or a compatible official version built with Go 1.27 or
newer. The repository has no custom lint exclusions or weakened rule set.
The dependency scan used official govulncheck 1.9.0 built with Go 1.27.3;
an older scanner built with Go 1.26 could not parse the current toolchain.
Regressions cover authentication, body/field/import boundaries, audit rollback,
duplicate-file preservation, cleanup/cancellation, concurrent storage and HTTP
workflows, deadlines, graceful/forced shutdown, response failures, and the
original limiter boundaries. Race checks cover executed paths; passing them is
not a proof that every possible race or vulnerability is absent.
