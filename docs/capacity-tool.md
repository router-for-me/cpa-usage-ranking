# Synthetic capacity test tool

`ranking-capacity` is an opt-in development tool, not part of the deployed
service. Run it only against an isolated test service and synthetic database:
it sends real authenticated writes and can overload its target. The tool does
not enforce host resource budgets or protect other services; arrange external
limits, monitoring, a stop mechanism and sufficient disk space first. Treat
results as specific to the fixture, load schedule and measured environment, not
as an unconditional client-capacity guarantee. See the [500 TPS benchmark](benchmark.md)
for a measured workload, environment and result limitations.

## Fixture and load

Build with the same Go/CGO requirements as the service. Choose a private,
new test directory and run from the repository root:

```sh
install -d -m 700 /private/test
go build -o /private/test/ranking-capacity ./cmd/ranking-capacity
/private/test/ranking-capacity seed \
  --db /private/test/seed.sqlite --fixture /private/test/fixture.json \
  --models config/models.json --clients 6000 --history-days 90 --backlog 1000
```

Both output files must not already exist; a failed seed may leave partial files.
The manifest contains a random seed that derives **synthetic bearer secrets**:
keep it private, do not log it or commit it, and do not reuse it with real
identities. Fixture and SQLite files are created mode 0600, subject to umask.
`--day` defaults to the current UTC date; a running service accepts only its
current reporting window. The fixture prepopulates identities, today/yesterday
snapshots and synthetic historical period aggregates; historical prefill is
not evidence of actual settlement. `--backlog` retains the newest closed day
as pending source data for that many identities so the real service can
finalize it. Eight catalog models are used.

Start a separate test service against a **copy** of the closed seed database,
with its own data directory, origin and resource controls. After readiness and
any separately measured initialization, for example:

```sh
/private/test/ranking-capacity load \
  --fixture /private/test/fixture.json --url http://127.0.0.1:4210 \
  --rps 10 --duration 10m --revision 2 --changed 2 \
  --ledger /private/test/steady.jsonl
```

The bounded open-loop schedule distributes requests evenly across identities;
it does not simulate jitter or automatic client retry. `--changed` selects
1–8 changed model rows per full snapshot. `--both-days` alternates today and
yesterday for each identity, and `--offset` selects a zero-based starting
identity index. Revisions increase automatically after a complete rotation:
for every identity/day, a later phase must use a revision greater than **all**
its previously attempted revisions, including rejected attempts. Every phase
requires a new ledger; keep all of them. Summary JSON reports scheduled, sent,
confirmed, generator drops, statuses and successful-request latencies. Status
0 denotes a transport failure; an incorrect 200 body is not confirmation.

## Synchronized clients and recovery

`burst` is a separate bounded mode; it does not change `load`. For a fresh
fixture and test-service database, for example:

```sh
/private/test/ranking-capacity burst \
  --fixture /private/test/fixture.json --url http://127.0.0.1:4210 \
  --duration 2m --cycle 10m --changed 2 \
  --ledger /private/test/attempts.jsonl \
  --events /private/test/tasks.jsonl --samples /private/test/samples.jsonl
```

It prepares one client per synthetic identity and releases today's first
request at a barrier. Every tenth client has a yesterday task queued behind
its own confirmed today task. Each identity processes all dates serially.
An independent clock continues to create one today update per identity per
cycle, plus one yesterday update per cycle for every tenth identity. Thus the
planned background rate is approximately `1.1 * clients / cycle_seconds`;
slow responses do not pause that clock. Initial yesterday tasks are counted as
pending even while waiting for today's confirmation. Each snapshot contains
all eight fixture models; the normal `--changed 2` changes two model rows.

Each retry retains its logical task, revision and body. Transport errors,
408, 429 and 5xx can be retried; retry delay observes `Retry-After` and adds
bounded identity-specific jitter. Defaults are `--retry-spread 1m`,
`--max-retry-delay 2m`, `--max-attempts 30`, and `--request-timeout 20s`.
Other 4xx, a bad success acknowledgement, exhausted retry bounds, a full
per-client queue, the total pending limit or evidence-write failure stop the
run explicitly; they do not silently discard work. The defaults are
`--queue-per-client 16` and `--max-pending 4*clients`; this mode permits up
to 20,000 clients and one million planned tasks. Every output path must be
new, and partial files can remain after an error.

The attempt ledger records task ID, attempt number, status, Retry-After,
planned/start/connection acquisition/local request-write/completion timestamps
and confirmation. It does not directly log request Authorization headers or
bodies, but includes the server's raw `Retry-After` header, truncated to 128 bytes;
protect the ledger as a private file. The task event file records each created
task even if it is never sent. The `release` event
supplies the initial tasks' common planned time (their created events use
zero); only a valid matching revision is confirmed. One-second samples include
produced, confirmed, pending, retries, statuses, connection waiting and the
number/peak of **written but unfinished** attempts. For a normal duration
expiry, `load_end` is captured before canceling outstanding requests;
external cancellation or a fatal error can race with that sample. The
`after_cancel` sample includes stopping effects. The summary reports a full
first-attempt write span only if every initial client wrote; otherwise that
field is null, and `observed_initial_write_span_ms` covers only the subset.
The delay from release likewise covers the last observed write. A late trace
callback makes the measurement incomplete. The `WroteRequest` timestamp is a write by the local
HTTP client to its connection, not proof of receipt or execution at a remote
server behind a tunnel; connection waiting is distinguishable from it.
Neither client count nor goroutine count is an in-flight measurement.

At the duration or an external cancellation the generator stops producing and
cancels outstanding requests; it does not silently drain the queue after load.
Reconcile created task events against confirmed attempts and retain all
unconfirmed tasks. A separate observer is necessary for server-side settlement
backlog and resource/cache measurements. Ensure the load generator and its
network path can actually produce the desired wave before interpreting a
capacity result.

## Experimental random sustained driver

`seed-sustained` and `sustained` are experimental building blocks. Invoking
these commands alone does **not** start a watchdog, enforce the search budget,
select stages, manage service transitions or prove a continuous confirmation.
Local mock results are not capacity measurements.

This guide covers the standalone CLI. Provision an isolated service, resource
limits, monitoring and a reliable stop mechanism separately. Environment-specific
remote orchestration and checkpoint-recovery scripts are not a portable deployment
or general-purpose recovery interface. A generator, transport, memory or evidence
limit must not be reported as an application capacity limit.

`assess-sustained` evaluates supplied stage and confirmation observations; it
does not generate traffic or establish host safety. Its recovery criteria are
measurement rules, not product latency or error-rate SLOs.

For each search or confirmation observation, marked `5 * rate` arrivals are
kept separate from ordinary input. Post-marker contiguous one-minute windows
must each supply at least 95% of target ordinary input; an overall average alone
cannot conceal a missing window. Once the burst has been produced, residual
debt is considered a recovery candidate when it falls overall and in the latter
half, with more falling than rising windows and at least two falling windows.
Brief reversals do not by themselves reject an otherwise clear downward trend;
flat or conflicting evidence remains uncertain. Alternatively, a verified
clear can leave a short ordinary in-flight tail if the final two windows are not
both growing. A confirmation additionally requires continuous active time and
three observed cache-generation changes; it does not require the old debt to
reach zero or impose a minimum decline speed. These are observation rules, not
an assurance that unbounded growth in database size remains sustainable.

For a new private local test directory and isolated test service, use
`seed-sustained` with the same fixture flags as `seed`, plus a fixed
`--model-seed`. The `--models` catalog must contain at least 48 distinct canonical
model names. It seeds variable-size today/yesterday snapshots and the same
synthetic historical shape/closed-day backlog. Save the stopped seed database
unchanged for later reconstruction; use a separate copy for the test service.
The manifest is private and contains the seed of the synthetic bearer secrets.
The fixed synthetic population is `--clients` offline-created seats, one writer
per seat. Use the current tool to create fresh seeds/journals; earlier mixed
registration workloads and their artifacts require their original tool version.

Start `sustained` with a private new `--journal`, `--fixture`, formal
`--models`, matching `--model-seed`, fixed `--arrival-seed`, and a numeric
loopback `--url`. It starts paused and reads one JSON command per input line:

```json
{"operation":"stage","stage":"example","rate":100}
{"operation":"pause"}
{"operation":"resume"}
{"operation":"stop"}
```

A `stage` starts a reproducible Poisson clock and adds `round(5 * rate)`
separately marked tasks. `pause` stops creation and dispatch without deleting
queued work; already in-flight requests can finish. `resume` continues that
stage without injecting another marked debt. `stop`, end of input, or a signal
cancels in-flight attempts and closes the journal, retaining ambiguous and
unconfirmed work. Restart with the same options, the same untouched fixture
and database, and `--resume`; the driver initially remains paused. Do not
replace the journal or use a different one to reset its active-time budget.
A malformed, truncated, unclean, or failed journal is **not** automatically
repaired or resumed. If a scheduled arrival cannot be materialized within a
local safety limit, the journal records the boundary when possible and remains
unclean; the run is tool-limited, not evidence of a server limit.

A **new** journal whose path ends in `.gz` is written as a streaming gzip JSONL
file. Each clean invocation writes a complete gzip member; resuming appends a
new member without rewriting previous evidence. Replay and `verify-sustained`
auto-detect both the original plain JSONL and gzip, check gzip trailers, and
reject an incomplete final JSONL record. Do not rename a journal between the
plain and `.gz` formats when resuming. Compression is lossless; it does not
remove request bodies, deltas or attempts. `--max-journal-bytes` applies to the
physical compressed bytes for gzip files and the physical bytes for plain
files; buffering and attempts already in flight can exceed this safety bound,
so it is **not** a filesystem quota. Estimate free space separately and retain
disk headroom. Compression also consumes generator CPU and must not be mistaken
for a server bottleneck.

Optionally supply a new private `--samples` JSONL path to the sustained driver
for a small live cumulative-sample stream. Immediately before a stage or resume,
the driver emits a `boundary: "before_stage"` sample: it accounts for all old
arrivals due at the recorded boundary, and the following stage samples contain
the same `stage_start_unix_ns`. The authoritative full journal remains required
for replay and verification; missing or stale live samples invalidate an
observation. Sample output failure cancels the driver. A different new sample
path is required on each invocation.

Each task is a frozen full snapshot and independently reconstructible per-row
delta. Tasks only call `PUT /v1/clients/report/{utc_day}` for pre-seeded seats;
there is no OAuth registration in the workload. About 10% of seats are hot and
receive half of selections when the hot group is present.
Client classes are 60/30/10 percent light/medium/heavy. Their per-client model
row limits are drawn from 1–4/5–16/17–48 respectively; actual daily snapshots
can contain fewer rows and grow with updates. Task days are approximately 90/10 today/yesterday;
calls per update are 80% 1–3, 18% 4–20 and 2% 21–100. Model selection is skewed toward hot models; see
`internal/capacity/sustained_model.go` for the fixed, versioned draw sequence.
These are synthetic assumptions, not a measured production distribution.

The private JSONL journal contains stage commands, planned/created tasks,
complete synthetic bodies and delta records, attempt starts and finishes,
sanitized error codes, and one-second cumulative samples. It contains no
bearer secret or Authorization header, but still reveals detailed test data.
`produced - confirmed` includes unsent, retrying and
in-flight tasks; only a correct matching acknowledgement counts as confirmed.
Retries preserve identity, day, revision and body and respect `Retry-After`.
The default local bounds are 200,000 pending tasks, 4,096 in-flight attempts,
30 attempts per task, a 4 GiB journal and a one-hour cumulative active-time
ceiling. For a new journal, the opt-in sustained CLI accepts up to 512 attempts
per task, including the first attempt. Use the same limits when resuming; editing
the journal header or resetting attempt counters is not supported by this
standalone workflow. Exponential backoff with jitter is capped at 15 seconds,
unless `Retry-After` requires a longer wait. Bound exhaustion is a tool limitation,
not an application ceiling.
The journal is read sequentially with bounded individual records; completed
snapshots are not accumulated per task. Replay and verification still retain
active identities, per-day state, outstanding tasks and each identity/day's
latest confirmed snapshot, so their memory grows with those quantities and is
not bounded by a constant independent of workload.
UTC rollover stops the driver instead of inventing a reporting window. Resource,
SSH, cache, settlement and host safety observations require separate tooling.

After stopping the isolated service, obtain a consistent final SQLite snapshot
with no nonempty WAL, retain the closed seed database and journal, and use a
**new** reconstruction path:

```sh
/private/test/ranking-capacity verify-sustained \
  --fixture /private/test/fixture.json --models config/models.json \
  --model-seed 1 --seed-db /private/test/seed.sqlite \
  --db /private/test/final.sqlite --journal /private/test/sustained.jsonl \
  --expected-db /private/test/reconstructed.sqlite
```

This mode independently accumulates journal deltas and compares versions,
digests and all twelve daily and period metrics against the stopped database;
ambiguous report commits are counted separately. Its result does
not establish capacity or replace independent inspection of the raw evidence.

After full verification, `summarize-sustained --journal ... --models ...` reads
the same plain or gzip journal sequentially and reports per-created-stage body
size, model-row count, changed-row count, confirmation delay, identities touched,
today/yesterday split, model calls and attempt counts. A task confirmed in
a later stage remains attributed to its creation stage; use cumulative live
samples and stage assessments for actual time-window arrival and confirmation
rates. Nearest-rank body percentiles are upper bounds in 16-byte buckets;
confirmation-delay bounds use 10ms/100ms/1s buckets, and `-1` explicitly means
more than one hour. Outstanding IDs are limited by `--max-pending`; identity
bitmaps and stage groups are capped at two million identities and 32 stages.
A bound failure is a tool limitation, not a server limit. This summary does not
replace the full database verifier.

## Offline checks

Stop the test service and preserve a consistent SQLite database, including any
required WAL sidecars. Supply **every** ledger from that database, comma-separated:

```sh
/private/test/ranking-capacity inspect --db /private/test/running.sqlite
/private/test/ranking-capacity verify --fixture /private/test/fixture.json \
  --db /private/test/running.sqlite --ledgers /private/test/steady.jsonl
/private/test/ranking-capacity probe --db /private/test/running.sqlite \
  --day "$(date -u +%F)"
```

`verify` checks each touched identity/day's latest version, digest and every
model/total metric against the attempted canonical snapshot. It accepts repeated
versions only when attempts have the same task ID and content and consecutive,
unique attempt numbers; legacy receipts without a task ID must not reuse a
version. A final revision with only explicit 4xx attempts is an error. If a
rejection and a confirmed or ambiguous attempt share that version,
`rejections_mixed` counts the rejection without claiming whether it wrote.
An unconfirmed 5xx, transport failure or incorrect success body can be an
ambiguous commit; if its revision is the final committed version,
`unconfirmed_commits` counts it separately rather than treating it as
confirmed. A rejected revision later superseded by another commit cannot be
proven unchanged from final state: `rejections_superseded` reports this
limitation, while `rejections_checked` counts rejections with an older final
version. Keep rejection and subsequent recovery identities disjoint when that
proof matters.

`inspect` reports raw counts and totals. `probe` separately times real
statistics-reader calls and sums their returned metrics; it does **not** time a
complete background cache refresh. Its timing depends on the machine, resource
limits and cache state, so measure and disclose those separately. Full rotation,
cache refreshes, rejected load, maintenance and consistency need independent
coverage; a short probe alone cannot establish stable capacity.
