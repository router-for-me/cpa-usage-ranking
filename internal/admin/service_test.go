package admin

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func testService(t *testing.T) *Service {
	t.Helper()
	s, err := New(Owner{Username: "owner", Password: "isolated-test-password"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestOwnerSessions(t *testing.T) {
	s := testService(t)
	now := time.Date(2026, 11, 16, 1, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	for _, input := range [][2]string{{"owner", "wrong"}, {"missing", "isolated-test-password"}} {
		if _, _, err := s.Login(context.Background(), input[0], input[1]); !errors.Is(err, ErrCredentials) {
			t.Fatalf("login: %v", err)
		}
	}
	token, session, err := s.Login(context.Background(), "owner", "isolated-test-password")
	if err != nil || !session.Identity.Owner || session.Identity.ID != "owner" || session.ExpiresAt.Sub(now) != SessionTTL {
		t.Fatalf("session: %+v %v", session.Identity, err)
	}
	if !ValidCSRF(session, session.CSRF) || ValidCSRF(session, "") || ValidCSRF(session, token) {
		t.Fatal("csrf boundary")
	}
	if _, err := s.Lookup(context.Background(), "forged"); !errors.Is(err, ErrSession) {
		t.Fatal(err)
	}
	now = now.Add(SessionTTL)
	if _, err := s.Lookup(context.Background(), token); !errors.Is(err, ErrSession) {
		t.Fatal("expired session accepted")
	}
	token, _, err = s.Login(context.Background(), "owner", "isolated-test-password")
	if err != nil {
		t.Fatal(err)
	}
	s.Logout(token)
	if _, err := s.Lookup(context.Background(), token); !errors.Is(err, ErrSession) {
		t.Fatal("logout did not revoke")
	}
	restarted := testService(t)
	if _, err := restarted.Lookup(context.Background(), token); !errors.Is(err, ErrSession) {
		t.Fatal("restart retained session")
	}
}

func TestBoundedConcurrentSessions(t *testing.T) {
	s := testService(t)
	base := time.Date(2026, 11, 16, 1, 0, 0, 0, time.UTC)
	now := base
	s.now = func() time.Time { return now }
	var firstToken, lastToken string
	for i := range maxSessions {
		now = base.Add(time.Duration(i) * time.Millisecond)
		tok, _, err := s.createSession(s.owner)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			firstToken = tok
		}
		lastToken = tok
	}
	// 满额时创建新会话应淘汰最早到期的旧会话，而非阻塞管理员登录。
	now = base.Add(time.Duration(maxSessions) * time.Millisecond)
	newestToken, _, err := s.createSession(s.owner)
	if err != nil {
		t.Fatalf("eviction at capacity failed: %v", err)
	}
	if len(s.sessions) != maxSessions {
		t.Fatalf("session map exceeded cap: %d", len(s.sessions))
	}
	if _, err := s.Lookup(context.Background(), firstToken); !errors.Is(err, ErrSession) {
		t.Fatal("earliest-expiring session was not evicted")
	}
	if _, err := s.Lookup(context.Background(), lastToken); err != nil {
		t.Fatalf("newer session was evicted: %v", err)
	}
	if _, err := s.Lookup(context.Background(), newestToken); err != nil {
		t.Fatalf("newest session missing: %v", err)
	}
	s.now = func() time.Time { return base.Add(SessionTTL + time.Minute) }
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			token, _, err := s.createSession(s.owner)
			if err != nil {
				t.Error(err)
				return
			}
			if _, err := s.Lookup(context.Background(), token); err != nil {
				t.Error(err)
			}
			s.Logout(token)
		})
	}
	wg.Wait()
}

func TestCredentialValidation(t *testing.T) {
	for _, input := range []Owner{{}, {Username: "a", Password: "long-enough-password"}, {Username: "owner", Password: "short"}, {Username: "bad name", Password: "long-enough-password"}} {
		if _, err := New(input, nil); err == nil {
			t.Fatal("invalid credentials accepted")
		}
	}
}
