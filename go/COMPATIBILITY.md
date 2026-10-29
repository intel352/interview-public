# Go compatibility review

The comparison baseline is upstream `d35ce33`. The earlier published hardening
head was `02ae9b0`. This table distinguishes requested behavior, intentional
restrictions and corrected regressions. It does not claim that observed defects
were deliberate interviewer traps. The original shared schema is not
interchangeable with the current ownership schema.

| Contract | Original / earlier behavior | Current decision and classification |
|---|---|---|
| Identity | Original email-shaped bearer preserved actor identity but could be spoofed. Earlier hardening collapsed every actor to `local-demo`. | **Corrected introduced regression:** distinct configured actor labels now map to independent random startup tokens. Labels and ownership persist; tokens do not. This is a local demo, not verified email authentication. |
| Contacts/files | Original shared records had no owner. | **User-requested isolation:** server-derived `owner_id`, actor foreign keys, filtering on every route and mutation. Foreign IDs return missing-resource `404`; no shared fallback. Responses add read-only `owner_id`. |
| Reports | Original reports aggregated all actors. | **Chosen privacy policy supporting the requested actor isolation:** callers see only their actor's report; multiple actor attribution survives in actual database rows. No cross-actor admin API is implied. |
| Audit action names | Original `export`, `download`, `view`; earlier hardening renamed them with `_attempt`. | **Corrected introduced compatibility regression:** original names are restored. Delivery actions are recorded before output; the names describe attempted operations, not proof of complete delivery. Failed validation, authorization and missing resources do not add successful-operation audits. |
| Health | Original API listener served `/health`; initial limiter also charged it. | **User-requested separation:** health has its own loopback listener, default API port + 1, and bypasses the business limiter. Each listener exposes only its own routes. |
| Limiter | New assignment behavior; initially fixed at 10/minute and capacity one. | **Assignment defaults preserved:** process-global leaky bucket, one admission per six seconds. **Later user-requested configuration:** count, period and capacity flags; larger capacity permits bursts and changes the strict spacing policy. Immediate rejection, pre-auth business scope and health exemption remain. Invalid settings fail before resources open; no per-actor fairness or distributed state is added. See [scope rationale](SCOPE.md) and [configuration](../README.md#request-limiting). |
| Empty responses | Original contact/file slices encoded as `null`; reports as `[]`. | **Intentional contract fix:** all empty collections are arrays; contact list still uses its `contacts` envelope. |
| PUT | Original replaced omitted fields with empty values. | **Preserved:** full replacement, not PATCH. Tests include omission behavior. |
| Contact input DTO | Original full GET objects could be PUT/imported, with server IDs overwritten; unknown/duplicate/case-folded/null fields were permissive. | **Explicit stricter contract:** only five exact writable string fields; no duplicates/nulls/unknown fields or metadata. GET→PUT requires projection. This is a documented compatibility restriction, not a claim that rejecting every metadata field is required for ownership security. |
| JSON media/limits | Original accepted missing Content-Type and unbounded contact JSON. | **Intentional security restriction:** application/json, bounded bodies/fields/import count. Normal parameterized JSON media types work. |
| Query validation | Original invalid/out-of-range `limit` fell back to 50 and invalid `since` fell back to 30 days. | **Intentional stricter validation:** invalid values now return `400`; valid defaults and bounds are unchanged. |
| CSV | Original exported raw cells, including formulas. | **Intentional safety tradeoff:** spreadsheet-leading formulas/phone prefixes are escaped; JSON supplies exact text. CSV retains original eight columns and second-level timestamp formatting. It is not a lossless backup. |
| Pagination | Descending timestamp/ID keyset, default 50 and maximum 100. Mixed timestamp representations could repeat/omit rows. | **Preserved and corrected:** live keyset with exact timestamp/ID ties, canonical UTC storage preserving nanoseconds, actor-bound cursors, continuation after deleted anchors. New rows before the cursor are not replayed; there is no snapshot guarantee. Old unscoped cursors are invalid. |
| Report time | Original local-time default versus UTC writes/explicit dates. | **Corrected inconsistency:** default 30 days is calculated in UTC, explicit date inclusive at UTC midnight. The report is computed before logging its own `view`, so that event appears on a later request. |
| Initialization | Original and earlier hardening required at least one contact to start. | **Corrected baseline defect:** fresh schemas and zero-contact databases serve; deleting the last contact does not disable files or future writes. |
| Seed | Original committed stages separately and inferred completion from contact count. | **Corrected baseline defect:** one database transaction plus an explicit completion marker; ordinary failure rolls back rows and removes only new files. Retry completes once, and repeated completed seeding is a no-op even with different options. |
| Network binding | Original default accepted LAN/wildcard/hostname binds. | **Intentional local-demo restriction:** numeric loopback only for both listeners. IPv4/IPv6 are supported; there is no LAN override. |
| Timeouts | Original had no server deadlines. | **Intentional bounded-resource tradeoff:** 5s headers, 30s read/write/request, 60s idle, 5s shutdown grace. Slow transfers may fail; these are fixed demo limits, not production upload tuning. |
| Cleanup | Original TestMain/os.Exit and unchecked errors could bypass cleanup or hide failures. | **Corrected:** resources close before process exit; error/status assertions, forced cancellation and dual-listener cleanup are tested. |

## Data and credential policy

The Go schema stores explicit actor identity and ownership. Actor IDs are
case-sensitive labels configured by the local operator. Reusing the same label
on a later startup restores access to its records with a new token; changing a
label does not transfer ownership. Seed and run should use the same actor list
when all seeded records should be accessible.

Shared legacy databases have no reliable owner history: imports used one audit
entry, some seed history is synthetic, and earlier hardening collapsed actors.
The server therefore refuses them before applying schema/WAL changes. It never
assigns old records to the first caller. Keep old data backed up and use a fresh
directory, or perform a separately reviewed offline migration with explicit owner
assignments. No destructive reset or data adoption occurs automatically. Unsupported
intermediate schemas also fail closed; timestamp storage uses the current
version's canonical UTC representation. Migration tests preserve existing blobs
and database contents on refusal.

New data is private to the local OS account, but the operator sees all startup
tokens and can directly access SQLite/files. This is application-level actor
isolation for a local demo, not isolation from a host administrator. SQLite and
blob files cannot commit atomically across a crash or power loss; orphaned files
may require recovery. Tests cover normal errors, cancellation and retry, not
physical power-loss durability.
