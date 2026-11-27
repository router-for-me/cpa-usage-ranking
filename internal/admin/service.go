// Package admin implements the small, single-process administrator console.
package admin

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"errors"
	"regexp"
	"sync"
	"time"

	"cpa-usage-ranking/internal/clients"
	"cpa-usage-ranking/internal/storage"
	"golang.org/x/crypto/bcrypt"
)

const SessionTTL = 8 * time.Hour
const maxSessions = 1024

var (
	ErrCredentials = errors.New("invalid administrator credentials")
	ErrSession     = errors.New("invalid administrator session")
	ErrCapacity    = errors.New("administrator session capacity reached")
	ErrInput       = errors.New("invalid administrator input")
	ErrForbidden   = errors.New("administrator permission denied")
	ErrNotFound    = errors.New("administrator target not found")
	ErrConflict    = errors.New("administrator username unavailable")
	ErrBanned      = errors.New("unban client before restoring rankings")
)

var usernamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{2,63}$`)

// Owner is read once from the private configuration file, never stored in SQLite.
type Owner struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func ValidCredentials(username, password string) bool {
	return usernamePattern.MatchString(username) && len(password) >= 12 && len(password) <= 72
}

type Identity struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Owner    bool   `json:"owner"`
}

type Session struct {
	Identity  Identity  `json:"user"`
	CSRF      string    `json:"csrf_token"`
	ExpiresAt time.Time `json:"expires_at"`
}

type Service struct {
	clients   *clients.Repository
	reader    *sql.DB
	writer    *sql.DB
	owner     Identity
	ownerHash []byte
	mu        sync.Mutex
	sessions  map[string]Session
	now       func() time.Time
}

func New(owner Owner, db *storage.DB) (*Service, error) {
	if !ValidCredentials(owner.Username, owner.Password) {
		return nil, ErrInput
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(owner.Password), bcrypt.DefaultCost)
	if err != nil {
		return nil, errors.New("hash administrator password")
	}
	s := &Service{owner: Identity{ID: "owner", Username: owner.Username, Owner: true}, ownerHash: hash, sessions: make(map[string]Session), now: time.Now}
	if db != nil {
		s.reader, s.writer = db.Reader, db.Writer
		s.clients = clients.NewRepository(db)
		var count int
		if err := s.reader.QueryRow("SELECT count(*) FROM admin_accounts WHERE username = ?", owner.Username).Scan(&count); err != nil {
			return nil, errors.New("read administrator accounts")
		}
		if count != 0 {
			return nil, errors.New("configured owner username conflicts with an ordinary administrator")
		}
	}
	return s, nil
}

func (s *Service) Login(ctx context.Context, username, password string) (string, Session, error) {
	identity := s.owner
	hash := s.ownerHash
	found := username == s.owner.Username
	if !found && s.reader != nil {
		var id int64
		err := s.reader.QueryRowContext(ctx, "SELECT id, password_hash FROM admin_accounts WHERE username = ?", username).Scan(&id, &hash)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return "", Session{}, errors.New("read administrator account")
		}
		found = err == nil
		if found {
			identity = Identity{ID: accountID(id), Username: username}
		} else {
			hash = s.ownerHash
		}
	}
	// Always pay the password-hash cost, including unknown usernames.
	valid := bcrypt.CompareHashAndPassword(hash, []byte(password)) == nil
	if !valid || !found {
		return "", Session{}, ErrCredentials
	}
	return s.createSession(identity)
}

func randomToken() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", errors.New("generate administrator session")
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

func (s *Service) createSession(identity Identity) (string, Session, error) {
	token, err := randomToken()
	if err != nil {
		return "", Session{}, err
	}
	csrf, err := randomToken()
	if err != nil {
		return "", Session{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UTC()
	for key, session := range s.sessions {
		if !now.Before(session.ExpiresAt) {
			delete(s.sessions, key)
		}
	}
	if len(s.sessions) >= maxSessions {
		// 达到内存会话上限时淘汰最早到期的旧会话，避免正常管理员无法登录。
		var oldestKey string
		var oldestExp time.Time
		first := true
		for key, existing := range s.sessions {
			if first || existing.ExpiresAt.Before(oldestExp) || (existing.ExpiresAt.Equal(oldestExp) && key < oldestKey) {
				oldestKey = key
				oldestExp = existing.ExpiresAt
				first = false
			}
		}
		delete(s.sessions, oldestKey)
	}
	session := Session{Identity: identity, CSRF: csrf, ExpiresAt: now.Add(SessionTTL)}
	s.sessions[token] = session
	return token, session, nil
}

func (s *Service) Lookup(ctx context.Context, token string) (Session, error) {
	s.mu.Lock()
	session, ok := s.sessions[token]
	valid := ok && s.now().Before(session.ExpiresAt)
	if !valid {
		delete(s.sessions, token)
	}
	s.mu.Unlock()
	if !valid {
		return Session{}, ErrSession
	}
	if !session.Identity.Owner {
		if s.reader == nil {
			s.Logout(token)
			return Session{}, ErrSession
		}
		var exists int
		if err := s.reader.QueryRowContext(ctx, "SELECT 1 FROM admin_accounts WHERE id = ?", session.Identity.ID).Scan(&exists); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				s.Logout(token)
				return Session{}, ErrSession
			}
			return Session{}, err
		}
	}
	return session, nil
}

func (s *Service) Logout(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, token)
}

func ValidCSRF(session Session, token string) bool {
	return token != "" && subtle.ConstantTimeCompare([]byte(session.CSRF), []byte(token)) == 1
}
