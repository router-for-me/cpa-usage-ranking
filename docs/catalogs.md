# Model, Avatar, and Brand Catalogs

[简体中文](catalogs.zh.md) · [README](../README.md)

This document describes the server's startup model catalog, embedded avatar catalog, and repository brand assets.

## Model Catalog

The server loads `config/models.json` (or the path set in `RANKING_MODELS_PATH`) once at startup before opening SQLite or listening for HTTP. Missing, invalid, or conflicting entries stop startup; edits take effect after restart (no remote fetching or hot reload). Deploy `models.json` alongside the binary even in API-only mode.

### Resolution and Storage

- On startup, all canonical `model_name` values are registered into the SQLite `models` dictionary in one transaction with stable positive IDs (ID `0` is reserved for all-model totals), preserving original `model_name` casing for storage and display. Removing a model keeps its historical dictionary row inactive; reintroducing it restores the original ID. Aliases and prices are not stored in SQLite.
- Lookup is indexed **ASCII case-insensitively**; cross-model case-insensitive collisions across canonical names and aliases are rejected at startup.
- When processing [daily reports](integration.md#daily-cumulative-snapshot-reporting), the resolver trims surrounding whitespace, strips any prefix through the final `/`, and validates the final segment (nonempty, valid UTF-8, `<= 128` bytes, no control characters). It first performs an ASCII case-insensitive lookup against canonical names and aliases; if unmatched, it strips up to two layers of whitelisted trailing gateway mode/channel suffixes and re-checks the catalog:
  - Colon suffixes: `:free`, `:thinking`, `:online`, `:nitro`, `:floor`, `:extended`, `:beta`, `:exacto`
  - Context-window brackets: `[1m]`, `[2m]`
  - Parenthesized reasoning budgets: `(minimal)`, `(low)`, `(medium)`, `(high)`, `(xhigh)`, `(max)`, `(none)`, `(off)`, `(auto)`, or `(<1..8 digits>)`
  - Hyphenated reasoning/tier suffixes: `-minimal`, `-low`, `-medium`, `-high`, `-xhigh`, `-thinking`, `-nothinking`, `-non-thinking`, `-tiered`
  - Official date snapshots, `-max`, and `-latest` are **never** stripped automatically; official dated variants, cross-channel dot/hyphen aliases, and Gemini thinking-intensity aliases (such as `gemini-4-carbon-high`) remain managed explicitly in `alias`.
- Matched names and aliases map to their canonical model ID. Unmatched or malformed unknown model entries are silently skipped during aggregation so a single unfamiliar model never rejects valid models in the same report, and are never inserted into SQLite. There is no HTTP model-management API.

### Schema and Reference Prices

- Top-level fields: optional reference metadata `currency` (defaults to `"USD"`) and `price_unit` (defaults to `"per_million_tokens"`), plus required `models` (array of `{model_name, alias?, prices?}`; `alias` and `prices` may be omitted).
- Names must be unique across models (ASCII case-insensitively); an alias cannot occupy another model's canonical name or alias.
- `prices` optionally maps `default`, `priority`, and `flex` tiers to `{input, output, cache_read, cache_write}` non-negative decimal strings (`null`/omitted means unknown, `"0"` means explicitly free; missing tiers do not inherit `default`; unknown or `null` tier entries are ignored).
- Price tiers are reference metadata only—they are **not** a reporting or statistics dimension (any `service_tier` field in daily reports is ignored), and no billing or cost calculation is performed.
- `config/model-sources.json` records provenance URLs, pricing policy (first-party standard USD rates preferred, Gemini promotional rates excluded, shorter TTL chosen when `cache_write` varies by duration, `models.dev` / `litellm` fallback when vendor USD rates are unavailable), and capture dates for shipped reference prices; only `config/models.json` is loaded at runtime.

## Avatar Catalog

The binary embeds `internal/avatars/assets/catalog.json` and 66 static `128×128` WebP images (`images/001.webp`–`images/066.webp`, default ID `1`; see [asset provenance](../internal/avatars/assets/PROVENANCE.md)). No external directory is needed at runtime. Clients fetch `/avatars/catalog.json` to render a registration avatar picker and pass an optional decimal-string `avatar_id` to [`POST /v1/clients/register/start`](integration.md#step-1-start-registration) (omit `avatar_id` during re-authorization unless the user actively chose a new avatar, so existing seats retain their current avatar); there are no custom image uploads or standalone avatar-edit APIs.

### Public HTTP Endpoints

- `GET` / `HEAD` / `OPTIONS /avatars/catalog.json`: returns `{"default_avatar_id": 1, "avatars": [{"avatar_id": 1, "path": "images/001.webp"}, ...]}` with `Cache-Control: no-cache`, content-derived `ETag`, public read-only CORS headers, and `Cross-Origin-Resource-Policy: cross-origin`.
- `GET` / `HEAD` / `OPTIONS /avatars/{path}`: serves WebP binaries with `Cache-Control: public, max-age=3600, must-revalidate`, `ETag`, conditional `304 Not Modified`, public read-only CORS headers, and `Cross-Origin-Resource-Policy: cross-origin`.
- Only allowlisted catalog entries are served; directories, provenance records, unknown paths, and traversal attempts return JSON `404`. `/assets/` remains the separate frontend namespace.

### Catalog Format and Extension Rules

- `avatars` must be non-empty; every `avatar_id` is a unique positive `int64` (JavaScript-safe `<= 9007199254740991` for web UI compatibility). `default_avatar_id` must exist in `avatars`.
- Each `path` must be a relative slash-separated regular file matching `^images/[a-zA-Z0-9_./-]+$` with no `.` or `..` segments.
- **Maintenance contract**: Extend the catalog by adding files/entries and rebuilding. Never renumber published IDs, reuse an ID for a different image, or delete resources referenced by stored seats.

## Brand Icons

The brand mark uses a warm-to-violet rounded tile, three coral steps, and a violet corner dot (decorative brand element, not a live status indicator).

- **README banners**: `assets/ranking-logo-light.svg`, `assets/ranking-logo-dark.svg`
- **Web & app icon masters**: `assets/ranking-logo.svg`, `web/public/assets/brand/logo.svg`, `web/public/assets/brand/project-icon.svg`, `web/public/favicon.svg`
- **Generated rasters**: `web/public/assets/brand/project-icon-{180,192,512}.png`, `web/public/assets/brand/favicon-{16,32,48}.png`, `web/public/assets/brand/favicon.ico`

Generated rasters are committed alongside the SVG masters so standard builds do not need a browser. After editing SVG masters, regenerate rasters with Playwright Chromium available:

```sh
npm --prefix web run icons:generate
```
