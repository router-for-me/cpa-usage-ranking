# Benchmark and Capacity Testing

[简体中文](benchmark.zh.md) · [README](../README.md)

This document summarizes the sustained 500 TPS SQLite ingestion benchmark and provides a quick reference for the built-in `cmd/ranking-capacity` load and ledger-verification CLI.

## 500 TPS Sustained Ingestion Benchmark

**Test date: 2026-11-13.** Under a `2 vCPU / 1 GiB RAM (0 swap)` cgroup v2 limit, the server sustained **499.91 logical report arrivals/sec** for **1,200.727 seconds** (~20 minutes) while draining an initial backlog and completing 4 periodic leaderboard cache rebuilds.

> **Scope & traceability**: This measurement validates the core ingestion engine (Go + SQLite WAL + `synchronous=FULL` + serialized write queue + settlement + atomic cache). It ran against an earlier build prior to GitHub OAuth registration and `service_tier` removal (about 1% of requests used the former registration route and snapshots included per-tier rows; generator baseline `fd5a81c`, tested server binary SHA-256 `3c70530c8131497465b469f1164dbb476f05a3967a8fc65c627bbff8bc0e3413`, server source commit unconfirmed). Treat it as a workload-specific reference rather than a maximum throughput, zero-error, or current-schema capacity guarantee.

### Environment and Workload

| Item | Server Host (Under Test) | Load Generator Host |
| --- | --- | --- |
| CPU / vCPU | Intel Xeon E5-2690 v3 @ 2.60GHz (3 vCPU KVM) | AMD Ryzen 9 9950X (2 vCPU KVM) |
| Visible RAM / Disk | ~2,978 MiB / 45 GiB VirtIO ext4 | ~3,884 MiB / 50 GiB VirtIO ext4 |
| OS / Kernel | Debian 13 / 6.12.113 | Debian 13 / 6.12.113 |
| Service cgroup v2 | `200%` CPU (`2 vCPU`), `MemoryMax 1 GiB`, `MemoryHigh 768 MiB`, `MemorySwapMax 0` | — |
| Software | Go `1.26.10` (CGO), Gin `1.12.0`, `go-sqlite3 1.14.53` / SQLite `3.54.1` (`WAL`, `FULL` sync, `5m` cache refresh) | OpenSSH `10.0p2` tunnel to loopback HTTP |

- **Dataset & arrival model**: Pre-seeded with 10,000 identities and 90-day historical shape (growing from **14,575 to 20,622** identities during confirmation), starting with **16,330 pending reports** plus **2,500 injected backlog items**, alongside a 500 TPS open-loop arrival rate (~90% today, ~10% yesterday; 60/30/10 light/medium/heavy clients; 10% hot clients driving 50% of updates; payload upper bounds P50 ≤1,344 B, P95 ≤7,808 B, P99 ≤14,208 B).
- **Rate limits**: Relaxed test-only limits were used (`10 µs` / `100,000` global burst; `100 µs` / `1,000` per-client burst) to measure engine throughput rather than default deployment throttles.

### Throughput and Backlog Recovery

| Metric | Result |
| --- | --- |
| New ordinary logical reports | `600,254` (**499.91/s**) |
| Confirmed ACKs (including backlog drain) | `619,077` (**515.59/s**) |
| Total HTTP attempts (reports / legacy registrations) | `708,155` (`702,108` / `6,047`, including `84,445` retries) |
| Report `200` (matching revision) / `503` | `619,077` / `83,024` (`11.83%` of received report responses) |
| Cache generation advances during window | `4` |
| Unconfirmed after shutdown drain | `6` (3 committed in DB with uncertain ACK; excluded from success counts) |

98.70% of all `503` responses occurred during the initial **185.727 s** backlog-drain phase (`42.36%` `503` rate while draining a peak backlog of `19,790` items). After clearing the initial backlog, the remaining **1,015.000 s** sustained ~`500.09/s` with a **`0.212%`** `503` rate (median pending depth: `5`, P99: `114`).

### End-to-End Latency (Planned Arrival to Confirmed ACK)

| Sample Scope | Confirmed Samples | P50 | P95 | P99 | Max |
| --- | --- | --- | --- | --- | --- |
| Full confirmation window (ordinary arrivals) | `600,247` | `14.24 ms` | `2.24 s` | `42.91 s` | `169.57 s` |
| Post-backlog steady phase | `507,584` | `11.44 ms` | `89.36 ms` | `205.34 ms` | `9.14 s` |

### Resource Usage and Data Integrity

- **Server process**: ~`1.01` average CPU cores, `120.28 MiB` peak RSS (process RSS excludes cgroup page cache); host minimum available memory stayed at `1,544.69 MiB`.
- **Ledger reconciliation**: Rebuilding expected state from the seed database and full load ledger (including tracking the 3 DB-committed uncertain-ACK items separately without counting them as confirmed) matched the final SQLite state across `38,622` daily revision/digest rows, `246,565` daily metric rows, and `920,000` period metric rows (all 12 metrics), passing SQLite integrity and foreign-key checks with zero miscounts.

## Capacity Test CLI (`cmd/ranking-capacity`)

`ranking-capacity` is an opt-in developer CLI for synthetic fixture seeding, bounded load generation, and offline database reconciliation. It is **excluded from production Docker images**—run it only against an isolated test server and synthetic database with external host resource limits.

> **Security note**: The generated fixture manifest contains a seed that derives synthetic Bearer secrets. Keep fixture directories (`0700`), manifests (`0600`), and JSONL ledgers private and never commit or reuse them with real identities.

### Build and Seed Synthetic Data

```sh
WORK=/path/to/private-test
install -d -m 700 "$WORK"
go build -o "$WORK/ranking-capacity" ./cmd/ranking-capacity

# Standard 8-model fixture for `load` or `burst`
# (For `sustained`, use `seed-sustained --model-seed 1` into a separate DB/fixture.)
"$WORK/ranking-capacity" seed \
  --db "$WORK/seed.sqlite" --fixture "$WORK/fixture.json" \
  --models config/models.json --clients 6000 --history-days 90 --backlog 1000
```

Keep `seed.sqlite` untouched as the reconciliation baseline. Start an isolated test `ranking` server against a **fresh copy** of the corresponding seed database for each independent load run.

### Generate Load (Independent Modes)

Each mode below is an **independent run** against pre-seeded seats (`PUT /v1/clients/report/{utc_day}`, no GitHub OAuth traffic). Do **not** run `load` and `burst` sequentially against the same DB copy—both start at revision `2`, so a shared DB turns the second run into idempotent ACKs and prevents combined ledger verification:

```sh
# Mode A: Fixed-rate open-loop load (no automatic retry)
"$WORK/ranking-capacity" load \
  --fixture "$WORK/fixture.json" --url http://127.0.0.1:4210 \
  --rps 10 --duration 10m --revision 2 --changed 2 \
  --ledger "$WORK/steady.jsonl"

# Mode B: Synchronized barrier wave + retryable background cycles (fresh DB copy)
"$WORK/ranking-capacity" burst \
  --fixture "$WORK/fixture.json" --url http://127.0.0.1:4210 \
  --duration 2m --cycle 10m --changed 2 \
  --ledger "$WORK/attempts.jsonl" \
  --events "$WORK/tasks.jsonl" --samples "$WORK/samples.jsonl"

# Mode C: Interactive Poisson sustained driver (against `seed-sustained` DB copy)
# Reads JSON lines on stdin: stage / pause / resume / stop; supports .jsonl or .jsonl.gz
"$WORK/ranking-capacity" sustained \
  --fixture "$WORK/sustained-fixture.json" --models config/models.json \
  --model-seed 1 --arrival-seed 2 --url http://127.0.0.1:4210 \
  --journal "$WORK/sustained.jsonl.gz"
```

### Offline Verification and Read Probing

Stop the test server cleanly first so SQLite has a consistent snapshot, then pass **all** ledgers generated against that database copy:

```sh
# Inspect raw row/metric totals
"$WORK/ranking-capacity" inspect --db "$WORK/running.sqlite"

# Verify all `load` or `burst` ledgers for one DB copy (comma-separated)
"$WORK/ranking-capacity" verify \
  --fixture "$WORK/fixture.json" --db "$WORK/running.sqlite" \
  --ledgers "$WORK/steady.jsonl"

# Verify and summarize a `sustained` journal against its untouched seed DB
"$WORK/ranking-capacity" verify-sustained \
  --fixture "$WORK/sustained-fixture.json" --models config/models.json \
  --model-seed 1 --seed-db "$WORK/sustained-seed.sqlite" \
  --db "$WORK/final.sqlite" --journal "$WORK/sustained.jsonl.gz" \
  --expected-db "$WORK/reconstructed.sqlite"
"$WORK/ranking-capacity" summarize-sustained \
  --journal "$WORK/sustained.jsonl.gz" --models config/models.json

# Time read-only statistics queries across periods (does not benchmark full cache builds)
"$WORK/ranking-capacity" probe \
  --db "$WORK/running.sqlite" --day "$(date -u +%F)"
```

Run `ranking-capacity <subcommand> -h` for the full flag list and safety bounds (`--max-in-flight`, `--max-pending`, `--max-journal-bytes`, `--active-budget`, and `assess-sustained`).
