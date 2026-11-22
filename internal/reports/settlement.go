package reports

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"cpa-usage-ranking/internal/periods"
)

var ErrSettlementOverflow = errors.New("settlement metrics overflow")

// Finalize 合并一个已封窗的完整客户端日快照；源不存在或尚未封窗时不做任何操作。
// 调用方应通过单 writer 串行调度；返回前事务已提交或回滚。
func Finalize(ctx context.Context, writer *sql.DB, day string, clientID int64) (bool, error) {
	bounds, err := periods.ForDay(day)
	if err != nil || clientID <= 0 {
		return false, ErrInvalid
	}
	var finalized bool
	err = withReportWriter(ctx, writer, func(conn *sql.Conn) error {
		var err error
		finalized, err = finalizeOnce(ctx, conn, bounds, clientID)
		return err
	})
	return finalized, err
}

func finalizeOnce(ctx context.Context, conn *sql.Conn, bounds periods.Boundaries, clientID int64) (bool, error) {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin settlement: %w", err)
	}
	defer tx.Rollback()
	// 封窗水位是唯一权限依据，时钟回退时不能重新开放已封闭的日期。
	var exists int
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM daily_reports WHERE day=? AND client_id=?
		AND day < (SELECT closed_before_day FROM report_window WHERE id=1)`, bounds.Day, clientID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read settlement source: %w", err)
	}
	rows, err := loadStats(ctx, tx, bounds.Day, clientID)
	if err != nil {
		return false, err
	}
	// 明细与合计是两套独立视图，不能把合计再次加入明细。
	for _, key := range sortedKeys(rows) {
		for _, p := range []struct{ kind, start string }{
			{periods.Week, bounds.WeekStart}, {periods.Month, bounds.MonthStart}, {periods.All, periods.AllStart},
		} {
			if err := mergePeriod(ctx, tx, p.kind, p.start, clientID, key, rows[key]); err != nil {
				return false, err
			}
		}
	}
	if total, ok := rows[Key{}]; ok {
		if err := insertPeriod(ctx, tx, periods.Day, bounds.Day, clientID, Key{}, total); err != nil {
			return false, err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM daily_stats WHERE day=? AND client_id=?`, bounds.Day, clientID); err != nil {
		return false, fmt.Errorf("delete settled stats: %w", err)
	}
	// 删除整日快照时一起清除该席位各端的日修订水位。
	if _, err := tx.ExecContext(ctx, `DELETE FROM daily_reports WHERE day=? AND client_id=?`, bounds.Day, clientID); err != nil {
		return false, fmt.Errorf("delete settled report: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit settlement: %w", err)
	}
	return true, nil
}

func mergePeriod(ctx context.Context, tx *sql.Tx, kind, start string, clientID int64, key Key, source Metrics) error {
	var existing Metrics
	err := tx.QueryRowContext(ctx, `SELECT total_tokens,input_tokens,output_tokens,cache_read_tokens,cache_write_tokens,reasoning_tokens,request_count,success_count,ttft_sum_ms,ttft_samples,latency_sum_ms,latency_samples
		FROM period_stats WHERE kind=? AND start_day=? AND model_id=? AND client_id=?`, kind, start, key.ModelID, clientID).
		Scan(&existing.TotalTokens, &existing.InputTokens, &existing.OutputTokens, &existing.CacheReadTokens, &existing.CacheWriteTokens, &existing.ReasoningTokens, &existing.RequestCount, &existing.SuccessCount, &existing.TTFTSumMS, &existing.TTFTSamples, &existing.LatencySumMS, &existing.LatencySamples)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read period: %w", err)
	}
	merged, ok := addMetrics(existing, source)
	if !ok {
		return ErrSettlementOverflow
	}
	if errors.Is(err, sql.ErrNoRows) {
		return insertPeriod(ctx, tx, kind, start, clientID, key, merged)
	}
	_, err = tx.ExecContext(ctx, `UPDATE period_stats SET total_tokens=?,input_tokens=?,output_tokens=?,cache_read_tokens=?,cache_write_tokens=?,reasoning_tokens=?,request_count=?,success_count=?,ttft_sum_ms=?,ttft_samples=?,latency_sum_ms=?,latency_samples=?
		WHERE kind=? AND start_day=? AND model_id=? AND client_id=?`,
		merged.TotalTokens, merged.InputTokens, merged.OutputTokens, merged.CacheReadTokens, merged.CacheWriteTokens, merged.ReasoningTokens, merged.RequestCount, merged.SuccessCount, merged.TTFTSumMS, merged.TTFTSamples, merged.LatencySumMS, merged.LatencySamples,
		kind, start, key.ModelID, clientID)
	if err != nil {
		return fmt.Errorf("update period: %w", err)
	}
	return nil
}

func insertPeriod(ctx context.Context, tx *sql.Tx, kind, start string, clientID int64, key Key, m Metrics) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO period_stats(kind,start_day,model_id,client_id,total_tokens,input_tokens,output_tokens,cache_read_tokens,cache_write_tokens,reasoning_tokens,request_count,success_count,ttft_sum_ms,ttft_samples,latency_sum_ms,latency_samples)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, kind, start, key.ModelID, clientID, m.TotalTokens, m.InputTokens, m.OutputTokens, m.CacheReadTokens, m.CacheWriteTokens, m.ReasoningTokens, m.RequestCount, m.SuccessCount, m.TTFTSumMS, m.TTFTSamples, m.LatencySumMS, m.LatencySamples)
	if err != nil {
		return fmt.Errorf("insert period: %w", err)
	}
	return nil
}
