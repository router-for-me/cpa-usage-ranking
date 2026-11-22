# Model, Avatar, and Brand Catalogs

[简体中文](catalogs.zh.md) · [README](../README.md)

This document describes the server's startup model catalog, embedded avatar catalog, and repository brand assets.

## Model Catalog

The server loads `config/models.json` (or the path set in `RANKING_MODELS_PATH`) once at startup before opening SQLite or listening for HTTP. Missing, invalid, or conflicting entries stop startup; edits take effect after restart (no remote fetching or hot reload). Deploy `models.json` alongside the binary even in API-only mode.

### Resolution and Storage

- On startup, all canonical `model_name` values are registered into the SQLite `models` dictionary in one transaction with stable positive IDs (ID `0` is reserved for all-model totals). Removing a model keeps its historical dictionary row inactive; reintroducing it restores the original ID. Aliases and prices are not stored in SQLite.
- When processing [daily reports](integration.md#daily-cumulative-snapshot-reporting), the resolver validates UTF-8, rejects control characters, enforces a 128-byte limit, trims surrounding whitespace, strips any prefix through the final `/`, and performs an **exact, case-sensitive** match against canonical names and aliases (no case folding, Unicode normalization, or automatic version-suffix stripping).
- Matched aliases map to their canonical model ID. Valid unmatched names are ignored from totals without error and are not inserted into SQLite. There is no HTTP model-management API.

### Schema and Reference Prices

- Top-level fields: `currency` (`"USD"`), `price_unit` (`"per_million_tokens"`), and `models` (array of `{model_name, alias, prices}`).
- Names must be unique across models; an alias cannot occupy another model's canonical name.
- `prices` maps `default`, `priority`, and `flex` tiers to `{input, output, cache_read, cache_write}` non-negative decimal strings (`null`/omitted means unknown, `"0"` means explicitly free; missing tiers do not inherit `default`).
- Price tiers are reference metadata only—they are **not** a reporting or statistics dimension (`service_tier` is not accepted in daily reports), and no billing or cost calculation is performed.
- `config/model-sources.json` records provenance URLs and capture dates for shipped reference prices; only `config/models.json` is loaded at runtime.

## Avatar Catalog

The binary embeds `internal/avatars/assets/catalog.json` and 66 static `128×128` WebP images (`images/001.webp`–`images/066.webp`, default ID `1`; see [asset provenance](../internal/avatars/assets/PROVENANCE.md)). No external directory is needed at runtime, and there are no user uploads or avatar-selection APIs.

### Public HTTP Endpoints

- `GET` / `HEAD /avatars/catalog.json`: returns `{"default_avatar_id": 1, "avatars": [{"avatar_id": 1, "path": "images/001.webp"}, ...]}` with `Cache-Control: no-cache` and content-derived `ETag`.
- `GET` / `HEAD /avatars/{path}`: serves WebP binaries with `Cache-Control: public, max-age=3600, must-revalidate`, `ETag`, and conditional `304 Not Modified`.
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
