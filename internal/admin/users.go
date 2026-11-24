package admin

import (
	"context"
	"cpa-usage-ranking/internal/clients"
	"database/sql"
	"errors"
)

func (s *Service) Users(ctx context.Context, query clients.ListQuery) (clients.Page, error) {
	return s.clients.List(ctx, query)
}

// SetDisabled only writes the target column, using the existing serial writer.
// The ordinary administrator is rechecked after acquiring the write transaction.
func (s *Service) SetDisabled(ctx context.Context, actor Identity, id int64, disabled bool) error {
	if id <= 0 {
		return ErrInput
	}
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := s.checkActor(ctx, tx, actor); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, "UPDATE clients SET disabled = ? WHERE id = ?", disabled, id)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

func (s *Service) SetRankingHidden(ctx context.Context, actor Identity, id int64, hidden bool) error {
	if id <= 0 {
		return ErrInput
	}
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := s.checkActor(ctx, tx, actor); err != nil {
		return err
	}
	if !hidden {
		var disabled bool
		if err := tx.QueryRowContext(ctx, "SELECT disabled FROM clients WHERE id = ?", id).Scan(&disabled); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if disabled {
			return ErrBanned
		}
	}
	result, err := tx.ExecContext(ctx, "UPDATE clients SET ranking_hidden = ? WHERE id = ?", hidden, id)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}
