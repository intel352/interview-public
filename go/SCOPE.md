# Assignment scope and rationale

This PR contains the original limiter assignment **and a substantially larger
set of later requested changes**. The larger set is not necessary to implement
a leaky bucket and should not be presented as a 30–45-minute solution.

The reference points are the prework brief supplied for this task and the starter
at [`d35ce33`](https://github.com/weave-lab/interview-public/tree/d35ce33bfd64dcac5c929138dc7c83cf8cdb3bed).
The starter README describes the CRM, toy email-bearer authentication and shared
data; it does not require replacing that model. Findings below describe observed
behavior, not presumed intentional interviewer traps. Go is the chosen
implementation; Java remains unchanged.

## 1. Original requested assignment

The brief asked to get the project running locally and implement a basic,
readable, maintainable **leaky-bucket request limiter**, process-local and global
across clients, at **10 requests per minute**. The expected take-home duration
was 30–45 minutes, followed by a small requirements change during live review.

The core implementation is [`internal/app/rate_limit.go`](internal/app/rate_limit.go)
plus one wrapper around the production business router in
[`cmd/server/main.go`](cmd/server/main.go). It introduces no limiter dependency,
database table, external service, background worker or per-client bucket.

The brief did not fully specify burst capacity, queueing or overflow behavior.
Our explicit choices are:

- A **capacity-one policer**: the first request is immediate, then one admission
  every six seconds. Elapsed time drains the bucket lazily on the next request.
- Overflow receives JSON `429` and rounded-up `Retry-After`; there is no queue.
  Rejections add no work, and idle time does not accumulate burst credit.
- One shared instance per server process, protected by a mutex. The downstream
  handler runs after unlocking. Restart resets state; processes do not coordinate.
- The business listener charges requests before authentication and routing,
  including failed requests. This is a chosen scope/precedence policy, not a
  separate requirement in the brief. Health originally shared that listener;
  separation was explicitly requested afterward, with exemption as the supporting
  liveness policy for the new listener.

This interpretation favors simple, predictable spacing over bursts or queued
shaping. It limits admissions, not overlapping long-running handlers, fairness
or aggregate traffic across several server processes. A different burst or
queue policy would need an explicit decision in a live requirements change.

## 2. Supporting work for a reliable limiter submission

These changes directly support implementing or checking the original behavior;
they do not require redesigning the CRM:

| Supporting work | Why it belongs near the assignment | Boundary |
|---|---|---|
| One production middleware wrapper; leave the reusable router independently usable | Ensures the shipped server enforces the global policy without accidentally creating one bucket per route/client or throttling the existing handler benchmarks | The dual-listener redesign is later expanded scope |
| Deterministic injected-clock tests | Verify first admission, exact six-second boundary, rounded retry delay, idle behavior and rejected attempts without minute-long sleeps | A test seam; production still uses the real clock |
| Concurrent admission and busy-handler checks | Verify shared state is synchronized and the lock is not held while handlers work | Race checks exercise finite schedules; they do not prove absence of every race |
| Focused router/SQLite rejection tests | Show that a rejected write never reaches storage and accepted behavior survives the wrapper | A small integration test suffices; full-app BDD is not required for this |
| Limiter response/error handling, preserved request logging and safe test cleanup | A failed rejection response must not be silently completed; rejected requests must still be observable; tests must release their temporary store/server resources | Full-app error cleanup, logging/audit policy and all baseline lint fixes are separate expansion |
| README assumptions and focused validation | Make the capacity, overflow, scope and process-local limitations explainable, and establish that the starter still runs | New authentication, schema migration and broad endpoint policy changes are unnecessary for the limiter |

The initial implementation commit,
[`a82f612`](https://github.com/intel352/interview-public/commit/a82f612a6415c36556d711d74261a15cf6eeaa08),
is a useful small review starting point, but is **not a perfectly isolated minimal
assignment commit**: it also includes the subsequently requested Go upgrade and
modernization. Later cleanup/review work followed it. Classify changes by purpose,
not simply by commit date.

## 3. Expanded scope and the reasoning behind it

The candidate subsequently requested a Go upgrade/modernization, full security
and Go-practice review, fixes for existing findings, full-app BDD/API testing,
separate health serving, distinct persisted actors and private contacts/files,
and a review for regressions introduced by our own changes. Those requests
authorized the expansion; the detailed policies below remain implementation
choices and are not retroactively requirements of the original assignment.

| Major group | Source of scope and rationale | Compatibility/tradeoff; separability |
|---|---|---|
| Go 1.27.3 and built-in modernization | Explicit later request. Update the Go directive and apply supported idioms; use compatible analysis tooling | Raises the required compiler version. Independent of the algorithm and can be reviewed separately; dependency versions and Java were not upgraded |
| Full security/idiom/lint cleanup | Later request to review and fix all verified findings, including baseline issues. Check errors, close resources, assert benchmark statuses and improve shutdown/storage behavior | Broad cleanup adds review surface. Only the narrow limiter/test cleanup above is needed for the original task; the rest can be a separate hardening change |
| HTTP and input boundaries | Implementation choices under that broader request: bounded JSON/import/file input, field validation, safe download headers, numeric loopback binding and server deadlines | Missing JSON media types, unknown/duplicate/case-mismatched/null fields, full GET objects used as PUT bodies and invalid query values may now be rejected. Fixed deadlines can interrupt slow transfers; loopback-only prevents LAN use. Exact limits and strict DTO policy were not prescribed by the assignment |
| Local credentials and actor isolation | Distinct actors, persisted attribution and private contacts/files were explicitly requested later. Independent random startup tokens avoid restoring spoofable email-bearer identity; owners are server-derived and checked on every applicable path | Tokens rotate on restart and are visible to the local operator. This is a local demo, not external identity verification. Owner columns, report filtering and actor-bound cursors change the original shared-data contract. Identity, ownership, report scoping and their tests belong together, separate from the limiter |
| Ownership/schema policy | Supporting the requested isolation required an explicit owner model. We chose foreign-key constraints and fail-closed refusal of legacy shared schemas because audit history cannot reliably reconstruct ownership | No first-caller adoption or automatic destructive migration. Old data needs a fresh directory or a separately reviewed explicit offline migration. Go and Java no longer share an interchangeable schema. The precise migration policy is our choice, not a limiter requirement |
| SQLite, audit and file consistency | Under broad reliability review: one-connection/WAL/busy-timeout policy, mutation+audit transactions, checked copy/sync/close and cleanup, duplicate-file preservation | Serializes local DB operations; audit failure now fails mutations instead of silently losing history. Export/download/report audit names stay stable, but denote attempts, not guaranteed delivery. SQLite and files are not power-loss atomic. This is separable storage hardening |
| Separate health server | Explicit later request: health on its own HTTP server, default API port + 1. Health cannot consume or be rejected by the business limiter; both listeners stop together | Health moves off the API port. Overrides, IPv6, port-zero handling, partial binds and dual shutdown are supporting work for this new feature, not the original limiter. Health is liveness, not readiness |
| Seed/startup, pagination and report corrections | Later full-app and regression review exposed contact-count initialization, partial seed completion and timestamp/cutoff problems. Explicit completion state, ordinary-failure rollback/retry and canonical UTC timestamps make those behaviors predictable | Zero-contact/fresh databases can now serve; seed completion no longer means “has contacts.” Timestamp/schema and cursor compatibility are documented. These fixes are worthwhile independently but not necessary to add a request limiter |
| CSV and collection contracts | Discretionary policy choices under the broad hardening request: spreadsheet-safe cells and consistent empty arrays | Formula protection also prefixes international phone numbers; CSV is not a lossless backup. JSON retains exact stored text. Empty arrays replace prior `null` collections. These choices can be reviewed/deferred independently of the limiter |
| Full-app BDD and convenient API workflow | Explicit later request to cover all app features and make API testing practical. Native Go tests use real HTTP/SQLite/files, generated credentials, an endpoint matrix and Make targets | Much larger than focused limiter tests; no framework or external service was added. Deterministic units and fault injection remain distinct from TCP integration |
| Behavioral test-strength review | Explicit latest request to recheck every case for realistic behavior. Added exact persisted fields/owners/audit tuples, HTTP interruption/cancellation, usable rotated credentials, two-actor operations→reports, rollback/recovery, independent size limits and correct selectable scenarios | Tests/docs only; no runtime behavior changed in this follow-up. The full matrix is 123 top-level tests/356 named nodes, including 60 BDD groups. This is additional assurance, not the minimal assignment or proof of every possible scenario |

### Corrections to our own changes

Earlier hardening collapsed all identities into `local-demo` and renamed stable
audit action keys with `_attempt`. Those were unnecessary losses of attribution
and compatibility. Distinct actors and the original `export`/`download`/`view`
keys are restored. We also fixed the seed CLI registering actors outside its
transaction. These are corrections to our implementation, not new benefits to
count as requested features or alleged starter traps.

Other restrictions are deliberate, not hidden fixes: writable-field projection
for PUT/import, numeric-loopback-only operation, strict input handling and
spreadsheet-modified CSV. See [COMPATIBILITY.md](COMPATIBILITY.md) for the full
original-to-current comparison and [HARDENING.md](HARDENING.md) for policies.

## How to present or separate the work

For the interview, start with the small limiter, its assumptions and focused
tests. Be ready to explain the mutex scope, lazy drain calculation, capacity-one
tradeoff, rejection precedence and process-local reset behavior. Then explicitly
label the rest as follow-up work requested after the original brief.

Reasonable separate review groups are: (1) toolchain/modernization, (2) general
input/storage/lifecycle hardening and baseline fixes, (3) actor ownership/schema
and migration, (4) separate health serving, and (5) broader BDD/test-quality work.
They remain in this authorized draft; nothing has been reverted. Mechanical
cherry-picking is not promised: current fixtures depend on the new schema/auth,
and ownership, cursor/report filtering, seed behavior and their tests are coupled.
A future scope split should be validated as its own change rather than deleting
pieces without checking dependencies.

No further expansion is implied. Quotas/retention, streaming very large exports,
production identity provisioning, multi-process limiting and crash recovery are
separate product/operational decisions. The admission limiter does not solve them.

## Validation and limits

The strengthened test sources passed native Go 1.27.3 full normal and race suites;
golangci-lint 2.15.0 reports zero issues. The test review distinguished helper and
middleware units, recorder/router integration, real TCP/SQLite/filesystem flows,
and in-process production startup/lifecycle tests. All 14 benchmarks were reviewed
but not rerun in this test-strength follow-up. No Docker or custom binary build was
used for it. Earlier coverage/runtime/advisory measurements must retain their
original commit attribution rather than being claimed as newly rerun results.

See [API_TESTING.md](API_TESTING.md) for exact scenarios and omissions. Passing
tests and race checks establish executed behavior, not exhaustive coverage,
production security, OS-signal testing or power-loss guarantees. The expanded
review found test/documentation weaknesses, but no additional confirmed runtime
defect requiring a production-source change in that follow-up.
