# Client-owned credentials

A client generates **32 bytes from a cryptographically secure random source**,
encodes them as unpadded base64url (RFC 4648 URL alphabet `A–Z a–z 0–9 - _`),
and durably saves the secret **before its first registration attempt**. The
canonical encoding is exactly 43 ASCII characters, without whitespace or `=`.
The server validates format, not the client's entropy source; character-distribution
heuristics cannot prove randomness.

Send exactly one `Authorization: Bearer <secret>` header over **HTTPS**.
The scheme is case-insensitive; one space separates it from the secret.
Do not put the secret in URLs, JSON bodies, display names or logs. Configure
reverse proxies and access/error logging to exclude Authorization headers and
request bodies, cookies, and OAuth query strings. HTTP on loopback is suitable
only for local verification or a trusted TLS-terminating proxy; public
deployment requires HTTPS.

`internal/auth.HashCredential` decodes the canonical text, computes SHA-256
**over the original 32 bytes** (not the 43 encoded characters), and returns the
32-byte digest used by the client repository. `BearerHash` requires a single
unambiguous Authorization header and returns only that digest. Invalid-input
errors are generic and contain no credential text. The database stores a unique
32-byte BLOB digest per writer; API responses do not carry that digest.
[Registration](registration-api.md) pairs a verified numeric GitHub ID and a
CPA UUID to select a seat. Each installation chooses and persists its own
secret; installations reporting for the same seat do not share credentials.

This is a high-entropy machine credential, not a human password. Client
credential handling does not generate or return secrets, add a password KDF,
issue sessions, send OTP, rotate credentials or recover a lost secret.
The optional [administrator console](admin.md) uses separate passwords and
sessions; client Bearer credentials never grant administrator access. Clients must preserve their
secret and reuse it for safe retries; keeping only `client_id` is insufficient.
There is no client SDK or Keeper integration in this service.

## Internal identity enforcement

`auth.NewAuthenticator(repo).Authenticate(ctx, headers)` hashes the Bearer
credential and loads its writer and seat. Missing, malformed and unknown secrets
fail; disabled or withdrawn seats each return a distinct rejection. Database errors are not
treated as authentication success.

`httpserver.ClientAuthentication` is Gin middleware for subsequent business
routes. `AuthenticatedClient` reads the middleware-established client; handlers
must use those IDs, never a body/query/path `client_id` as authorization. Rejections
are safe JSON: 401 `invalid_credential` (with `WWW-Authenticate: Bearer`),
403 `client_disabled` or `client_withdrawn`, or generic 500 for storage failures. Authentication does
not issue sessions or provide login, credential recovery or profile-edit endpoints.
Authenticated reporting is documented in the [daily report API](report-snapshots.md).

Authentication describes state when checked. The report transaction rechecks
the writer's seat and its disabled/withdrawn status before applying a change or
confirming a retry. Reporting does not require a GitHub connection after
registration.
