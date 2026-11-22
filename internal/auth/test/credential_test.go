package test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"cpa-usage-ranking/internal/auth"
	"cpa-usage-ranking/internal/clients"
	"cpa-usage-ranking/internal/storage"
	"cpa-usage-ranking/internal/testfixture"
)

func TestCredentialEncodingAndDigest(t *testing.T) {
	for _, raw := range [][]byte{make([]byte, 32), bytes.Repeat([]byte{255}, 32), randomBytes(t)} {
		text := base64.RawURLEncoding.EncodeToString(raw)
		hash, err := auth.HashCredential(text)
		if err != nil || hash != clients.CredentialHash(sha256.Sum256(raw)) {
			t.Fatalf("unexpected digest/error: %v", err)
		}
		if len(text) != 43 {
			t.Fatal("bad client encoding")
		}
	}
	// 全零格式也有效：服务端不以字符分布猜测客户端随机源。
}

func randomBytes(t *testing.T) []byte {
	t.Helper()
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestInvalidCredentialAndBearerHeaders(t *testing.T) {
	valid := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{255}, 32))
	for _, text := range []string{"", valid + "=", valid[:42], valid + "a", strings.Repeat("+", 43), strings.Repeat("/", 43), valid[:42] + "9", valid[:42] + "\n", " " + valid, valid + " "} {
		_, err := auth.HashCredential(text)
		if !errors.Is(err, auth.ErrInvalidCredential) || err.Error() != "invalid client credential" {
			t.Fatal("credential format error must be generic")
		}
	}
	for _, values := range [][]string{nil, {""}, {"Basic " + valid}, {"Bearer"}, {"Bearer  " + valid}, {"Bearer\t" + valid}, {"Bearer " + valid + ", Bearer " + valid}, {"Bearer " + valid, "Bearer " + valid}} {
		_, err := auth.BearerHash(http.Header{"Authorization": values})
		if !errors.Is(err, auth.ErrInvalidCredential) {
			t.Fatal("invalid/ambiguous header accepted")
		}
	}
	for _, scheme := range []string{"Bearer", "bearer", "BEARER"} {
		if _, err := auth.BearerHash(http.Header{"Authorization": {scheme + " " + valid}}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestStorageContainsOnly32ByteDigest(t *testing.T) {
	ctx := context.Background()
	db, err := storage.Open(ctx, filepath.Join(t.TempDir(), "credential.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := storage.InitSchema(ctx, db.Writer); err != nil {
		t.Fatal(err)
	}
	text := base64.RawURLEncoding.EncodeToString(randomBytes(t))
	hash, err := auth.HashCredential(text)
	if err != nil {
		t.Fatal(err)
	}
	repo := clients.NewRepository(db)
	writer, err := testfixture.Seat(ctx, db.Writer, hash, "public", 1)
	if err != nil {
		t.Fatal(err)
	}
	var stored []byte
	if err := db.Reader.QueryRowContext(ctx, "SELECT token_hash FROM client_writers WHERE id = ?", writer.WriterID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if len(stored) != 32 || !bytes.Equal(stored, hash[:]) || bytes.Equal(stored, []byte(text)) {
		t.Fatal("database must contain the digest only")
	}
	other, _ := auth.HashCredential(base64.RawURLEncoding.EncodeToString(randomBytes(t)))
	if _, err := repo.ByCredentialHash(ctx, other); !errors.Is(err, clients.ErrNotFound) {
		t.Fatal("wrong secret matched identity")
	}
}
