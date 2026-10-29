# Actor-isolated rate limiting

Approved scope: actor-isolated leaky buckets, no fixed actor-count maximum,
keyed credential lookup, extended BDD coverage. Registration remains through
configured startup actors and the existing store API; no registration endpoint.

## Global Design Guidance

Sources: `README.md`, `go/HARDENING.md`, `go/SCOPE.md`, `go/COMPATIBILITY.md`.
Keep local loopback listeners, ephemeral credentials, ownership boundaries,
precise lazy draining, startup validation, unthrottled router reuse, health
isolation, and real SQLite/HTTP verification. No `docs/PORTFOLIO.md` exists.

## Requirements

- R1: Keep Requests/Period/Capacity validation and exact scaled-integer drain;
  defaults become 10/minute, capacity one for each actor.
- R2: Concurrent keyed actor map; independent bucket mutex, level and timestamp.
  Lazy creation is safe under same-actor races. Read the clock under bucket lock;
  release it before response writes and handlers. No background work or eviction.
- R3: Parse credentials once before limiting, using a snapshot keyed lookup;
  identity comes from `auth.UserID`, never a caller-asserted actor label or IP.
  Different credentials for one actor share a bucket. Snapshot mappings remain
  immutable and invalid configuration fails closed.
- R4: Remove 64-count checks from actor validation and credential parsing.
  Preserve nonempty actor list, uniqueness, UTF-8/length/character validation.
  Use SHA-256 credential digests as fixed-size lookup keys; no credential scan.
- R5: All missing/invalid credentials share one anonymous bucket. Failed and
  unknown-route requests charge the selected bucket. Existing 429 JSON,
  no-store and actor-specific Retry-After remain. Health bypasses limiting.
- R6: `NewRouter` stays unthrottled; a rate-limited constructor composes the
  same routing with credential parsing outside the limiter. Production and BDD
  use that constructor; injected clocks use its private counterpart. Standalone
  limiter documentation requires authenticated context before invocation.
- R7: Unit and BDD proof: independent actor budgets/refills, concurrent capacity
  per actor, aliases sharing a bucket, anonymous abuse isolation, more than 64
  registered/authenticated actors, exact persisted ownership/audits, rejected
  writes absent, health exemption, configured flags on the actual server.
  Assert concurrency and persisted counts per actor, not only aggregate totals;
  actual CLI startup must accept >64 actors and authenticate its last actor.
- R8: Update README, hardening, compatibility, scope rationale and API coverage.

## Security Review

SHA-256 maps avoid credential prefix comparisons and linear scans; do not claim
the map lookup is constant-time. Preserve duplicate-header rejection and fail
closed snapshots. Unknown credentials never create distinct buckets. Only
server-authenticated actor IDs grow the bucket map. Authentication executes
before admission; this is budget isolation, not CPU or in-flight isolation.

## Infrastructure Impact

No new dependency, store, schema, endpoint or listener. Memory grows with the
configured credential set and actors used; no arbitrary application count cap.
Aggregate admissions scale with active actors. State remains per process and
resets on restart. Existing OS/header/command-line resource limits still apply.

## Multi-Component Validation

| integration | classification | proof |
|---|---|---|
| CLI → auth → limiter → router → SQLite | runtime-integrated | actual-server BDD, ownership/audit assertions |
| HTTP actor consumers → same pipeline | runtime-integrated | injected-clock HTTP BDD including >64 actors |
| separate health listener | runtime-integrated | available after actor/anonymous exhaustion |

## Assumptions

- Startup credentials remain static during a server run; no runtime registration
  or token revocation API is being added.
- Actors receive equal configured policies; aliases for one actor share policy.
- Finite configured actors bound live bucket entries; no TTL eviction needed.

## Alternatives and Self-Challenge

- Prebuilt ordinary map: simple but allocates unused buckets; lazy `sync.Map`
  permits independent creation and keyed lookup without serializing actors.
- Dynamic eviction: unnecessary for static credentials and risks resetting
  debt; reconsider with a runtime identity provider.
- Distributed state: useful across processes, adds unrequested operations.
- Doubts addressed: removing count cap exposes O(n) credential scans (replace
  with map); invalid tokens could grow state (one anonymous key); authentication
  composition could silently leave all requests anonymous (production-path BDD).

## Rollback

Restore prior limiter/auth/wiring and corresponding docs/tests, then rebuild and
smoke-check both listeners. Databases require no migration or rollback.

Decision: `decisions/0001-key-actor-rate-limits.md`.
