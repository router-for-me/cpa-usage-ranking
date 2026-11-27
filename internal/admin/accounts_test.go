package admin

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"cpa-usage-ranking/internal/storage"
	"golang.org/x/crypto/bcrypt"
)

func serviceDB(t *testing.T) (*Service, *storage.DB) {
	t.Helper()
	db, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "ranking.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := storage.InitSchema(context.Background(), db.Writer); err != nil {
		t.Fatal(err)
	}
	s, err := New(Owner{Username: "owner", Password: "isolated-test-password"}, db)
	if err != nil {
		t.Fatal(err)
	}
	return s, db
}

func TestOrdinaryAccountLifecycle(t *testing.T) {
	ctx := context.Background()
	s, db := serviceDB(t)
	account, err := s.CreateAccount(ctx, s.owner, "staff", "ordinary-test-password")
	if err != nil {
		t.Fatal(err)
	}
	var hash []byte
	if err := db.Reader.QueryRow("SELECT password_hash FROM admin_accounts WHERE id = ?", account.ID).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	if string(hash) == "ordinary-test-password" || bcrypt.CompareHashAndPassword(hash, []byte("ordinary-test-password")) != nil {
		t.Fatal("unsafe password storage")
	}
	token, session, err := s.Login(ctx, "staff", "ordinary-test-password")
	if err != nil || session.Identity.Owner || session.Identity.ID != account.ID {
		t.Fatal("ordinary login", err)
	}
	if _, _, err := s.Accounts(ctx, session.Identity); !errors.Is(err, ErrForbidden) {
		t.Fatal("ordinary listed accounts")
	}
	if _, err := s.CreateAccount(ctx, session.Identity, "more-staff", "ordinary-test-password"); !errors.Is(err, ErrForbidden) {
		t.Fatal("ordinary created account")
	}
	if err := s.DeleteAccount(ctx, session.Identity, 1); !errors.Is(err, ErrForbidden) {
		t.Fatal("ordinary deleted account")
	}
	if _, err := s.CreateAccount(ctx, s.owner, "owner", "ordinary-test-password"); !errors.Is(err, ErrConflict) {
		t.Fatal("reserved owner name")
	}
	if _, err := s.CreateAccount(ctx, s.owner, "staff", "ordinary-test-password"); !errors.Is(err, ErrConflict) {
		t.Fatal("duplicate accepted")
	}
	restarted, err := New(Owner{Username: "owner", Password: "isolated-test-password"}, db)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := restarted.Login(ctx, "staff", "ordinary-test-password"); err != nil {
		t.Fatal("account not persistent", err)
	}
	if _, err := restarted.Lookup(ctx, token); !errors.Is(err, ErrSession) {
		t.Fatal("restart retained session")
	}
	if _, err := New(Owner{Username: "staff", Password: "isolated-test-password"}, db); err == nil {
		t.Fatal("owner name collision accepted")
	}
	if err := s.DeleteAccount(ctx, s.owner, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Lookup(ctx, token); !errors.Is(err, ErrSession) {
		t.Fatal("deleted session valid")
	}
	recreated, err := s.CreateAccount(ctx, s.owner, "staff", "ordinary-test-password")
	if err != nil || recreated.ID == account.ID {
		t.Fatal("id reused", err)
	}
	if _, err := s.Lookup(ctx, token); !errors.Is(err, ErrSession) {
		t.Fatal("old cookie inherited recreated account")
	}
}

func TestOrdinaryLookupKeepsSessionOnCanceledContext(t *testing.T) {
	ctx := context.Background()
	s, db := serviceDB(t)
	account, err := s.CreateAccount(ctx, s.owner, "staff", "ordinary-test-password")
	if err != nil {
		t.Fatal(err)
	}
	token, session, err := s.Login(ctx, "staff", "ordinary-test-password")
	if err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.Lookup(canceled, token); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled lookup error = %v, want context.Canceled", err)
	}
	got, err := s.Lookup(ctx, token)
	if err != nil || got.Identity != session.Identity || got.CSRF != session.CSRF {
		t.Fatalf("session lost after canceled lookup: got=%+v err=%v", got, err)
	}
	// 直接删除数据库账号行，验证 Lookup 命中 sql.ErrNoRows 时仍会销毁内存会话并返回 ErrSession。
	if _, err := db.Writer.ExecContext(ctx, "DELETE FROM admin_accounts WHERE id = ?", account.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Lookup(ctx, token); !errors.Is(err, ErrSession) {
		t.Fatalf("deleted account lookup error = %v, want ErrSession", err)
	}
	s.mu.Lock()
	_, stillCached := s.sessions[token]
	s.mu.Unlock()
	if stillCached {
		t.Fatal("deleted account session remained in memory")
	}
}
