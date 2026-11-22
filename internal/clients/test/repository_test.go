package test

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"cpa-usage-ranking/internal/clients"
	"cpa-usage-ranking/internal/storage"
	"cpa-usage-ranking/internal/testfixture"

	"github.com/mattn/go-sqlite3"
)

func openRepository(t *testing.T, path string) (*storage.DB, *clients.Repository) {
	t.Helper()
	db, err := storage.Open(context.Background(), path)
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
	return db, clients.NewRepository(db)
}

// 固定字符串仅用于测试摘要；实际凭据的生成和校验不属于存储层。
func credentialHash(label string) clients.CredentialHash { return sha256.Sum256([]byte(label)) }

func create(t *testing.T, db *storage.DB, label string) clients.Client {
	t.Helper()
	writer, err := testfixture.Seat(context.Background(), db.Writer, credentialHash(label), "同名 🚀", 4096)
	if err != nil {
		t.Fatal(err)
	}
	return writer.Client
}

func assertClient(t *testing.T, got, want clients.Client) {
	t.Helper()
	if got.ID != want.ID || got.DisplayName != want.DisplayName || got.AvatarID != want.AvatarID || got.Disabled != want.Disabled || !got.CreatedAt.Equal(want.CreatedAt) {
		t.Fatalf("client = %+v, want %+v", got, want)
	}
	if got.CreatedAt.Location() != time.UTC {
		t.Fatalf("creation timestamp is not UTC: %s", got.CreatedAt)
	}
}

func TestCreateAndLookupKeepSameNameClientsDistinct(t *testing.T) {
	db, repository := openRepository(t, filepath.Join(t.TempDir(), "ranking.db"))
	before := time.Now().UTC().Truncate(time.Millisecond)
	one, two := create(t, db, "one"), create(t, db, "two")
	if one.ID <= 0 || two.ID <= 0 || one.ID == two.ID {
		t.Fatal("client identities were not independently allocated")
	}
	if one.Disabled || one.CreatedAt.Before(before) || one.CreatedAt.After(time.Now()) {
		t.Fatalf("invalid initial metadata: %+v", one)
	}
	for _, entry := range []struct {
		label  string
		client clients.Client
	}{{"one", one}, {"two", two}} {
		byID, err := repository.ByID(context.Background(), entry.client.ID)
		if err != nil {
			t.Fatal(err)
		}
		assertClient(t, byID, entry.client)
		byHash, err := repository.ByCredentialHash(context.Background(), credentialHash(entry.label))
		if err != nil {
			t.Fatal(err)
		}
		assertClient(t, byHash.Client, entry.client)
	}
}

func TestDuplicateCredentialDoesNotOverwriteExistingClient(t *testing.T) {
	db, repository := openRepository(t, filepath.Join(t.TempDir(), "ranking.db"))
	original := create(t, db, "same credential")
	if err := repository.SetDisabled(context.Background(), original.ID, true); err != nil {
		t.Fatal(err)
	}
	original.Disabled = true
	_, err := testfixture.Seat(context.Background(), db.Writer, credentialHash("same credential"), "不得覆盖", 1)
	if !duplicateHash(err) {
		t.Fatalf("duplicate error = %v", err)
	}
	got, err := repository.ByCredentialHash(context.Background(), credentialHash("same credential"))
	if err != nil {
		t.Fatal(err)
	}
	assertClient(t, got.Client, original)
}

func TestProfileUpdatePreservesIdentityCredentialTimestampAndStatus(t *testing.T) {
	db, repository := openRepository(t, filepath.Join(t.TempDir(), "ranking.db"))
	original := create(t, db, "profile")
	if err := repository.SetDisabled(context.Background(), original.ID, true); err != nil {
		t.Fatal(err)
	}
	name := "新昵称 ' ; -- 🚀"
	for range 2 {
		if err := repository.UpdateProfile(context.Background(), original.ID, name, 1<<40); err != nil {
			t.Fatal(err)
		}
	}
	got, err := repository.ByCredentialHash(context.Background(), credentialHash("profile"))
	if err != nil {
		t.Fatal(err)
	}
	want := original
	want.DisplayName, want.AvatarID, want.Disabled = name, 1<<40, true
	assertClient(t, got.Client, want)
	// 只有显式状态操作可以恢复启用，修改资料不会顺带重置状态。
	for range 2 {
		if err := repository.SetDisabled(context.Background(), original.ID, false); err != nil {
			t.Fatal(err)
		}
	}
	byID, err := repository.ByID(context.Background(), original.ID)
	if err != nil {
		t.Fatal(err)
	}
	want.Disabled = false
	assertClient(t, byID, want)
}

func TestClientAndWriterSurviveSameSchemaReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ranking.db")
	db, repository := openRepository(t, path)
	original := create(t, db, "persisted")
	if err := repository.UpdateProfile(context.Background(), original.ID, "重开后仍保留", 512); err != nil {
		t.Fatal(err)
	}
	if err := repository.SetDisabled(context.Background(), original.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopenedDB, reopened := openRepository(t, path)
	got, err := reopened.ByCredentialHash(context.Background(), credentialHash("persisted"))
	if err != nil {
		t.Fatal(err)
	}
	original.DisplayName, original.AvatarID, original.Disabled = "重开后仍保留", 512, true
	assertClient(t, got.Client, original)
	if create(t, reopenedDB, "new").ID <= original.ID {
		t.Fatal("reopened database reused an identity")
	}
}

func TestMissingClientAndCancellationErrors(t *testing.T) {
	db, repository := openRepository(t, filepath.Join(t.TempDir(), "ranking.db"))
	ctx := context.Background()
	_, byID := repository.ByID(ctx, 999)
	_, byHash := repository.ByCredentialHash(ctx, credentialHash("missing"))
	for _, err := range []error{byID, byHash, repository.UpdateProfile(ctx, 999, "missing", 1), repository.SetDisabled(ctx, 999, true)} {
		if !errors.Is(err, clients.ErrNotFound) {
			t.Fatalf("missing client returned %v", err)
		}
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, err := testfixture.Seat(cancelled, db.Writer, credentialHash("cancelled"), "cancelled", 1)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled creation returned %v", err)
	}
	_, err = repository.ByID(cancelled, 999)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled lookup returned %v", err)
	}
	var count int
	if err := db.Reader.QueryRow("SELECT COUNT(*) FROM clients").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("cancelled creation left a row")
	}
}

func TestWithdrawNoOpWhileWriterBusy(t *testing.T) {
	db, repo := openRepository(t, filepath.Join(t.TempDir(), "ranking.db"))
	create(t, db, "withdrawn")
	hash := credentialHash("withdrawn")
	if changed, err := repo.Withdraw(t.Context(), hash); !changed || err != nil {
		t.Fatalf("first withdrawal: changed=%t err=%v", changed, err)
	}
	tx, err := db.Writer.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()

	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	if changed, err := repo.Withdraw(ctx, credentialHash("missing")); changed || !errors.Is(err, clients.ErrNotFound) {
		t.Errorf("unknown withdrawal: changed=%t err=%v", changed, err)
	}
	if changed, err := repo.Withdraw(ctx, hash); changed || err != nil {
		t.Errorf("repeated withdrawal: changed=%t err=%v", changed, err)
	}
}

func TestConcurrentDuplicateCredentialCreatesOneClient(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ranking.db")
	db, repository := openRepository(t, path)
	other, _ := openRepository(t, path)
	databases := []*storage.DB{db, other}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	type result struct {
		client clients.Client
		err    error
	}
	results := make(chan result, 8)
	start := make(chan struct{})
	for i := range 8 {
		go func() {
			<-start
			writer, err := testfixture.Seat(ctx, databases[i%2].Writer, credentialHash("concurrent"), fmt.Sprintf("client-%d", i), int64(i+1))
			results <- result{writer.Client, err}
		}()
	}
	close(start)
	var winner clients.Client
	created := 0
	for range 8 {
		result := <-results
		if result.err == nil {
			created++
			winner = result.client
		} else if !duplicateHash(result.err) {
			t.Errorf("unexpected creation error: %v", result.err)
		}
	}
	if created != 1 {
		t.Fatalf("created %d clients", created)
	}
	got, err := repository.ByCredentialHash(ctx, credentialHash("concurrent"))
	if err != nil {
		t.Fatal(err)
	}
	assertClient(t, got.Client, winner)
	var count int
	if err := db.Reader.QueryRow("SELECT COUNT(*) FROM clients").Scan(&count); err != nil || count != 1 {
		t.Fatalf("row count=%d, error=%v", count, err)
	}
}

func TestInvalidAvatarDoesNotCreateOrPartiallyUpdate(t *testing.T) {
	db, repository := openRepository(t, filepath.Join(t.TempDir(), "ranking.db"))
	original := create(t, db, "valid")
	for _, avatar := range []int64{0, -1} {
		_, err := testfixture.Seat(context.Background(), db.Writer, credentialHash("invalid"), "invalid", avatar)
		if err == nil || duplicateHash(err) {
			t.Fatalf("invalid avatar creation: %v", err)
		}
		if err := repository.UpdateProfile(context.Background(), original.ID, "must not replace", avatar); err == nil {
			t.Fatal("invalid avatar update succeeded")
		}
	}
	got, err := repository.ByID(context.Background(), original.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertClient(t, got, original)
	if _, err := repository.ByCredentialHash(context.Background(), credentialHash("invalid")); !errors.Is(err, clients.ErrNotFound) {
		t.Fatalf("invalid creation left a client: %v", err)
	}
}

func TestDeletedIdentityIsNotReallocated(t *testing.T) {
	db, _ := openRepository(t, filepath.Join(t.TempDir(), "ranking.db"))
	original := create(t, db, "old")
	// 不提供删除 API；这里直接验证数据库不会将已用过的编号分配给新身份。
	if _, err := db.Writer.Exec("DELETE FROM client_writers WHERE client_id=?; DELETE FROM clients WHERE id=?", original.ID, original.ID); err != nil {
		t.Fatal(err)
	}
	if create(t, db, "replacement").ID <= original.ID {
		t.Fatal("database reused a prior identity")
	}
}

func duplicateHash(err error) bool {
	var sqliteErr sqlite3.Error
	return errors.As(err, &sqliteErr) && sqliteErr.ExtendedCode == sqlite3.ErrConstraintUnique
}
