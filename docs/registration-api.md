# GitHub registration and withdrawal API

Use the deployment's HTTPS origin for Ranking requests. Before starting, generate and persist one [32-byte random Bearer secret](client-credentials.md) for this reporting installation. Ranking stores only SHA-256 of the decoded bytes. Both registration POSTs accept a single UTF-8 JSON object with exactly the listed fields, without duplicates or trailing JSON. Use uncompressed `application/json` (charset parameters are accepted); default maximum body size is 4096 bytes.

## 1. Start a request

```http
POST /v1/clients/register/start
Authorization: Bearer <secret>
Content-Type: application/json

{"cpa_uuid":"550e8400-e29b-41d4-a716-446655440000","return_to":"http://localhost:8080/settings?tab=ranking#status"}
```

- `cpa_uuid` is the persistent, nonzero UUID supplied by the CPA instance, in `8-4-4-4-12` hex format (case-insensitive). This is an identifier, not a secret or verified CPA ownership. Ranking never connects to CPA. The UUID above is illustrative, not an integration default.
- `return_to` is an absolute HTTP or HTTPS URL without user information. Loopback, LAN, public IPs and DNS names are allowed; ports, paths, query strings and fragments are preserved. `localhost` is the browser's device. No preregistration or matching of the initiating site is required. Never put secrets in this URL.

`200 OK` returns `{"request_id":"…","authorize_url":"https://ranking.example.com/auth/github/authorize?request_id=…"}`. Save the opaque ID (default lifetime: ten minutes) and open the returned `authorize_url` in the user's browser without adding parameters. This step does not create a ranking seat or grant write access. A lost start response can be recovered by starting again with the same saved secret.

## 2. Browser authorization

The browser follows Ranking's `/auth/github/authorize` redirect to GitHub. GitHub returns to Ranking's fixed `/auth/github/callback`. Ranking binds authorization to the browser with a Secure, HttpOnly, SameSite=Lax cookie, validates a one-time state and uses S256 PKCE. Ranking exchanges the code server-side and reads the authenticated GitHub user; it requests no repository or email scopes and does not retain the GitHub token. A repeated browser opening or callback does not start another exchange for the same ID.

After recording approval or user denial, Ranking redirects to the exact saved `return_to` without appending parameters or making any server request to it. Invalid/unverifiable callbacks do not redirect or authorize a request. Browser redirects and errors are not cached and set `Referrer-Policy: no-referrer`. The return page need not implement a specific path or send any notification to Ranking. API and browser flows are separate; a return page visit is not proof of registration.

## 3. Complete the registration

```http
POST /v1/clients/register/complete
Authorization: Bearer <same-secret-as-start>
Content-Type: application/json

{"request_id":"<original-request-id>"}
```

Call after authorization. If the valid transaction is still waiting, the response is `202 Accepted` with `Retry-After` (seconds) and no body; retry the same ID and Bearer after that delay. Do not use a browser callback or a leaderboard as write confirmation. Successful completion returns:

```json
{"client_id":"42","display_name":"Alice 🙂","avatar_id":"1"}
```

- `201 Created` means a new ranking seat was created. `200 OK` means an existing seat was reused or a committed result was recovered. IDs are positive signed-64-bit values encoded as decimal **strings**.
- A seat is unique per **GitHub numeric user ID + CPA UUID**. Each installation has its own writer credential; the server never rebinds a credential to another seat. A seat's initial display name comes from GitHub `name` under the [existing rules](display-names.md), falling back to the GitHub `login` (truncated to fit). New seats use the [default catalog avatar](avatar-catalog.md); this is not GitHub avatar sync. Additional writers do not change the profile.
- A timeout or lost completion response may hide a committed result. Until the original ID expires, retry with the original **ID and secret**; the stored result returns the same `client_id` and its current profile, without creating another writer or extending the deadline. After expiry, keep the original secret and start and authorize again with the same GitHub account and CPA UUID. If the earlier attempt succeeded, completion reuses that writer and seat. No lookup, token issuance, lost-secret recovery, or retry-only endpoint is provided.
- Disabled or withdrawn seats cannot be restored by retries or authorization with another secret. Revoking GitHub access does not itself change an existing seat. Established reports do not depend on GitHub availability.

## Withdraw the entire seat

```http
POST /v1/clients/withdraw
Authorization: Bearer <registered-writer-secret>
```

No body, query, or supplied `client_id`. `204 No Content` means the seat is marked withdrawn in SQLite. Repeating from any of its writers is safe. This does not delete data or change administrator flags, but **every writer** of this seat stops reporting (even previously accepted retries). Other seats, including another CPA UUID for the same GitHub account, are unaffected. All public boards and scoring references exclude the seat in the next successful cache publication; `204` does not imply that the new cache is already live. There is no automatic or public rejoin operation.

## Error contract

Read `error.code` from `{"error":{"code":"invalid_request","message":"…"}}`; messages are not a protocol. Responses do not echo credential or upstream details.

| Status | Code | Meaning |
| --- | --- | --- |
| 400 | `invalid_request` | Invalid input, including missing/malformed UUID or return URL |
| 400 | `invalid_source` | No valid source could be resolved for start |
| 401 | `invalid_credential` | Invalid/missing Bearer, wrong Bearer for this request, credential already bound to another seat, or unknown withdrawal credential; includes `WWW-Authenticate: Bearer` |
| 403 | `github_authorization_denied` | The user denied this request; do not start another authorization automatically |
| 403 | `client_disabled` | The seat is disabled by an administrator |
| 403 | `client_withdrawn` | The seat has withdrawn; stop attempting to register or send reports |
| 410 | `registration_request_expired` | The request ID is expired or no longer exists; use the original secret and start again if still wanted |
| 413 | `request_too_large` | Body exceeds the configured bound |
| 415 | `unsupported_media_type` | Not uncompressed application/json |
| 429 | `registration_rate_limited` / `registration_capacity` | Start rejected by the request rate or capacity gates; retry after the specified seconds, with jitter |
| 503 | `registration_closed` | New registration is closed |
| 500 | `internal_error` | Storage, GitHub exchange, or another internal failure; no confirmed registration |

An upstream exchange failure invalidates that authorization; after the failure has been resolved, start another request if still wanted. `POST /v1/clients/register` and `PUT /v1/reports/{utc_day}` are not served. Use [the report API](report-snapshots.md) for existing credentials.

## Startup configuration

Register a **GitHub OAuth App** with callback `https://<public-ranking-origin>/auth/github/callback`. Supply the following as private runtime environment variables, not in source or in the image:

| Variable | Default / meaning |
| --- | --- |
| `RANKING_PUBLIC_ORIGIN` | Unset; public HTTPS origin, optional port, no path, query, or fragment |
| `RANKING_GITHUB_CLIENT_ID` | Unset; OAuth App client ID |
| `RANKING_GITHUB_CLIENT_SECRET` | Unset; OAuth App client secret |
| `RANKING_REGISTRATION_ENABLED` | `true`; `false` prevents starts and new writer bindings |
| `RANKING_REGISTRATION_MAX_BODY_BYTES` | `4096` for both registration POSTs |
| `RANKING_REGISTRATION_SOURCE_REQUESTS` | `20` start attempts per source per window |
| `RANKING_REGISTRATION_GLOBAL_REQUESTS` | `200` start attempts across all sources per window |
| `RANKING_REGISTRATION_REQUEST_WINDOW` | `1m` fixed window |
| `RANKING_REGISTRATION_REQUEST_MAX_SOURCES` | `10000` live source-rate entries |

Set all three OAuth variables or none. If unconfigured, new registration is closed; reports, withdrawal, committed unexpired completion retries, and public reads still work. Partial or invalid configuration fails startup. The fixed `authorize_url` origin is not derived from request or proxy headers. Terminate TLS at a trusted reverse proxy, forward `/auth/github/*`, and keep Authorization, cookies, bodies and OAuth query strings out of proxy logs. See [deployment](docker.md#github-oauth-app) and [start admission](registration-admission.md).
