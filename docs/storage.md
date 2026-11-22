# SQLite storage

The service initializes its current schema before listening for HTTP requests.
It creates the [client seats and writers](client-storage.md), bounded registration
transactions, model dictionary, report-window watermark, daily snapshots and
writer versions, finalized period statistics and ordinary administrator tables;
startup creates no participant records. Startup
atomically registers canonical model names. IDs are positive and stable; ID 0
is reserved for internal totals and is not a dictionary row. Removed names
remain in the dictionary but are not active until reintroduced. The watermark
advances to at least UTC yesterday. See the [administrator console](admin.md).
Opening a database alone does **not** make `/readyz` return success: the first
complete leaderboard cache round must also succeed.

## Build and file location

The standard build uses `database/sql` with `github.com/mattn/go-sqlite3`
v1.14.53, bundling SQLite **3.54.1**. It requires CGO and a C compiler; it does
not require the system `sqlite3` command or a separately installed SQLite
library. Tests query the actual linked engine, not the command-line utility.
This release includes the official [WAL-reset fix](https://sqlite.org/wal.html#walreset)
(first fixed in 3.51.3). Builds using a different system SQLite library must
be verified separately.

```sh
RANKING_SQLITE_PATH=/srv/cpa-ranking/data/ranking.db go run ./cmd/ranking
```

- Default: `data/ranking.db`, relative to the process working directory.
- This is a **file path**, not a DSN. URI characters in the filename are escaped;
  they cannot override connection settings. No in-memory database mode is offered.
- Missing parent directories are created with mode 0700 (subject to umask).
  Existing directory permissions are unchanged: protect the configured data
  directory and use local persistent storage writable by the service account.
- WAL requires local filesystem coordination; do not use an NFS/network share.
- SQLite may create `-wal` and `-shm` sidecars. Do not remove or copy them
  independently while the database is open. Stop the service before an offline
  copy, or use SQLite-aware backup tools.
- An inaccessible path or invalid database fails startup. The application does
  not delete, repair, rename or replace an existing database to make startup succeed.

## Connections

| Setting | Current value |
| --- | --- |
| Write pool | 1 open / 1 idle connection |
| Read pool | 8 open / 4 idle connections, created on demand |
| Reader access | SQLite `mode=ro` |
| Journal mode | WAL, verified at open |
| Synchronization | FULL |
| Foreign keys | enabled |
| Busy timeout | 5 seconds |
| Writer transaction start | IMMEDIATE |

Connection-local settings are supplied in the driver DSN, so new connections
and reconnects receive the same settings. Readers use independent snapshots;
a read transaction does not prevent a writer from committing in WAL mode.
Keep read transactions short so they do not hold back checkpoints. SQLite uses
automatic checkpoints; there is no custom checkpoint worker. The storage
connection layer has no general-purpose retry loop. Report and maintenance
writers implement bounded, cancellable SQLite lock retries; see
[daily report snapshots](report-snapshots.md).

The SQL connection bound is not a write-admission queue or a throughput promise.
Storage is closed after HTTP shutdown; both pools are closed even if one close
returns an error. Tests live under `internal/storage/test/` and use real files.

## Schema initialization

`storage.InitSchema(ctx, db.Writer)` creates the current tables and indexes in
one transaction and is safe to repeat on a database with the same structure.
There is no schema version ledger, upgrade, downgrade, repair or conversion of
old databases. Use a new database when the schema changes.

`models` stores only IDs and unique canonical names, not aliases or prices.
`daily_writer_versions` uses `(day, writer_id)` for revision and digest. The
single effective snapshot uses `(day, client_id)`; deleting it during settlement
also deletes its writer versions. Daily snapshots and versions use `WITHOUT ROWID`;
a partial index supports total-row reads. The total uses model ID 0 without
a model dictionary row; real details use positive model IDs. `daily_stats`
uses `(day, client_id, model_id)` and `period_stats` uses
`(kind, start_day, model_id, client_id)`, both without a tier dimension.
Its daily rows are total-only; other periods keep totals and model details separately. Weeks start Monday UTC and months start on day 1
UTC, both with inclusive starts and exclusive ends. Cumulative rows use the
fixed internal start `0001-01-01`. Each row stores the same 12 sums and counts,
not averages. Totals and details are alternative views and must not be summed together.

Internal candidate reads combine one period's finalized rows with all unsettled
rows in that UTC period, in a single read-only SQLite transaction. Cumulative
reads include the full backlog. The transaction ends before derived processing;
reads time out independently and reject overflow or more than 100,000 clients
per candidate view. All-model totals and individual model views are
separate. Daily views support totals only, because finalized model-level daily
rows are not retained. Annual views combine finalized monthly rows with the year's unsettled daily rows in the same transaction. Public builds also read profiles and exclude disabled, hidden, and withdrawn seats before scoring in that snapshot. The limit is not a verified client-capacity claim.

Candidate metrics may be derived from raw sums and counts without changing the
stored int64 values. Ratios and means are unavailable when their denominator
is zero; measured zero remains distinct from a missing measurement. Means use
actual sample counts, and coverage is samples divided by requests. Overall
output tokens/second is calculated only with complete latency coverage and a
positive latency sum. The estimate excluding first-token wait additionally
requires complete TTFT coverage and latency sum greater than TTFT sum. These
are aggregate estimates, not concurrent throughput or an arithmetic mean of
per-request speeds; incomplete coverage is not guessed.

The internal cache worker attempts initialization at startup and refreshes at
`RANKING_CACHE_REFRESH_INTERVAL` (default `5m`, a positive duration). Build
items run sequentially; every item releases its read transaction before encoding.
Only a complete successful round replaces the previous immutable snapshot. A
failure, timeout or capacity limit retains the previous round; a missing first
round is unavailable, and reads never rebuild it. Pending ticks during a build
are discarded. Ordinary uploads and settlement do not trigger builds; a
committed withdrawal requests an asynchronous refresh and supersedes a build
already in progress. Six real period items each build five leaderboards; see [leaderboard calculations](leaderboards.md). The initial
100,000-candidate, 16 MiB encoded-round and 32-item limits are safety bounds,
not evidence of tested client capacity.
