# Follow-ups

## Planned

- Productionization decisions and planned work: [roadmap](PRODUCTIONIZATION.md).
  Includes actor/session lifecycle, safe bucket eviction, client/organization
  ownership, single- versus multi-tenant behavior, and multiple replicas under
  either tenancy model, plus CI/CD hardening and OpenAPI-owned versioned API
  contracts with generated server code. These are future changes.

## Completed — 2026-10-29

- Upgraded Homebrew `govulncheck` from 1.6.0 to **1.9.0**, built with Go 1.27.3.
  From `go/`, `govulncheck -test ./...` completed successfully: **No vulnerabilities found.**
- Upgraded Homebrew `golangci-lint` from 2.11.4 to **2.15.0**, built with Go 1.27.3.
  From `go/`, `golangci-lint run --no-config ./...` completed successfully: **0 issues.**

Both commands resolve through Homebrew on the current PATH. This resolves the
earlier Go 1.27 parser/export-data incompatibilities. The project toolchain,
module dependencies and lint rules were unchanged. See [review evidence](reviews/2026-10-29-pending-changes.md).
