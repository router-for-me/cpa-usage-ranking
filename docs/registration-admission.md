# Registration start admission

Only `POST /v1/clients/register/start` uses the source/global request gate. Admitted attempts count **before** Bearer and body validation, including malformed input. Attempts refused by per-source or source-state capacity do not consume remaining global slots; an exhausted global gate stops immediately. Source resolution follows [trusted-proxy rules](registration-sources.md).

| Variable | Default | Meaning |
| --- | --- | --- |
| `RANKING_REGISTRATION_SOURCE_REQUESTS` | `20` | Attempts per source per window |
| `RANKING_REGISTRATION_GLOBAL_REQUESTS` | `200` | Attempts across all sources per window |
| `RANKING_REGISTRATION_REQUEST_WINDOW` | `1m` | Fixed window duration from the first attempt |
| `RANKING_REGISTRATION_REQUEST_MAX_SOURCES` | `10000` | Maximum live source-rate entries |

The in-memory fixed-window state is per process and resets on restart. Expired sources are reclaimed on admission; new sources cannot evict active ones. `registration_rate_limited` indicates the source or global budget is exhausted. `registration_capacity` indicates the source-state or outstanding-request limit was reached. Both are HTTP `429` with `Retry-After` seconds rounded up, minimum one. Wait and add jitter; a subsequent attempt is not guaranteed admission.

An admitted valid start can allocate an opaque request ID. At most 2,000 unexpired transactions (including retained completion results) exist at once; each has a ten-minute lifetime. SQLite holds this bounded state across restarts; expiry is enforced on use and expired rows are removed on startup, periodically and as needed on start. No still-valid result is evicted. The control limits allocation, not the number of successful seats a source or GitHub account may register. Complete never spends another start slot. Reports have separate rate limits keyed by seat; health, avatars, and public reads are unaffected. See the [registration API](registration-api.md).
