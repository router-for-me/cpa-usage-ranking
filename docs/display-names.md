# Public display names

`internal/profiles.NormalizeDisplayName(string) (string, error)` validates a
public nickname and returns the value to store and return to the client.
Invalid input returns an empty string and an error matching
`profiles.ErrInvalidDisplayName` with `errors.Is`; errors do not include the
submitted name. [Registration](registration-api.md) applies this helper to the
GitHub `name` for a new seat. An empty or invalid name falls back to the GitHub
`login`, truncated to fit. Clients do not submit a name; an additional writer
or completion retry does not change the current stored profile.

## Rules, in order

1. Require valid UTF-8, then apply Unicode **NFKC** normalization using
   `golang.org/x/text/unicode/norm`.
2. Reject Unicode control characters (`Cc`), line/paragraph separators
   (`Zl`/`Zp`) and `Bidi_Control` characters, **before** trimming. For example,
   an otherwise valid name with a leading newline or trailing tab is invalid.
3. Remove leading/trailing Unicode whitespace. Preserve internal spaces,
   punctuation and letter case.
4. Require **1–32 Unicode code points and at most 128 UTF-8 bytes**, measured
   after normalization and trimming. These are not grapheme-cluster limits:
   emoji joiners, modifiers and variation selectors count individually.
5. Require visible content. Whitespace, combining marks (`M`), format
   characters (`Cf`), `Other_Default_Ignorable_Code_Point` characters (including
   Hangul fillers), and the Braille blank (`U+2800`) do not establish visibility
   on their own. They may remain alongside visible content. This is a
   code-point check, not font-dependent rendering or confusable detection.

Legitimate emoji sequences retain their ZWJ, variation selectors, skin-tone
modifiers and tag characters. Format characters are **not** categorically
banned; a name made only of invisible characters is rejected. Combining marks
with a visible base are allowed. Thirty-two four-byte emoji occupy exactly
128 bytes and are accepted; normalization can either expand or compose input,
so limits are not applied to the original byte length.

Examples:

| Input | Stored name |
| --- | --- |
| `　Ａｌｉｃｅ １２３！ ` | `Alice 123!` |
| `e` + combining acute accent | `é` |
| `  Alice  & Bob!  ` | `Alice  & Bob!` |
| `👩🏽‍💻` or `❤️` | Unchanged |
| `13800138000` | Unchanged; no phone-number filter |

Names are public display text, not login names or unique identities. Duplicate
names are allowed. This module adds no real-name policy, reserved-name list,
profanity filter, CAPTCHA or account recovery. Display names must be rendered
as escaped plain text, not interpreted as HTML or automatically made into links;
normalization itself does not perform HTML escaping.

Unit tests are in `internal/profiles/test/` and do not require a database or
frontend build. Registration HTTP validation has separate real HTTP/SQLite tests.
The service does not expose a profile-edit route or account UI.
