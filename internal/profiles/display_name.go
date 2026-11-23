// Package profiles 校验公开展示资料，不承担身份识别或内容审核。
package profiles

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

var ErrInvalidDisplayName = errors.New("invalid display name")

// NormalizeDisplayName 返回应保存及展示的 NFKC 昵称；失败时不返回部分结果。
// 对控制字符、行/段分隔符与双向控制符进行清理，并对超长文本按完整 rune 边界截断。
func NormalizeDisplayName(name string) (string, error) {
	if !utf8.ValidString(name) {
		return "", fmt.Errorf("%w: must be valid UTF-8", ErrInvalidDisplayName)
	}
	name = norm.NFKC.String(name)
	var cleaned strings.Builder
	cleaned.Grow(len(name))
	dropped := false
	for _, r := range name {
		if unicode.IsControl(r) || unicode.In(r, unicode.Zl, unicode.Zp, unicode.Bidi_Control) {
			if unicode.IsSpace(r) {
				cleaned.WriteByte(' ')
			} else {
				dropped = true
			}
			continue
		}
		cleaned.WriteRune(r)
	}
	name = cleaned.String()
	if dropped {
		// 过滤不可见控制符后重新做一次 NFKC，确保结果满足幂等归一化。
		name = norm.NFKC.String(name)
	}
	name = strings.TrimSpace(name)
	// 按完整 Unicode 码点边界安全截断至最多 32 个码点且不超过 128 UTF-8 字节。
	var truncated strings.Builder
	runes := 0
	for _, r := range name {
		rLen := utf8.RuneLen(r)
		if runes >= 32 || truncated.Len()+rLen > 128 {
			break
		}
		truncated.WriteRune(r)
		runes++
	}
	name = strings.TrimSpace(truncated.String())
	if name == "" {
		return "", fmt.Errorf("%w: must contain visible content", ErrInvalidDisplayName)
	}
	for _, r := range name {
		// 格式字符、附加标记和默认可忽略字符可以保留，但不能独自构成可见昵称。
		// U+2800 是不属于空白或默认可忽略属性的盲文空格。
		if !unicode.IsSpace(r) && !unicode.IsMark(r) && !unicode.In(r, unicode.Cf, unicode.Other_Default_Ignorable_Code_Point) && r != '\u2800' {
			return name, nil
		}
	}
	return "", fmt.Errorf("%w: must contain visible content", ErrInvalidDisplayName)
}
