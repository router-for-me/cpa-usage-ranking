package test

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"cpa-usage-ranking/internal/auth"
	"cpa-usage-ranking/internal/clients"
	"cpa-usage-ranking/internal/httpserver"
	"cpa-usage-ranking/internal/storage"
	"cpa-usage-ranking/internal/testfixture"
	"github.com/gin-gonic/gin"
)

func TestClientAuthenticationIgnoresClaimedIdentity(t *testing.T) {
	ctx := context.Background()
	db, err := storage.Open(ctx, filepath.Join(t.TempDir(), "authentication.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := storage.InitSchema(ctx, db.Writer); err != nil {
		t.Fatal(err)
	}
	repo := clients.NewRepository(db)
	create := func(value byte) (clients.Client, string) {
		raw := make([]byte, 32)
		raw[0] = value
		secret := base64.RawURLEncoding.EncodeToString(raw)
		hash, err := auth.HashCredential(secret)
		if err != nil {
			t.Fatal(err)
		}
		writer, err := testfixture.Seat(ctx, db.Writer, hash, "unchanged", 1)
		if err != nil {
			t.Fatal(err)
		}
		return writer.Client, secret
	}
	owner, secret := create(1)
	other, otherSecret := create(2)
	router := httpserver.NewRouter(slog.New(slog.NewTextHandler(io.Discard, nil)))
	// 仅测试路由：生产没有资料修改/演示身份接口。
	router.POST("/test/write/:client_id", httpserver.ClientAuthentication(auth.NewAuthenticator(repo)), func(c *gin.Context) {
		client, ok := httpserver.AuthenticatedClient(c)
		if !ok {
			t.Error("identity missing")
			c.Status(500)
			return
		}
		if err := repo.UpdateProfile(c.Request.Context(), client.ID, "owner changed", 1); err != nil {
			t.Error(err)
			c.Status(500)
			return
		}
		c.JSON(200, gin.H{"client_id": client.ID})
	})
	send := func(token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", fmt.Sprintf("/test/write/%d?client_id=%d&credential=%s", other.ID, other.ID, otherSecret), strings.NewReader(fmt.Sprintf(`{"client_id":%d,"credential":%q}`, other.ID, otherSecret)))
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		return w
	}
	w := send(secret)
	if w.Code != 200 || !strings.Contains(w.Body.String(), fmt.Sprintf(`"client_id":%d`, owner.ID)) {
		t.Fatalf("authenticated identity: %d %s", w.Code, w.Body)
	}
	stored, err := repo.ByID(ctx, other.ID)
	if err != nil || stored.DisplayName != "unchanged" {
		t.Fatalf("other identity overwritten: %+v %v", stored, err)
	}
	for _, token := range []string{"", "invalid", base64.RawURLEncoding.EncodeToString(make([]byte, 32))} {
		w := send(token)
		if w.Code != http.StatusUnauthorized || w.Header().Get("WWW-Authenticate") != "Bearer" {
			t.Fatalf("wrong credential: %d %s", w.Code, w.Body)
		}
		if strings.Contains(w.Body.String(), otherSecret) || strings.Contains(w.Body.String(), secret) {
			t.Fatal("error leaked secret")
		}
	}
	if err := repo.SetDisabled(ctx, owner.ID, true); err != nil {
		t.Fatal(err)
	}
	if w := send(secret); w.Code != http.StatusForbidden {
		t.Fatalf("disabled: %d %s", w.Code, w.Body)
	}
	if err := db.Reader.Close(); err != nil {
		t.Fatal(err)
	}
	if w := send(otherSecret); w.Code != http.StatusInternalServerError || strings.Contains(w.Body.String(), "database") {
		t.Fatalf("database failure: %d %s", w.Code, w.Body)
	}
}
