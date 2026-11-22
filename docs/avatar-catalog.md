# Static avatar catalog loader

`internal/avatars` loads a server-maintained, extensible catalog from an
`io/fs.FS`. Startup loads the embedded production catalog, then serves it and
its approved images independently of the frontend. There are no uploads,
remote URLs, avatar database tables or image-generation dependencies. The small
SVG catalog under `internal/avatars/test/testdata/` remains a test fixture only.

## Production assets and HTTP

The binary embeds `internal/avatars/assets/catalog.json` and `images/*.webp`.
The initial catalog has stable IDs 1–66 mapped to `images/001.webp`–`066.webp`,
with default ID 1. These are 128×128 WebP files reused with the project owner's
approval. See [asset provenance](../internal/avatars/assets/PROVENANCE.md);
no MIT/CC0 or other stock-image license is asserted.

- `GET`/`HEAD /avatars/catalog.json`: JSON in the format below, `Cache-Control:
  no-cache` and a content-derived ETag. Resolve each relative `path` against
  `/avatars/` (for example `/avatars/images/001.webp`).
- `GET`/`HEAD /avatars/images/001.webp`: WebP; `public, max-age=3600,
  must-revalidate`, ETag and conditional 304 responses. Stable filenames are
  **not** immutable.
- Only listed assets and the catalog are exposed. Directories, source records,
  missing resources and traversal paths return JSON 404, never a directory
  listing or SPA page. `/assets/` remains the frontend's separate namespace.

Avatar assets are embedded; no external asset directory is required at runtime.
Adding assets requires updating the production catalog and rebuilding.
There is no avatar selection UI or management API.

## Directory format

Provide `catalog.json` at the supplied filesystem root. For example (test IDs
and assets, not production defaults):

```text
catalog.json
images/
  circle.svg
  square.svg
```

```json
{
  "default_avatar_id": 1099511627776,
  "avatars": [
    { "avatar_id": 67, "path": "images/circle.svg" },
    { "avatar_id": 1099511627776, "path": "images/square.svg" }
  ]
}
```

- The file contains exactly one JSON object with the fields shown above;
  unknown fields, malformed JSON and trailing data are errors.
- `avatars` must not be empty. Every `avatar_id` is a unique, positive `int64`
  integer, not an array index or `uint8`. IDs need not be consecutive; 66 and
  255 are **not** limits.
- `default_avatar_id` is required and must refer to a listed avatar. It need
  not be the first or smallest ID. The loader does not guess a default.
- `path` is a slash-separated file path relative to the **supplied filesystem
  root**, not a URL. Absolute paths, `.`/`..` components, empty components,
  backslashes, colons, query strings and fragments are rejected, not cleaned.
- Every resource, including the default, must exist as a regular file.
  Loading checks filesystem metadata, not image content or licensing. Broken
  or missing resources are errors; they are never silently dropped.

For compatibility with the web UI, IDs must also be JavaScript-safe integers
(at most `9007199254740991`), and paths must match `^images/[a-zA-Z0-9_./-]+$`
without empty, `.` or `..` components. The UI ignores incompatible entries and
uses the catalog default for missing IDs. If the default entry is incompatible,
the UI falls back to a neutral avatar instead of using the catalog.

Extend the catalog by adding files and entries. Published IDs must keep their
meaning: do not renumber entries, reuse an ID for another image or remove
resources still referenced by stored clients. This is a catalog-maintenance
contract, not a comparison against previous releases or a database migration.
Multiple IDs may reference one resource. Array order is preserved for listing;
identity and lookup always use the explicit ID.

## Go interface

- `avatars.Load(files fs.FS) (*avatars.Catalog, error)` reads and validates the
  whole catalog. Any failure returns a nil catalog. Filesystem errors remain
  inspectable with `errors.Is`, including `fs.ErrNotExist`.
- `catalog.DefaultID() int64` returns the configured default. Registration
  assigns it to a new seat; clients do not submit an avatar selection.
- `catalog.Lookup(id int64) (avatars.Avatar, bool)` performs membership lookup.
  An unknown ID, including zero or a negative value, returns false.
- `catalog.List() []avatars.Avatar` returns a copy in catalog order. `Avatar`
  contains `ID` (`avatar_id`) and `Path` (`path`). The loaded catalog is read-only
  and can be shared by concurrent readers.

The caller owns the filesystem boundary/lifetime. Use an embedded filesystem
(or `fs.Sub` of one) or an `os.Root.FS()` constrained to the intended asset root.
`os.DirFS` alone does **not** prevent symlink escapes. Static serving uses
the same constrained root and an explicit resource allowlist; a local
`path` is not an authorization to serve arbitrary filesystem locations. Treat
the catalog and files as a release unit, not resources to mutate while serving.

The client repository remains independent of this loader and expects validated
IDs. Registration does not synchronize GitHub avatars.
Tests in `internal/avatars/test/` cover sparse/large IDs, extension, default
membership, duplicates, invalid paths, missing resources and copy-safe listing.
HTTP tests cover serving/cache/path boundaries; `web/test/avatars.spec.ts`
decodes all 66 images against the real production binary in Chromium.
