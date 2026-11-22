// Package cache 顺序构建并原子发布完整的内部缓存轮次。
package cache

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"cpa-usage-ranking/internal/periods"
	"cpa-usage-ranking/internal/statistics"
)

const (
	DefaultRefreshInterval = 5 * time.Minute
	DefaultItemTimeout     = 30 * time.Second
	DefaultMaxBytes        = 16 << 20
	MaxItems               = 32
)

var (
	ErrUnavailable = errors.New("cache unavailable")
	ErrNoItems     = errors.New("no cache build items")
	ErrCapacity    = errors.New("cache capacity exceeded")
	ErrRunning     = errors.New("cache worker already running")
)

// Item 是编译时确定的构建项。读取统一由缓存负责，Encode 只接收已结束事务的候选数据。
type Item struct {
	ID, Kind       string
	ModelID        int64
	DayOffset      int  // 今日或昨日；相对日期在整轮开始时确定。
	CompletePeriod bool // 周/月榜读取上一完整周期。
	Public         bool
	Encode         func(context.Context, statistics.Snapshot, io.Writer) error
}

type Options struct {
	Interval, ItemTimeout time.Duration
	MaxBytes              int
	Now                   func() time.Time
}

func DefaultOptions() Options {
	return Options{Interval: DefaultRefreshInterval, ItemTimeout: DefaultItemTimeout, MaxBytes: DefaultMaxBytes, Now: time.Now}
}

// Entry 和 Snapshot 对外只暴露不可变的字符串与值，周期标识和内容一起发布。
type Entry struct{ Kind, Start, End, GeneratedAt, Content string }
type Snapshot struct{ entries map[string]Entry }

func (s Snapshot) Get(id string) (Entry, bool) { e, ok := s.entries[id]; return e, ok }
func (s Snapshot) Len() int                    { return len(s.entries) }

type Cache struct {
	reader           *statistics.Reader
	items            []Item
	opts             Options
	logger           *slog.Logger
	current          atomic.Pointer[Snapshot]
	running          atomic.Bool
	refreshRequested chan struct{}
	publicationMu    sync.Mutex
	refreshEpoch     uint64
}

func New(reader *statistics.Reader, items []Item, opts Options, logger *slog.Logger) (*Cache, error) {
	if reader == nil || opts.Interval <= 0 || opts.ItemTimeout <= 0 || opts.MaxBytes <= 0 || len(items) > MaxItems || logger == nil {
		return nil, errors.New("invalid cache options")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	seen := make(map[string]bool, len(items))
	for _, item := range items {
		if item.ID == "" || len(item.ID) > 128 || seen[item.ID] || item.Encode == nil || (item.DayOffset != 0 && item.DayOffset != -1) || (item.CompletePeriod && (item.DayOffset != 0 || (item.Kind != periods.Week && item.Kind != periods.Month))) {
			return nil, errors.New("invalid cache item")
		}
		seen[item.ID] = true
	}
	return &Cache{reader: reader, items: append([]Item(nil), items...), opts: opts, logger: logger, refreshRequested: make(chan struct{}, 1)}, nil
}

// Current 返回快照值副本；调用方覆盖自己的副本不会改写已发布快照。
func (c *Cache) Current() (Snapshot, error) {
	s := c.current.Load()
	if s == nil {
		return Snapshot{}, ErrUnavailable
	}
	return *s, nil
}

// RequestRefresh 合并已提交的资格变更，不在请求协程上构建榜单。
func (c *Cache) RequestRefresh() {
	c.publicationMu.Lock()
	defer c.publicationMu.Unlock()
	c.refreshEpoch++
	select {
	case c.refreshRequested <- struct{}{}:
	default:
	}
}

// Run 启动即尝试一次，之后由原计时器或合并信号触发；构建始终串行。
// 长轮次的旧定时 tick 被丢弃，管理变更信号则保留一次后续构建。
func (c *Cache) Run(ctx context.Context) error {
	if !c.running.CompareAndSwap(false, true) {
		return ErrRunning
	}
	defer c.running.Store(false)
	ticker := time.NewTicker(c.opts.Interval)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := c.refresh(ctx); err != nil && ctx.Err() == nil {
			c.logger.Error("cache refresh failed", "error", err.Error())
		}
		select {
		case <-ticker.C:
		default:
		} // 丢弃构建期间合并的一次旧 tick。
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		case <-c.refreshRequested:
		}
	}
}
func (c *Cache) refresh(ctx context.Context) error {
	if len(c.items) == 0 {
		return ErrNoItems
	}
	c.publicationMu.Lock()
	epoch := c.refreshEpoch
	c.publicationMu.Unlock()
	when := c.opts.Now().UTC()
	next := &Snapshot{entries: make(map[string]Entry, len(c.items))}
	remaining := c.opts.MaxBytes
	for _, item := range c.items {
		entry, err := c.build(ctx, item, when, remaining)
		if err != nil {
			return fmt.Errorf("build %s: %w", item.ID, err)
		}
		remaining -= len(entry.Content)
		next.entries[item.ID] = entry
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	c.publicationMu.Lock()
	defer c.publicationMu.Unlock()
	// 各周期分别读取 SQLite；期间资格变更会留下刷新信号，不发布可能含旧资格的轮次。
	if epoch == c.refreshEpoch {
		c.current.Store(next)
	}
	return nil
}
func (c *Cache) build(ctx context.Context, item Item, when time.Time, max int) (Entry, error) {
	ctx, cancel := context.WithTimeout(ctx, c.opts.ItemTimeout)
	defer cancel()
	day := when.AddDate(0, 0, item.DayOffset).Format("2006-01-02")
	if item.CompletePeriod {
		day = periods.LeaderboardDay(item.Kind, when)
	}
	s, err := c.reader.Read(ctx, statistics.Selection{Kind: item.Kind, Day: day, ModelID: item.ModelID, Public: item.Public})
	if err != nil {
		return Entry{}, err
	}
	// Read 已提交只读事务，编码或派生不能拖长 SQLite 读快照。
	buf := &boundedBuffer{limit: max}
	if err := item.Encode(ctx, s, buf); err != nil {
		return Entry{}, err
	}
	if buf.exceeded {
		return Entry{}, ErrCapacity
	}
	if err := ctx.Err(); err != nil {
		return Entry{}, err
	}
	return Entry{Kind: s.Kind, Start: s.Start, End: s.End, GeneratedAt: when.Format(time.RFC3339Nano), Content: buf.buf.String()}, nil
}

type boundedBuffer struct {
	buf      bytes.Buffer
	limit    int
	exceeded bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.buf.Len() {
		b.exceeded = true
		return 0, ErrCapacity
	}
	return b.buf.Write(p)
}
