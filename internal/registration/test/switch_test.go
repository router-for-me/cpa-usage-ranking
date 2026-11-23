package test

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"path/filepath"
	"testing"

	"cpa-usage-ranking/internal/auth"
	"cpa-usage-ranking/internal/clients"
	"cpa-usage-ranking/internal/registration"
)

func TestClosedRegistrationKeepsExistingRetryAndAuthentication(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registration.db")
	db, service := open(t, path, registration.DefaultOptions())
	ctx := context.Background()
	secret := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	hash, err := auth.HashCredential(secret)
	if err != nil {
		t.Fatal(err)
	}
	id, err := service.Start(ctx, hash, serverID, returnTo)
	if err != nil {
		t.Fatal(err)
	}
	approve(t, db, id)
	first, err := service.Complete(ctx, hash, id)
	if err != nil || !first.Created {
		t.Fatal(first, err)
	}
	other, err := service.Start(ctx, digest(1), serverID, returnTo)
	if err != nil {
		t.Fatal(err)
	}
	approve(t, db, other)
	opts := registration.DefaultOptions()
	opts.Enabled = false
	_, closed := open(t, path, opts)
	r, err := closed.Complete(ctx, hash, id)
	if err != nil || r.Created || r.Client.ID != first.Client.ID {
		t.Fatalf("closed retry: %+v %v", r, err)
	}
	if _, err := closed.Start(ctx, hash, serverID, returnTo); !errors.Is(err, registration.ErrRegistrationClosed) {
		t.Fatalf("new start accepted when closed: %v", err)
	}
	if _, err := closed.Complete(ctx, digest(1), other); !errors.Is(err, registration.ErrRegistrationClosed) {
		t.Fatalf("new writer accepted when closed: %v", err)
	}
	repo := clients.NewRepository(db)
	authenticator := auth.NewAuthenticator(repo)
	headers := http.Header{"Authorization": {"Bearer " + secret}}
	client, err := authenticator.Authenticate(ctx, headers)
	if err != nil || client.ID != first.Client.ID {
		t.Fatalf("switch broke authentication: %+v %v", client, err)
	}
	if err := repo.SetDisabled(ctx, first.Client.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := closed.Complete(ctx, hash, id); !errors.Is(err, clients.ErrDisabled) {
		t.Fatalf("switch obscured disabled state: %v", err)
	}
	if _, err := authenticator.Authenticate(ctx, headers); !errors.Is(err, clients.ErrDisabled) {
		t.Fatalf("disabled authentication: %v", err)
	}
	counts(t, db, 1, 1, 2)
}
