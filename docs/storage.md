# Storage, Settlement, and Caching

[简体中文](storage.zh.md) · [README](../README.md)

The server uses a single local SQLite database initialized directly to the current schema on startup—no external database server is required, and no sample participants or legacy-schema migrations are performed.

## Engine, File Location, and Connection Pools

- **Linked SQLite engine**: Standard builds use `database/sql` with `github.com/mattn/go-sqlite3` `v1.14.53`, bundling SQLite `3.54.1` (including the official [WAL-reset fix](https://sqlite.org/wal.html#walreset) from 3.51.3). CGO and a C compiler are required at build time; no system `sqlite3` library is needed at runtime.
- **Database path (`RANKING_SQLITE_PATH`)**: Defaults to `data/ranking.db` (source) or `/data/ranking.db` (Docker). Treated strictly as a file path (URI characters are escaped; no in-memory mode). Missing parent directories are created with mode `0700` (subject to umask). Invalid or inaccessible files fail startup without deleting or repairing existing files.
- **Local storage & WAL sidecars**: WAL requires local filesystem coordination—never place the database on NFS/network shares or share it across multiple replicas. Keep `-wal` and `-shm` sidecars intact while running; stop the service cleanly before offline copies or use SQLite-aware backups (see [Deployment and Configuration Guide](deployment.md#data-permissions-stopping-and-backups)).

| Connection Pool Setting | Value |
| --- | --- |
| Write pool | `1` open / `1` idle connection (`BEGIN IMMEDIATE`) |
| Read pool | `8` open / `4` idle connections (`mode=ro`) |
| Journal & Sync | `WAL` (verified on open) + `synchronous=FULL` |
| Foreign keys | Enabled |
| Busy timeout | `5s` |

Read transactions run on independent WAL snapshots without blocking writes. Both pools close cleanly after HTTP shutdown.

## Schema Overview

Schema initialization runs inside one transaction on startup and is idempotent on databases matching the current schema. When the schema changes incompatibly, deploy against a fresh database.

| Table | Key & Purpose |
| --- | --- |
| `clients` | Positive `AUTOINCREMENT` `id`; unique `(github_id, server_id)` seat; `display_name`, `avatar_id`, `created_at_ms`; independent `disabled`, `ranking_hidden`, `withdrawn` flags |
| `client_writers` | `(id, client_id)` and globally unique 32-byte `SHA-256` `token_hash` per writer (never raw credentials, never rebound across seats) |
| `registration_requests` | `WITHOUT ROWID` keyed by `id`; bounded `10m` OAuth requests (max `2,000` unexpired) storing `server_id`, `return_to`, `avatar_id`, writer digest, state/PKCE, and retained completion result |
| `models` | Stable positive integer IDs mapped to unique canonical model names (ID `0` reserved for internal totals; aliases and prices are not persisted) |
| `daily_reports` | `WITHOUT ROWID` keyed by `(day, client_id)`; seat/day parent row (`received_at_ms`) cascading deletes to versions and stats on settlement |
| `daily_writer_versions` | `WITHOUT ROWID` keyed by `(day, writer_id)`; each writer's latest `revision` and 32-byte snapshot digest |
| `daily_stats` | `WITHOUT ROWID` keyed by `(day, client_id, model_id)`; 12 raw sums/counts for the active snapshot (`model_id = 0` holds the all-model total) |
| `period_stats` | Keyed by `(kind, start_day, model_id, client_id)`; finalized daily totals plus finalized weekly, monthly, and cumulative (`0001-01-01`) sums/counts |
| `report_window` | Single-row (`id = 1`) forward-only `closed_before_day` UTC boundary |
| `admin_accounts` | Optional ordinary database administrator accounts (bcrypt password hashes only) |

**Registration transaction safety**: The browser callback validates and consumes one-time state, browser cookie proof, and S256 PKCE without holding a SQLite write lock during the GitHub HTTP exchange, and never stores GitHub access tokens. Only `POST /v1/clients/register/complete` binds a writer inside one `IMMEDIATE` transaction; errors roll back completely.

## Serialized Writer, Window Closure, and Settlement

- **Serialized writer**: A dedicated writer serializes report commits and maintenance work with a default `128`-entry report queue, a single-entry maintenance queue (alternating turns when both are busy), and a `5s` per-transaction timeout (`RANKING_REPORT_TRANSACTION_TIMEOUT`). SQLite lock waits respect timeouts, cancellation, and graceful shutdown.
- **Persistent UTC watermark (`closed_before_day`)**: Advances only forward from observed server UTC time (to at least UTC yesterday) on startup, periodically (at most `1m` recheck), and before/inside every report transaction. Clock rollbacks, retries, and restarts cannot reopen a closed day.
- **Background settlement**:
  - Scans snapshots before `closed_before_day` in `(day, client_id)` order and settles each seat/day in its own transaction, adding its 12 raw sums/counts into `period_stats` (weekly, monthly, cumulative) plus a total-only finalized daily row (omitted for empty snapshots).
  - Deletes the source `daily_reports` row (cascading to `daily_stats` and `daily_writer_versions`) in the same transaction; overflow checks or DB errors roll back the item for retry without double-counting.
- **Retention (`RANKING_SETTLED_DAY_RETENTION_DAYS`)**: Applies only to finalized `period_stats` (never unsettled `daily_stats`), running in bounded batches through the same writer:
  - **Daily total summaries**: retains UTC today plus the preceding `30` days by default (`31` days inclusive; configurable `1`–`36500`).
  - **Weekly rows**: retains the current and previous Monday-start UTC weeks (inclusive).
  - **Monthly and cumulative rows**: retained indefinitely.

## Candidate Reads and Atomic Cache Worker

- **Consistent read snapshots**: Candidate reads combine finalized `period_stats` with unsettled `daily_stats` in one `mode=ro` transaction (annual views merge finalized monthly rows with current-year unsettled daily rows; all-time includes the full backlog). Seat profiles and status flags are read in the same snapshot so banned, hidden, and withdrawn seats are filtered before scoring.
- **Metric storage and derivation**: Token counts, execution counts, and latency sample counts are stored as exact `int64` (`INTEGER`), while `ttft_sum_ms` and `latency_sum_ms` are stored as `float64` (`REAL`). Ratios and averages are derived without mutating stored rows; zero denominators yield `null` rather than synthetic zeros.
- **Atomic 6-period cache**: A single serial worker builds all 6 periods × 5 boards at startup and every `RANKING_CACHE_REFRESH_INTERVAL` (`5m`), atomically replacing the immutable snapshot only after a complete round succeeds (failures keep the previous snapshot). Ordinary reports and settlement do not trigger immediate rebuilds; moderation updates and withdrawals trigger an asynchronous coalesced refresh.
- **Safety bounds**: Default guards (`100,000` candidates per view, `16 MiB` encoded round, `32` build items) cap memory usage and are safeguards rather than capacity claims.
