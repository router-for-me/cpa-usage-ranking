// Package registration 管理有界的 GitHub 注册事务与凭据绑定。
package registration

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"cpa-usage-ranking/internal/avatars"
	"cpa-usage-ranking/internal/clients"
	"cpa-usage-ranking/internal/storage"
)

var (
	ErrInvalidRequest     = errors.New("invalid registration request")
	ErrInvalidAvatar      = errors.New("invalid avatar selection")
	ErrWrongCredential    = errors.New("registration credential mismatch")
	ErrRegistrationClosed = errors.New("new registration is closed")
	ErrExpired            = errors.New("registration request expired")
	ErrPending            = errors.New("registration authorization pending")
	ErrDenied             = errors.New("GitHub authorization denied")
	ErrGitHub             = errors.New("GitHub authorization failed")
)

const PendingRetryAfter = 2 * time.Second

var (
	serverIDPattern      = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	compactServerIDRegex = regexp.MustCompile(`(?i)^[0-9a-f]{32}$`)
)

func normalizeServerID(raw string) (string, bool) {
	trimmed := strings.TrimSpace(raw)
	var normalized string
	switch {
	case serverIDPattern.MatchString(trimmed):
		normalized = strings.ToLower(trimmed)
	case compactServerIDRegex.MatchString(trimmed):
		lower := strings.ToLower(trimmed)
		normalized = lower[0:8] + "-" + lower[8:12] + "-" + lower[12:16] + "-" + lower[16:20] + "-" + lower[20:32]
	default:
		return "", false
	}
	if normalized == "00000000-0000-0000-0000-000000000000" {
		return "", false
	}
	return normalized, true
}

type Result struct {
	Client  clients.Client
	Created bool
}

type Options struct {
	Enabled        bool
	TTL            time.Duration
	Capacity       int
	Now            func() time.Time
	RequestRefresh func()
}

func DefaultOptions() Options {
	return Options{Enabled: true, TTL: 10 * time.Minute, Capacity: 2000}
}

type Service struct {
	reader           *sql.DB
	writer           *sql.DB
	catalog          *avatars.Catalog
	options          Options
	authorizeLimiter *RequestLimiter
}

func NewService(db *storage.DB, catalog *avatars.Catalog, options Options) (*Service, error) {
	if db == nil || db.Reader == nil || db.Writer == nil || catalog == nil || catalog.DefaultID() <= 0 || options.TTL < time.Millisecond || options.Capacity <= 0 {
		return nil, fmt.Errorf("invalid registration options")
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	authorizeLimiter, err := NewRequestLimiter(RequestLimits{
		SourceRequests: 6,
		GlobalRequests: 3000,
		Window:         time.Minute,
		MaxSources:     max(options.Capacity, 2000),
	}, options.Now)
	if err != nil {
		return nil, err
	}
	return &Service{reader: db.Reader, writer: db.Writer, catalog: catalog, options: options, authorizeLimiter: authorizeLimiter}, nil
}

func opaqueToken() string {
	var raw [32]byte
	rand.Read(raw[:])
	return base64.RawURLEncoding.EncodeToString(raw[:])
}

func validToken(token string) bool {
	if len(token) != 43 {
		return false
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(token)
	return err == nil && len(decoded) == 32
}

func httpURL(raw string) (*url.URL, bool) {
	// 浏览器和 net/url 对反斜杠与控制字符的解释不同，不接受这类歧义地址。
	if strings.ContainsAny(raw, " \\") || strings.IndexFunc(raw, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
		return nil, false
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil {
		return nil, false
	}
	if port := u.Port(); port != "" {
		if _, err := strconv.ParseUint(port, 10, 16); err != nil {
			return nil, false
		}
	}
	if strings.HasPrefix(u.Host, "[") {
		if addr, err := netip.ParseAddr(u.Hostname()); err != nil || !addr.Is6() {
			return nil, false
		}
	} else if strings.ContainsRune(u.Hostname(), ':') {
		return nil, false
	}
	return u, true
}

func (s *Service) isBoundCredential(ctx context.Context, queryRow func(context.Context, string, ...any) *sql.Row, tokenHash []byte) (bool, error) {
	var exists int
	err := queryRow(ctx, `SELECT 1 FROM client_writers WHERE token_hash=?`, tokenHash).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// Start 固定 server_id、可选返回地址、显式所选头像和凭据摘要，不创建席位或上报端。
func (s *Service) Start(ctx context.Context, hash clients.CredentialHash, serverID, returnTo string, avatarID *int64) (string, error) {
	normalizedServerID, ok := normalizeServerID(serverID)
	if !ok {
		return "", ErrInvalidRequest
	}
	if strings.TrimSpace(returnTo) == "" {
		returnTo = ""
	} else if _, valid := httpURL(returnTo); !valid {
		return "", ErrInvalidRequest
	}
	if !s.options.Enabled {
		// 关闭新注册时放行已绑定老凭据，支持老设备恢复或退榜重回。
		bound, err := s.isBoundCredential(ctx, s.reader.QueryRowContext, hash[:])
		if err != nil {
			return "", err
		}
		if !bound {
			return "", ErrRegistrationClosed
		}
	}
	var storedAvatar any
	if avatarID != nil {
		if _, ok := s.catalog.Lookup(*avatarID); !ok {
			return "", ErrInvalidAvatar
		}
		storedAvatar = *avatarID
	}
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if !s.options.Enabled {
		bound, err := s.isBoundCredential(ctx, tx.QueryRowContext, hash[:])
		if err != nil {
			return "", err
		}
		if !bound {
			return "", ErrRegistrationClosed
		}
	}
	now := s.options.Now()
	if _, err := tx.ExecContext(ctx, `DELETE FROM registration_requests WHERE expires_at_ms<=?`, now.UnixMilli()); err != nil {
		return "", err
	}
	var count int
	var firstExpiry int64
	if err := tx.QueryRowContext(ctx, `SELECT count(*),coalesce(min(expires_at_ms),0) FROM registration_requests`).Scan(&count, &firstExpiry); err != nil {
		return "", err
	}
	if count >= s.options.Capacity {
		// 表满时优先清理已终结的拒绝或失败事务，避免异常记录占用活跃注册容量。
		if _, err := tx.ExecContext(ctx, `DELETE FROM registration_requests WHERE stage IN ('denied','failed')`); err != nil {
			return "", err
		}
		if err := tx.QueryRowContext(ctx, `SELECT count(*),coalesce(min(expires_at_ms),0) FROM registration_requests`).Scan(&count, &firstExpiry); err != nil {
			return "", err
		}
	}
	if count >= s.options.Capacity {
		// 若仍满，按最早到期顺序淘汰必要的已完成事务，仅活跃未完成请求打满容量时才拒绝。
		evict := count - s.options.Capacity + 1
		if _, err := tx.ExecContext(ctx, `DELETE FROM registration_requests WHERE id IN (
			SELECT id FROM registration_requests WHERE stage='completed' ORDER BY expires_at_ms ASC, id ASC LIMIT ?
		)`, evict); err != nil {
			return "", err
		}
		if err := tx.QueryRowContext(ctx, `SELECT count(*),coalesce(min(expires_at_ms),0) FROM registration_requests`).Scan(&count, &firstExpiry); err != nil {
			return "", err
		}
	}
	if count >= s.options.Capacity {
		return "", &LimitError{Code: "registration_capacity", RetryAfter: time.UnixMilli(firstExpiry).Sub(now)}
	}
	id := opaqueToken()
	if _, err := tx.ExecContext(ctx, `INSERT INTO registration_requests(id,token_hash,server_id,return_to,avatar_id,expires_at_ms,stage)
		VALUES(?,?,?,?,?,?,'pending')`, id, hash[:], normalizedServerID, returnTo, storedAvatar, now.Add(s.options.TTL).UnixMilli()); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return id, nil
}

func (s *Service) PruneExpired(ctx context.Context) error {
	_, err := s.writer.ExecContext(ctx, `DELETE FROM registration_requests WHERE expires_at_ms<=?`, s.options.Now().UnixMilli())
	return err
}

// Complete 先用读池检查请求，仅 approved 进入 IMMEDIATE 写事务复查并绑定上报端。
func (s *Service) Complete(ctx context.Context, hash clients.CredentialHash, id string) (Result, error) {
	if !validToken(id) {
		return Result{}, ErrInvalidRequest
	}
	var now time.Time
	var expiry int64
	var stage, serverID string
	var avatarID sql.NullInt64
	var githubID sql.NullInt64
	var name sql.NullString
	checkRequest := func(queryRow func(context.Context, string, ...any) *sql.Row) (Result, error) {
		var savedHash []byte
		var clientID sql.NullInt64
		err := queryRow(ctx, `SELECT token_hash,expires_at_ms,stage,server_id,avatar_id,github_id,display_name,client_id
			FROM registration_requests WHERE id=?`, id).Scan(&savedHash, &expiry, &stage, &serverID, &avatarID, &githubID, &name, &clientID)
		if errors.Is(err, sql.ErrNoRows) {
			// 过期清理后的编号与未签发编号不可区分，也没有额外的找回入口。
			return Result{}, ErrExpired
		}
		if err != nil {
			return Result{}, err
		}
		if subtle.ConstantTimeCompare(savedHash, hash[:]) != 1 {
			return Result{}, ErrWrongCredential
		}
		now = s.options.Now()
		if expiry <= now.UnixMilli() {
			return Result{}, ErrExpired
		}
		switch stage {
		case "denied":
			return Result{}, ErrDenied
		case "failed":
			return Result{}, ErrGitHub
		case "completed":
			seat, err := readSeat(queryRow(ctx, seatFields+` WHERE id=?`, clientID.Int64))
			if err != nil {
				return Result{}, err
			}
			if seat.Withdrawn {
				return Result{}, clients.ErrWithdrawn
			}
			if seat.Disabled {
				return Result{}, clients.ErrDisabled
			}
			if expiry <= s.options.Now().UnixMilli() {
				return Result{}, ErrExpired
			}
			return Result{Client: seat}, nil
		case "approved":
			return Result{}, nil
		default:
			return Result{}, ErrPending
		}
	}
	if result, err := checkRequest(s.reader.QueryRowContext); err != nil || stage != "approved" {
		return result, err
	}
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return Result{}, err
	}
	defer tx.Rollback()
	if result, err := checkRequest(tx.QueryRowContext); err != nil || stage != "approved" {
		return result, err
	}

	var boundID int64
	err = tx.QueryRowContext(ctx, `SELECT client_id FROM client_writers WHERE token_hash=?`, hash[:]).Scan(&boundID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Result{}, err
	}
	seat, err := readSeat(tx.QueryRowContext(ctx, seatFields+` WHERE github_id=? AND server_id=?`, githubID.Int64, serverID))
	newSeat := errors.Is(err, sql.ErrNoRows)
	if boundID != 0 && (newSeat || (seat.ID != 0 && seat.ID != boundID)) {
		return Result{}, ErrWrongCredential
	}
	if err != nil && !newSeat {
		return Result{}, err
	}
	if seat.Disabled {
		return Result{}, clients.ErrDisabled
	}
	if boundID == 0 && !s.options.Enabled {
		return Result{}, ErrRegistrationClosed
	}
	rejoined := seat.Withdrawn
	profileChanged := false
	if newSeat {
		seatAvatarID := s.catalog.DefaultID()
		if avatarID.Valid {
			seatAvatarID = avatarID.Int64
		}
		seat = clients.Client{DisplayName: name.String, AvatarID: seatAvatarID, CreatedAt: time.UnixMilli(now.UnixMilli()).UTC()}
		err = tx.QueryRowContext(ctx, `INSERT INTO clients(github_id,server_id,display_name,avatar_id,created_at_ms)
			VALUES(?,?,?,?,?) RETURNING id`, githubID.Int64, serverID, seat.DisplayName, seat.AvatarID, now.UnixMilli()).Scan(&seat.ID)
		if err != nil {
			return Result{}, err
		}
	} else {
		// 已有席位授权完成时自动同步最新昵称；仅显式提交 avatar_id 时更新头像。
		nextName := name.String
		nextAvatarID := seat.AvatarID
		if avatarID.Valid {
			nextAvatarID = avatarID.Int64
		}
		profileChanged = nextName != seat.DisplayName || nextAvatarID != seat.AvatarID
		if rejoined || profileChanged {
			if _, err := tx.ExecContext(ctx, `UPDATE clients SET withdrawn=0, display_name=?, avatar_id=? WHERE id=?`, nextName, nextAvatarID, seat.ID); err != nil {
				return Result{}, err
			}
			seat.Withdrawn = false
			seat.DisplayName = nextName
			seat.AvatarID = nextAvatarID
		}
	}
	if boundID == 0 {
		if _, err := tx.ExecContext(ctx, `INSERT INTO client_writers(client_id,token_hash) VALUES(?,?)`, seat.ID, hash[:]); err != nil {
			return Result{}, err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE registration_requests SET stage='completed',client_id=?,github_id=NULL,display_name=NULL WHERE id=?`, seat.ID, id); err != nil {
		return Result{}, err
	}
	if expiry <= s.options.Now().UnixMilli() {
		return Result{}, ErrExpired
	}
	if err := tx.Commit(); err != nil {
		return Result{}, err
	}
	if (rejoined || profileChanged) && s.options.RequestRefresh != nil {
		s.options.RequestRefresh()
	}
	return Result{Client: seat, Created: newSeat}, nil
}

const seatFields = `SELECT id,display_name,avatar_id,disabled,withdrawn,ranking_hidden,created_at_ms FROM clients`

func readSeat(row *sql.Row) (clients.Client, error) {
	var c clients.Client
	var created int64
	if err := row.Scan(&c.ID, &c.DisplayName, &c.AvatarID, &c.Disabled, &c.Withdrawn, &c.RankingHidden, &created); err != nil {
		return clients.Client{}, err
	}
	c.CreatedAt = time.UnixMilli(created).UTC()
	return c, nil
}
