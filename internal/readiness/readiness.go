// Package readiness 将已经初始化的依赖与首轮真实缓存组合为业务就绪条件。
package readiness

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"time"

	"cpa-usage-ranking/internal/avatars"
	"cpa-usage-ranking/internal/cache"
	"cpa-usage-ranking/internal/models"
	"cpa-usage-ranking/internal/storage"
)

const readerProbeTTL = time.Second

type probeFlight struct {
	done chan struct{}
	ok   bool
}

type Check struct {
	db      *storage.DB
	models  *models.Registry
	avatars *avatars.Catalog
	cache   *cache.Cache

	mu         sync.Mutex
	now        func() time.Time
	probeTTL   time.Duration
	probeValid bool
	probeUntil time.Time
	inFlight   *probeFlight
}

func New(db *storage.DB, registry *models.Registry, catalog *avatars.Catalog, snapshots *cache.Cache) *Check {
	return &Check{
		db:       db,
		models:   registry,
		avatars:  catalog,
		cache:    snapshots,
		now:      time.Now,
		probeTTL: readerProbeTTL,
	}
}

// Ready 不构建缓存，也不争抢唯一 SQLite 写连接；仅校验缓存可用与只读连接探针（1s 短时复用与并发合并）。
func (c *Check) Ready(ctx context.Context) bool {
	if c.db == nil || c.db.Reader == nil || c.db.Writer == nil || c.models == nil || c.avatars == nil || c.cache == nil {
		return false
	}
	if ctx.Err() != nil {
		return false
	}
	if _, err := c.cache.Current(); err != nil {
		return false
	}
	return c.probeReader(ctx)
}

func (c *Check) probeReader(ctx context.Context) bool {
	c.mu.Lock()
	if c.probeValid && c.now().Before(c.probeUntil) {
		c.mu.Unlock()
		return true
	}
	if flight := c.inFlight; flight != nil {
		c.mu.Unlock()
		select {
		case <-flight.done:
			return flight.ok && ctx.Err() == nil
		case <-ctx.Done():
			return false
		}
	}
	flight := &probeFlight{done: make(chan struct{})}
	c.inFlight = flight
	c.mu.Unlock()

	probeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer cancel()
	var row int
	err := c.db.Reader.QueryRowContext(probeCtx, `SELECT 1 FROM period_stats LIMIT 1`).Scan(&row)
	ok := err == nil || errors.Is(err, sql.ErrNoRows)

	c.mu.Lock()
	flight.ok = ok
	if ok {
		c.probeValid = true
		c.probeUntil = c.now().Add(c.probeTTL)
	} else {
		c.probeValid = false
	}
	c.inFlight = nil
	close(flight.done)
	c.mu.Unlock()

	return ok && ctx.Err() == nil
}
