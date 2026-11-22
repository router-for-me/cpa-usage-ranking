# Registration sources and trusted proxies

A source is an abuse-control key for [registration start](registration-admission.md),
**not identity or account ownership**. By default only the connection's
`RemoteAddr` is used. It does not determine the return URL or authorize a seat.

`network.RegistrationSource` uses Gin's existing `ClientIP` trusted-proxy
resolution, then parses and canonicalizes the address. IPv4-mapped IPv6 is
unmapped to IPv4. With `RANKING_REGISTRATION_IPV6_64=true` (default), native IPv6
sources share their masked /64 key; `false` retains the canonical full address.
Invalid remote addresses fail closed. Query/body fields never set the source.

`RANKING_TRUSTED_PROXIES` is unset by default (trust none). To deploy behind a
proxy, explicitly set a comma-separated list of that deployment's proxy IPs or
CIDRs, for example `127.0.0.1,10.2.0.0/24`. Invalid/empty entries fail startup.
Only a connection from a trusted peer can use `X-Forwarded-For` (preferred) or
`X-Real-IP`. Gin walks forwarded chains from right to left to the first
untrusted hop; malformed headers fall back to the connection address.
`Forwarded`, CDN-specific headers and arbitrary client-provided fields are not
independent authority.

Proxies must append/replace headers correctly. Trust only actual proxy peers;
trusting the entire internet or a client-accessible network defeats this
boundary. Keep the backend listener private behind the TLS terminator. These
settings take effect on restart; no proxy framework, IP identity binding or
configuration hot-reload service is added.
