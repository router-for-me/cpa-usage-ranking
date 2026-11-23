// Package auth handles client-owned bearer credentials; it never issues secrets.
package auth

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"

	"cpa-usage-ranking/internal/clients"
)

var ErrInvalidCredential = errors.New("invalid client credential")

// HashCredential decodes a 32-byte base64url (or standard base64) client secret
// with optional single '=' padding and surrounding whitespace, and returns the
// SHA-256 digest of the decoded 32 bytes. Canonical 43-character unpadded
// base64url remains the recommended client format and yields the exact same hash.
func HashCredential(text string) (clients.CredentialHash, error) {
	trimmed := strings.TrimSpace(text)
	// 32 字节编码为 43 字符，标准填充形式仅含单个尾随 '='。
	unpadded := strings.TrimSuffix(trimmed, "=")
	if len(unpadded) != 43 {
		return clients.CredentialHash{}, ErrInvalidCredential
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(unpadded)
	if err != nil {
		decoded, err = base64.RawStdEncoding.Strict().DecodeString(unpadded)
	}
	if err != nil || len(decoded) != 32 {
		return clients.CredentialHash{}, ErrInvalidCredential
	}
	return clients.CredentialHash(sha256.Sum256(decoded)), nil
}

// BearerHash reads a single Authorization header, never URL/body parameters.
// No credential text is retained or included in errors.
func BearerHash(header http.Header) (clients.CredentialHash, error) {
	values := header.Values("Authorization")
	if len(values) != 1 {
		return clients.CredentialHash{}, ErrInvalidCredential
	}
	fields := strings.Fields(values[0])
	if len(fields) != 2 || !strings.EqualFold(fields[0], "Bearer") {
		return clients.CredentialHash{}, ErrInvalidCredential
	}
	return HashCredential(fields[1])
}
