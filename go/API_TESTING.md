# Testing the Go API

See [scope and rationale](SCOPE.md) for the distinction between focused limiter
verification and the later requested full-app testing expansion.

Requires Go 1.27.3 or newer. The tests use native Go, real local HTTP listeners,
private temporary SQLite databases and files, and generated ephemeral actor
credentials. They do not need Docker, an external service, manual credentials,
or a running development server.

```bash
make test-api   # HTTP contracts, handlers, ownership and server lifecycle
make test-bdd   # Verbose Given/When/Then scenarios
make test-race  # Full suite with the Go race detector
```

For a focused scenario:

```bash
go test -count=1 -run '^TestBDDActorIdentityFlowsToOwnershipAndAudit$' -v ./internal/app
go test -race -count=10 -run '^TestBDDGlobalRateLimitAcrossHTTPConsumers$' ./internal/app
```

BDD fixtures own their temporary stores; cleanup stops clients/servers before
closing/removing data. Tests never call an external endpoint. Business workflow
tests use the real router without the limiter so operations can run promptly.
Dedicated tests wrap that router in the actual global limiter with a synchronized
injected clock. CLI/server scenarios exercise actual listeners and production
wiring, including configurable limiting, the separate health listener and
shutdown. Clock injection is not exposed as a production API. Rate, period and
capacity use startup flags; see [configuration semantics](../README.md#request-limiting-go).

The suite deliberately uses several test layers:

- `internal/app` BDD cases send requests through real TCP HTTP servers, routing,
  authentication, SQLite and files. Dedicated limiter cases add the real limiter;
  business workflows omit it explicitly. Persisted rows and audit tuples are
  checked independently of response bodies.
- `internal/api/integration_test.go` invokes the real router and middleware with
  request/response recorders and real storage. These are handler integration
  tests, not network tests. Its failing response writer is a deterministic fault
  seam, not a simulated complete TCP disconnect.
- Parser, credential and clock tests call the relevant helpers or middleware
  directly. Exact JSON size and time boundaries belong here; they do not prove
  socket behavior. The oversized upload Content-Length fixture tests the header
  guard, while the store's generated reader tests the actual file byte cap.
- Store and seed tests use real SQLite/files. SQL triggers and failing readers
  inject faults at defined points, without mocking transactions. Direct fixture
  timestamps make ordering and date boundaries repeatable; separate workflows
  prove API writes reach the report and ownership tables.
- Server tests exercise `run` with actual listeners and storage, or `serve` with
  controlled handlers to isolate lifecycle behavior. They do not launch a child
  process or exercise OS signal delivery. Shutdown joins the server loops;
  arbitrary handlers still need to cooperate with cancellation.
- Root `api_test.go` contains benchmarks, not ordinary correctness tests. They
  check HTTP status and body I/O but do not validate every response field or run
  during `go test ./...`. Mutation benchmarks grow disposable data as they run;
  their measurements are not a steady-state load or capacity guarantee.

## Coverage matrix

The matrix identifies explicit scenarios, not every conceivable input or fault.
Given/When/Then comments and named subtests describe setup, action and observable
outcome. Deeper existing unit/fault tests remain alongside the BDD layer.

| Feature / route | Observable scenarios | Evidence |
|---|---|---|
| All 12 business method/path pairs | Missing, wrong and email-only credentials denied before parsing or side effects | `TestBDDProtectedBusinessRoutes` |
| `POST /api/auth/token` | Actual actor identity, response fields, no issued token, foreign instance credential denied | `TestBDDCredentialVerificationContract`, `TestBDDUnknownActorCredentialIsRejected` |
| Actor propagation | Two tokens → API context → owned SQLite rows and audit actors | `TestBDDActorIdentityFlowsToOwnershipAndAudit` |
| Contact isolation | Cross-actor GET/PUT/DELETE return identical missing-resource errors; spoofed owner rejected; no unauthorized mutation/audit | `TestBDDActorsCannotReadMutateOrReassignEachOthersContacts` |
| Contact CRUD | Create/read/replace/delete, schema and timestamps, optional empty strings, missing IDs | `TestBDDContactsCRUD`, `TestBDDContactOptionalFieldsMayBeEmpty` |
| `GET /api/contacts` | Empty `[]`, default/min/max page sizes, tied timestamps, terminal cursor omission, actor isolation | `TestBDDContactPaginationWithTiedTimestamps`, `TestBDDContactPaginationPageSizes`, `TestBDDActorContactCollectionsAndCursorsAreIsolated` |
| Live keyset pagination | Deleted anchor still works, sequential inserts between pages on both sides of cursor, foreign/malformed cursor rejection | `TestBDDContactCursorContinuesAfterDeletionAndInterveningInserts`, store ownership/cursor tests |
| Contact writes | Exact writable field names/types, malformed/trailing JSON, null/duplicate/case-mismatched fields, limits, immutable state on error; actual TCP truncated create/replace/import | `TestBDDContactValidationPreservesState`, `TestDecodeContactRequiresExactUniqueStringFields`, `TestBDDInterruptedContactRequestsDoNotCommit`, parser boundary tests |
| `PUT /api/contacts/{id}` | Full replacement; read-modify-write by projecting writable fields | `TestBDDContactReadModifyWriteProjection` |
| `POST /api/contacts/import` | Empty/successful arrays, every writable field persisted, validation before any writes, owner isolation; exact 10,000 item and 8 MiB parser limits | `TestBDDContactImports`, `TestBDDContactImportPersistsEveryWritableField`, `TestDecodeImportLimitsAndShape`, `TestDecodeImportBodyLimit` |
| `GET /api/contacts/export` | Actor-only rows, empty header-only CSV, quoting, formula-safe values; stored JSON unchanged | `TestBDDContactCSVExport`, actor collection scenarios |
| Contact mutation/audit transactions | Create/update/delete/import roll back on audit failure and work after recovery | `TestBDDContactAuditFailuresRollbackEveryMutation`, store rollback tests |
| `GET/POST /api/files` | Empty/populated lists, owned metadata/schema, binary and empty file upload, MIME fallback, basename normalization, interrupted multipart cleanup, audit failure/retry | `TestBDDFilesRoundTrip`, `TestBDDFileUploadNormalizesPathComponents`, `TestBDDInterruptedMultipartUploadLeavesNoState`, `TestBDDFileUploadAuditFailureRollsBackAndRecovers` |
| `GET /api/files/{id}` | Exact bytes, safe filename/media/security headers, missing/invalid IDs, absent/inconsistent blob, audit-before-delivery | `TestBDDFileRequestErrors`, `TestBDDUnavailableFileFailsBeforeDelivery`, API integrity tests |
| File isolation | Foreign metadata/download denied before blob access; multipart owner injection rejected | `TestBDDFilesArePrivateToTheirOwner`, `TestBDDFileOwnerCannotBeSuppliedByUpload` |
| `GET /api/reports/activity` | Real authenticated HTTP operations → exact persisted audit tuples → actor-only aggregation; UTC default 30 days, inclusive date, empty result, invalid date, current view excluded from its own snapshot | `TestBDDActivityReportsReflectAuthenticatedHTTPWorkflows`, `TestBDDActivityReportIncludesSinceBoundary`, `TestBDDActivityReportDefaultAndEmptyWindows`, `TestBDDActivityReportsArePrivateToActor`, `TestReportSinceUsesUTCForDefaultCutoff` |
| Routing/error contracts | Unknown route, unsupported method, JSON error envelope for handler input failures | `TestBDDBusinessRoutingAndErrorContract` |
| Global limiter | Twelve independent Alice/Bob HTTP clients share one admission, rejected writes absent, real ownership/audit of winner, exact six-second boundary | `TestBDDGlobalRateLimitAcrossHTTPConsumers` |
| Configured global limiter | Twelve HTTP clients share a three-request burst, fractional refill admits one slot, long idle restores only capacity; exact persisted owners/audits and no rejected writes | `TestBDDConfiguredGlobalRateLimitAcrossHTTPConsumers` |
| Limiter detail | Default ten/minute, varied rates/periods/capacities, burst/refill, fractional drain and retry ceiling, idle/backward clock, busy downstream handler, interrupted rejection | `TestRateLimit*` tests |
| Limiter configuration | Constructor bounds and overflow; exact period parsing; invalid CLI settings fail before data/listeners/credentials; real server flags govern shared actors' admissions, persisted writes and health exemption | `TestRateLimitConfigurationValidation`, `TestRateLimitMaximumDurationRetryDoesNotOverflow`, `TestRateLimitPeriodParsing`, `TestBDDRateLimitConfigurationFailsBeforeOpeningData`, `TestBDDConfiguredRateLimitGovernsActualServer` |
| Separate `GET /health` | Public liveness only; no API routes on health, no health handler on API; health bypasses full API bucket | `TestBDDHealthListenerIsolation`, `TestBDDHealthIsIndependentOfAPILimit`, `TestBDDListenersExposeOnlyTheirOwnRoutes` |
| Listener configuration | API port + 1, override, IPv4/IPv6, zero/overflow/collision, partial bind cleanup, no early credential output | `TestBDDListenerAddressSelection`, `TestBDDBindFailureClosesOtherListener`, `TestBDDEphemeralListenersAreSeparate` |
| Dual lifecycle | Graceful and forced shutdown, either listener fails, active peer cancellation, output failure closes both listeners | `TestBDDServersDrainTogether`, `TestBDDServersForceCloseTogether`, `TestBDDServerFailureCancelsActivePeerAfterGrace`, `TestBDDStartupOutputFailureReleasesBothPorts` |
| Restart/empty stores | Credentials rotate; ownership/data persist; deleting final contact does not block files or new writes; fresh/zero-contact stores serve | `TestBDDRestartChangesCredentialAndKeepsData`, `TestBDDDeletingLastContactDoesNotBlockRestartOrFiles`, CLI tests |
| Seeding | Explicit completion marker, zero count, failure/cancel rollback and retry, concurrent once-only completion, existing app data preserved | `TestBDDSeedDoesNotInferCompletionFromContactCount`, `TestBDDInterruptedSeedRollsBackAndCanRetry`, `TestBDDConcurrentSeedRequestsCompleteOnce` |
| CLI seed boundary | Failed/repeated seed cannot leak actor registration outside transaction | `TestBDDCLISeedingDoesNotLeakActorRegistration` |
| Ownership schema/migration | Foreign keys on replacement connections, server-derived owners, reopen persistence, legacy shared DB refused without mutation | `TestActorForeignKeysAndRegistration`, `TestScopedDatabaseReopensWithoutChangingOwners`, `TestLegacySharedDatabaseIsRejectedWithoutSchemaChanges` |
| Storage/cancellation | Concurrent actors' exact rows/audits, distinct duplicate-upload winner, canceled connection wait/copy, HTTP cancellation, cleanup/recovery, permissions, exact 100 MiB file success and one-byte-over rejection | Existing `internal/store` tests, `TestFileAtExactSizeLimitPersistsAndClosesCleanly`, `TestConcurrentHTTPMutationsAndAudits`, `TestHTTPCanceledMutationDoesNotPersist` |

## Manual API exercise

Use a new disposable data directory, keeping any old database untouched:

```bash
go run ./cmd/server --data=api-demo-new --actors=alice@example.com,bob@example.com
```

The terminal prints API/health addresses and one token per actor. Keep that
terminal private. In another terminal, paste Alice's token without shell history:

```bash
read -r -s TOKEN
curl http://127.0.0.1:8081/health
curl -i -H "Authorization: Bearer $TOKEN" http://127.0.0.1:8080/api/auth/token -X POST
```

Health can be called freely. With defaults, API calls share the six-second admission interval,
including negative authentication attempts. Wait six seconds between the
following examples, or honor `Retry-After`:

```bash
curl -i -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"first_name":"Alice","last_name":"Example","email":"alice@example.com","phone":"+15551234","company":"Synthetic"}' \
  http://127.0.0.1:8080/api/contacts

curl -i -H "Authorization: Bearer $TOKEN" 'http://127.0.0.1:8080/api/contacts?limit=1'

curl -i -H 'Authorization: Bearer someone@example.com' \
  http://127.0.0.1:8080/api/contacts
unset TOKEN
```

A successful contact response has `id`, `owner_id`, five writable fields and
`created_at`/`updated_at`. Copy only `first_name`, `last_name`, `email`, `phone` and
`company` into a PUT body when editing a GET response. PUT replaces the entire
writable record; omission clears a field. IDs, owner and timestamps are derived
by the server and rejected as input. Field names must be exact lowercase names,
unique within an object, with non-null string values. Missing fields remain valid
empty strings. Handler validation errors use `{"error":"..."}`; authentication
and router-level errors retain ordinary HTTP text responses.

For an isolation check, use Bob's printed token to request Alice's contact ID.
The result is `404`, identical to an unknown ID; Alice can still read it. Automated
BDD tests perform this symmetrically and verify SQLite rows and audits, rather
than relying only on status codes.

CSV is spreadsheet-safe presentation. Formula protection can prefix text,
including international phone numbers, and CSV dates retain the starter's
second-level format. Paginated JSON provides exact stored text and timestamp
precision. Neither live pagination nor CSV is a transactional backup snapshot.

## Explicit limits

This suite does not test Java, production identity provisioning, external account
permissions, OS administrator isolation, every malformed byte sequence, indefinite
load, every network/filesystem failure, or power-loss recovery across SQLite and
files. Exact JSON body and stored-file size boundaries are tested below the
network layer; the suite does not send a full 100 MiB upload over HTTP or prove
all 101 MiB multipart-body boundary cases. Arbitrary readers that ignore
cancellation require cooperation. A race pass covers executed interleavings, not
all possible ones. The limiter governs admissions, not in-flight concurrency.
Health is process liveness, not a database readiness probe.
