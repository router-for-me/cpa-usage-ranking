// Package testfixture seeds disposable SQLite databases for tests that are not testing OAuth.
package testfixture

import (
	"context"
	"database/sql"
	"time"

	"cpa-usage-ranking/internal/clients"
)

func Seat(ctx context.Context, db *sql.DB, hash clients.CredentialHash, name string, avatar int64) (clients.Writer, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return clients.Writer{}, err
	}
	defer tx.Rollback()
	seat := clients.Client{DisplayName: name, AvatarID: avatar, CreatedAt: time.Now().UTC().Truncate(time.Millisecond)}
	if err := tx.QueryRowContext(ctx, `INSERT INTO clients(github_id,cpa_uuid,display_name,avatar_id,created_at_ms)
		SELECT coalesce(max(github_id),0)+1,'550e8400-e29b-41d4-a716-446655440000',?,?,? FROM clients RETURNING id`,
		name, avatar, seat.CreatedAt.UnixMilli()).Scan(&seat.ID); err != nil {
		return clients.Writer{}, err
	}
	id, err := Writer(ctx, tx, seat.ID, hash)
	if err != nil {
		return clients.Writer{}, err
	}
	return clients.Writer{Client: seat, WriterID: id}, tx.Commit()
}

func Writer(ctx context.Context, db interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, seatID int64, hash clients.CredentialHash) (int64, error) {
	var id int64
	err := db.QueryRowContext(ctx, `INSERT INTO client_writers(client_id,token_hash) VALUES(?,?) RETURNING id`, seatID, hash[:]).Scan(&id)
	return id, err
}
