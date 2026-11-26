# Administrator Console

[简体中文](admin.zh.md) · [README](../README.md)

The administrator console is disabled by default; public leaderboards and client reporting work independently without it.

## Enabling and Signing In

Create a private JSON file outside `RANKING_WEB_DIR`, point `RANKING_ADMIN_CONFIG_FILE` to it, and restart the service:

```json
{
  "username": "admin",
  "password": "replace-with-strong-password-12-to-72-bytes"
}
```

- **Username**: `3`–`64` ASCII letters, digits, `_`, `.`, or `-`, starting with a letter or digit (case-sensitive).
- **Password**: `12`–`72` bytes (bcrypt maximum); there is no default password.
- **File permissions and format**: `0600` or `0640` (or stricter read-only modes such as `0400`/`0440` readable by the service user) is recommended; broader permissions emit a startup warning rather than blocking startup. Regular files and non-directory secret mounts or named pipes up to `8192` bytes are accepted, and unknown JSON fields are ignored. The file must reside outside `RANKING_WEB_DIR` (symlinks pointing into `RANKING_WEB_DIR` are also rejected). Missing, directory, or malformed config fails startup without echoing file contents. Account edits take effect on restart (no hot reload).

When enabled, the header shows **Sign in** and switches to **Console** (`/admin/leaderboards`) while authenticated. Visiting `/admin` or `/admin/` redirects (`302 Found`) to `/admin/leaderboards`, and trailing slashes on `/admin/{login,leaderboards,accounts,users}/` redirect to their canonical paths. `GET /v1/admin/session` exposes only whether admin is enabled/signed in to anonymous callers.

## Ordinary Administrators

The configured owner account can open `/admin/accounts` to list, create, and delete ordinary database administrators stored in SQLite (`admin_accounts`, bcrypt password hashes only).

- Ordinary administrators cannot view or manage administrator accounts.
- Usernames are unique; the configured owner username is reserved (a collision with an existing DB admin on startup fails boot).
- Deleting an ordinary administrator immediately revokes all of their active sessions; recreating the same username allocates a new ID and cannot inherit old cookies. Password resets are handled by deleting and recreating the account. Accounts persist across restarts; sessions do not.

## User Management and Moderation

Both owner and ordinary administrators can access `/admin/users` and `/admin/leaderboards`.

- **User search (`/admin/users`)**: Lists all registered seats (active, unreported, banned, or withdrawn; never admin accounts) with internal ID, avatar/display name, compact `server_id`, UTC creation time, seat status (`Active`, `Banned`, `Withdrawn`), and board status (`Listed`, `Excluded`, `Off board`). `GET /v1/admin/users` returns `id`, `github_id`, `server_id`, `display_name`, `avatar_id`, `disabled`, `withdrawn`, `ranking_hidden`, and `created_at`, and supports `disabled`, `hidden`, and `withdrawn` boolean query filters. The unified search input (`q`, outer whitespace trimmed) matches literal display-name substrings, exact positive numeric `id` or `github_id`, and case-insensitive `server_id` substrings with cursor pagination (`50` per page by default, API max `100`). Secrets, digests, and report payloads are never returned.
- **Ban / Unban (`disabled`)**: Applies to the seat (`client_id`) and all of its writers (other `server_id` seats under the same GitHub account are unaffected). Banned seats are rejected on subsequent reports—including requests still awaiting the final write transaction—and removed from all five boards, all six backend periods, and scoring references without deleting data. Requests set `disabled: true/false` explicitly (idempotent).
- **Exclude / Restore (`ranking_hidden`)**: Available both in `/admin/users` (so operators can moderate seats outside the Top 100 directly) and in `/admin/leaderboards` (**Active Board** and **Excluded Users** views). Exclusion removes a seat from all five boards, all six backend periods, and scoring references **before** scoring, while continuing to accept its daily reports until manually restored. Exclusion persists across period boundaries and restarts.

### Moderation State Matrix (When Not Withdrawn)

| Banned (`disabled`) | Excluded (`ranking_hidden`) | Reports Accepted | Leaderboard Eligibility |
| --- | --- | --- | --- |
| No | No | Yes | Eligible under standard rules |
| No | Yes | Yes | Excluded |
| Yes | No | Rejected | Excluded |
| Yes | Yes | Rejected | Excluded |

- Ban and exclusion flags are independent: unbanning does not clear `ranking_hidden`, and a banned-and-excluded seat must be unbanned first before restoring leaderboard eligibility (attempting to clear `ranking_hidden` while banned returns `409 client_banned`).
- Client-initiated [seat withdrawal](integration.md#withdraw-an-entire-seat) is independent of moderation: neither unbanning nor restoring leaderboard visibility clears `withdrawn`. Once the seat is not banned, its user can rejoin through registration with the same GitHub account and `server_id`. Rejoining preserves `ranking_hidden`.
- **Cache refresh**: Moderation changes, withdrawals, and successful rejoins signal the cache worker to rebuild all periods early and publish atomically (coalescing bursts). A write confirmation does not guarantee the new cache is already live, and the UI **Refresh** button only re-fetches published leaderboards; failed rebuilds retain the previous complete snapshot. The console never edits scores, resets data, deletes seats, or restores withdrawn seats.
- **Disabling the console**: Unset `RANKING_ADMIN_CONFIG_FILE` and restart. Stored DB admins and ban/exclusion flags remain in SQLite and continue affecting public leaderboards.

## Security and Docker Mount

- **Sessions & CSRF**: Server-side in-memory sessions last `8h` (capped at `1024` active sessions, evicting the earliest-expiring session when full so active operators are not locked out; cleared on restart). Cookies are `HttpOnly`, `SameSite=Strict`, and `Secure` by default. Login (`POST /v1/admin/session`) requires Same-Origin JSON (omitted `Content-Type` and unknown JSON fields are tolerated; explicit non-JSON returns `415`); logout and account `DELETE` endpoints require Same-Origin and `X-CSRF-Token` without a request body. Client Bearer secrets cannot log in to the admin console.
- **Login rate limits**: `5` attempts/minute per source and `2000`/minute globally, reusing the deployment's trusted-proxy and IPv6 `/64` source rules (separate from registration quotas; see [Deployment & Configuration](deployment.md#reverse-proxy-security-and-source-resolution)).
- **HTTPS and reverse proxies**: Serve admin routes only over HTTPS behind a trusted proxy listed in `RANKING_TRUSTED_PROXIES`. Under trusted proxy peers, `sameOrigin` reads the first comma-separated segment of `X-Forwarded-Proto` (case-insensitive), honors an optional single valid `X-Forwarded-Host`, and compares `Origin` case-insensitively while stripping default scheme ports (`:443` / `:80`). If admin console is enabled without `RANKING_TRUSTED_PROXIES` or `RANKING_ADMIN_ALLOW_INSECURE_LOCAL_HTTP=true`, startup logs a warning and the login UI surfaces targeted hints on `https_required` / `same_origin_required` errors. For isolated local or container HTTP testing only, set `RANKING_ADMIN_ALLOW_INSECURE_LOCAL_HTTP=true` (preserves strict `Origin` and `X-CSRF-Token` checks; never enable on the public internet).

### Docker Read-Only Mount Example

The container runs as UID/GID `10001:10001`. Prepare a host file readable by UID/GID `10001` (for example owned by `root:10001` with mode `0640` inside a traversable parent directory) and mount it read-only—never bake real credentials into images:

```yaml
volumes:
  - /etc/cpa-ranking/admin.json:/etc/cpa-ranking/admin.json:ro
environment:
  RANKING_ADMIN_CONFIG_FILE: /etc/cpa-ranking/admin.json
```
