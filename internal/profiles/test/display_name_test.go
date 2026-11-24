package test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"cpa-usage-ranking/internal/profiles"
)

func TestNormalizeDisplayName(t *testing.T) {
	family := "👩‍👩‍👧‍👦"
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"single code point", "名", "名"},
		{"spaces and punctuation", "  Alice  & Bob! #1  ", "Alice  & Bob! #1"},
		{"digits are not a phone filter", "13800138000", "13800138000"},
		{"compatibility normalization and whitespace", "\u3000Ａｌｉｃｅ １２３！\u00a0", "Alice 123!"},
		{"canonical composition", "e\u0301", "é"},
		{"case preserved", "ÉcHo", "ÉcHo"},
		{"combining vowel with base", "कि", "कि"},
		{"text is not HTML", "<b>名字</b>", "<b>名字</b>"},
		{"family ZWJ sequence", family, family},
		{"emoji skin tone and ZWJ", "👩🏽‍💻", "👩🏽‍💻"},
		{"emoji variation selector", "❤️", "❤️"},
		{"text variation selector", "❤\ufe0e", "❤\ufe0e"},
		{"keycap", "1️⃣", "1️⃣"},
		{"flag", "🇨🇳", "🇨🇳"},
		{"emoji tag sequence", "🏴\U000e0067\U000e0062\U000e0073\U000e0063\U000e0074\U000e007f", "🏴\U000e0067\U000e0062\U000e0073\U000e0063\U000e0074\U000e007f"},
		{"ZWNJ in a name", "می\u200cروم", "می\u200cروم"},
		{"format characters with visible text", "\ufeffA\u200bB\u2060", "\ufeffA\u200bB\u2060"},
		{"32 ASCII code points after trim", "  " + strings.Repeat("a", 32) + "  ", strings.Repeat("a", 32)},
		{"32 CJK code points", strings.Repeat("名", 32), strings.Repeat("名", 32)},
		{"32 code points and 128 bytes", strings.Repeat("🚀", 32), strings.Repeat("🚀", 32)},
		{"count after composition", strings.Repeat("e\u0301", 32), strings.Repeat("é", 32)},
		{"count after expansion", strings.Repeat("ﬃ", 10) + "ab", strings.Repeat("ffi", 10) + "ab"},
		{"joiners count toward limit", strings.Repeat(family, 4) + "abcd", strings.Repeat(family, 4) + "abcd"},
		{"newline before trimming", "\nname", "name"},
		{"tab after trimming", "name\t", "name"},
		{"line separator replaced", "name\u2028next", "name next"},
		{"paragraph separator replaced", "name\u2029next", "name next"},
		{"control stripped and NFKC idempotent", "e\x00\u0301", "é"},
		{"33 ASCII code points truncated", strings.Repeat("a", 33), strings.Repeat("a", 32)},
		{"over byte and code point limits truncated", strings.Repeat("🚀", 33), strings.Repeat("🚀", 32)},
		{"expansion exceeds limit truncated", strings.Repeat("ﬃ", 11), strings.Repeat("ffi", 10) + "ff"},
		{"trailing space at truncation boundary trimmed", strings.Repeat("a", 31) + "  b", strings.Repeat("a", 31)},
		{"code point boundary truncation for ZWJ", strings.Repeat("👩‍👩‍👧‍👦", 5), string([]rune(strings.Repeat("👩‍👩‍👧‍👦", 5))[:32])},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := profiles.NormalizeDisplayName(tc.input)
			if err != nil || got != tc.want {
				t.Fatalf("NormalizeDisplayName(%q) = %q, %v; want %q", tc.input, got, err, tc.want)
			}
			again, err := profiles.NormalizeDisplayName(got)
			if err != nil || again != got {
				t.Fatalf("normalization is not idempotent: %q, %v", again, err)
			}
		})
	}
}

func assertInvalidName(t *testing.T, input string) {
	t.Helper()
	got, err := profiles.NormalizeDisplayName(input)
	if !errors.Is(err, profiles.ErrInvalidDisplayName) || got != "" {
		t.Fatalf("NormalizeDisplayName(%q) = %q, %v; want empty result and ErrInvalidDisplayName", input, got, err)
	}
}

func TestRejectInvalidDisplayNames(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{"empty", ""},
		{"all whitespace", " \u00a0\u3000\u2003 "},
		{"controls and separators only", "\n\t\r\x00\u2028\u2029\u202e"},
		{"invalid UTF-8", string([]byte{0xff})},
		{"truncated UTF-8", "name\xe2\x82"},
		{"UTF-8 encoded surrogate", "\xed\xa0\x80"},
		{"format characters only", "\u200c\u200d\u200b\u2060\ufeff\u00ad"},
		{"variation selectors only", "\ufe0e\ufe0f\U000e0100"},
		{"combining marks only", "\u0301\u20dd\u093f"},
		{"hangul fillers", "\u115f\u1160\u3164\uffa0"},
		{"braille blanks", "\u2800\u2800"},
		{"other default ignorables", "\ufff0\u2065"},
		{"emoji tags without a base", "\U000e0067\U000e007f"},
		{"spaces and invisible characters", " \u200d \ufe0f \u3164 "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { assertInvalidName(t, tc.input) })
	}
}

func TestStripControlAndBidiCharacters(t *testing.T) {
	for r := rune(0); r <= 0x9f; r++ {
		if r > 0x1f && r < 0x7f {
			continue
		}
		t.Run(fmt.Sprintf("control U+%04X", r), func(t *testing.T) {
			got, err := profiles.NormalizeDisplayName("a" + string(r) + "b")
			if err != nil {
				t.Fatalf("control U+%04X failed: %v", r, err)
			}
			if got != "ab" && got != "a b" {
				t.Fatalf("control U+%04X = %q", r, got)
			}
			again, err := profiles.NormalizeDisplayName(got)
			if err != nil || again != got {
				t.Fatalf("not idempotent: %q %v", again, err)
			}
		})
	}
	for _, r := range []rune{0x061c, 0x200e, 0x200f, 0x202a, 0x202b, 0x202c, 0x202d, 0x202e, 0x2066, 0x2067, 0x2068, 0x2069} {
		t.Run(fmt.Sprintf("bidi U+%04X", r), func(t *testing.T) {
			got, err := profiles.NormalizeDisplayName("a" + string(r) + "b")
			if err != nil || got != "ab" {
				t.Fatalf("bidi U+%04X = %q, %v; want %q", r, got, err, "ab")
			}
		})
	}
}
