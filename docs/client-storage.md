# Client seat and writer storage

Ranking initializes the current SQLite schema directly. It does not create sample seats or convert databases from the former registration protocol.

| Table / field | Meaning |
| --- | --- |
| `clients.id` | Stable positive SQLite `client_id` allocated with `AUTOINCREMENT` |
| `clients.github_id`, `clients.cpa_uuid` | Unique pair of a verified numeric GitHub user ID and normalized CPA UUID |
| `clients.display_name`, `clients.avatar_id` | Current public profile; duplicate names are allowed |
| `clients.disabled`, `clients.ranking_hidden`, `clients.withdrawn` | Independent flags; hidden alone does not block writing, withdrawn is never cleared by moderation or registration |
| `clients.created_at_ms` | Creation time in UTC Unix milliseconds |
| `client_writers.id`, `client_writers.client_id` | A writer and the seat it reports for |
| `client_writers.token_hash` | Globally unique 32-byte SHA-256 digest, never the raw credential |

[Registration completion](registration.md) owns seat creation and writer binding. The repository looks up seat metadata and derives both IDs from a credential digest, preserving disabled and withdrawn states. Public responses omit writer IDs, digests, GitHub IDs and CPA UUIDs. Profile updates change only the name and avatar. Withdrawal sets the seat's flag through the existing writer binding without deleting reports or credentials; repeating it does not change the result. Credentials are never moved to a different seat.

See [SQLite storage](storage.md) for the daily version and snapshot keys, and the [registration API](registration-api.md) for the client contract.
