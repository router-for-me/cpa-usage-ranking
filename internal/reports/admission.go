package reports

import (
	"bytes"
	"compress/gzip"
	"container/list"
	"errors"
	"fmt"
	"io"
	"math"
	"sync"
	"time"
)

var (
	ErrDecodeBusy   = errors.New("report decode capacity reached")
	ErrBodyTooLarge = errors.New("report body too large")
)

type IngestionLimits struct {
	MaxBodyBytes      int
	MaxItems          int
	ConcurrentDecodes int
	ClientInterval    time.Duration
	ClientBurst       int
	GlobalInterval    time.Duration
	GlobalBurst       int
	MaxClients        int
	StateTTL          time.Duration
}

func DefaultIngestionLimits() IngestionLimits {
	return IngestionLimits{MaxBodyBytes: 128 << 10, MaxItems: 1024, ConcurrentDecodes: 16,
		ClientInterval: 10 * time.Second, ClientBurst: 10, GlobalInterval: 5 * time.Millisecond,
		GlobalBurst: 400, MaxClients: 100000, StateTTL: 10 * time.Minute}
}

func (l IngestionLimits) Validate() error {
	if l.MaxBodyBytes <= 0 || int64(l.MaxBodyBytes) == math.MaxInt64 || l.MaxItems <= 0 || l.ConcurrentDecodes <= 0 || l.ClientInterval <= 0 || l.ClientBurst <= 0 || l.GlobalInterval <= 0 || l.GlobalBurst <= 0 || l.MaxClients <= 0 || l.StateTTL <= 0 {
		return fmt.Errorf("report ingestion limits must be positive and bounded")
	}
	return nil
}

type AdmissionError struct {
	Code       string
	RetryAfter time.Duration
}

func (e *AdmissionError) Error() string { return e.Code }
func (e *AdmissionError) RetryAfterSeconds() int64 {
	seconds := int64(e.RetryAfter / time.Second)
	if e.RetryAfter%time.Second != 0 {
		seconds++
	}
	if seconds < 1 {
		return 1
	}
	return seconds
}

type bucket struct {
	tokens float64
	last   time.Time
}

type clientBucket struct {
	id       int64
	bucket   bucket
	lastSeen time.Time
}

// Admission is independent from registration, health and static resources.
// State is bounded; only fully refilled, idle expired clients are removed.
type Admission struct {
	mu       sync.Mutex
	limits   IngestionLimits
	now      func() time.Time
	lastNow  time.Time
	global   bucket
	clients  map[int64]*list.Element
	expiry   list.List
	decoders chan struct{}
}

func NewAdmission(limits IngestionLimits, now func() time.Time) (*Admission, error) {
	if err := limits.Validate(); err != nil {
		return nil, err
	}
	if now == nil {
		now = time.Now
	}
	return &Admission{limits: limits, now: now, clients: make(map[int64]*list.Element), decoders: make(chan struct{}, limits.ConcurrentDecodes)}, nil
}

func refill(b *bucket, now time.Time, interval time.Duration, burst int) {
	if b.last.IsZero() {
		b.last = now
		b.tokens = float64(burst)
		return
	}
	if now.After(b.last) {
		b.tokens = math.Min(float64(burst), b.tokens+float64(now.Sub(b.last))/float64(interval))
		b.last = now
	}
}

func waitTokens(tokens, target float64, interval time.Duration) time.Duration {
	if tokens >= target {
		return 0
	}
	ns := math.Ceil((target - tokens) * float64(interval))
	if ns >= float64(math.MaxInt64) {
		return time.Duration(math.MaxInt64)
	}
	return time.Duration(ns)
}

// Admit checks per-client capacity and per-client rate before checking the
// global bucket, debiting both only when admission succeeds so a noisy client
// cannot exhaust global capacity for others. It runs after authentication and
// before reading the report body.
func (a *Admission) Admit(clientID int64) error {
	if clientID <= 0 {
		return ErrInvalid
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	if now.Before(a.lastNow) {
		now = a.lastNow
	} else {
		a.lastNow = now
	}
	refill(&a.global, now, a.limits.GlobalInterval, a.limits.GlobalBurst)
	for front := a.expiry.Front(); front != nil; front = a.expiry.Front() {
		client := front.Value.(*clientBucket)
		refill(&client.bucket, now, a.limits.ClientInterval, a.limits.ClientBurst)
		if now.Sub(client.lastSeen) < a.limits.StateTTL || client.bucket.tokens < float64(a.limits.ClientBurst) {
			break
		}
		delete(a.clients, client.id)
		a.expiry.Remove(front)
	}
	globalWait := waitTokens(a.global.tokens, 1, a.limits.GlobalInterval)
	limited := func(code string, delay time.Duration) error {
		if globalWait > delay {
			delay = globalWait
		}
		return &AdmissionError{Code: code, RetryAfter: delay}
	}
	element := a.clients[clientID]
	if element == nil {
		if len(a.clients) >= a.limits.MaxClients {
			oldest := a.expiry.Front().Value.(*clientBucket)
			idle := a.limits.StateTTL - now.Sub(oldest.lastSeen)
			full := waitTokens(oldest.bucket.tokens, float64(a.limits.ClientBurst), a.limits.ClientInterval)
			if full > idle {
				idle = full
			}
			return limited("report_capacity", idle)
		}
		if a.global.tokens < 1 {
			return limited("report_rate_limited", 0)
		}
		client := &clientBucket{id: clientID, lastSeen: now}
		refill(&client.bucket, now, a.limits.ClientInterval, a.limits.ClientBurst)
		element = a.expiry.PushBack(client)
		a.clients[clientID] = element
	} else {
		client := element.Value.(*clientBucket)
		refill(&client.bucket, now, a.limits.ClientInterval, a.limits.ClientBurst)
		client.lastSeen = now
		a.expiry.MoveToBack(element)
		clientWait := waitTokens(client.bucket.tokens, 1, a.limits.ClientInterval)
		if clientWait > 0 {
			return limited("report_rate_limited", clientWait)
		}
		if a.global.tokens < 1 {
			return limited("report_rate_limited", 0)
		}
	}
	client := element.Value.(*clientBucket)
	client.bucket.tokens--
	a.global.tokens--
	return nil
}

// Decode bounds concurrent body reads/decodes as well as bytes and raw items.
func (a *Admission) Decode(body io.Reader) (Report, error) {
	select {
	case a.decoders <- struct{}{}:
	default:
		return Report{}, ErrDecodeBusy
	}
	defer func() { <-a.decoders }()
	data, err := io.ReadAll(io.LimitReader(body, int64(a.limits.MaxBodyBytes)+1))
	if err != nil {
		return Report{}, err
	}
	if len(data) > a.limits.MaxBodyBytes {
		return Report{}, ErrBodyTooLarge
	}
	return DecodeLimited(data, a.limits.MaxItems)
}

// DecodeGzip 对 gzip 压缩请求体先后限制压缩流与解压后体积，防止压缩炸弹并受同一解码并发槽位约束。
func (a *Admission) DecodeGzip(body io.Reader) (Report, error) {
	select {
	case a.decoders <- struct{}{}:
	default:
		return Report{}, ErrDecodeBusy
	}
	defer func() { <-a.decoders }()
	compressed, err := io.ReadAll(io.LimitReader(body, int64(a.limits.MaxBodyBytes)+1))
	if err != nil {
		return Report{}, err
	}
	if len(compressed) > a.limits.MaxBodyBytes {
		return Report{}, ErrBodyTooLarge
	}
	gz, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return Report{}, ErrInvalid
	}
	defer gz.Close()
	data, err := io.ReadAll(io.LimitReader(gz, int64(a.limits.MaxBodyBytes)+1))
	if err != nil {
		return Report{}, ErrInvalid
	}
	if len(data) > a.limits.MaxBodyBytes {
		return Report{}, ErrBodyTooLarge
	}
	if err := gz.Close(); err != nil {
		return Report{}, ErrInvalid
	}
	return DecodeLimited(data, a.limits.MaxItems)
}
