// Package capacity provides synthetic, offline fixtures and HTTP load generation.
// It is not imported by the production service.
package capacity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"time"

	"cpa-usage-ranking/internal/models"
	"cpa-usage-ranking/internal/reports"
	"cpa-usage-ranking/internal/storage"
)

type Fixture struct {
	Clients     int      `json:"clients"`
	Day         string   `json:"day"`
	HistoryDays int      `json:"history_days"`
	Backlog     int      `json:"backlog"`
	Models      []string `json:"models"`
	Seed        string   `json:"seed"`
}

func (f Fixture) Validate() error {
	if f.Clients < 1 || f.Clients > 100000 || f.HistoryDays < 2 || f.HistoryDays > 366 || f.Backlog < 0 || f.Backlog > f.Clients || len(f.Models) == 0 {
		return errors.New("fixture requires 1..100000 clients, 2..366 history days, valid backlog and at least one model")
	}
	day, err := time.Parse(time.DateOnly, f.Day)
	if err != nil || day.Format(time.DateOnly) != f.Day {
		return errors.New("invalid fixture day")
	}
	seed, err := hex.DecodeString(f.Seed)
	if err != nil || len(seed) != 32 {
		return errors.New("invalid synthetic credential seed")
	}
	seen := map[string]bool{}
	for _, name := range f.Models {
		if name == "" || seen[name] {
			return errors.New("fixture model names must be distinct")
		}
		seen[name] = true
	}
	return nil
}

func ReadFixture(path string) (Fixture, error) {
	var f Fixture
	data, err := os.ReadFile(path)
	if err != nil {
		return f, err
	}
	if err = json.Unmarshal(data, &f); err != nil {
		return f, err
	}
	return f, f.Validate()
}

// Secret is only for the synthetic identities in this fixture. Never log it.
func (f Fixture) Secret(id int) string {
	raw := sha256.Sum256([]byte(f.Seed + ":" + strconv.Itoa(id)))
	return base64.RawURLEncoding.EncodeToString(raw[:])
}

// Snapshot replaces a full eight-model day, changing only R model rows.
func (f Fixture) Snapshot(revision int64, changed int) ([]byte, error) {
	if revision < 1 || changed < 1 || changed > len(f.Models) {
		return nil, errors.New("invalid revision or changed-model count")
	}
	items := make([]map[string]any, 0, len(f.Models))
	for i, name := range f.Models {
		delta := int64(0)
		if i < changed {
			delta = revision - 1
		}
		items = append(items, map[string]any{"model": name, "total_tokens": strconv.FormatInt(1000+delta, 10), "input_tokens": strconv.FormatInt(600+delta, 10), "output_tokens": "400", "cache_read_tokens": "200", "cache_write_tokens": "20", "reasoning_tokens": "50", "request_count": "10", "success_count": "9", "ttft_avg_ms": 100 + i, "ttft_samples": "8", "latency_avg_ms": 500 + i, "latency_samples": "10"})
	}
	return json.Marshal(map[string]any{"schema_version": 1, "revision": strconv.FormatInt(revision, 10), "items": items})
}

const metricColumns = "total_tokens,input_tokens,output_tokens,cache_read_tokens,cache_write_tokens,reasoning_tokens,request_count,success_count,ttft_sum_ms,ttft_samples,latency_sum_ms,latency_samples"
const metricPlaceholders = "?,?,?,?,?,?,?,?,?,?,?,?"

func metricValues(m reports.Metrics, factor int) []any {
	n := int64(factor)
	v := float64(factor)
	return []any{m.TotalTokens * n, m.InputTokens * n, m.OutputTokens * n, m.CacheReadTokens * n, m.CacheWriteTokens * n, m.ReasoningTokens * n, m.RequestCount * n, m.SuccessCount * n, m.TTFTSumMS * v, m.TTFTSamples * n, m.LatencySumMS * v, m.LatencySamples * n}
}

type periodKey struct{ kind, start string }

func week(t time.Time) time.Time { return t.AddDate(0, 0, -(int(t.Weekday())+6)%7) }
func history(f Fixture, skipNewest bool) map[periodKey]int {
	now, _ := time.Parse(time.DateOnly, f.Day)
	result := map[periodKey]int{}
	for age := 2; age < f.HistoryDays+2; age++ {
		if skipNewest && age == 2 {
			continue
		}
		d := now.AddDate(0, 0, -age)
		result[periodKey{"all", "0001-01-01"}]++
		result[periodKey{"month", d.Format("2006-01") + "-01"}]++
		if !week(d).Before(week(now).AddDate(0, 0, -7)) {
			result[periodKey{"week", week(d).Format(time.DateOnly)}]++
		}
		if age < 31 {
			result[periodKey{"day", d.Format(time.DateOnly)}]++
		}
	}
	return result
}

// Seed exclusively creates a new SQLite file. Historical periods are synthetic
// prefill, NOT evidence of settlement; Backlog creates actual pending work.
func Seed(ctx context.Context, path, catalogPath string, clients, historyDays, backlog int, day string) (Fixture, error) {
	return seed(ctx, path, catalogPath, clients, historyDays, backlog, day, nil)
}

// SeedSustained creates the same synthetic historical shape and real closed-day
// backlog as Seed, but uses the reproducible variable-size model for both open
// days. The model seed is not a credential; the fixture's credential seed is
// independently generated and must remain private.
func SeedSustained(ctx context.Context, path, catalogPath string, clients, historyDays, backlog int, day string, modelSeed uint64) (Fixture, error) {
	return seed(ctx, path, catalogPath, clients, historyDays, backlog, day, &modelSeed)
}

func seed(ctx context.Context, path, catalogPath string, clients, historyDays, backlog int, day string, modelSeed *uint64) (Fixture, error) {
	f := Fixture{Clients: clients, Day: day, HistoryDays: historyDays, Backlog: backlog}
	catalog, err := models.Load(catalogPath)
	if err != nil {
		return f, err
	}
	for i, m := range catalog.List() {
		if i == 8 {
			break
		}
		f.Models = append(f.Models, m.Name)
	}
	raw := make([]byte, 32)
	if _, err = rand.Read(raw); err != nil {
		return f, err
	}
	f.Seed = hex.EncodeToString(raw)
	if err = f.Validate(); err != nil {
		return f, err
	}
	var synthetic *SustainedModel
	if modelSeed != nil {
		names := make([]string, 0, len(catalog.List()))
		for _, model := range catalog.List() {
			names = append(names, model.Name)
		}
		synthetic, err = NewSustainedModel(day, names, clients, *modelSeed)
		if err != nil {
			return f, err
		}
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return f, err
	}
	if err = file.Close(); err != nil {
		return f, err
	}
	db, err := storage.Open(ctx, path)
	if err != nil {
		return f, err
	}
	defer db.Close()
	if err = storage.InitSchema(ctx, db.Writer); err != nil {
		return f, err
	}
	registry, err := models.Register(ctx, db.Writer, catalog)
	if err != nil {
		return f, err
	}
	now, _ := time.Parse(time.DateOnly, f.Day)
	if err = reports.AdvanceWindow(ctx, db.Writer, now); err != nil {
		return f, err
	}
	body, _ := f.Snapshot(1, 2)
	report, err := reports.Decode(body)
	if err != nil {
		return f, err
	}
	prepared, err := reports.Prepare(report)
	if err != nil {
		return f, err
	}
	rows, err := prepared.Aggregate(registry)
	if err != nil {
		return f, err
	}
	digest := prepared.Digest()
	keys := make([]reports.Key, 0, len(rows))
	for k := range rows {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].ModelID < keys[j].ModelID })
	normal, behind := history(f, false), history(f, true)
	periods := make([]periodKey, 0, len(normal))
	for k := range normal {
		periods = append(periods, k)
	}
	sort.Slice(periods, func(i, j int) bool {
		if periods[i].kind == periods[j].kind {
			return periods[i].start < periods[j].start
		}
		return periods[i].kind < periods[j].kind
	})
	for first := 1; first <= clients; first += 250 {
		err = func() error {
			tx, e := db.Writer.BeginTx(ctx, nil)
			if e != nil {
				return e
			}
			defer tx.Rollback()
			cs, e := tx.PrepareContext(ctx, `INSERT INTO clients(id,github_id,server_id,display_name,avatar_id,created_at_ms)
				VALUES(?,?,'550e8400-e29b-41d4-a716-446655440000',?,1,?)`)
			if e != nil {
				return e
			}
			defer cs.Close()
			ws, e := tx.PrepareContext(ctx, `INSERT INTO client_writers(id,client_id,token_hash) VALUES(?,?,?)`)
			if e != nil {
				return e
			}
			defer ws.Close()
			rs, e := tx.PrepareContext(ctx, `INSERT INTO daily_reports(day,client_id,received_at_ms) VALUES(?,?,?)`)
			if e != nil {
				return e
			}
			defer rs.Close()
			vs, e := tx.PrepareContext(ctx, `INSERT INTO daily_writer_versions(day,writer_id,client_id,revision,digest) VALUES(?,?,?,1,?)`)
			if e != nil {
				return e
			}
			defer vs.Close()
			ds, e := tx.PrepareContext(ctx, `INSERT INTO daily_stats(day,client_id,model_id,`+metricColumns+`) VALUES(?,?,?,`+metricPlaceholders+`)`)
			if e != nil {
				return e
			}
			defer ds.Close()
			ps, e := tx.PrepareContext(ctx, `INSERT INTO period_stats(kind,start_day,client_id,model_id,`+metricColumns+`) VALUES(?,?,?,?,`+metricPlaceholders+`)`)
			if e != nil {
				return e
			}
			defer ps.Close()
			for id := first; id < first+250 && id <= clients; id++ {
				secret, _ := base64.RawURLEncoding.DecodeString(f.Secret(id))
				hash := sha256.Sum256(secret)
				if _, e = cs.ExecContext(ctx, id, id, fmt.Sprintf("capacity-%06d", id), now.UnixMilli()); e != nil {
					return e
				}
				if _, e = ws.ExecContext(ctx, id, id, hash[:]); e != nil {
					return e
				}
				days := 2
				if id <= backlog {
					days = 3
				}
				for age := 0; age < days; age++ {
					date := now.AddDate(0, 0, -age).Format(time.DateOnly)
					dayDigest, dayRows, dayKeys := digest, rows, keys
					if synthetic != nil && age < 2 {
						initial, x := synthetic.Initial(id, date)
						if x != nil {
							return x
						}
						r, x := reports.Decode(initial)
						if x != nil {
							return x
						}
						p, x := reports.Prepare(r)
						if x != nil {
							return x
						}
						dayDigest = p.Digest()
						dayRows, x = p.Aggregate(registry)
						if x != nil {
							return x
						}
						dayKeys = make([]reports.Key, 0, len(dayRows))
						for k := range dayRows {
							dayKeys = append(dayKeys, k)
						}
						sort.Slice(dayKeys, func(i, j int) bool { return dayKeys[i].ModelID < dayKeys[j].ModelID })
					}
					if _, e = rs.ExecContext(ctx, date, id, now.UnixMilli()); e != nil {
						return e
					}
					if _, e = vs.ExecContext(ctx, date, id, id, dayDigest[:]); e != nil {
						return e
					}
					for _, k := range dayKeys {
						args := append([]any{date, id, k.ModelID}, metricValues(dayRows[k], 1)...)
						if _, e = ds.ExecContext(ctx, args...); e != nil {
							return e
						}
					}
				}
				factors := normal
				if id <= backlog {
					factors = behind
				}
				for _, p := range periods {
					if factors[p] == 0 {
						continue
					}
					for _, k := range keys {
						if p.kind == "day" && k.ModelID != 0 {
							continue
						}
						args := append([]any{p.kind, p.start, id, k.ModelID}, metricValues(rows[k], factors[p])...)
						if _, e = ps.ExecContext(ctx, args...); e != nil {
							return e
						}
					}
				}
			}
			return tx.Commit()
		}()
		if err != nil {
			return f, err
		}
	}
	if _, err = db.Writer.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		return f, err
	}
	return f, nil
}

// Inspect uses a read-only connection and does not inspect private credentials.
func Inspect(ctx context.Context, path string) (map[string]any, error) {
	db, err := readDB(path)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	result := map[string]any{}
	for name, q := range map[string]string{"clients": "SELECT count(*) FROM clients", "daily_reports": "SELECT count(*) FROM daily_reports", "daily_rows": "SELECT count(*) FROM daily_stats", "period_rows": "SELECT count(*) FROM period_stats", "pending_tokens": "SELECT coalesce(sum(total_tokens),0) FROM daily_stats WHERE model_id=0", "settled_tokens": "SELECT coalesce(sum(total_tokens),0) FROM period_stats WHERE kind='all' AND model_id=0", "pending_revision_sum": "SELECT coalesce(sum(revision),0) FROM daily_writer_versions", "closed_backlog": "SELECT count(*) FROM daily_reports WHERE day<(SELECT closed_before_day FROM report_window WHERE id=1)"} {
		var n int64
		if err = db.QueryRowContext(ctx, q).Scan(&n); err != nil {
			return nil, err
		}
		result[name] = n
	}
	return result, nil
}

// Keep filesystem paths separate from SQLite URI query options.
func readDB(path string) (*sql.DB, error) { return openReadDB(path) }
