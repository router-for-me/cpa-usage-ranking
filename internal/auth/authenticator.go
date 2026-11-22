package auth

import (
	"context"
	"errors"
	"net/http"

	"cpa-usage-ranking/internal/clients"
)

type Authenticator struct{ repo *clients.Repository }

func NewAuthenticator(repo *clients.Repository) *Authenticator {
	return &Authenticator{repo: repo}
}

// Authenticate binds a report to the writer and seat selected by its credential.
func (a *Authenticator) Authenticate(ctx context.Context, header http.Header) (clients.Writer, error) {
	hash, err := BearerHash(header)
	if err != nil {
		return clients.Writer{}, err
	}
	writer, err := a.repo.ByCredentialHash(ctx, hash)
	if errors.Is(err, clients.ErrNotFound) {
		return clients.Writer{}, ErrInvalidCredential
	}
	if err != nil {
		return clients.Writer{}, err
	}
	if writer.Withdrawn {
		return clients.Writer{}, clients.ErrWithdrawn
	}
	if writer.Disabled {
		return clients.Writer{}, clients.ErrDisabled
	}
	return writer, nil
}
