# Registration transactions

The [GitHub registration API](registration-api.md) uses bounded, short-lived SQLite requests. A start records the CPA UUID, exact return URL, writer digest and deadline but no seat. Browser authorization creates a one-time state, a browser proof and an S256 PKCE challenge. The callback consumes state and browser proof together before exchanging a code; it does not hold a SQLite write lock while calling GitHub. An unverifiable or repeated callback cannot grant access. No token or authorization code is persisted. See [start admission](registration-admission.md) for request budgets and expiry.

Only complete can bind a writer. In one immediate transaction it checks the original Bearer digest and deadline, validates the approved seat and current status, creates or reuses the seat, binds the writer if needed, and remembers the successful `client_id` through the original deadline. An error rolls back the entire change. Duplicate credentials cannot be moved to another seat. A successful retry reads the same seat and current profile; it may not restore a disabled or withdrawn seat. No API returns successful completion for an expired request.

The service initializes the [current SQLite schema](storage.md) directly; it does not upgrade a database created by the former registration API. Already authenticated reporting and public reads do not invoke GitHub.
