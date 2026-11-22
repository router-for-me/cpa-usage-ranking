package registration

import (
	"container/list"
	"fmt"
	"sync"
	"time"
)

// LimitError is a temporary admission rejection. It never contains request data.
type LimitError struct {
	Code       string
	RetryAfter time.Duration
}

func (e *LimitError) Error() string { return e.Code }
func (e *LimitError) RetryAfterSeconds() int64 {
	seconds := int64(e.RetryAfter / time.Second)
	if e.RetryAfter%time.Second != 0 {
		seconds++
	}
	if seconds < 1 {
		return 1
	}
	return seconds
}

type RequestLimits struct {
	SourceRequests int
	GlobalRequests int
	Window         time.Duration
	MaxSources     int
}

func DefaultRequestLimits() RequestLimits {
	return RequestLimits{SourceRequests: 20, GlobalRequests: 200, Window: time.Minute, MaxSources: 10000}
}

func (l RequestLimits) Validate() error {
	if l.SourceRequests <= 0 || l.GlobalRequests <= 0 || l.Window <= 0 || l.MaxSources <= 0 {
		return fmt.Errorf("registration request limits must be positive")
	}
	return nil
}

type requestWindow struct {
	source string
	until  time.Time
	count  int
}

type RequestLimiter struct {
	mu      sync.Mutex
	limits  RequestLimits
	now     func() time.Time
	global  requestWindow
	sources map[string]*requestWindow
	expiry  list.List
}

// NewRequestLimiter uses fixed windows starting at the first attempt. A nil
// clock uses time.Now; tests may inject nondecreasing time without sleeping.
func NewRequestLimiter(limits RequestLimits, now func() time.Time) (*RequestLimiter, error) {
	if err := limits.Validate(); err != nil {
		return nil, err
	}
	if now == nil {
		now = time.Now
	}
	return &RequestLimiter{limits: limits, now: now, sources: make(map[string]*requestWindow)}, nil
}

// Admit runs before credential/body validation on every start attempt.
// Complete does not consume a new start slot.
func (l *RequestLimiter) Admit(source string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	// 所有来源窗口长度相同，按建立顺序到期；不刷新有效记录、不逐出活跃来源。
	for front := l.expiry.Front(); front != nil; front = l.expiry.Front() {
		window := front.Value.(*requestWindow)
		if now.Before(window.until) {
			break
		}
		delete(l.sources, window.source)
		l.expiry.Remove(front)
	}
	if !now.Before(l.global.until) {
		l.global = requestWindow{until: now.Add(l.limits.Window)}
	}
	window := l.sources[source]
	if l.global.count >= l.limits.GlobalRequests {
		until := l.global.until
		if window != nil && window.count >= l.limits.SourceRequests && window.until.After(until) {
			until = window.until
		}
		if window == nil && len(l.sources) >= l.limits.MaxSources {
			capacityExpiry := l.expiry.Front().Value.(*requestWindow).until
			if capacityExpiry.After(until) {
				until = capacityExpiry
			}
		}
		return &LimitError{Code: "registration_rate_limited", RetryAfter: until.Sub(now)}
	}
	if window == nil {
		if len(l.sources) >= l.limits.MaxSources {
			return &LimitError{Code: "registration_capacity", RetryAfter: l.expiry.Front().Value.(*requestWindow).until.Sub(now)}
		}
		window = &requestWindow{source: source, until: now.Add(l.limits.Window)}
		l.sources[source] = window
		l.expiry.PushBack(window)
	}
	if window.count >= l.limits.SourceRequests {
		return &LimitError{Code: "registration_rate_limited", RetryAfter: window.until.Sub(now)}
	}
	l.global.count++
	window.count++
	return nil
}
