# Go security and reliability design

The request limiter remains independent middleware. The broader Go changes cover
actor ownership, HTTP/resource boundaries, storage consistency and lifecycle.
Java is unchanged. See [compatibility review](COMPATIBILITY.md) for behavior
changes from the starter and [API testing](API_TESTING.md) for scenario evidence.

## Local actors and ownership

The API and health servers bind only numeric loopback addresses. After both bind,
startup generates one 256-bit random token per configured actor and prints each
once to the local operator. Defaults are `alice@example.com,bob@example.com`;
`--actors` accepts 1–64 case-sensitive labels, each at most 254 UTF-8 bytes without
whitespace, commas or controls. They are local labels, not verified email claims.
Tokens stay in memory and rotate on restart. Email-shaped strings do not grant
access. Duplicate authorization headers and invalid/empty mappings fail closed;
the immutable middleware snapshot compares credentials in constant time.

An authenticated actor is carried into every contact/file ownership and audit
operation. The schema has an actor registry, non-null ownership columns and
foreign keys enabled on every SQLite connection. The server derives owners;
client-supplied metadata is rejected. Lists, CRUD, imports, exports, file metadata,
downloads and reports filter by the current actor. Foreign resource IDs yield the
same not-found response as absent IDs. Actor-bound cursors cannot be directly
reused by another actor; cursor tampering never removes the owner filter.

Only identity labels persist. Reconfiguring the same actor after restart gives
access with a fresh token; omitting it from configuration grants no credential.
The local operator sees every startup credential and can access SQLite/files.
This does not provide isolation from that operator or a compromised host. Do not
expose the demo through a proxy/tunnel and call it production authentication.

## HTTP listeners and limiting

The business API defaults to `127.0.0.1:8080`; health defaults to the same IP and
API port + 1. `--health-addr` overrides it. API port `0` requests independent
OS-assigned ports for both listeners. Port `65535` requires an explicit override.
IPv6 loopback works. Invalid/colliding binds fail before any credential output;
a partial bind is closed.

The health listener exposes only public `GET /health`, returning process liveness
without database details. It has no business handlers and never uses the API
limiter. The API listener has no health handler. One global leaky bucket on the
API listener defaults to one request per six seconds across all actors and
outcomes. Count, period and capacity are startup flags; larger capacity permits
bursts. Invalid settings fail before resources open. Rejected requests return
non-cacheable JSON `429` and rounded-up `Retry-After` until one more request fits.
It limits admissions, not concurrent in-flight handlers or fairness. See the
[configuration contract](../README.md#request-limiting-go) for precision and bounds.

Both servers set 5-second header, 30-second read/write/request and 60-second idle
deadlines, with a 16 KiB header limit. The write budget includes body reading.
These fixed local-demo limits can reject legitimate slow transfers. Both listeners
stop together on cancellation or server failure. Cancellation allows active
handlers up to five seconds to finish; expiration cancels requests and forcibly
closes connections. A server whose serving loop fails closes its own connections
immediately; its peer follows the graceful cancellation path. Server loops are
joined before shared storage closes. Handlers/readers must cooperate with
cancellation; no unbounded background-copy workaround is introduced.

## Input and response rules

| Input | Policy |
|---|---|
| Create/update contact | application/json, at most 1 MiB, one object |
| Contact import | application/json, at most 8 MiB, streaming array, at most 10,000 objects |
| Writable fields | Exactly `first_name`, `last_name`, `email`, `phone`, `company`; no duplicate/case-folded keys or null values |
| Missing fields / PUT | Empty strings allowed; PUT replaces all writable fields, so omitted fields clear |
| Names/company | At most 256 Unicode code points each |
| Phone | At most 64 Unicode code points |
| Email | Optional plain parsed address, at most 254 bytes |
| Contact controls | Rejected |
| Upload | Exactly one `file` part, at most 100 MiB content; multipart body at most 101 MiB |
| Filename | Go's multipart parser first takes the platform basename (forward-slash paths on macOS); the resulting name must be nonempty, at most 255 bytes, with no remaining separators or controls. Storage always uses a server-generated UUID. |

Invalid JSON/shape/fields returns `400`, unsupported media `415`, excessive bytes
`413`. Imports finish validation before mutation. Query limits/date formats are
validated. Empty collections are arrays. Contact/file responses include read-only
`owner_id`; a GET object must be projected to writable fields before PUT/import.

CSV prefixes potentially formula-interpreted cells with an apostrophe, including
markers after whitespace/control prefixes. Stored values do not change. This
also changes some phone/text representations; use JSON for exact values. CSV is
not a lossless backup and live keyset pagination is not a snapshot.

Downloads validate regular-file status and size before headers and use a safely
formatted attachment filename, validated media type, Content-Length, `nosniff`
and sandbox policy. Responses are non-cacheable. Failed JSON/CSV/file writes
abort instead of completing a partial body or appending another status. Logs omit
bodies and credentials; startup token output must remain private.

## SQLite, timestamps, audit and seed

One SQLite connection serializes local database operations; waiting callers can
cancel. WAL and a 5-second busy timeout apply to each replacement connection.
Other processes can still cause bounded contention. Transaction callbacks use
the transaction connection rather than reacquiring the single pool connection.

Each mutation and its audit entry commit together. Audit failure rolls back the
mutation. Read delivery actions retain the original `export`, `download`, `view`
labels and are recorded before output; they represent attempts, not guaranteed
receipt. Validation/authorization/missing-resource failures add no successful
operation audit. Reports compute a snapshot before recording their own `view`,
which appears on a later request. Reports are actor-filtered with UTC cutoffs.

Schema version 2 stores fixed-width UTC timestamps with nine fractional digits.
SQLite defaults and Go bindings use the same format; schema checks reject mixed
representations. This preserves nanoseconds, indexed chronological tuple ordering
and exact inclusive report cutoffs. Live pagination survives deletion of its
anchor, filters every page by actor and omits later inserts before that cursor.

Startup does not infer initialization from contact count. Seeding uses one SQLite
transaction for actors, contacts, metadata, activity and an explicit completion
marker. Existing application data does not masquerade as a completed seed; zero
contacts are valid. Completed seeding is a no-op. Normal failure/cancellation
rolls back all seed rows and removes only newly created files, making retry safe.
Seeded activity includes 5,000 explicitly synthetic historical events, not proof
that corresponding business requests occurred.

New directories use `0700`; database/blobs use `0600`. Existing permissions are
preserved. File IDs are validated and files created exclusively. Copy, sync,
close, size, cancellation, metadata and audit errors are checked; duplicate IDs
cannot overwrite another blob. Cleanup removes only files owned by that attempt.
SQLite and files cannot be one crash-atomic transaction: abrupt process/power
loss can leave orphaned blobs requiring recovery. No power-loss durability claim
is made.

Legacy/shared or unsupported intermediate schemas are rejected read-only before
schema/WAL changes. Ownership cannot reliably be reconstructed from old audit
history, so no automatic assignment or shared fallback is provided. Back up old
data and use a fresh directory or a separately reviewed explicit offline migration.

## Verification

```bash
go test -count=1 ./...
go test -race -count=1 ./...
make test-bdd
go test -run '^$' -bench . -benchtime=1x -count=1 .
go vet ./...
go build -o bin/server ./cmd/server
go fix -diff ./...
golangci-lint run --no-config ./...
govulncheck -test ./...
```

Use Go 1.27.3 and a compatible official linter (validation uses golangci-lint
2.15.0 built with Go 1.27.0). The official vulnerability scanner is govulncheck
1.9.0 built with Go 1.27.3. No linter exclusions or weakened rule set were added.
Tests and benchmarks check actual statuses/errors and clean up before os.Exit.
Passing race tests or advisory scans does not establish absence of every possible
race or vulnerability; explicit coverage and exclusions are in API_TESTING.md.
