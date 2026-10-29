# Productionization roadmap

Status: planned; tenancy decision open. These changes are future work, not
features provided by the pending actor-limiter change.

## Current boundary

This is a loopback-only local demo: startup credentials last until restart,
actors have independent process-local limits, and buckets have no eviction.
Contacts/files use `owner_id → actors.id`; reports/audits are actor-scoped.
Actors have no client, organization or membership association. `Contact.company`
is descriptive text, not an ownership/security boundary. Actor isolation alone
does not establish a production tenancy model.

Evidence: [auth](../go/internal/auth/auth.go), [limiter](../go/internal/app/rate_limit.go),
[schema](../go/internal/store/schema.sql), [current safeguards](../go/HARDENING.md).

## Decision required: who owns the workspace?

**Open question:** one operating client per deployment, or multiple isolated
clients in one deployment? Preferred direction: multiple clients, each with
one or more subsidiary companies/sub-brands; final choice remains open.

- Distinguish an operating client/tenant from companies managed as CRM records.
- Decide whether actors can belong to several clients/orgs, who can see sibling
  brands, and whether parent access to descendants is explicit or inherited.
- Decide which records remain personal versus shared within an organization,
  and who may transfer ownership or run aggregate reports.
- Choose shared-service isolation versus a dedicated deployment/database per
  client; organization hierarchies are useful in either deployment model.

| Area | Single client per deployment | Multiple clients per deployment |
|---|---|---|
| Identity | Stable actor IDs; one configured client, org hierarchy and memberships | Explicit tenants, orgs belonging to a tenant, scoped memberships; actors may join multiple tenants if approved |
| Ownership | Add owning org; keep creator/updater actor attribution separate | Add tenant and owning org to contacts/files; database constraints prevent cross-tenant org/record references |
| Access | Authorize organization/brand sharing and parent rollups; single tenancy does not imply all-user access | Authenticate actor, validate tenant selection/membership, then authorize org/resource scope on every path |
| Queries | Scope CRUD, import/export, files, cursors and reports to permitted orgs | Carry verified tenant/org scope through those same paths, caches and future jobs; index and constrain by tenant |
| Rollups | Authorized parent/client totals across approved descendants | Aggregate within authorized tenant descendants; cross-client reporting requires a separate explicit grant and audit trail |
| Operations | Deployment-bound client configuration; client-wide backup/recovery | Per-client quotas, retention, export/deletion, restore scope and operational access; evaluate shared DB, separate schemas or databases |

For both scenarios: model organizations with stable IDs, acyclic parent links
and controlled hierarchy changes. Hierarchy must not silently grant access.
Keep audit actor identity and owning tenant/org at event time; actor removal
must not orphan records or erase history. Namespace file storage and enforce
authorization before reads; prefixes alone are not access control.

After the decision: define membership roles and sharing rules, version the
schema/API, explicitly map existing actors' records to organizations, migrate
with backup/verification/rollback, then update handlers, queries, cursors,
reports, imports/exports, files and audit together. Never infer organizations
from email labels or existing company text. For dedicated deployments, bind
each instance to its client and retain the same org/membership rules.

Acceptance: two isolated client deployments or two tenants in a shared service,
each with multiple brands; same-client sharing and authorized rollups;
sibling/cross-client denials; revoked membership; record transfer;
unknown/foreign resource equivalence; migration and restored-backup checks.

## Bucket pruning and identity lifecycle

| Current limitation | Planned production change |
|---|---|
| Startup-only tokens, no sessions/expiry/revocation | Managed identity or a reviewed session/token service; expiry, revocation, rotation and membership refresh |
| Bucket survives all inactivity until restart | Configurable idle retention and bounded incremental cleanup; track last use separately from drain time |
| Unsafe eviction could reset debt or split one actor into two buckets | Retire/delete only fully drained idle entries, coordinated with acquisition/admission; stale holders retry. A bare `sync.Map.Delete` is insufficient |
| Per-process state resets on restart | Shared atomic admission state for deployment-wide budgets; define outage behavior and clock/expiry semantics |

Recreated buckets may start empty only after their old debt has drained. Drain
horizon is `ceil(level / requests)` nanoseconds, at most
`ceil(capacity × period_ns / requests)`; compute without overflowing. Session
expiry does not reset an actor's allowance while other credentials can act for
it. Decide actor-global versus `(tenant, actor)` keys after tenancy selection;
add organization/client quotas as an explicit policy, retaining actor fairness.

Cleanup must have bounded work, cancellation and joined shutdown. Keep invalid
credentials from creating per-token entries. Define memory-pressure behavior
without evicting active debt, and monitor entry count, cleanup lag and memory
without actor/token labels in metrics. Capacity safeguards must not reintroduce
a fixed actor-registration cap.

Acceptance: expiry/rotation cannot reset allowance; idle drained eviction;
active-debt retention; concurrent eviction/admission and stale references;
alias credentials; clock boundaries; cleanup shutdown; memory-pressure behavior;
concurrent admission and eviction across replicas.

## Multiple instances: required for either tenancy model

Replicas must be interchangeable behind a load balancer. **The current app is
not ready for that by replacing SQLite alone.**

| Current deficiency | Required change |
|---|---|
| Each process generates different startup credentials (`auth.NewCredentials`) | Shared identity verification/session state and consistent membership/revocation; a credential must work on every authorized replica without sticky sessions |
| Limiter maps and mutexes coordinate only one process | Atomic shared drain/admit/evict; adding/restarting replicas must not reset/multiply budgets. Version/fence policy changes during rollouts; choose time source and outage policy |
| `Store` embeds SQLite SQL/schema/PRAGMAs and one connection | Introduce focused storage boundaries. Postgres needs its driver, SQL/migrations, bounded pools and transactions; DynamoDB needs access-pattern/index/key design, conditional writes and transactional audit—not a driver swap |
| Uploads/downloads use each process's local directory | Shared durable object storage with authorized access, upload completion/reconciliation and DB/blob recovery; test upload on A and download on B |
| Schema/actor/seed initialization assumes local storage and SQLite serialization | Coordinate migrations outside request startup; make provisioning idempotent and seed a controlled demo job. Use database locking/conditional writes, not Go mutexes, for shared invariants |
| Listener/lifecycle setup targets a local process | Define ingress/private-backend binding, dependency readiness, drain-before-stop and rolling-version compatibility; coordinate any singleton cleanup/jobs with leases/fencing |

Keep mutation/audit atomicity, owner/tenant predicates, pagination and report
semantics through either storage migration. DynamoDB needs aggregate/index and
relationship-integrity designs. Its 100-action/4 MB transaction limit cannot
directly preserve today's atomic 10,000-contact import: design staged publication
or explicitly approve a changed API contract, rather than partial commits.
Avoid unbounded scans. Retry outcomes/version preconditions
must be shared across replicas (see below). Distributed storage TTL alone is
not credential-expiry enforcement or permission to discard live limiter debt.

Acceptance: at least two real replicas sharing the selected DB/object store;
alternate authenticated requests without affinity; race one actor's admissions
and edits; upload on A/read on B; kill/restart A and roll versions; verify
revocation, quotas, tenant isolation, audit/idempotency, dependency outages and
recovery on B. Repeat for one-client and multiple-client deployment scenarios.

## CI/CD and OpenAPI contracts

These are required production release gates; local tool installation alone
does not enforce them.

| Area | Planned change |
|---|---|
| Required checks | PR/main/release pipelines run formatting, vet, linters, vulnerability checks, unit tests, race tests, explicit BDD and integration suites. Use ephemeral real dependencies for the chosen production backend and exercise multiple replicas |
| Pipeline security | Pin actions, Go and tool versions; least-privilege permissions and short-lived deployment credentials; isolate untrusted PRs from secrets/release writes; require protected-branch checks and retain test reports |
| Release control | Build once, scan/sign artifacts and publish provenance/SBOM; promote the same immutable artifact through staging and production with smoke, migration and rollback checks |
| Contract source of truth | Add a version-controlled OpenAPI spec covering routes, auth, parameters, DTOs, validation, media types, statuses, errors and headers, including 429/Retry-After. Pin a spec dialect supported by the tooling |
| Generated server integration | Generate Go DTOs, strict server interfaces and route adapters; implement interfaces in handwritten business handlers and register generated routes in the existing server harness behind auth/limiting. Preserve ownership checks and strict input/resource limits |
| Drift and conformance | CI validates/lints the spec, regenerates deterministically and rejects generated-file drift. Validate real requests/responses against the spec, including negative, multipart and CSV cases; retain independent BDD assertions for authorization and persisted side effects |
| API versioning | Introduce versioned routes (e.g. `/api/v1`) and contract SemVer in `info.version`, separate from the OpenAPI dialect and service build version. Diff against each supported released contract; require minor bumps for compatible additions, patch bumps for compatible fixes, and a new major API version for breaking changes |

An implementation violating its published contract fails CI and must be fixed.
Intentional breaking changes require a reviewed spec/version update, migration
and deprecation plan, and continued conformance for supported older API versions;
a version increment alone cannot waive those checks. First capture today's
behavior in the spec, then migrate handler wiring incrementally. Publish the
versioned spec with each release; choose and pin compatible generation,
validation and breaking-change tools rather than maintaining parallel schemas.

## Additional production work

Priority: P0 before external clients; P1 before sustained production usage.
Retain existing validation, ownership filtering, transactional audit and
separate liveness while making these changes.

| Priority | Evidence / gap | Planned change and proof |
|---|---|---|
| P0 | No org/tenant model; static identity | Resolve the decisions above; prove membership and resource authorization end to end |
| P0 | HTTP/local credentials assume a trusted local operator | TLS at a defined ingress, private backend binding, trusted-proxy policy, secret management and least-privilege runtime; retain safe defaults |
| P0 | Actor admission does not bound total/in-flight work | Set ingress/auth abuse limits, concurrency and upload/storage quotas; load-test actor/org fairness and overload behavior |
| P0 | Bounded arbitrary uploads lack a content policy | Define allowed content, quarantine/scanning and safe delivery before external uploads; test rejection and scan failure |
| P0 | Schema v2 is installed directly; unsupported DBs are refused | Versioned reviewed migrations, explicit owner mapping, backup/restore drills and rollback compatible with deployed binaries |
| P0 | SQLite and blob files cannot commit atomically across crashes | Consistent DB/blob backup and reconciliation; orphan cleanup with retention safeguards; test process crash, full disk and restore |
| P0 | Replicas cannot share identity, admission and local files correctly | Implement the multi-instance changes above; load-test pool/partition capacity, failures and tenant isolation on the chosen backend |
| P1 | File listing and contact exports materialize whole actor collections | Paginate files; bounded streaming or authorized export jobs; test large collections and cancellation |
| P1 | Liveness only; default logger records full request URIs | Add readiness/SLOs, route-template logs with query/PII redaction, request IDs, bounded-cardinality metrics and alerts |
| P1 | Retries create fresh IDs; updates lack version preconditions | Tenant-scoped idempotency keys with payload binding/atomic outcomes; ETags/If-Match for shared-record edits; test interrupted successes and concurrent conflicts |
| P1 | Demo audit covers business actions, not full security lifecycle | Audit denied access, membership/admin changes and exports; protected retention, actor/tenant attribution and support-access controls |
| P1 | No retention/deletion/offboarding workflow | Agree retention and deletion requirements; safe actor/org offboarding, record reassignment, tenant export/purge and backup policy |
| P0 | No enforced CI/CD or OpenAPI-owned API contract | Implement the gates and generated server integration above; retain parser fuzzing and deployment/rollback smoke checks |

Delivery order: decide tenancy/storage boundaries → shared identity/authorization
→ migration/recovery → shared lifecycle/quotas/eviction → multi-instance rollout
and load/abuse validation.
New infrastructure, costs and production deployment require separate rollout
decisions; this document does not select a provider or authorize deployment.

## Review of pending changes

Code quality, Go practices and security reviews completed; logging, test cleanup
and documentation issues were corrected. See [review results](reviews/2026-10-29-pending-changes.md)
for verification, including completed lint and source vulnerability scans.
The architectural changes above remain planned.

References: [OWASP tenancy](https://cheatsheetseries.owasp.org/cheatsheets/Multi_Tenant_Security_Cheat_Sheet.html),
[session lifecycle](https://cheatsheetseries.owasp.org/cheatsheets/Session_Management_Cheat_Sheet.html),
[Go security](https://go.dev/doc/security/best-practices),
[SQLite backup](https://www.sqlite.org/backup.html),
[Postgres locking](https://www.postgresql.org/docs/current/explicit-locking.html),
[DynamoDB conditional writes](https://docs.aws.amazon.com/amazondynamodb/latest/developerguide/WorkingWithItems.html#WorkingWithItems.ConditionalUpdate),
[transaction limits](https://docs.aws.amazon.com/amazondynamodb/latest/developerguide/transaction-apis.html),
[OpenAPI](https://spec.openapis.org/oas/latest.html),
[Go generation candidate](https://github.com/oapi-codegen/oapi-codegen),
[contract-diff candidate](https://github.com/oasdiff/oasdiff).
