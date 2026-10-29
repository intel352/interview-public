# Pending-change review — 2026-10-29

Scope: working-tree actor-isolated limiter, uncapped actor configuration,
digest-keyed authentication, production composition, BDD and documentation.
Independent passes: code quality/Go practices, security, overall-app readiness;
fixes re-reviewed. No Critical/Important pending-change defect remains.

## Findings addressed

| Finding | Root cause and correction | Verification |
|---|---|---|
| Optional logging omitted 429 responses | Logger was inside limiter. Wrap the composed handler; invariant: one request log for every outcome when enabled | New `TestRateLimitedRouterLogsAdmissionsAndRejections` failed with one log instead of two, then passed; also checks disabled logging and credential omission |
| New BDD health client lacked lifecycle bounds | Add five-second timeout and idle-connection cleanup, matching other clients | HTTP BDD and full/race suites |
| Hardening doc overstated tool validation | Remove unsupported exact-version/success claims; distinguish manual review, binary scanning and unavailable source tooling | Documentation reconciled with actual command results |

## Review coverage

- Go/code quality: immutable snapshots, keyed lookup, first-use races, per-actor
  locking, overflow-safe arithmetic, clock boundaries, alias credentials,
  anonymous cardinality, handler execution outside locks, errors and cleanup.
- Security: authentication before admission, server-derived actor/owner scope,
  fail-closed mappings, duplicate headers, no credential logging, no-store
  responses, SQL parameters, file-ID guards and transactional ownership/audit.
- BDD: 128 configured actors, per-actor admissions and persisted records/audits,
  noisy-neighbor/refill isolation, anonymous abuse, health and real CLI wiring.
- Overall app: tenancy, session/eviction lifecycle, multi-instance assumptions,
  recovery, bounded reads, observability, retry safety and concurrent edits.
  Future gaps are in [the roadmap](../PRODUCTIONIZATION.md); no tenant model,
  eviction worker or distributed backend was implemented by this review.

## Verification

Run Go commands from `go/`:

- `go test -count=1 ./...` — passed.
- `go test -race -count=1 ./...` — passed.
- `go vet ./...` — passed.
- `go build -o <temporary-server> ./cmd/server` — passed.
- `govulncheck -mode=binary <temporary-server>` — completed: no vulnerabilities found.
- `govulncheck -test ./...` — completed: no vulnerabilities found (1.9.0).
- `golangci-lint run --no-config ./...` — completed: 0 issues (2.15.0).
- `git diff --check` — passed.

The initial tooling incompatibilities were resolved on 2026-10-29 by upgrading
the Homebrew installations to govulncheck 1.9.0 and golangci-lint 2.15.0, both
built with Go 1.27.3. Full source analysis and lint ran successfully against the
current working tree. Go/module versions and lint rules were unchanged.
Successful scans do not prove the absence of every vulnerability.
See [completed follow-ups](../FOLLOWUPS.md).
