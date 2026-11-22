# Storage, Settlement, and Caching

[简体中文](storage.zh.md) · [README](../README.md)

The server uses a single local SQLite database initialized directly to the current schema on startup—no external database server is required, and no sample participants or legacy-schema migrations are performed.

## Engine, File Location, and Connection Pools

- **Linked SQLite engine**: Standard builds use `database/sql` with `github.com/mattn/go-sqlite3` `v1.14.53`, bundling SQLite `3.54.1` (including the official [WAL-reset fix](https://sqlite.org/wal.html#walreset) from 3.51.3). CGO and a C compiler are required at build time; no system `sqlite3` library is needed at runtime.
- **Database path (`RANKING_SQLITE_PATH`)**: Defaults to `data/ranking.db` (source) or `/data/ranking.db` (Docker). Treated strictly as a file path (URI characters are escaped; no in-memory mode). Missing parent directories are created with mode `0700` (subject to umask). Invalid or inaccessible files fail startup without deleting or repairing existing files.
- **Local storage & WAL sidecars**: WAL requires local filesystem coordination—never place the database on NFS/network shares or share it across multiple replicas. Keep `-wal` and `-shm` sidecars intact while running; stop the service cleanly before offline copies or use SQLite-aware backups (see [Deployment & Configuration](deployment.md#data-permissions-stopping-and-backups)).

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

| Table / Area | Key & Purpose |
| --- | --- |
| `clients` | Stable positive `AUTOINCREMENT` `id`; unique `(github_id, cpa_uuid)` seat identity; `display_name`, `avatar_id`, `created_at_ms`; independent `disabled`, `ranking_hidden`, and `withdrawn` flags |
| `client_writers` | `(id, client_id)` and globally unique 32-byte `SHA-256` `token_hash` per reporting installation (never raw credentials); credentials are never rebound across seats |
| Registration transactions | Bounded, short-lived pending OAuth requests (`10m` lifetime, max `2,000` unexpired rows) storing `cpa_uuid`, exact `return_to`, writer digest, deadline, and retained completion result until expiry |
| `models` | Stable positive integer IDs mapped to unique canonical model names (ID `0` is reserved for internal all-model totals; aliases and prices are not stored here) |
| `daily_writer_versions` | `WITHOUT ROWID` keyed by `(day, writer_id)`, storing each writer's latest `revision` and order-independent snapshot digest |
| `daily_stats` | `WITHOUT ROWID` keyed by `(day, client_id, model_id)` storing 12 raw sums/counts for the seat's active snapshot (model ID `0` holds the all-model total, separate from per-model rows) |
| `period_stats` | Keyed by `(kind, start_day, model_id, client_id)` storing finalized daily total summaries and finalized weekly, monthly, and cumulative (`0001-01-01`) sums/counts |
| Watermark & `admin_accounts` | Persistent forward-only `closed_before_day` UTC boundary and optional ordinary DB admin bcrypt hashes |

**Registration transaction safety**: The browser callback validates and consumes one-time state + browser proof + S256 PKCE without holding a SQLite write lock during the GitHub HTTP exchange, and never stores GitHub access tokens. Only `POST /v1/clients/register/complete` binds a writer inside a single `IMMEDIATE` transaction; errors roll back completely.

## Serialized Writer, Window Closure, and Settlement

- **Serialized writer**: A dedicated writer serializes report commits and maintenance work with a default `128`-entry report queue, a single-entry maintenance queue (alternating turns when both are busy), and a `5s` per-transaction timeout (`RANKING_REPORT_TRANSACTION_TIMEOUT`). SQLite lock waits respect timeouts, request cancellation, and graceful shutdown.
- **Persistent UTC window watermark (`closed_before_day`)**: Advances only forward from observed server UTC time (to at least UTC yesterday) on startup, periodically (at most `1m` recheck), and before/inside every report transaction. Clock rollbacks, retries, and restarts can never reopen a closed day.
- **Background settlement**: Once a UTC day falls before `closed_before_day`, a background producer scans closed snapshots in `(day, client_id)` order and settles each seat/day in its own transaction: adding its 12 raw sums/counts into the corresponding UTC weekly, monthly, and cumulative rows in `period_stats`, writing a total-only finalized daily summary (omitted for empty snapshots), and deleting the `daily_stats` and `daily_writer_versions` source rows. Checked arithmetic overflow or DB errors roll back the item for retry without double-counting.
- **Retention (`RANKING_SETTLED_DAY_RETENTION_DAYS`)**: Applies only to finalized `period_stats` (never unsettled `daily_stats`), running in bounded batches through the same writer:
  - **Daily total summaries**: retains UTC today plus the preceding `30` days by default (`31` days inclusive; configurable `1`–`36500`).
  - **Weekly rows**: retains the current and previous Monday-start UTC weeks (inclusive).
  - **Monthly and cumulative rows**: retained indefinitely.

## Candidate Reads and Atomic Cache Worker

- **Consistent read snapshots**: For each UTC period, candidate reads combine finalized `period_stats` with any unsettled `daily_stats` in that period inside a single `mode=ro` transaction (annual views merge finalized monthly rows with the current year's unsettled daily rows; all-time includes the full unsettled backlog). Seat profiles and `disabled`/`ranking_hidden`/`withdrawn` flags are read in the same transaction so ineligible seats are excluded before scoring.
- **Derived metrics**: Ratios and averages are computed from raw int64 sums and sample counts without mutating stored totals; zero denominators yield unavailable (`null`) rather than synthetic zeros.
- **Atomic 6-period cache**: A single serial cache worker builds all 6 periods × 5 boards at startup and on `RANKING_CACHE_REFRESH_INTERVAL` (default `5m`). Only a complete successful round replaces the immutable in-memory snapshot; failures retain the previous round along with its original period bounds and `generated_at`. Ordinary reports and settlement do not trigger immediate rebuilds, whereas admin moderation mutations and client withdrawals signal an asynchronous coalesced refresh.
- **Safety bounds**: Default guards (`100,000` candidates per view, `16 MiB` encoded round, `32` build items) prevent unbounded memory growth and are safeguards rather than capacity claims.
