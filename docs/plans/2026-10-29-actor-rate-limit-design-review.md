# Adversarial Design Review

**Phase:** design
**Status:** PASS
**Artifact:** `2026-10-29-actor-rate-limit-design.md`

| ID | Severity | Finding | Resolution |
|---|---|---|---|
| D1 | Minor | >64 fixture alone misses CLI scale | R7 and Task 3 require real startup and last actor authentication |
| D2 | Minor | Standalone limiter can silently run before auth | R6 and Task 2 require ordering documentation; BDD uses composed constructor |
| D3 | Minor | Aggregate concurrency can hide per-actor starvation | R7 and Task 3 require per-actor admissions and persisted audits |

## Bug-Class Scan

| Class | Result | Evidence |
|---|---|---|
| Project guidance | Clean | Local listeners, ownership, arithmetic, health preserved |
| Assumptions | Clean | Static credentials bound possible bucket keys |
| Repo precedent | Clean | Existing context/middleware and injected clocks |
| Artifact precedent | Clean | Existing BDD and server fixture files |
| YAGNI | Clean | No new endpoint/store/worker |
| Failure modes | Clean | Same-key races, clocks, malformed auth, restart, rejected writes |
| Security/privacy | Clean | Auth-derived keys, digest lookup, immutable snapshots |
| Infrastructure | Clean | No schema/dependency; proportional memory stated |
| Multi-component validation | D1/D3 addressed | HTTP/SQLite and actual CLI, per actor |
| Declared integrations | Clean | Matrix covers runtime paths |
| Contributed UI | Clean | None |
| Rollback | Clean | Revert and smoke; no migration |
| Simpler alternatives | Clean | Preallocation, eviction, distributed store considered |
| User-intent drift | Clean | Approved startup-actor model retained without count cap |
| Existence/runtime validity | Clean | Referenced files and APIs inspected |

Alternative: typed map with creation lock avoids type assertions, but introduces
shared lookup synchronization; sync.Map fits write-once/read-many buckets.

Verdict: no Critical/Important issues; minor acceptance details incorporated.
