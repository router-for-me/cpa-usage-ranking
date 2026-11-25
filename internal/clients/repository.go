// Package clients 保存客户端身份与当前展示资料，不负责注册、鉴权或资料校验。
package clients

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"cpa-usage-ranking/internal/storage"
)

var (
	ErrNotFound  = errors.New("client or writer not found")
	ErrWithdrawn = errors.New("client withdrawn")
	ErrDisabled  = errors.New("client disabled")
)

// CredentialHash 是凭据的 SHA-256 摘要；存储层不接收或计算凭据原文。
type CredentialHash [32]byte

// Client 是存储层元数据，不是公开 API 响应结构；不携带凭据摘要。
type Client struct {
	ID            int64
	GitHubID      int64
	ServerID      string
	DisplayName   string
	AvatarID      int64
	Disabled      bool
	RankingHidden bool
	Withdrawn     bool
	CreatedAt     time.Time
}

// Writer 是通过凭据查到的上报端及其席位，而不是调用方提供的资料。
type Writer struct {
	Client
	WriterID int64
}

type Repository struct {
	writer *sql.DB
	reader *sql.DB
}

func NewRepository(db *storage.DB) *Repository {
	return &Repository{writer: db.Writer, reader: db.Reader}
}

const clientFields = "c.id, c.github_id, c.server_id, c.display_name, c.avatar_id, c.disabled, c.created_at_ms, c.ranking_hidden, c.withdrawn"
const selectClient = "SELECT " + clientFields + " FROM clients c"

func (r *Repository) ByID(ctx context.Context, id int64) (Client, error) {
	return scanClient(r.reader.QueryRowContext(ctx, selectClient+" WHERE c.id = ?", id))
}

// ByCredentialHash 保留禁用和退出状态；这些席位仍然存在，凭据也不能改绑。
func (r *Repository) ByCredentialHash(ctx context.Context, hash CredentialHash) (Writer, error) {
	var writer Writer
	var err error
	writer.Client, err = scanClient(r.reader.QueryRowContext(ctx, `SELECT `+clientFields+`, w.id
		FROM client_writers w JOIN clients c ON c.id=w.client_id WHERE w.token_hash=?`, hash[:]), &writer.WriterID)
	return writer, err
}

func scanClient(row interface{ Scan(...any) error }, extra ...any) (Client, error) {
	var client Client
	var createdAt int64
	if err := row.Scan(append([]any{&client.ID, &client.GitHubID, &client.ServerID, &client.DisplayName, &client.AvatarID, &client.Disabled, &createdAt, &client.RankingHidden, &client.Withdrawn}, extra...)...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Client{}, ErrNotFound
		}
		return Client{}, fmt.Errorf("read client: %w", err)
	}
	client.CreatedAt = time.UnixMilli(createdAt).UTC()
	return client, nil
}

// Withdraw 先用读池核验凭据和退出状态，实际退出与上报事务共享 SQLite 写锁。
// 禁用、隐藏或已经退出均不妨碍退出。只有首次退出需要请求刷新。
func (r *Repository) Withdraw(ctx context.Context, hash CredentialHash) (bool, error) {
	var withdrawn bool
	err := r.reader.QueryRowContext(ctx, `SELECT c.withdrawn FROM client_writers w JOIN clients c ON c.id=w.client_id
		WHERE w.token_hash=?`, hash[:]).Scan(&withdrawn)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrNotFound
	}
	if err != nil || withdrawn {
		return false, err
	}
	tx, err := r.writer.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var id int64
	err = tx.QueryRowContext(ctx, `SELECT c.id,c.withdrawn FROM client_writers w JOIN clients c ON c.id=w.client_id
		WHERE w.token_hash=?`, hash[:]).Scan(&id, &withdrawn)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrNotFound
	}
	if err != nil || withdrawn {
		return false, err
	}
	if err := updateResult(tx.ExecContext(ctx, `UPDATE clients SET withdrawn=1 WHERE id=?`, id)); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// UpdateProfile 只改当前展示资料，不改变身份、凭据、创建时间或席位状态。
func (r *Repository) UpdateProfile(ctx context.Context, id int64, displayName string, avatarID int64) error {
	return updateResult(r.writer.ExecContext(ctx,
		"UPDATE clients SET display_name = ?, avatar_id = ? WHERE id = ?", displayName, avatarID, id))
}

// SetDisabled 是内部存储操作，不构成鉴权或管理接口。
func (r *Repository) SetDisabled(ctx context.Context, id int64, disabled bool) error {
	return updateResult(r.writer.ExecContext(ctx, "UPDATE clients SET disabled = ? WHERE id = ?", disabled, id))
}

func updateResult(result sql.Result, err error) error {
	if err != nil {
		return fmt.Errorf("update client: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read updated client count: %w", err)
	}
	if count == 0 {
		return ErrNotFound
	}
	return nil
}
