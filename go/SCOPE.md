# Change rationale and scope

- **Baseline:** starter `d35ce33`; prior behavior reviewed through `12b4e29`, followed by the explicitly requested limiter configuration change below.
- **A — assignment:** run locally; readable, maintainable, process-global leaky bucket at 10 requests/minute.
- **S — direct support:** wiring, failure handling and focused verification needed for a reliable limiter submission.
- **E — expansion:** later requests for modernization, strict full-app fixes, actors/isolation, separate health, BDD/API testing, regression review and configurable limiting. Specific policies below are our choices unless explicitly identified as requested.
- **Reading guide:** each entry states the change, why it belongs, and the choice or cost. No claim that starter defects were intentional interview traps.

## Original limiter and direct support

- **A · Global budget:** one limiter instance around the business router; defaults to the shared 10/minute requirement without per-client or external state.
- **A, choice · Default capacity-one policing:** first admission immediate, then six-second spacing; conservative default burst policy. Lazy elapsed-time draining needs no ticker, queue or worker; idle time cannot accumulate more than configured capacity.
- **E · Configurable policy:** explicitly requested count, period and capacity flags retain defaults of 10/minute and one. The interval derives from period/count, avoiding contradictory settings; larger capacity deliberately permits bursts rather than enforcing a strict count per window.
- **E, choice · Exact bounded arithmetic:** scaled integer work preserves fractional drain rates without floating-point or interval-truncation drift. Validate positive values, nanosecond resolution and arithmetic bounds before opening resources; invalid explicit settings fail instead of falling back. Settings are startup-only, not environment/config-file or hot-reload controls.
- **E · Actionable CLI failures:** explicitly requested flag/value-specific errors, safe numeric bounds and corrective guidance; multiple invalid basic values report together, then valid values receive relationship checks. One stderr diagnostic replaces duplicated stdout/usage output; help remains stdout/exit 0 and failures exit 1. Constructor validation and real entry-point subprocess tests keep the contract aligned.
- **A, choice · Overflow and scope:** immediate JSON `429`, ceiling `Retry-After`, no-store; rejected attempts add no work. Charging before auth/routing keeps one rule for all business outcomes, but unauthenticated clients can consume the shared allowance.
- **S · Synchronization:** mutex protects clock/drain/admit, never downstream handlers; prevents double admission without serializing handlers. Backward clocks cannot add credit; production uses `time.Now`.
- **S · Wiring and logging:** wrap production once, keep `NewRouter` independently reusable, move logging outside the limiter; enforces the budget and logs rejections without throttling handler benchmarks.
- **S/E · Focused tests:** injected time, exact boundaries, retry rounding, concurrent consumers, busy handlers and real SQLite write suppression cover the original rule; varied settings, burst/refill and CLI validation/wiring cover the requested configuration expansion without sleeping through minutes.
- **S/E · Failed responses:** check writes and abort interrupted responses with `http.ErrAbortHandler`; avoids reporting a partial body as complete. Limiter handling is direct support; JSON/CSV/download/health handling belongs to broader cleanup.

Evidence: [limiter](internal/app/rate_limit.go), [wiring](cmd/server/main.go), [focused tests](internal/app/rate_limit_test.go).

## Expanded identity, ownership and storage

- **E · Distinct actors:** explicitly requested persisted attribution and private contacts/files; configured case-sensitive actor labels replace the earlier single `local-demo` identity. Labels are not verified email identities.
- **E, choice · Ephemeral credentials:** independent random 256-bit tokens, memory-only, printed after both binds, rotated on restart; avoids spoofable email bearers without adding an identity provider or stored secrets. The local operator sees all tokens.
- **E, choice · Auth boundaries:** snapshot token mappings, constant-time token comparison, duplicate-header rejection, fail-closed configuration and Bearer challenges; avoids ambiguous/mutable credential state. The auth endpoint verifies identity; it does not issue tokens.
- **E · Ownership end to end:** server-derived `owner_id`, actor foreign keys and filtering on CRUD/list/import/export/file/download paths; fulfills isolation. Foreign valid IDs return the same missing-resource response; no client-supplied owner is trusted.
- **E, choice · Private reports:** filter aggregates by actor; prevents reports from exposing another actor's activity. This supports requested isolation but removes the starter's shared report view; no admin-report API was added.
- **E, choice · Legacy refusal:** inspect existing schema/integrity read-only and refuse unsupported databases; ownership cannot be reliably inferred from incomplete/synthetic audit history. No first-caller adoption or automatic migration; fresh data or explicit offline migration is required.
- **E · Timestamp/cursor consistency:** schema v2 canonical fixed-width UTC preserves nanoseconds and SQL-default compatibility; fixes repeated/missed boundary rows. Actor-bound, size-bounded cursors retain live keyset behavior after anchor deletion; old cursors break and there is no snapshot guarantee.
- **E · SQLite concurrency:** one pooled connection, WAL and five-second busy timeout, with pragmas reapplied per connection and escaped database paths; addresses SQLite contention and preserves foreign keys. Local DB work is serialized; cross-process contention remains possible.
- **E, choice · Atomic audit:** mutation and audit share a transaction; prevents successful writes with missing audit history. Audit failure now rejects the mutation. Original `export`/`download`/`view` names remain, logged before output as attempts, not proof of delivery.
- **E · File durability/cleanup:** exclusive creation, bounded copying, cancellation checks, sync/close checks and removal of only this attempt's failed file; preserves duplicate winners and avoids false success. Filesystem and SQLite are not crash-atomic.
- **E, choice · Local permissions:** new directories use 0700 and new database/blob files 0600; limits accidental local disclosure. Existing permissions are not silently changed; OS administrators are outside the isolation boundary.

Evidence: [auth](internal/auth/auth.go), [schema/store](internal/store/store.go), [contacts](internal/store/contacts.go), [files](internal/store/files.go), [reports](internal/store/activity.go).

## Expanded API and export contracts

- **E, choice · Bounded contact input:** 1 MiB contact body, 8 MiB import body, incremental validation and 10,000-item cap; bounds each admitted request before writes. Name/company/phone/email limits and control-character rejection add intentional restrictions.
- **E, choice · Strict JSON:** exact, unique, non-null writable string fields and `application/json`; surfaces ambiguous client input. Unknown/server metadata is rejected; GET→PUT/import requires writable-field projection. This is stricter than ownership security alone requires.
- **E · Validation/error policy:** invalid fields/query values return 400, excessive body size 413, unsupported media 415; makes rejection explicit instead of silently defaulting. Encode JSON before committing status; add nosniff and check encoding/streaming failures.
- **E · Contact response contracts:** return real creation timestamps after updates and consistent empty arrays; fixes misleading metadata/null collections. Preserve full PUT replacement: omitted writable fields still clear, rather than becoming PATCH.
- **E, choice · Upload contract:** exactly one `file` part, no extra fields, 100 MiB content plus bounded multipart overhead; prevents owner injection and uncontrolled forms. Validate the parser-normalized basename, use UUID storage keys, clean temporary parts, and fall back to a safe media type.
- **E · Download contract:** verify owned metadata and regular-file/size consistency before delivery; format attachment names safely and set length/nosniff/sandbox headers. Prevents malformed headers or serving known inconsistent blobs; interrupted output aborts.
- **E, choice · Spreadsheet-safe CSV:** escape formula-leading cells; reduces spreadsheet execution risk while leaving stored JSON unchanged. Also prefixes `+` phone numbers; CSV keeps eight columns/second-level dates and is not a lossless backup.
- **E · Ordering/report boundaries:** deterministic ID tie-breaks for file lists/exports and UTC-inclusive report cutoffs; prevents ambiguous ordering and local-time/DST inconsistencies. A report is computed before its own `view` event, which appears only in a later report.

Evidence: [request validation](internal/api/requests.go), [contacts/CSV](internal/api/contacts.go), [files](internal/api/files.go), [report cutoff](internal/api/reports.go).

## Expanded health, startup and lifecycle

- **E · Separate health server:** explicitly requested API port + 1 default; independent liveness remains available when the business bucket is full. Health has no business routes, and the API listener has no health route; exemption is the supporting liveness policy.
- **E · Address/bind handling:** health override, IPv4/IPv6, independent ephemeral ports by default for API port 0, overflow/collision checks and partial-bind cleanup; makes the new two-listener feature usable. Both bind before credentials are printed.
- **E, choice · Loopback and deadlines:** numeric loopback only, 5s headers, 30s read/write/request, 60s idle and 16 KiB headers; limits unintended exposure and resource occupation. LAN/hostname binds and sufficiently slow transfers are no longer supported.
- **E · Coordinated shutdown:** returning startup helpers, signal context, five-second grace then cancellation/forced close, error propagation and joined server loops; avoids abrupt exits/ignored failures and releases shared storage afterward. Arbitrary handlers/readers still must cooperate.
- **E · Empty-store startup:** remove contact-count initialization gate; a fresh database or deleting the last contact must not disable files or later writes. Seeding becomes optional.
- **E · Seed completion/retry:** one DB transaction for actors/data/audit/completion marker; ordinary failure rolls back rows and removes only new blobs. Fixes partial completion and false “already seeded”; repeat after completion is a no-op, including changed options.
- **E · Seed identity/files:** distribute configured totals across actors, use random blob IDs and actual import/upload audits alongside synthetic history; makes isolated fixtures usable without overwriting existing blobs. Synthetic history is illustrative, not evidence of real user actions.

Evidence: [server](cmd/server/main.go), [health router](internal/app/health.go), [seeding](internal/seed/seed.go).

## Expanded tooling, tests and documentation

- **E · Go 1.27.3/modernization:** explicitly requested upgrade from 1.26.3 and supported built-in modernization; keeps the chosen toolchain current. Earlier mechanical changes can be superseded by later redesigns; dependencies and Java were not upgraded.
- **S/E · Test/benchmark lifecycle:** return before `os.Exit` so cleanup runs; check close/status/body-I/O errors, bound clients and use benchmark helpers/`B.Loop`. Makes results trustworthy; all 14 benchmark workloads remain, but mutation benchmarks grow disposable data.
- **E · Full-app BDD/API workflow:** explicitly requested endpoint coverage and convenient testing; native Go/Make targets use generated credentials, actual HTTP/SQLite/files and an explicit matrix. No BDD framework or CI workflow was added; ordinary tests do not run benchmarks.
- **E · Test realism:** exact persisted fields/owners/audit tuples, HTTP operations→reports, usable rotated tokens, symmetric isolation, real interrupted bodies/cancellation and rollback/retry; closes gaps that status/count-only assertions missed.
- **E · Independent expectations/fixtures:** documented byte/count limits, real exact-size files, original legacy schema, controlled timestamp fixtures and synchronized concurrency; catches policy drift and reproducible edge failures. Dependent workflow phases are no longer selectively runnable subtests.
- **E · Analysis/error cleanup:** compatible lint, race checks and independent reviews prompted checked errors/resource handling; avoids treating pre-existing warnings as acceptable. No blanket suppression or configuration weakening; finite checks are not proof of all defects.
- **E · Explicit documentation:** README commands, [API_TESTING](API_TESTING.md), [COMPATIBILITY](COMPATIBILITY.md) and [HARDENING](HARDENING.md) expose changed contracts and test layers; prevents portraying optional hardening as assignment requirements or the demo as production-ready.

## History corrections and review boundaries

- **`a82f612`:** limiter plus requested toolchain work; useful small starting point, but not a pure assignment-only commit.
- **`b823d4b` / `02ae9b0`:** narrow cleanup followed by broader hardening. The temporary single-actor model and renamed audit keys were later corrected; they are not final features.
- **`43ae2d8`:** restores distinct attribution and original audit names; adds requested ownership/health/BDD and compatibility fixes. Seed actor registration outside its transaction was caught during local development, not a separately reverted published commit.
- **`12b4e29`:** strengthens behavioral tests/docs without runtime changes; `bd2ca74` adds the itemized rationale only. The subsequent configuration change modifies runtime behavior only when settings differ from defaults.
- **Separable review groups:** toolchain; general hardening; actor/schema/migration; health; broad tests; configurable limiting. All remain authorized in this PR; coupled schema/auth fixtures make blind cherry-picking unsafe.
- **Interview focus:** explain the small limiter first, then label the expansion. Thousands of lines of follow-up work are not a 30–45-minute requirement.

## Accuracy and limits

- **Prior review:** full `d35ce33..12b4e29` diff, five implementation commits, code/tests and independent runtime/test-documentation reviewers; rationale reflects recorded requests or explicit choices, not invented historical intent. Configuration is a later explicit request, not an original assignment requirement.
- **Existing validation at `12b4e29`:** 123 top-level tests/356 named nodes, 60 BDD groups, full normal/race suites and lint zero issues. Documentation-only updates do not constitute a fresh runtime, coverage, benchmark or advisory scan.
- **Configuration validation at `78b3cf9`:** 132 top-level tests/436 named nodes, 63 BDD groups; full normal/race suites and lint zero issues. Independent review checked arithmetic, CLI wiring, persisted behavior and documentation. Later CLI-usability verification is separate; no new benchmark, coverage or advisory scan is claimed.
- **Unchanged limits:** local-operator trust; no data/audit quotas; whole-collection exports/lists; no distributed limiter or power-loss guarantee. [API_TESTING](API_TESTING.md) lists untested failure modes and distinguishes helpers, recorder integration and real TCP behavior.
