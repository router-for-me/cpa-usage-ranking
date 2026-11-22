package reports

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"

	"cpa-usage-ranking/internal/clients"
	"cpa-usage-ranking/internal/models"
)

type RevisionConflict struct{ Current int64 }

func (e *RevisionConflict) Error() string { return "report revision conflict" }

// Store replaces one client/day snapshot atomically using the single writer.
// The clock may be injected by tests; it must be safe for concurrent use.
type Store struct {
	writer   *sql.DB
	registry *models.Registry
	now      func() time.Time
}

func NewStore(writer *sql.DB, registry *models.Registry, now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{writer: writer, registry: registry, now: now}
}

// Apply confirms only after commit, or after reading an identical committed
// version. Watermark advancement is intentionally a separate committed write.
func (s *Store) Apply(ctx context.Context, writerID int64, day string, prepared Prepared) (int64, error) {
	if !prepared.valid || writerID <= 0 {
		return 0, ErrInvalid
	}
	var revision int64
	err := withReportWriter(ctx, s.writer, func(conn *sql.Conn) error {
		if err := advanceWindow(ctx, s.now(), conn.ExecContext); err != nil {
			return fmt.Errorf("advance report window: %w", err)
		}
		var err error
		revision, err = s.applyOnce(ctx, conn, writerID, day, prepared)
		return err
	})
	return revision, err
}

func (s *Store) applyOnce(ctx context.Context, conn *sql.Conn, writerID int64, day string, prepared Prepared) (int64, error) {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin report: %w", err)
	}
	defer tx.Rollback()
	// 写锁内重新查席位状态；退出之后连旧版本的幂等确认也不再允许。
	var clientID int64
	var disabled, withdrawn bool
	err = tx.QueryRowContext(ctx, `SELECT c.id,c.disabled,c.withdrawn FROM client_writers w JOIN clients c ON c.id=w.client_id WHERE w.id=?`, writerID).
		Scan(&clientID, &disabled, &withdrawn)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, clients.ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("read client state: %w", err)
	}
	if withdrawn {
		return 0, clients.ErrWithdrawn
	}
	if disabled {
		return 0, clients.ErrDisabled
	}
	if err := CheckWindow(ctx, tx, day, s.now()); err != nil {
		return 0, err
	}

	var current Version
	var digest []byte
	err = tx.QueryRowContext(ctx, "SELECT revision,digest FROM daily_writer_versions WHERE day=? AND writer_id=?", day, writerID).Scan(&current.Revision, &digest)
	exists := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("read report version: %w", err)
	}
	if exists {
		if len(digest) != len(current.Digest) {
			return 0, errors.New("invalid stored report digest")
		}
		copy(current.Digest[:], digest)
	}
	var previous *Version
	if exists {
		previous = &current
	}
	switch DecideVersion(previous, Version{Revision: prepared.revision, Digest: prepared.digest}) {
	case Conflict:
		if err := CheckWindow(ctx, tx, day, s.now()); err != nil {
			return 0, err
		}
		return 0, &RevisionConflict{Current: current.Revision}
	case Confirm:
		if err := CheckWindow(ctx, tx, day, s.now()); err != nil {
			return 0, err
		}
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		return current.Revision, nil
	}

	// Matching is intentionally after the decision: an old retry must not be
	// invalidated by current catalog changes or new aggregation overflow.
	rows, err := prepared.Aggregate(s.registry)
	if err != nil {
		return 0, err
	}
	old, err := loadStats(ctx, tx, day, clientID)
	if err != nil {
		return 0, err
	}
	// 修订号只在同一上报端之间比较，但整日数据始终按席位替换。
	if _, err := tx.ExecContext(ctx, `INSERT INTO daily_reports(day,client_id,received_at_ms) VALUES(?,?,?)
		ON CONFLICT(day,client_id) DO UPDATE SET received_at_ms=excluded.received_at_ms`, day, clientID, s.now().UnixMilli()); err != nil {
		return 0, fmt.Errorf("write daily report: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO daily_writer_versions(day,writer_id,client_id,revision,digest) VALUES(?,?,?,?,?)
		ON CONFLICT(day,writer_id) DO UPDATE SET revision=excluded.revision,digest=excluded.digest`, day, writerID, clientID, prepared.revision, prepared.digest[:]); err != nil {
		return 0, fmt.Errorf("write report version: %w", err)
	}
	for _, key := range sortedKeys(old) {
		if _, ok := rows[key]; !ok {
			if _, err := tx.ExecContext(ctx, `DELETE FROM daily_stats WHERE day=? AND client_id=? AND model_id=?`, day, clientID, key.ModelID); err != nil {
				return 0, fmt.Errorf("delete report row: %w", err)
			}
		}
	}
	for _, key := range sortedKeys(rows) {
		metric := rows[key]
		if before, ok := old[key]; ok && before == metric {
			continue
		}
		if err := writeStats(ctx, tx, day, clientID, key, metric); err != nil {
			return 0, err
		}
	}
	if err := CheckWindow(ctx, tx, day, s.now()); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit report: %w", err)
	}
	return prepared.revision, nil
}

func sortedKeys(rows map[Key]Metrics) []Key {
	keys := make([]Key, 0, len(rows))
	for key := range rows {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].ModelID < keys[j].ModelID })
	return keys
}

func loadStats(ctx context.Context, tx *sql.Tx, day string, clientID int64) (map[Key]Metrics, error) {
	result, err := tx.QueryContext(ctx, `SELECT model_id,total_tokens,input_tokens,output_tokens,cache_read_tokens,cache_write_tokens,reasoning_tokens,request_count,success_count,ttft_sum_ms,ttft_samples,latency_sum_ms,latency_samples FROM daily_stats WHERE day=? AND client_id=?`, day, clientID)
	if err != nil {
		return nil, fmt.Errorf("read report rows: %w", err)
	}
	defer result.Close()
	rows := make(map[Key]Metrics)
	for result.Next() {
		var k Key
		var m Metrics
		if err := result.Scan(&k.ModelID, &m.TotalTokens, &m.InputTokens, &m.OutputTokens, &m.CacheReadTokens, &m.CacheWriteTokens, &m.ReasoningTokens, &m.RequestCount, &m.SuccessCount, &m.TTFTSumMS, &m.TTFTSamples, &m.LatencySumMS, &m.LatencySamples); err != nil {
			return nil, fmt.Errorf("scan report row: %w", err)
		}
		rows[k] = m
	}
	if err := result.Err(); err != nil {
		return nil, fmt.Errorf("iterate report rows: %w", err)
	}
	return rows, nil
}

func writeStats(ctx context.Context, tx *sql.Tx, day string, clientID int64, k Key, m Metrics) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO daily_stats(day,client_id,model_id,total_tokens,input_tokens,output_tokens,cache_read_tokens,cache_write_tokens,reasoning_tokens,request_count,success_count,ttft_sum_ms,ttft_samples,latency_sum_ms,latency_samples)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(day,client_id,model_id) DO UPDATE SET
		total_tokens=excluded.total_tokens,input_tokens=excluded.input_tokens,output_tokens=excluded.output_tokens,
		cache_read_tokens=excluded.cache_read_tokens,cache_write_tokens=excluded.cache_write_tokens,reasoning_tokens=excluded.reasoning_tokens,
		request_count=excluded.request_count,success_count=excluded.success_count,ttft_sum_ms=excluded.ttft_sum_ms,ttft_samples=excluded.ttft_samples,
		latency_sum_ms=excluded.latency_sum_ms,latency_samples=excluded.latency_samples`,
		day, clientID, k.ModelID, m.TotalTokens, m.InputTokens, m.OutputTokens, m.CacheReadTokens, m.CacheWriteTokens,
		m.ReasoningTokens, m.RequestCount, m.SuccessCount, m.TTFTSumMS, m.TTFTSamples, m.LatencySumMS, m.LatencySamples)
	if err != nil {
		return fmt.Errorf("write report row: %w", err)
	}
	return nil
}
