# Public leaderboards

`GET /v1/leaderboards/{board}/{period}` reads only the latest complete published cache. The allowed boards are `overall`, `usage`, `cache`, `success`, `ttft`; periods are `today`, `yesterday`, `week`, `month`, `year`, `all`. No query parameters, arbitrary dates, model or tier selectors are accepted. Example:

```json
{"board":"usage","period":"today","start":"2026-12-31","end":"2027-01-01","generated_at":"2026-12-31T12:00:00Z","entries":[{"rank":1,"client_id":"42","display_name":"Example","avatar_id":"1","metric":"9007199254740993"}]}
```

`start` is inclusive and `end` exclusive, as UTC dates; `all` uses internal start `0001-01-01` and `end: null`. `generated_at` is the UTC start of the published build round, not a client's last report. For all boards, `client_id`, `avatar_id` and `metric` are decimal strings to preserve precision. The metric is overall points, total tokens, cache reuse percentage, conservative success **score in points**, or mean TTFT milliseconds respectively. Except for integer tokens, metrics have two decimal places. `rank` is a JSON integer. Names are plain text; avatar IDs reference the [avatar catalog](avatar-catalog.md). The API preserves the complete integer; compact UI units do not alter its value or ranking.

The web page offers all five boards for `today`, `yesterday`, `week` and `month`, defaulting to `overall/today`; it has no year/all, model filter, pagination or personal view. It displays only the public rows and actual UTC period and generation time. Loading, empty and failure states are distinct; retry does not invent data. Malformed JSON and transport failures show generic localized messages rather than raw parser or network diagnostics. When a selection or retry is pending or fails, any retained result keeps its own board/period/time label. Avatar paths come only from the served catalog, with its default and a neutral fallback on image failure.

A successful empty view returns `200` with `entries: []`. Before the first complete round, the response is `503` with `{"error":{"code":"leaderboard_unavailable","message":"Leaderboard cache is not ready"}}`. Invalid board, period or query parameters return `400` with `invalid_parameter`; unknown paths and methods retain JSON `404`/`405`. Later build failures retain the prior complete result and its original period/time; there is no cache-expiry SLA. No success counts, measurement counts, other token breakdowns, score contributions, coverage, credentials, hashes or IP addresses are returned. `/readyz` independently checks database and catalogs as well as cache initialization.

## Overall detail values

Only `overall` entries additionally contain a `details` object. These six values come from the same period totals and cache build as the score, not separate board requests:

```json
{"total_tokens":"100","request_count":"4","success_rate":"75.00","cache_read_rate":"35.71","ttft_mean_ms":"75.00","latency_mean_ms":null}
```

| Field | Meaning |
| --- | --- |
| `total_tokens` | Exact total tokens, as an int64 decimal string. |
| `request_count` | Exact accepted execution count, as an int64 decimal string. |
| `success_rate` | Observed successes / executions × 100; **not** the Wilson success score. |
| `cache_read_rate` | Cache-read tokens / input tokens × 100; `null` without input tokens. |
| `ttft_mean_ms` | TTFT sum / TTFT samples, in milliseconds; `null` without samples. |
| `latency_mean_ms` | Total latency sum / latency samples, in milliseconds; `null` without samples. |

Percentages and measured means are decimal strings rounded to two places; percentages range from `"0.00"` to `"100.00"`. Means use the combined sums and sample counts, not averages of daily averages. A measured zero remains `"0.00"`; unknown values are explicit JSON `null` and displayed as `—`. The page shows all six values alongside each overall result, using aligned columns on desktop and grouped cards on narrower screens. Older responses without `details` display unknown values, not fabricated zeroes. Other boards retain their existing entry fields. Scores, ranking rules, client report fields and stored statistics are unchanged; TPM/RPM are not added.

## List presentation

User names are visually emphasized, primary scores are distinct from supporting metrics, and numeric columns use aligned tabular figures. The page uses Keeper-style `K` (1,000), `M` (1,000,000) and `B` (1,000,000,000) for token and request counts. Scaling uses exact integer arithmetic before rounding the compact display to two decimals; it does not first convert int64 counters to JavaScript `Number`.

Durations adapt from milliseconds to seconds, minutes, hours and days. Positive sub-millisecond values show `<1 ms`, measured zero shows `0 ms`, and missing values remain `—`. Numbers and units are separated by a space (`1.20 B`, `99.00 %`, `1.75 s`) while retaining their different visual emphasis. Each metric is displayed once: there is no duplicate original-value column or expandable row. Retained results continue to identify their own board and period while loading or on failure.

Formatting is presentation only: the API, score calculation and sorting remain unchanged. Different raw values can share the same compact label; the API retains the complete published values.

## Calculations

The cache builds five views from all eligible clients: overall, token usage,
cache reuse, conservative success score, and measured mean time to first token
(TTFT). Eligibility requires an enabled, non-hidden, non-withdrawn seat with
at least one accepted execution in the selected UTC period. There is no minimum sample count or success rate. Only all-model totals are used; totals and model details are not added.

The six fixed periods are UTC today, yesterday, the previous complete
Monday-start UTC week, the previous complete UTC calendar month, the current
calendar year, and all time. Starts are inclusive and ends exclusive. `week`
and `month` are breaking semantic changes: they no longer mean the current
partial week or month. On Monday or the first of a month, yesterday can still
be corrected through the ordinary reporting window, so a complete period does
not mean immutable data. A successful cache round publishes any corrections;
a failed round retains the previous complete publication. Yearly
raw totals combine finalized monthly sums and all unsettled daily sums of that
calendar year in one SQLite read snapshot; there is no year table. Cumulative
statistics remain persisted and maintained. Profiles, seat flags and raw
statistics are read in the same snapshot; banned, hidden and withdrawn seats
are filtered before candidate limits and all scoring references. Hiding alone
still permits reports; banning and [withdrawal](registration-api.md#withdraw-the-entire-seat)
reject reports. These states are independent and survive restarts. A withdrawn
seat cannot be restored via the [admin console](admin.md) or registration; it
is removed from public results after the next successful cache publication.
Daily and weekly retention follows the [daily report storage rules](report-snapshots.md).

Overall has at most 100 points: usage 40, success 30, cache 20, TTFT 10.
Positive token totals use their mid-percentile among positive totals (equal
values share a percentile); zero earns no usage points. Success uses the lower
bound of the two-sided 95% Wilson interval, not the observed success
percentage. Cache is cache-read tokens divided by input tokens. Missing input
cannot produce a cache rate; actual zero cache reads are valid. TTFT response
performance is `1/(1 + mean / median_positive_mean)` multiplied by the
fraction of requests with TTFT samples. If all measured means are zero, the
performance factor is 0.5; no samples add no TTFT points. Each weighted
contribution is rounded half-up to 0.01 points before summation; missing terms
are not redistributed. Cache ratios use exact integer arithmetic for half-up rounding, including at int64 boundaries. TTFT contributions combine the calculated finite floating-point means with exact rational coverage and half-up rounding to avoid losing exact half-point boundaries to intermediate floating-point operations.

Usage requires positive tokens, cache requires positive input, and TTFT requires
at least one sample; other eligible clients can still enter the overall and
success views. Usage is sorted by exact int64 totals. Overall is sorted by its
integer hundredths of points. The cache percentage, conservative success score
and TTFT mean are rounded half-up to two decimals before sorting and
publication; equal published metric values tie. Compact UI labels do not define ties. Higher is better except TTFT, where lower
is better. Equal values receive competition ranks (1, 1, 3); stable client ID
orders tied rows without affecting rank. Each view is truncated to 100 only
after all candidates and references are considered, without expanding ties.
TTFT can be based on very few samples and is not a controlled performance
certification. These self-reported results are not proof of authenticity or SLA.

A single serial cache worker publishes all six period items together. An empty
view is a valid result; an empty build-item list is not. A failed build keeps
the previous complete snapshot, including its actual period and generation time.
The candidate and encoded-round limits are safeguards, not capacity claims.
