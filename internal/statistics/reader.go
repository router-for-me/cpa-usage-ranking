// Package statistics 提供内部候选统计，不定义公开榜单或查询接口。
package statistics

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"cpa-usage-ranking/internal/periods"
	"cpa-usage-ranking/internal/reports"
)

const (
	DefaultMaxCandidates = 100000
	DefaultReadTimeout   = 15 * time.Second
)

var (
	ErrInvalidSelection = errors.New("invalid statistics selection")
	ErrCapacity         = errors.New("statistics candidate capacity exceeded")
	ErrOverflow         = errors.New("statistics metrics overflow")
)

// Selection 指定一种视角；日期仅用于选择包含它的 UTC 周期。
// 全模型合计是 ModelID=0 的独立视角，不能与明细相加。
type Selection struct {
	Kind    string
	Day     string
	ModelID int64
	Public  bool // 同一快照内读取资料，在评分前排除禁用、隐藏和退出的席位。
}

type Profile struct {
	DisplayName string
	AvatarID    int64
}

type Snapshot struct {
	Kind, Start, End string                    // End 为空表示累计周期无上界。
	Rows             map[int64]reports.Metrics // client ID -> 原始统计
	Profiles         map[int64]Profile
}

type Reader struct {
	db            *sql.DB
	maxCandidates int
	timeout       time.Duration
}

func NewReader(db *sql.DB, maxCandidates int, timeout time.Duration) (*Reader, error) {
	if db == nil || maxCandidates <= 0 || timeout <= 0 {
		return nil, errors.New("invalid statistics reader options")
	}
	return &Reader{db: db, maxCandidates: maxCandidates, timeout: timeout}, nil
}

// Read 的两部分查询和数值合并处于同一真实 SQLite 快照中；返回前结束事务。
// 已结日摘要只有全模型合计，因此日视角不支持模型明细，即使该日尚未日结。
func (r *Reader) Read(ctx context.Context, s Selection) (Snapshot, error) {
	bounds, err := periods.ForDay(s.Day)
	if err != nil || s.ModelID < 0 {
		return Snapshot{}, ErrInvalidSelection
	}
	result := Snapshot{Kind: s.Kind}
	switch s.Kind {
	case periods.Day:
		if s.ModelID != 0 {
			return Snapshot{}, ErrInvalidSelection
		}
		d, _ := time.Parse("2006-01-02", s.Day)
		result.Start, result.End = s.Day, d.AddDate(0, 0, 1).Format("2006-01-02")
		if len(result.End) != 10 {
			return Snapshot{}, ErrInvalidSelection
		}
	case periods.Week:
		result.Start, result.End = bounds.WeekStart, bounds.WeekEnd
	case periods.Month:
		result.Start, result.End = bounds.MonthStart, bounds.MonthEnd
	case periods.Year:
		d, _ := time.Parse("2006-01-02", s.Day)
		result.Start = time.Date(d.Year(), 1, 1, 0, 0, 0, 0, time.UTC).Format("2006-01-02")
		result.End = time.Date(d.Year()+1, 1, 1, 0, 0, 0, 0, time.UTC).Format("2006-01-02")
		if len(result.End) != 10 {
			return Snapshot{}, ErrInvalidSelection
		}
	case periods.All:
		result.Start = periods.AllStart
	default:
		return Snapshot{}, ErrInvalidSelection
	}
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Snapshot{}, fmt.Errorf("begin statistics read: %w", err)
	}
	defer tx.Rollback()
	result.Rows = make(map[int64]reports.Metrics)
	if s.Public {
		result.Profiles = make(map[int64]Profile)
	}
	columns := `s.client_id,s.total_tokens,s.input_tokens,s.output_tokens,s.cache_read_tokens,s.cache_write_tokens,s.reasoning_tokens,s.request_count,s.success_count,s.ttft_sum_ms,s.ttft_samples,s.latency_sum_ms,s.latency_samples`
	join := ""
	if s.Public {
		columns += `,c.display_name,c.avatar_id`
		join = ` JOIN clients c ON c.id=s.client_id AND c.disabled=0 AND c.ranking_hidden=0 AND c.withdrawn=0`
	}
	query := `SELECT ` + columns + ` FROM period_stats s` + join + ` WHERE s.kind=? AND s.model_id=?`
	kind := s.Kind
	if kind == periods.Year {
		kind = periods.Month
	}
	args := []any{kind, s.ModelID}
	if s.Kind == periods.Year {
		query += ` AND s.start_day>=? AND s.start_day<?`
		args = append(args, result.Start, result.End)
	} else {
		query += ` AND s.start_day=?`
		args = append(args, result.Start)
	}
	if err := r.merge(ctx, tx, &result, query, args...); err != nil {
		return Snapshot{}, err
	}
	query = `SELECT ` + columns + ` FROM daily_stats s` + join + ` WHERE s.model_id=?`
	args = []any{s.ModelID}
	if s.Kind != periods.All {
		query += ` AND s.day>=? AND s.day<?`
		args = append(args, result.Start, result.End)
	}
	if err := r.merge(ctx, tx, &result, query, args...); err != nil {
		return Snapshot{}, err
	}
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	if err := tx.Commit(); err != nil {
		return Snapshot{}, fmt.Errorf("finish statistics read: %w", err)
	}
	return result, nil
}

func (r *Reader) merge(ctx context.Context, tx *sql.Tx, result *Snapshot, query string, args ...any) error {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("query statistics: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var m reports.Metrics
		var profile Profile
		values := []any{&id, &m.TotalTokens, &m.InputTokens, &m.OutputTokens, &m.CacheReadTokens, &m.CacheWriteTokens, &m.ReasoningTokens, &m.RequestCount, &m.SuccessCount, &m.TTFTSumMS, &m.TTFTSamples, &m.LatencySumMS, &m.LatencySamples}
		if result.Profiles != nil {
			values = append(values, &profile.DisplayName, &profile.AvatarID)
		}
		if err := rows.Scan(values...); err != nil {
			return fmt.Errorf("scan statistics: %w", err)
		}
		old, exists := result.Rows[id]
		if !exists && len(result.Rows) >= r.maxCandidates {
			return ErrCapacity
		}
		merged, ok := reports.AddMetrics(old, m)
		if !ok {
			return fmt.Errorf("client %d: %w", id, ErrOverflow)
		}
		result.Rows[id] = merged
		if result.Profiles != nil {
			result.Profiles[id] = profile
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate statistics: %w", err)
	}
	return nil
}
