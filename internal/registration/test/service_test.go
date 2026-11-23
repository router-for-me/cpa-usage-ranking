package test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"cpa-usage-ranking/internal/avatars"
	"cpa-usage-ranking/internal/clients"
	"cpa-usage-ranking/internal/registration"
	"cpa-usage-ranking/internal/storage"
)

const serverID = "550e8400-e29b-41d4-a716-446655440000"
const returnTo = "http://localhost:8080/settings"

func builtinCatalog(t *testing.T) *avatars.Catalog {
	t.Helper()
	catalog, err := avatars.Load(avatars.BuiltinFS())
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

func openWithCatalog(t *testing.T, path string, catalog *avatars.Catalog, options registration.Options) (*storage.DB, *registration.Service) {
	t.Helper()
	db, err := storage.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := storage.InitSchema(context.Background(), db.Writer); err != nil {
		t.Fatal(err)
	}
	service, err := registration.NewService(db, catalog, options)
	if err != nil {
		t.Fatal(err)
	}
	return db, service
}

func open(t *testing.T, path string, options registration.Options) (*storage.DB, *registration.Service) {
	t.Helper()
	return openWithCatalog(t, path, builtinCatalog(t), options)
}

func avatarPtr(id int64) *int64 { return &id }

func digest(i byte) clients.CredentialHash { return clients.CredentialHash{i} }

func approve(t *testing.T, db *storage.DB, id string) {
	t.Helper()
	// 模拟已经过浏览器回调的状态；完整授权流程由 HTTP 测试覆盖。
	if _, err := db.Writer.Exec(`UPDATE registration_requests SET stage='approved',github_id=42,display_name='GitHub User' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
}

func counts(t *testing.T, db *storage.DB, seats, writers, requests int) {
	t.Helper()
	var s, w, r int
	if err := db.Reader.QueryRow(`SELECT (SELECT count(*) FROM clients),(SELECT count(*) FROM client_writers),(SELECT count(*) FROM registration_requests)`).Scan(&s, &w, &r); err != nil || s != seats || w != writers || r != requests {
		t.Fatalf("counts: %d %d %d, want %d %d %d, err=%v", s, w, r, seats, writers, requests, err)
	}
}

func TestConcurrentSafeRetriesAcrossIndependentSQLitePools(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registration.db")
	db, one := open(t, path, registration.DefaultOptions())
	_, two := open(t, path, registration.DefaultOptions())
	ctx := context.Background()
	ids := make([]string, 2)
	services := []*registration.Service{one, two}
	for i, service := range services {
		var err error
		ids[i], err = service.Start(ctx, digest(byte(i+1)), serverID, returnTo, nil)
		if err != nil {
			t.Fatal(err)
		}
		approve(t, db, ids[i])
	}
	const n = 16
	results := make(chan registration.Result, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			r, err := services[i%2].Complete(ctx, digest(byte(i%2+1)), ids[i%2])
			if err != nil {
				t.Error(err)
				return
			}
			results <- r
		})
	}
	wg.Wait()
	close(results)
	created := 0
	for r := range results {
		if r.Client.ID != 1 || r.Client.DisplayName != "GitHub User" || r.Client.AvatarID != 1 {
			t.Fatalf("unexpected seat: %+v", r.Client)
		}
		if r.Created {
			created++
		}
	}
	if created != 1 {
		t.Fatalf("created %d seats", created)
	}
	counts(t, db, 1, 2, 2)
}

func TestReadOnlyRegistrationWhileWriterBusy(t *testing.T) {
	db, s := open(t, filepath.Join(t.TempDir(), "registration.db"), registration.DefaultOptions())
	hash := digest(1)
	pending, err := s.Start(t.Context(), hash, serverID, returnTo, nil)
	if err != nil {
		t.Fatal(err)
	}
	completed, err := s.Start(t.Context(), hash, serverID, returnTo, nil)
	if err != nil {
		t.Fatal(err)
	}
	approve(t, db, completed)
	created, err := s.Complete(t.Context(), hash, completed)
	if err != nil || !created.Created {
		t.Fatalf("complete fixture: %+v %v", created, err)
	}
	tx, err := db.Writer.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()

	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	unknown := strings.Repeat("A", 43) // 格式合法但未签发，确保查到数据库。
	for _, tc := range []struct {
		name, id string
		wantID   int64
		wantErr  error
	}{
		{"unknown", unknown, 0, registration.ErrExpired},
		{"pending", pending, 0, registration.ErrPending},
		{"completed", completed, created.Client.ID, nil},
	} {
		got, err := s.Complete(ctx, hash, tc.id)
		if !errors.Is(err, tc.wantErr) || got.Client.ID != tc.wantID || got.Created {
			t.Errorf("complete %s: client=%d created=%t err=%v; want client=%d created=false err=%v", tc.name, got.Client.ID, got.Created, err, tc.wantID, tc.wantErr)
		}
	}
	if _, err := s.Authorize(ctx, unknown); !errors.Is(err, registration.ErrInvalidRequest) {
		t.Errorf("unknown authorize: %v", err)
	}
	if _, err := s.Callback(ctx, nil, unknown, unknown, ""); !errors.Is(err, registration.ErrInvalidRequest) {
		t.Errorf("unknown callback: %v", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return fn(r) }

func TestCallbackDisconnectStillRecordsOutcome(t *testing.T) {
	for _, tc := range []struct {
		name     string
		verified bool
	}{{"identity received", true}, {"identity not received", false}} {
		t.Run(tc.name, func(t *testing.T) {
			_, s := open(t, filepath.Join(t.TempDir(), "registration.db"), registration.DefaultOptions())
			id, err := s.Start(t.Context(), digest(1), serverID, returnTo, nil)
			if err != nil {
				t.Fatal(err)
			}
			a, err := s.Authorize(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			browser, disconnect := context.WithCancel(t.Context())
			defer disconnect()
			github, err := registration.NewGitHub(registration.GitHubOptions{PublicOrigin: "https://ranking.example", ClientID: "test", ClientSecret: "test"}, roundTripFunc(func(r *http.Request) (*http.Response, error) {
				body := `{"access_token":"test-token"}`
				if r.URL.Path == "/user" {
					disconnect()
					if !errors.Is(r.Context().Err(), context.Canceled) {
						t.Fatal("browser disconnect must still cancel the GitHub request")
					}
					if !tc.verified {
						return nil, r.Context().Err()
					}
					body = `{"id":42,"login":"example"}` // response already received
				}
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			dest, callbackErr := s.Callback(browser, github, a.State, a.BrowserSession, "test-code")
			seat, completeErr := s.Complete(t.Context(), digest(1), id)
			if tc.verified {
				if callbackErr != nil || dest != returnTo || completeErr != nil || !seat.Created {
					t.Fatalf("verified identity not saved: callback=%v complete=%v created=%t", callbackErr, completeErr, seat.Created)
				}
			} else if !errors.Is(callbackErr, registration.ErrGitHub) || !errors.Is(completeErr, registration.ErrGitHub) {
				t.Fatalf("unverified identity not recorded as failed: callback=%v complete=%v", callbackErr, completeErr)
			}
		})
	}
}

func TestDatabaseFailureAndCancellationLeaveNoPartialIdentity(t *testing.T) {
	db, service := open(t, filepath.Join(t.TempDir(), "registration.db"), registration.DefaultOptions())
	ctx := context.Background()
	id, err := service.Start(ctx, digest(1), serverID, returnTo, nil)
	if err != nil {
		t.Fatal(err)
	}
	approve(t, db, id)
	if _, err := db.Writer.Exec(`CREATE TRIGGER reject_registration BEFORE UPDATE OF client_id ON registration_requests BEGIN SELECT RAISE(ABORT, 'test failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Complete(ctx, digest(1), id); err == nil {
		t.Fatal("failed result write confirmed")
	}
	counts(t, db, 0, 0, 1)
	if _, err := db.Writer.Exec("DROP TRIGGER reject_registration"); err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := service.Complete(cancelled, digest(1), id); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled registration: %v", err)
	}
	counts(t, db, 0, 0, 1)
	if result, err := service.Complete(ctx, digest(1), id); err != nil || !result.Created {
		t.Fatalf("retry after rollback: %+v %v", result, err)
	}
	counts(t, db, 1, 1, 1)
}

func TestNewServiceRequiresValidCatalog(t *testing.T) {
	db, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "registration.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := registration.NewService(db, nil, registration.DefaultOptions()); err == nil {
		t.Fatal("nil catalog accepted")
	}
	if _, err := registration.NewService(db, &avatars.Catalog{}, registration.DefaultOptions()); err == nil {
		t.Fatal("uninitialized catalog accepted")
	}
}

func TestRegistrationAvatarSelectionAndExistingSeatPreservation(t *testing.T) {
	ctx := context.Background()
	db, svc := open(t, filepath.Join(t.TempDir(), "registration.db"), registration.DefaultOptions())

	// Omitting avatar_id uses the catalog default (1).
	defaultID, err := svc.Start(ctx, digest(1), serverID, returnTo, nil)
	if err != nil {
		t.Fatal(err)
	}
	approve(t, db, defaultID)
	defaultSeat, err := svc.Complete(ctx, digest(1), defaultID)
	if err != nil || !defaultSeat.Created || defaultSeat.Client.AvatarID != 1 {
		t.Fatalf("default avatar seat: %+v %v", defaultSeat, err)
	}

	// Explicit valid non-default avatar_id (66) applies to a new seat.
	const otherServerID = "550e8400-e29b-41d4-a716-446655440001"
	customID, err := svc.Start(ctx, digest(2), otherServerID, returnTo, avatarPtr(66))
	if err != nil {
		t.Fatal(err)
	}
	approve(t, db, customID)
	customSeat, err := svc.Complete(ctx, digest(2), customID)
	if err != nil || !customSeat.Created || customSeat.Client.AvatarID != 66 || customSeat.Client.DisplayName != "GitHub User" {
		t.Fatalf("custom avatar seat: %+v %v", customSeat, err)
	}

	// Second writer for the same (github_id, server_id) with a different avatar_id preserves the existing seat (Strategy A).
	secondWriterID, err := svc.Start(ctx, digest(3), otherServerID, returnTo, avatarPtr(15))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Writer.Exec(`UPDATE registration_requests SET stage='approved',github_id=42,display_name='Different Name' WHERE id=?`, secondWriterID); err != nil {
		t.Fatal(err)
	}
	reused, err := svc.Complete(ctx, digest(3), secondWriterID)
	if err != nil || reused.Created || reused.Client.ID != customSeat.Client.ID || reused.Client.AvatarID != 66 || reused.Client.DisplayName != "GitHub User" {
		t.Fatalf("existing seat profile mutated: %+v %v", reused, err)
	}
	counts(t, db, 2, 3, 3)
}

func TestRegistrationAvatarValidationAndLargeCatalogIDs(t *testing.T) {
	ctx := context.Background()
	db, svc := open(t, filepath.Join(t.TempDir(), "invalid-avatar.db"), registration.DefaultOptions())
	for _, invalid := range []int64{-1, 0, 67, 999} {
		if _, err := svc.Start(ctx, digest(1), serverID, returnTo, avatarPtr(invalid)); !errors.Is(err, registration.ErrInvalidAvatar) {
			t.Fatalf("avatar %d: got %v, want ErrInvalidAvatar", invalid, err)
		}
	}
	counts(t, db, 0, 0, 0)

	// Closed registration takes precedence over avatar catalog lookup.
	closedOpts := registration.DefaultOptions()
	closedOpts.Enabled = false
	_, closedSvc := open(t, filepath.Join(t.TempDir(), "closed-avatar.db"), closedOpts)
	if _, err := closedSvc.Start(ctx, digest(1), serverID, returnTo, avatarPtr(999)); !errors.Is(err, registration.ErrRegistrationClosed) {
		t.Fatalf("closed with invalid avatar: got %v, want ErrRegistrationClosed", err)
	}
	if _, err := closedSvc.Start(ctx, digest(1), "bad-server-id", returnTo, avatarPtr(999)); !errors.Is(err, registration.ErrInvalidRequest) {
		t.Fatalf("closed with invalid server_id: got %v, want ErrInvalidRequest", err)
	}

	// Sparse / large 64-bit avatar IDs from testdata catalog.
	largeCatalog, err := avatars.Load(os.DirFS(filepath.Join("..", "..", "avatars", "test", "testdata", "catalog")))
	if err != nil {
		t.Fatal(err)
	}
	largeDB, largeSvc := openWithCatalog(t, filepath.Join(t.TempDir(), "large-avatar.db"), largeCatalog, registration.DefaultOptions())
	const largeDefault int64 = 1099511627776
	const sparseID int64 = 67

	reqDefault, err := largeSvc.Start(ctx, digest(1), serverID, returnTo, nil)
	if err != nil {
		t.Fatal(err)
	}
	approve(t, largeDB, reqDefault)
	seatDefault, err := largeSvc.Complete(ctx, digest(1), reqDefault)
	if err != nil || !seatDefault.Created || seatDefault.Client.AvatarID != largeDefault {
		t.Fatalf("large default avatar seat: %+v %v", seatDefault, err)
	}

	const secondServerID = "550e8400-e29b-41d4-a716-446655440002"
	reqSparse, err := largeSvc.Start(ctx, digest(2), secondServerID, returnTo, avatarPtr(sparseID))
	if err != nil {
		t.Fatal(err)
	}
	approve(t, largeDB, reqSparse)
	seatSparse, err := largeSvc.Complete(ctx, digest(2), reqSparse)
	if err != nil || !seatSparse.Created || seatSparse.Client.AvatarID != sparseID {
		t.Fatalf("sparse avatar seat: %+v %v", seatSparse, err)
	}
	if _, err := largeSvc.Start(ctx, digest(3), secondServerID, returnTo, avatarPtr(1)); !errors.Is(err, registration.ErrInvalidAvatar) {
		t.Fatalf("unlisted avatar in sparse catalog: %v", err)
	}
}

func TestConcurrentCompleteRetriesPreserveSelectedAvatar(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "concurrent-avatar.db")
	db, one := open(t, path, registration.DefaultOptions())
	_, two := open(t, path, registration.DefaultOptions())
	id, err := one.Start(ctx, digest(1), serverID, returnTo, avatarPtr(42))
	if err != nil {
		t.Fatal(err)
	}
	approve(t, db, id)
	const n = 16
	results := make(chan registration.Result, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			svc := []*registration.Service{one, two}[i%2]
			r, err := svc.Complete(ctx, digest(1), id)
			if err != nil {
				t.Error(err)
				return
			}
			results <- r
		})
	}
	wg.Wait()
	close(results)
	created := 0
	for r := range results {
		if r.Client.ID != 1 || r.Client.AvatarID != 42 {
			t.Fatalf("unexpected seat avatar on retry: %+v", r.Client)
		}
		if r.Created {
			created++
		}
	}
	if created != 1 {
		t.Fatalf("created %d seats", created)
	}
	counts(t, db, 1, 1, 1)
}
