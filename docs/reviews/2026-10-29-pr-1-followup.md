# PR #1 review follow-up

Source: all eight inline comments across seven threads, fetched with `gh`.
Callouts checked against their original diff, current source and commit history.

| Comment | Assessment and response |
|---|---|
| [Health/anonymous scope](https://github.com/intel352/interview-public/pull/1#discussion_r4402717022) | Health exclusion already implemented in `43ae2d8`. `e956c36` separates anonymous allowance from each authenticated actor, preventing anonymous traffic from exhausting actor quotas |
| [Rate versus capacity](https://github.com/intel352/interview-public/pull/1#discussion_r4402759159) | 10/minute and one admission per six seconds express the same drain rate. Burst capacity is a separate policy; default one was explicitly retained, while `78b3cf9` made it configurable |
| [Configurable policy](https://github.com/intel352/interview-public/pull/1#discussion_r4402805787) | Already implemented in `78b3cf9`; count/period/capacity flags, bounds and actual-server tests. Diagnostics improved in `b9193a5` |
| [Endpoint middleware](https://github.com/intel352/interview-public/pull/1#discussion_r4402829678) | Valid outstanding request. Added validated standard middleware factory, shared/independent policy semantics and stricter endpoint layering tests; production uses that factory |
| [Short bursts](https://github.com/intel352/interview-public/pull/1#discussion_r4403558909) | Old hardcoding lacked bursts; configurable capacity since `78b3cf9` supports them. Added exact 10/minute, capacity-ten burst/refill proof and README guidance. Retained approved capacity-one default; no claim of a strict rolling-minute ceiling |
| [Modern goroutines](https://github.com/intel352/interview-public/pull/1#discussion_r4403650952) | Valid: replaced manual Add/Done and parameter capture with `wg.Go` and per-iteration capture. Loop-capture semantics date to Go 1.22; `WaitGroup.Go` is available in this Go 1.27.3 target |
| [Busy handler](https://github.com/intel352/interview-public/pull/1#discussion_r4403702150) | Callout misreads the fixed-clock test: it proves prompt rejection while the bucket is full, not handler serialization. Added a separate fake-time test admitting another request after refill while the first handler remains blocked |
| [Unauthenticated DoS](https://github.com/intel352/interview-public/pull/1#discussion_r4403726574) | Original shared-budget concern was valid and is fixed in `e956c36`. Anonymous requests still intentionally receive a separate policy; they cannot drain authenticated actors' allowances. Transport/auth CPU overload remains planned production work |

## Implementation and verification

- Factory state is allocated once per policy; sharing the returned middleware
  shares actor buckets across routes. Separate constructors isolate policies.
  Authentication precedes admission; outer admission is charged even on inner
  rejection. Existing handler-wrapper API and test clock seam are preserved.
- `go fix -diff ./...` initially found no automatic changes. Ran the built-in
  `go fix ./...` after edits; no additional rewrites. The reviewed goroutine
  modernization was applied explicitly.
- `go test -count=1 ./...`, `go test -race -count=1 ./...`, `go vet ./...`,
  `golangci-lint run --no-config ./...` and `govulncheck -test ./...` all pass.
  No vulnerabilities or lint findings. Final `go fix -diff ./...` is empty.
- Independent review found no blocking issues. Applied its three minor fixes:
  join both request workers, prove exact burst/refill boundaries and clarify
  policy lifetime.
- Built with `go build -o /private/tmp/interview-public-review-server ./cmd/server`.
  Launched on ephemeral loopback ports with two actors, capacity two and a
  one-hour period: anonymous responses `401,401,429`; each actor independently
  `200,200,429` with `Retry-After: 360`; health remained `200`. SIGTERM exited
  zero; failure-signature scan clean. Startup credentials omitted.
- GitHub reports no configured CI checks for this PR; local verification is
  separate from the future CI/CD gates in the productionization roadmap.
