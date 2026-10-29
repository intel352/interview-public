# Actor-isolated Rate Limiting Implementation Plan

> **For the implementing agent:** REQUIRED SUB-SKILL: Use autodev:executing-plans to implement this plan task-by-task.

**Goal:** Isolate admission budgets by authenticated actor, remove the fixed actor-count cap, and prove the behavior through BDD.

**Architecture:** Snapshot credential-digest map authenticates before an actor-keyed concurrent bucket map. Each bucket owns its drain state and mutex; all invalid credentials share one anonymous bucket.

**Tech Stack:** Existing Go, chi, net/http, SQLite; standard-library SHA-256 and sync.Map.

Run all Go commands from `go/`.

**Base branch:** prework/leaky-bucket-go127

## Scope Manifest

**PR Count:** 1
**Tasks:** 3
**Estimated Lines of Change:** ~650

**Out of scope:**
- Runtime registration endpoint, credential rotation/revocation API, per-actor policy overrides, distributed storage, eviction, new dependencies.
- Publishing, merging, or deploying; deliver changes locally in the existing working tree. PR grouping describes one reviewable changeset only.

**PR Grouping:**

| PR # | Title | Tasks | Branch |
|------|-------|-------|--------|
| 1 | Isolate rate limits by actor and remove actor-count cap | Task 1, Task 2, Task 3 | prework/leaky-bucket-go127 |

**Status:** Complete 2026-10-29T18:55:15Z

## Guidance and Decisions

Inherit design guidance from README and Go hardening/scope/compatibility docs.
See `2026-10-29-actor-rate-limit-design.md` and `decisions/0001-key-actor-rate-limits.md`.
Keep immutable credentials, no-store responses, ownership, health isolation,
configuration arithmetic bounds and handlers outside limiter locks.

### Task 1: Uncap actor registration and key authentication

Files: `go/internal/auth/auth.go`, `go/internal/auth/auth_test.go`.

1. Replace tests rejecting 65 actors with large actor-list validation,
   credential generation and authentication assertions, including aliases and
   immutable snapshots. Preserve malformed labels/mapping rejection tests.
2. Run `go test ./internal/auth -count=1`; expect new >64 tests to fail on the cap.
3. Remove count ceiling; replace credential slice/scan with SHA-256-keyed map.
4. Run the same command; expect exit 0. Inspect for no request-time scan.

Rollback: restore auth change; rebuild and smoke-check current default actors.

### Task 2: Isolate buckets and compose authentication before admission

Files: `go/internal/app/rate_limit.go`, `go/internal/app/app.go`,
`go/internal/app/rate_limit_test.go`, `go/internal/app/rate_limit_config_test.go`,
`go/cmd/server/main.go`.

1. Add failing tests for two actor budgets, independently staggered refill,
   aliases sharing one budget, same-actor concurrency, anonymous isolation and
   authenticated unknown-route/error charging. Exercise real auth middleware;
   invalid config through the composed constructor returns error and no handler.
2. Run focused tests; expect Bob's first request to fail due to global state.
3. Extract bucket state; lazy `sync.Map` by `auth.UserID`, per-bucket mutex.
   Preserve precise arithmetic, Retry-After/body/write-error behavior.
4. Add `NewRateLimitedRouter` and injected-clock private counterpart. Factor
   shared routing so credential parsing happens once before the limiter, while
   `NewRouter` stays unthrottled. Document standalone limiter ordering. Wire
   production to the new constructor.
5. Run `go test ./internal/app -run '^TestRateLimit' -count=1`; expect exit 0.

Rollback: restore middleware/wiring; rebuild and smoke-check both listeners.

### Task 3: Extend HTTP/CLI BDD and reconcile documentation

Files: `go/internal/app/bdd_helpers_test.go`, `go/internal/app/bdd_rate_limit_test.go`,
`go/cmd/server/rate_limit_config_test.go`, `go/cmd/server/health_test.go`,
`README.md`, `go/API_TESTING.md`, `go/HARDENING.md`, `go/COMPATIBILITY.md`, `go/SCOPE.md`.

1. Update fixture composition to use the production constructor, with configurable
   actors and injected time. Rewrite shared-global BDD expectations per actor.
2. Add >64 real registered actors with valid credentials, HTTP writes/audits,
   independent limits; add noisy actor, anonymous abuse, staggered refill and
   concurrent per-actor acceptance counts. Keep health and write suppression proof.
3. Extend real CLI server BDD to >64 configured actors and per-actor bursts;
   remove stale global-capacity/precedence assumptions while preserving failures.
4. Run `go test -run '^TestBDD.*(RateLimit|Actor)' -count=1 ./internal/app ./cmd/server`;
   expect exit 0, persisted records/audits match each admitted actor.
5. Update public docs for per-actor policy, keyed lookup, no fixed actor maximum,
   anonymous policy, process scope, memory growth and aggregate throughput.
6. Run `go test -count=1 ./...`, `go test -race -count=1 ./...`, `go vet ./...`,
   `go build -o <temporary-directory>/server ./cmd/server`; expect exit 0.
7. Launch that binary with >64 actors and long refill interval, verify Alice
   gets admitted then 429 while Bob is admitted, anonymous 401 then 429,
   another actor remains admitted, and health stays 200. Stop via SIGTERM;
   expect exit 0 and no failure signatures. Never retain/log raw credentials.

Rollback: restore complete changeset and rerun smoke; no schema changes.

## Integration Matrix

| integration | class | task/evidence |
|---|---|---|
| CLI/auth/limiter/router/SQLite | runtime-integrated | Task 3 real-server BDD + launched binary |
| concurrent HTTP actor consumers | runtime-integrated | Task 3 injected-clock BDD |
| public health listener | runtime-integrated | Task 3 health under exhausted quotas |

## Acceptance Trace

R1/R2/R5/R6 → Task 2; R3/R4 → Task 1 and Task 2; R7/R8 → Task 3.

## Completion Evidence

- Task 1: large-set tests failed at the previous 64-actor cap and passed after
  keyed authentication changes. Auth unit and race tests passed; independent
  spec/code review: SHIP-IT.
- Task 2: isolation tests failed on shared global state, then focused
  `go test -count=1 -run '^TestRateLimit' ./internal/app` passed. Independent
  reviewer repeated the focused tests and returned SHIP-IT.
- Task 3: per-actor BDD initially failed against old fixture composition;
  composed HTTP/CLI BDD passed, including every actor in the 128-actor cases.
  Independent spec/code review: SHIP-IT; coverage evidence pointer reconciled.
- Lead verification: `go test -count=1 ./...`, `go test -race -count=1 ./...`,
  `go vet ./...`, and built server all exited 0. Tests used a writable temporary
  Go cache and authorized loopback listeners/private temporary stores.
- Built binary launched with 128 actors and one admission/hour/actor: first
  actor 201 then 429; anonymous 401 then 429 for a different invalid credential;
  last actor 201 then 429; quiet actor 201; health 200. SQLite contained exactly
  128 registered actors and one contact/audit for each of the three admitted
  actors. SIGTERM exit 0; failure-signature scrape clean. No raw tokens retained.
- Initial `golangci-lint` execution could not analyze Go 1.27 export data.
  Follow-up on 2026-10-29 upgraded Homebrew golangci-lint to 2.15.0 and govulncheck
  to 1.9.0 (both built with Go 1.27.3). Full lint returned 0 issues and source
  scanning including tests returned no vulnerabilities; see `docs/FOLLOWUPS.md`.
  No compiler/vet/test failure remains. `git diff --check` passed.
