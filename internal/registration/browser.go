package registration

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"errors"
	"time"
)

type Authorization struct {
	State, BrowserSession, Challenge string
	Expires                          time.Time
}

// Authorize 签发一次浏览器会话和 S256 PKCE；第二次打开不能覆盖原会话。
func (s *Service) Authorize(ctx context.Context, id string) (Authorization, error) {
	if !s.options.Enabled {
		return Authorization{}, ErrRegistrationClosed
	}
	if !validToken(id) {
		return Authorization{}, ErrInvalidRequest
	}
	var expires int64
	err := s.reader.QueryRowContext(ctx, `SELECT expires_at_ms FROM registration_requests
		WHERE id=? AND stage='pending' AND expires_at_ms>?`, id, s.options.Now().UnixMilli()).Scan(&expires)
	if errors.Is(err, sql.ErrNoRows) {
		return Authorization{}, ErrInvalidRequest
	}
	if err != nil {
		return Authorization{}, err
	}
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return Authorization{}, err
	}
	defer tx.Rollback()
	state, session, verifier := opaqueToken(), opaqueToken(), opaqueToken()
	browserHash := sha256.Sum256([]byte(session))
	err = tx.QueryRowContext(ctx, `UPDATE registration_requests
		SET stage='authorizing',state=?,browser_hash=?,verifier=? WHERE id=? AND stage='pending' AND expires_at_ms>?
		RETURNING expires_at_ms`, state, browserHash[:], verifier, id, s.options.Now().UnixMilli()).Scan(&expires)
	if errors.Is(err, sql.ErrNoRows) {
		return Authorization{}, ErrInvalidRequest
	}
	if err != nil {
		return Authorization{}, err
	}
	if err := tx.Commit(); err != nil {
		return Authorization{}, err
	}
	challenge := sha256.Sum256([]byte(verifier))
	return Authorization{State: state, BrowserSession: session, Challenge: base64.RawURLEncoding.EncodeToString(challenge[:]), Expires: time.UnixMilli(expires).UTC()}, nil
}

// Callback 验证并一次性消耗 state + 浏览器证明，再进行固定的 GitHub 换码和用户查询。
// 空 code 只用于 HTTP 层已确认的 access_denied 回调。
func (s *Service) Callback(ctx context.Context, github *GitHub, state, browserSession, code string) (string, error) {
	id, verifier, err := s.claimCallback(ctx, state, browserSession)
	if err != nil {
		return "", err
	}
	if code == "" {
		return s.finishCallback(ctx, id, "denied", nil, nil)
	}
	user, err := github.exchange(ctx, code, verifier)
	if err != nil {
		if _, writeErr := s.finishCallback(ctx, id, "failed", nil, nil); writeErr != nil {
			return "", writeErr
		}
		return "", err
	}
	return s.finishCallback(ctx, id, "approved", user.id, user.displayName)
}

func (s *Service) claimCallback(ctx context.Context, state, browserSession string) (string, string, error) {
	if !validToken(state) || !validToken(browserSession) {
		return "", "", ErrInvalidRequest
	}
	const selectAuthorization = `SELECT id,verifier,browser_hash FROM registration_requests
		WHERE stage='authorizing' AND state=? AND expires_at_ms>?`
	browserHash := sha256.Sum256([]byte(browserSession))
	var id, verifier string
	check := func(row *sql.Row) error {
		var savedBrowserHash []byte
		err := row.Scan(&id, &verifier, &savedBrowserHash)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrInvalidRequest
		}
		if err != nil {
			return err
		}
		if subtle.ConstantTimeCompare(savedBrowserHash, browserHash[:]) != 1 {
			return ErrInvalidRequest
		}
		return nil
	}
	if err := check(s.reader.QueryRowContext(ctx, selectAuthorization, state, s.options.Now().UnixMilli())); err != nil {
		return "", "", err
	}
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return "", "", err
	}
	defer tx.Rollback()
	if err := check(tx.QueryRowContext(ctx, selectAuthorization, state, s.options.Now().UnixMilli())); err != nil {
		return "", "", err
	}
	// 先提交消费，再放开写锁访问 GitHub；重复/并发回调不再发起外部请求。
	if _, err := tx.ExecContext(ctx, `UPDATE registration_requests SET stage='verifying',state=NULL,browser_hash=NULL,verifier=NULL WHERE id=?`, id); err != nil {
		return "", "", err
	}
	if err := tx.Commit(); err != nil {
		return "", "", err
	}
	return id, verifier, nil
}

func (s *Service) finishCallback(requestCtx context.Context, id, stage string, githubID, name any) (string, error) {
	// state 已消耗；即使浏览器断线，最终落库仍使用与 SQLite 一致的写超时。
	ctx, cancel := context.WithTimeout(context.WithoutCancel(requestCtx), 5*time.Second)
	defer cancel()
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var returnTo string
	err = tx.QueryRowContext(ctx, `UPDATE registration_requests SET stage=?,github_id=?,display_name=?
		WHERE id=? AND stage='verifying' AND expires_at_ms>? RETURNING return_to`, stage, githubID, name, id, s.options.Now().UnixMilli()).Scan(&returnTo)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrInvalidRequest
	}
	if err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return returnTo, nil
}
