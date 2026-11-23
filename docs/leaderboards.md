# Leaderboards and Scoring

[简体中文](leaderboards.zh.md) · [README](../README.md)

`GET /v1/leaderboards/{board}/{period}` reads exclusively from the latest complete published in-memory cache. Allowed boards are `overall`, `usage`, `cache`, `success`, and `ttft`; allowed periods are `today`, `yesterday`, `week`, `month`, `year`, and `all`. No query parameters, arbitrary dates, pagination, or model filters are accepted.

## Response Format

```json
{
  "board": "usage",
  "period": "today",
  "start": "2026-12-31",
  "end": "2027-01-01",
  "generated_at": "2026-12-31T12:00:00Z",
  "entries": [
    {
      "rank": 1,
      "client_id": "42",
      "display_name": "Example",
      "avatar_id": "1",
      "metric": "9007199254740993"
    }
  ]
}
```

- `start` is inclusive and `end` is exclusive in UTC dates (`all` uses `start: "0001-01-01"` and `end: null`).
- `generated_at` is the UTC start time of the published cache build round, not a client's last report time.
- `client_id`, `avatar_id`, and `metric` are decimal strings to preserve exact precision. Except for integer `usage` tokens, metrics have two decimal places. `rank` is a JSON integer. Display names are plain text; avatar IDs reference the [avatar catalog](catalogs.md#avatar-catalog).
- Empty views return `200` with `entries: []`. Before the first complete cache round finishes, the endpoint returns `503` (`leaderboard_unavailable`). Invalid board, period, or query parameters return `400` (`invalid_parameter`). Subsequent failed cache refreshes retain the previous complete snapshot along with its original period bounds and `generated_at`.
- Responses never expose credentials, digests, GitHub IDs, CPA server IDs, IP addresses, or score breakdowns.

### Overall Detail Values

Only `overall` entries include an additional `details` object computed from the same period totals and cache round:

```json
{
  "total_tokens": "100",
  "request_count": "4",
  "success_rate": "75.00",
  "cache_read_rate": "35.71",
  "ttft_mean_ms": "75.00",
  "latency_mean_ms": null
}
```

| Field | Meaning |
| --- | --- |
| `total_tokens` | Exact total tokens as an int64 decimal string |
| `request_count` | Exact accepted execution count as an int64 decimal string |
| `success_rate` | Observed `success_count / request_count * 100` (**not** the Wilson success score) |
| `cache_read_rate` | `cache_read_tokens / input_tokens * 100`; `null` when `input_tokens == 0` |
| `ttft_mean_ms` | `ttft_sum / ttft_samples` in milliseconds; `null` when unmeasured |
| `latency_mean_ms` | `latency_sum / latency_samples` in milliseconds; `null` when unmeasured |

Percentages (`"0.00"`–`"100.00"`, clamped to `[0, 100%]`) and means are decimal strings rounded to two decimal places. Cross-day means weight sums by sample counts rather than averaging daily averages. Measured zero stays `"0.00"`; missing values are explicit JSON `null` (rendered as `—`). Other boards do not include `details`.

## Periods and Eligibility

| Period | UI Label | UTC Range (`[start, end)`) |
| --- | --- | --- |
| `today` | Today (`今日`) | Current UTC calendar day `[today, tomorrow)` |
| `yesterday` | Yesterday (`昨日`) | Previous UTC calendar day `[yesterday, today)` |
| `week` | Weekly (`周榜`) | Previous complete Monday-start UTC week `[last Mon, this Mon)` |
| `month` | Monthly (`月榜`) | Previous complete UTC calendar month `[1st of last month, 1st of this month)` |
| `year` | API only | Current UTC calendar year (combines finalized monthly rows and unsettled daily rows) |
| `all` | API only | All-time cumulative totals |

- Because the open reporting window includes UTC yesterday, `week` on Mondays and `month` on the 1st can still update until yesterday closes.
- Eligibility requires an **enabled, non-hidden, non-withdrawn** seat with at least one accepted execution in the selected UTC period (no minimum sample count or success rate). Only all-model totals are used.
- Seat profiles, flags, and statistics are read in one consistent SQLite snapshot; banned, hidden, and withdrawn seats are filtered **before** candidate limits and scoring references.
  - Hiding (`ranking_hidden`) still permits reports; banning (`disabled`) and [withdrawal](integration.md#withdraw-an-entire-seat) reject reports. See [Admin Console](admin.md) and [Storage & Caching](storage.md).

## Scoring and Ranking Rules

| Board | Metric | Eligibility & Sort Order |
| --- | --- | --- |
| `overall` | 0–100 composite points | Any eligible seat; sorted descending by integer hundredths of points |
| `usage` | Matched `total_tokens` | Requires `total_tokens > 0`; sorted descending by exact int64 value |
| `cache` | `cache_read_tokens / input_tokens * 100` | Requires `input_tokens > 0`; sorted descending |
| `success` | Two-sided 95% Wilson lower bound (`0`–`100` points) | Any eligible seat; sorted descending |
| `ttft` | Measured mean TTFT (`ms`) | Requires `ttft_samples > 0`; sorted **ascending** (lower is better) |

### Overall Composite Formula (Max 100 Points)

- **Usage (40 pts)**: Mid-rank percentile among all eligible seats with positive `total_tokens` (equal totals share the same percentile); zero tokens earn `0`.
- **Reliability (30 pts)**: Two-sided 95% Wilson score lower bound of `success_count / request_count` (not raw success percentage).
- **Cache Reuse (20 pts)**: `cache_read_tokens / input_tokens` (exact integer arithmetic for half-up rounding, including near int64 bounds). Missing input earns `0`; measured zero cache read is valid (`0`).
- **First-Token Latency (10 pts)**: `1 / (1 + mean / median_positive_mean)` multiplied by sample coverage (`ttft_samples / request_count`), using exact rational coverage with half-up rounding. If all measured means are `0`, the performance factor is `0.5`; unmeasured seats earn `0`.

Each weighted component is rounded **half-up to `0.01` points** before summation; missing components are not redistributed.

### Rounding, Ties, and Truncation

- `cache`, `success`, and `ttft` metrics are rounded half-up to two decimal places before sorting and publication; equal published metrics tie.
- Ties receive standard competition ranks (`1, 1, 3`) and are ordered deterministically by stable `client_id` without altering rank.
- Each board is truncated to the **Top 100** only after evaluating all candidates and scoring references, without expanding ties beyond 100.
- Metrics reflect self-reported client data and are not controlled performance certifications or SLA guarantees.

## Web UI Presentation

- The web dashboard exposes all five boards across `today`, `yesterday`, `week`, and `month` (defaulting to `overall/today`), with distinct loading, empty, and error states. While a reload is pending or failing, any retained table keeps its own board, period, and `generated_at` label.
- Numeric columns use aligned tabular figures. Token and request counts use compact `K` (`1,000`), `M` (`1,000,000`), and `B` (`1,000,000,000`) units computed with exact `BigInt` integer arithmetic before rounding to two decimals—never converting int64 counters through JavaScript `Number`.
- Durations adapt across `ms`, `s`, `m`, `h`, and `d`; positive sub-millisecond values show `<1 ms`, measured zero shows `0 ms`, and missing values show `—`. Numbers and units are separated by a space (`1.20 B`, `99.00 %`, `1.75 s`).
- Compact formatting is display-only and does not affect API precision, scoring, or tie-breaking.
