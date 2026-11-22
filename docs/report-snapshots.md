# Daily report snapshots

Submit an authenticated full daily snapshot with `PUT /v1/clients/report/{utc_day}`. Use HTTPS and the writer's own `Authorization: Bearer <secret>` as described in the [registration API](registration-api.md). That credential selects the writer and its seat; neither the body nor query parameters select a `client_id`.

```http
PUT /v1/clients/report/2026-11-01
Authorization: Bearer <secret>
Content-Type: application/json

{"schema_version":1,"revision":"8","items":[{"model":"vendor/claude-fable-5","total_tokens":"120","input_tokens":"80","output_tokens":"40","cache_read_tokens":"60","cache_write_tokens":"5","reasoning_tokens":"10","request_count":"2","success_count":"2","ttft_avg_ms":null,"ttft_samples":"0","latency_avg_ms":3000,"latency_samples":"2"}]}
```

Each submission replaces the entire day's matched snapshot, not just changed models. An empty array clears prior statistics while keeping the new version. This is a breaking protocol change: `service_tier` is no longer accepted, even as `default` or `null`. Clients must merge usage by model before submission; the server does not merge tiers. No identity, model list or ignored-model list is returned.

A report object contains `schema_version` (JSON integer `1`), `revision` (nonnegative int64 decimal string, starting at `0`), and `items` (an array, possibly empty). Each item requires `model`, six token fields (`total_tokens`, `input_tokens`, `output_tokens`, `cache_read_tokens`, `cache_write_tokens`, `reasoning_tokens`), `request_count`, `success_count`, `ttft_avg_ms`, `ttft_samples`, `latency_avg_ms`, and `latency_samples`. All counts and token values use decimal strings; averages use JSON numbers or `null`.

Every listed field is required even when zero or null. Duplicate and unknown JSON object fields, invalid UTF-8, unpaired Unicode surrogates in model names, out-of-range integers, and extra JSON values are rejected. Decimal strings may have leading zeroes; a plus sign is not accepted. All counts and token values must be nonnegative int64 values. Successes and each sample count cannot exceed requests; cache read plus cache write cannot exceed input, reasoning cannot exceed output, and input plus output cannot exceed the reported total. Total tokens remain the client's value and may include an unclassified remainder. A nonzero usage item must have requests. Averages must be null when their sample count is zero, and otherwise finite and nonnegative; averages are multiplied by their own sample counts with overflow checks. Missing measurements must not be invented as zero. Any invalid item rejects the whole report, including an unknown model.

Raw model names must be valid UTF-8, at most 128 bytes, without control characters, and have a nonempty final path component after trimming. Duplicate normalized model names and aliases resolving to the same canonical model are rejected, including zero-execution items. Clients must combine their source groups (including any tiers) into a single model record: sum counts and duration sums, then divide each duration sum by its own sample count to produce its weighted average. Never average group averages equally. A stable, typed, order-independent digest includes every item, including valid unknown and zero-execution items, before catalog matching; it does not include catalog or price configuration. Unknown models are skipped from statistics and totals. Zero-execution items produce no rows. Integer and floating-point aggregation overflow rejects a replacement.

Revisions and digests are tracked per **writer and UTC day**. A writer's first revision may be zero. Its first report or any higher revision replaces the whole **seat/day** snapshot. Writers do not compare revisions with one another and there is no addition across writers: the last committed replacement wins. An identical revision and digest confirms that writer's prior report without reprocessing or overwriting current data, even if another writer's snapshot is now effective. An older revision or the same revision with a different digest returns `409` with the writer's current decimal-string `revision` and an `error` object. A `200 {"revision":"8"}` acknowledges that writer's committed revision. A replacement atomically saves writer revision/digest, seat received time, changed detail rows, and total. It can decrease values, delete omitted rows or leave an empty snapshot. Identical retries do not rewrite any metadata or rematch the current catalog. A failed transaction does not acknowledge a replacement.

The reporting window is UTC today and yesterday. Future or invalid dates are rejected; older dates are expired. The database has separate daily revision metadata and 12-value statistics tables; each nonempty matched snapshot has one internal total row, separate from model detail. A persistent `closed_before_day` watermark advances only forward from observed server UTC time, independently of any report transaction. Startup advances it, and the service observes the UTC boundary even without reports (with a one-minute maximum recheck interval for clock corrections). A report write must advance it separately before its transaction and recheck time and watermark inside the transaction, including before commit. The writer's seat and its disabled/withdrawn status are rechecked in that transaction, before any report or identical retry can succeed.

The internal writer serializes report and maintenance transactions, with a default 128-entry pending report queue, a separate single-entry maintenance queue, and a five-second transaction timeout for either kind. When both have work, they take turns. Full or closed queues reject new submissions. SQLite write-lock waits for a report are retried within that deadline and respond to request cancellation or shutdown. Startup/background window checkpoints use the same cancellable lock handling with a five-second maximum attempt; shutdown can cancel them sooner. The original connection busy timeout is restored before returning it to other database writers. Cancellation while enqueuing or waiting for a write returns 503 if a response can still be delivered, never an empty 200. A lost response around commit is ambiguous; retry the same revision. Shutdown stops admission, drains accepted work within its deadline, then cancels unfinished work and waits for rollback before closing the database.

A closed snapshot can be finalized internally using the persistent watermark,
not the current wall clock. Each client/day is one transaction: its separate
model details and total are added to the corresponding UTC week, month and
cumulative rows, a total-only daily summary is stored, then the daily source and
all its writer versions are deleted. Empty snapshots produce no summary. Checked integer
and floating-point sums or a database failure roll back the entire item, leaving
the source for retry; rerunning a committed item cannot add it twice. Durations
remain sums with their own sample counts. This storage step does not expose a
ranking or historical query.

After startup, a background producer scans closed snapshots in `(day, client_id)`
order using bounded pages and limited rounds, submitting each item separately to
the writer. Failures roll back that item and do not block later keys. A bounded
sweep-wide cooldown delays retry after errors without retaining an unbounded
per-client failure map; newly inserted keys behind the cursor may wait for the
next sweep. Warnings are rate limited. Internal logs periodically report the
closed backlog count, oldest UTC date and its age, with a bounded read timeout;
no management API is added. The producer stops before the writer drains or
cancels accepted work, and the database closes only after transactions release
it. SQLite external-lock waits during maintenance obey the transaction deadline
and shutdown cancellation, restoring the connection's ordinary busy timeout.

Retention touches finalized `period_stats` only, never `daily_reports` or
`daily_stats`. By default daily total summaries retain UTC today and the
preceding 30 UTC dates (31 dates including today); the cutoff itself is kept.
Weekly details and totals retain the current UTC Monday-based week and the
previous week, also inclusive. Monthly and cumulative rows are retained
indefinitely. A deployment can adjust the daily count using
`RANKING_SETTLED_DAY_RETENTION_DAYS` (default `31`, range `1`–`36500`).
Cleanup runs in bounded batches through the same writer and deadline as
settlement; a large backlog takes multiple rounds. If an old snapshot is still
awaiting settlement, its source remains until settlement succeeds.

Report admission is separate from registration. All writers of one seat share its report budget. Configuration is read at startup; values must be positive. These conservative starting defaults are deployment parameters, not a validated 100,000-client capacity promise:

| Environment variable | Default | Meaning |
| --- | --- | --- |
| `RANKING_REPORT_MAX_BODY_BYTES` | 131072 | Maximum ordinary JSON body bytes |
| `RANKING_REPORT_MAX_ITEMS` | 128 | Maximum raw items before model matching |
| `RANKING_REPORT_DECODE_CONCURRENCY` | 16 | Concurrent body reads and decodes |
| `RANKING_REPORT_QUEUE_CAPACITY` | 128 | Pending report writes |
| `RANKING_REPORT_TRANSACTION_TIMEOUT` | `5s` | Per-report or per-maintenance writer execution deadline |
| `RANKING_REPORT_CLIENT_INTERVAL` | `1m` | One token per interval per authenticated client |
| `RANKING_REPORT_CLIENT_BURST` | 2 | Per-client token bucket capacity |
| `RANKING_REPORT_GLOBAL_INTERVAL` | `5ms` | One global token per interval |
| `RANKING_REPORT_GLOBAL_BURST` | 400 | Global token bucket capacity |
| `RANKING_REPORT_MAX_CLIENTS` | 100000 | Maximum tracked client rate states |
| `RANKING_REPORT_STATE_TTL` | `10m` | Idle time before a fully refilled state may expire |

Rate state is bounded in memory and resets on restart. Capacity exhaustion rejects new clients instead of evicting active state. No request decompression is provided. Send ordinary `application/json` with no `Content-Encoding` or `identity`; charset parameters are accepted. Authentication and admission happen before complete body parsing, and rejected attempts consume global/client admission where applicable.

## Responses and retries

| Status | Code / body | Meaning |
| --- | --- | --- |
| 200 | `{"revision":"8"}` | Committed revision, or confirmation of an identical committed retry |
| 401 | `invalid_credential` | Missing, invalid or unknown Bearer credential |
| 403 | `client_disabled` | Seat disabled before or after admission |
| 403 | `client_withdrawn` | Entire seat has withdrawn; stop reporting, including retries |
| 409 | `revision_conflict`, `revision` | Same revision with different content, or older revision for this writer |
| 410 | `expired_day` | UTC date is before the persistent open window |
| 413 | `request_too_large` | Body exceeds configured bytes |
| 415 | `unsupported_media_type` | Not uncompressed JSON |
| 422 | `invalid_day` | Invalid date or future UTC date |
| 422 | `invalid_report` | Invalid JSON, schema, fields, metrics, model name or aggregation |
| 422 | `too_many_items` | Raw item count exceeds configured limit |
| 429 | `report_rate_limited` / `report_capacity` | Per-client/global budget or state capacity exhausted |
| 503 | `report_overloaded` | Decoder, writer queue, shutdown or transaction deadline is busy |
| 500 | `internal_error` | Storage or other internal failure; no success confirmation |

Errors do not echo credentials or submitted reports. `429` and temporary `503` include integer `Retry-After` seconds. Retry after that delay with jitter; admission may still reject later. If a response is lost or a request times out, the transaction may already have committed: retry the exact same revision and full content. A retry is still subject to the UTC window and admission limits. The service provides no status-query or historical-repair endpoint. `/readyz` requires database and catalogs plus a successful complete leaderboard cache round; a failed refresh retains the previous round. Rejected reports, failed transactions, retries, clock rollback and restart cannot reopen an already recorded boundary.
