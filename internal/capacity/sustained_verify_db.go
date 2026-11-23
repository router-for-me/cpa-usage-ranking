package capacity

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"reflect"
	"sort"
	"time"

	"cpa-usage-ranking/internal/reports"
)

func copySustainedSeed(src, dst string) error {
	input, err := os.Open(src)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(output, input)
	if copyErr == nil {
		copyErr = output.Sync()
	}
	if err = output.Close(); copyErr == nil {
		copyErr = err
	}
	return copyErr
}

func insertExpectedSnapshot(ctx context.Context, tx *sql.Tx, id int64, day string, snapshot verifySnapshot, ids map[string]int64) error {
	_, details, err := snapshotRows(snapshot.body, ids)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO daily_reports(day,client_id,received_at_ms) VALUES(?,?,0) ON CONFLICT(day,client_id) DO NOTHING`, day, id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO daily_writer_versions(day,writer_id,client_id,revision,digest) VALUES(?,?,?,?,?)
		ON CONFLICT(day,writer_id) DO UPDATE SET revision=excluded.revision,digest=excluded.digest`, day, id, id, snapshot.revision, snapshot.digest[:]); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM daily_stats WHERE day=? AND client_id=?", day, id); err != nil {
		return err
	}
	var total reports.Metrics
	keys := make([]reports.Key, 0, len(details))
	for k, v := range details {
		keys = append(keys, k)
		total, err = verificationAdd(total, v)
		if err != nil {
			return err
		}
	}
	if len(keys) > 0 {
		details[reports.Key{}] = total
		keys = append(keys, reports.Key{})
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].ModelID < keys[j].ModelID })
	for _, k := range keys {
		args := append([]any{day, id, k.ModelID}, metricValues(details[k], 1)...)
		if _, err = tx.ExecContext(ctx, "INSERT INTO daily_stats(day,client_id,model_id,"+metricColumns+") VALUES(?,?,?,"+metricPlaceholders+")", args...); err != nil {
			return err
		}
	}
	return nil
}

type closedKey struct {
	day string
	id  int64
}
type closedRow struct {
	key     reports.Key
	metrics reports.Metrics
}

func settleExpected(ctx context.Context, tx *sql.Tx, k closedKey) error {
	date, err := time.Parse(time.DateOnly, k.day)
	if err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, "SELECT model_id,"+metricColumns+" FROM daily_stats WHERE day=? AND client_id=?", k.day, k.id)
	if err != nil {
		return err
	}
	var source []closedRow
	for rows.Next() {
		var r closedRow
		v := &r.metrics
		if err = rows.Scan(&r.key.ModelID, &v.TotalTokens, &v.InputTokens, &v.OutputTokens, &v.CacheReadTokens, &v.CacheWriteTokens, &v.ReasoningTokens, &v.RequestCount, &v.SuccessCount, &v.TTFTSumMS, &v.TTFTSamples, &v.LatencySumMS, &v.LatencySamples); err != nil {
			rows.Close()
			return err
		}
		source = append(source, r)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	updates := `total_tokens=total_tokens+excluded.total_tokens,input_tokens=input_tokens+excluded.input_tokens,output_tokens=output_tokens+excluded.output_tokens,cache_read_tokens=cache_read_tokens+excluded.cache_read_tokens,cache_write_tokens=cache_write_tokens+excluded.cache_write_tokens,reasoning_tokens=reasoning_tokens+excluded.reasoning_tokens,request_count=request_count+excluded.request_count,success_count=success_count+excluded.success_count,ttft_sum_ms=ttft_sum_ms+excluded.ttft_sum_ms,ttft_samples=ttft_samples+excluded.ttft_samples,latency_sum_ms=latency_sum_ms+excluded.latency_sum_ms,latency_samples=latency_samples+excluded.latency_samples`
	for _, r := range source {
		for _, p := range []struct{ kind, start string }{{"week", week(date).Format(time.DateOnly)}, {"month", date.Format("2006-01") + "-01"}, {"all", "0001-01-01"}} {
			args := append([]any{p.kind, p.start, r.key.ModelID, k.id}, metricValues(r.metrics, 1)...)
			q := "INSERT INTO period_stats(kind,start_day,model_id,client_id," + metricColumns + ") VALUES(?,?,?,?," + metricPlaceholders + ") ON CONFLICT(kind,start_day,model_id,client_id) DO UPDATE SET " + updates
			if _, err = tx.ExecContext(ctx, q, args...); err != nil {
				return err
			}
		}
		if r.key.ModelID == 0 {
			args := append([]any{"day", k.day, 0, k.id}, metricValues(r.metrics, 1)...)
			if _, err = tx.ExecContext(ctx, "INSERT INTO period_stats(kind,start_day,model_id,client_id,"+metricColumns+") VALUES(?,?,?,?,"+metricPlaceholders+")", args...); err != nil {
				return err
			}
		}
	}
	_, err = tx.ExecContext(ctx, "DELETE FROM daily_reports WHERE day=? AND client_id=?", k.day, k.id)
	return err
}

func compareQuery(ctx context.Context, expected, final *sql.DB, name, query string) (int64, error) {
	a, err := expected.QueryContext(ctx, query)
	if err != nil {
		return 0, err
	}
	defer a.Close()
	b, err := final.QueryContext(ctx, query)
	if err != nil {
		return 0, err
	}
	defer b.Close()
	cols, err := a.Columns()
	if err != nil {
		return 0, err
	}
	read := func(rows *sql.Rows) ([]any, error) {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if e := rows.Scan(ptrs...); e != nil {
			return nil, e
		}
		for i, v := range vals {
			if data, ok := v.([]byte); ok {
				vals[i] = append([]byte(nil), data...)
			}
		}
		return vals, nil
	}
	var count int64
	for {
		hasA, hasB := a.Next(), b.Next()
		if hasA != hasB {
			return count, fmt.Errorf("%s row count differs at row %d", name, count+1)
		}
		if !hasA {
			break
		}
		count++
		va, e := read(a)
		if e != nil {
			return count, e
		}
		vb, e := read(b)
		if e != nil {
			return count, e
		}
		if !reflect.DeepEqual(va, vb) {
			return count, fmt.Errorf("%s differs at ordered row %d", name, count)
		}
	}
	if err = a.Err(); err != nil {
		return count, err
	}
	if err = b.Err(); err != nil {
		return count, err
	}
	return count, nil
}

func compareSustainedDatabases(ctx context.Context, seedPath, expectedPath string, final *sql.DB, s *verificationState) (map[string]int64, error) {
	if err := copySustainedSeed(seedPath, expectedPath); err != nil {
		return nil, err
	}
	expected, err := sql.Open("sqlite3", expectedPath)
	if err != nil {
		return nil, err
	}
	defer expected.Close()
	expected.SetMaxOpenConns(1)
	if _, err = expected.ExecContext(ctx, "PRAGMA foreign_keys=ON"); err != nil {
		return nil, err
	}
	tx, err := expected.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	keys := make([]verificationKey, 0, len(s.acks))
	for k := range s.acks {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].client != keys[j].client {
			return keys[i].client < keys[j].client
		}
		return keys[i].day < keys[j].day
	})
	for _, k := range keys {
		if err = insertExpectedSnapshot(ctx, tx, int64(k.client), k.day, s.acks[k], s.modelIDs); err != nil {
			return nil, err
		}
	}
	day, _ := time.Parse(time.DateOnly, s.fixture.Day)
	closedBefore := day.AddDate(0, 0, -1).Format(time.DateOnly)
	finalClosed := map[closedKey]bool{}
	rows, err := final.QueryContext(ctx, "SELECT day,client_id FROM daily_reports WHERE day<?", closedBefore)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var k closedKey
		if err = rows.Scan(&k.day, &k.id); err != nil {
			rows.Close()
			return nil, err
		}
		finalClosed[k] = true
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	rows, err = tx.QueryContext(ctx, "SELECT day,client_id FROM daily_reports WHERE day<?", closedBefore)
	if err != nil {
		return nil, err
	}
	var closed []closedKey
	for rows.Next() {
		var k closedKey
		if err = rows.Scan(&k.day, &k.id); err != nil {
			rows.Close()
			return nil, err
		}
		closed = append(closed, k)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	for _, k := range closed {
		if !finalClosed[k] {
			if err = settleExpected(ctx, tx, k); err != nil {
				return nil, err
			}
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	counts := map[string]int64{"closed": int64(len(finalClosed))}
	queries := []struct{ name, query string }{
		{"clients", "SELECT id,github_id,server_id,display_name,avatar_id,disabled,ranking_hidden,withdrawn,created_at_ms FROM clients ORDER BY id"},
		{"client_writers", "SELECT id,client_id,token_hash FROM client_writers ORDER BY id"},
		{"models", "SELECT id,model_name FROM models ORDER BY id"},
		{"report_window", "SELECT id,closed_before_day FROM report_window ORDER BY id"},
		{"daily_reports", "SELECT day,client_id FROM daily_reports ORDER BY day,client_id"},
		{"daily_writer_versions", "SELECT day,writer_id,client_id,revision,digest FROM daily_writer_versions ORDER BY day,writer_id"},
		{"daily_stats", "SELECT day,client_id,model_id," + metricColumns + " FROM daily_stats ORDER BY day,client_id,model_id"},
		{"period_stats", "SELECT kind,start_day,model_id,client_id," + metricColumns + " FROM period_stats ORDER BY kind,start_day,model_id,client_id"},
	}
	for _, q := range queries {
		count, e := compareQuery(ctx, expected, final, q.name, q.query)
		if e != nil {
			return nil, e
		}
		counts[q.name] = count
	}
	_, err = expected.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)")
	if err != nil {
		return nil, err
	}
	return counts, nil
}
