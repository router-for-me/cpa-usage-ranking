package test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"cpa-usage-ranking/internal/clients"
	"cpa-usage-ranking/internal/models"
	"cpa-usage-ranking/internal/periods"
	"cpa-usage-ranking/internal/reports"
	"cpa-usage-ranking/internal/statistics"
	"cpa-usage-ranking/internal/storage"
	"cpa-usage-ranking/internal/testfixture"
	"github.com/mattn/go-sqlite3"
)

type fixture struct {
	db       *storage.DB
	registry *models.Registry
	path     string
	now      time.Time
	a, b     int64
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "stats.db")
	db, err := storage.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err = storage.InitSchema(ctx, db.Writer); err != nil {
		t.Fatal(err)
	}
	catalogPath := filepath.Join(t.TempDir(), "models.json")
	if err = os.WriteFile(catalogPath, []byte(`{"currency":"USD","price_unit":"per_million_tokens","models":[{"model_name":"alpha","alias":[],"prices":{}},{"model_name":"beta","alias":[],"prices":{}}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	catalog, err := models.Load(catalogPath)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := models.Register(ctx, db.Writer, catalog)
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{db: db, registry: reg, path: path}
	f.a = f.client(t)
	f.b = f.client(t)
	return f
}
func (f *fixture) client(t *testing.T) int64 {
	t.Helper()
	var hash clients.CredentialHash
	rand.Read(hash[:])
	writer, err := testfixture.Seat(context.Background(), f.db.Writer, hash, "client", 1)
	if err != nil {
		t.Fatal(err)
	}
	if writer.ID != writer.WriterID {
		t.Fatal("single-writer fixture requires equal IDs")
	}
	return writer.ID
}
func item(name string, n int64) reports.Item {
	return reports.Item{Model: name, TotalTokens: n, InputTokens: n, RequestCount: 1, SuccessCount: 1}
}
func (f *fixture) put(t *testing.T, id int64, day string, rev int64, items ...reports.Item) {
	t.Helper()
	now, err := time.Parse(time.RFC3339, day+"T12:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	store := reports.NewStore(f.db.Writer, f.registry, func() time.Time { return now })
	p, err := reports.Prepare(reports.Report{Revision: rev, Items: items})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Apply(context.Background(), id, day, p); err != nil {
		t.Fatal(err)
	}
}
func (f *fixture) closeDay(t *testing.T, day string, id int64) {
	t.Helper()
	ok, err := reports.Finalize(context.Background(), f.db.Writer, day, id)
	if err != nil || !ok {
		t.Fatalf("settlement %v %v", ok, err)
	}
}
func (f *fixture) reader(t *testing.T, max int) *statistics.Reader {
	t.Helper()
	r, err := statistics.NewReader(f.db.Reader, max, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func read(t *testing.T, r *statistics.Reader, kind, day string, model int64) statistics.Snapshot {
	t.Helper()
	s, err := r.Read(context.Background(), statistics.Selection{Kind: kind, Day: day, ModelID: model})
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func check(t *testing.T, s statistics.Snapshot, id, want int64) {
	t.Helper()
	if got := s.Rows[id].TotalTokens; got != want {
		t.Fatalf("%s %s id=%d got=%d want=%d", s.Kind, s.Start, id, got, want)
	}
}

func TestReadSettledPendingBoundariesAndIsolation(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	// 隔周、跨月、跨年，10 月旧日故意留在日结积压中。
	f.put(t, f.a, "2026-10-30", 1, item("alpha", 2))
	f.put(t, f.a, "2026-12-27", 1, item("alpha", 3))
	f.put(t, f.a, "2026-12-31", 1, item("alpha", 12), item("beta", 11))
	f.put(t, f.b, "2026-12-31", 1, item("alpha", 13))
	f.put(t, f.a, "2027-01-01", 1, item("alpha", 17))
	f.put(t, f.a, "2027-01-04", 1, item("alpha", 19))
	if err := reports.AdvanceWindow(ctx, f.db.Writer, time.Date(2027, 1, 6, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	r := f.reader(t, 100)
	alpha, _ := f.registry.CanonicalID("alpha")
	beta, _ := f.registry.CanonicalID("beta")
	assertions := func() {
		s := read(t, r, periods.All, "2027-01-04", 0)
		check(t, s, f.a, 64)
		check(t, s, f.b, 13)
		s = read(t, r, periods.Week, "2027-01-01", 0)
		if s.Start != "2026-12-28" || s.End != "2027-01-04" {
			t.Fatal(s.Start, s.End)
		}
		check(t, s, f.a, 40)
		check(t, s, f.b, 13)
		check(t, read(t, r, periods.Month, "2026-12-31", 0), f.a, 26)
		check(t, read(t, r, periods.Month, "2027-01-01", 0), f.a, 36)
		check(t, read(t, r, periods.Day, "2026-12-31", 0), f.a, 23)
		check(t, read(t, r, periods.All, "2027-01-01", alpha), f.a, 53)
		check(t, read(t, r, periods.All, "2027-01-01", beta), f.a, 11)
	}
	assertions()
	f.closeDay(t, "2026-12-31", f.a)
	f.closeDay(t, "2026-12-31", f.b)
	assertions()
	if _, err := r.Read(ctx, statistics.Selection{Kind: periods.Day, Day: "2026-12-31", ModelID: alpha}); !errors.Is(err, statistics.ErrInvalidSelection) {
		t.Fatalf("model day allowed: %v", err)
	}
	for _, s := range []statistics.Selection{{Kind: "bad", Day: "2027-01-01"}, {Kind: periods.All, Day: "no"}, {Kind: periods.All, Day: "2027-01-01", ModelID: -1}} {
		if _, err := r.Read(ctx, s); !errors.Is(err, statistics.ErrInvalidSelection) {
			t.Fatal(s, err)
		}
	}
}

func TestReadOverflowCapacityAndCancellation(t *testing.T) {
	f := newFixture(t)
	f.put(t, f.a, "2026-12-30", 1, item("alpha", 1))
	f.put(t, f.b, "2026-12-30", 1, item("alpha", 1))
	f.put(t, f.a, "2026-12-31", 1, item("alpha", 1))
	if _, err := f.reader(t, 1).Read(context.Background(), statistics.Selection{Kind: periods.All, Day: "2026-12-31"}); !errors.Is(err, statistics.ErrCapacity) {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		column string
		value  any
	}{{"total_tokens", int64(math.MaxInt64)}, {"ttft_sum_ms", math.MaxFloat64}} {
		if _, err := f.db.Writer.Exec(`UPDATE daily_stats SET `+tc.column+`=? WHERE client_id=?`, tc.value, f.a); err != nil {
			t.Fatal(err)
		}
		if _, err := f.reader(t, 3).Read(context.Background(), statistics.Selection{Kind: periods.All, Day: "2026-12-31"}); !errors.Is(err, statistics.ErrOverflow) {
			t.Fatalf("%s: %v", tc.column, err)
		}
		if _, err := f.db.Writer.Exec(`UPDATE daily_stats SET `+tc.column+`=1 WHERE client_id=?`, f.a); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.reader(t, 3).Read(ctx, statistics.Selection{Kind: periods.All, Day: "2026-12-31"}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	// 等待读池连接也受独立读取超时约束。
	f.db.Reader.SetMaxOpenConns(1)
	conn, err := f.db.Reader.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	r, err := statistics.NewReader(f.db.Reader, 3, 20*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.Read(context.Background(), statistics.Selection{Kind: periods.All, Day: "2026-12-31"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	conn.Close()
	read(t, r, periods.All, "2026-12-31", 0)
	if f.db.Reader.Stats().InUse != 0 {
		t.Fatal("reader leaked")
	}
}

// 测试驱动只在两次真实查询之间暂停；SQLite 本身仍处理 WAL 快照及事务。
type pauseDriver struct{ ready, release chan struct{} }
type pauseConn struct {
	driver.Conn
	owner *pauseDriver
}

func (d *pauseDriver) Open(name string) (driver.Conn, error) {
	c, e := (&sqlite3.SQLiteDriver{}).Open(name)
	if e != nil {
		return nil, e
	}
	return &pauseConn{Conn: c, owner: d}, nil
}
func (c *pauseConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	return c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
}
func (c *pauseConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	if strings.Contains(q, "FROM daily_stats") {
		close(c.owner.ready)
		select {
		case <-c.owner.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, q, args)
}

var driverID atomic.Int64

func pausedReader(t *testing.T, f *fixture, timeout time.Duration) (*statistics.Reader, *sql.DB, *pauseDriver) {
	t.Helper()
	d := &pauseDriver{make(chan struct{}), make(chan struct{})}
	name := fmt.Sprintf("snapshot-test-%d", driverID.Add(1))
	sql.Register(name, d)
	u := url.URL{Scheme: "file", Path: f.path, RawQuery: "mode=ro"}
	db, err := sql.Open(name, u.String())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	r, err := statistics.NewReader(db, 100, timeout)
	if err != nil {
		t.Fatal(err)
	}
	return r, db, d
}
func TestReadSQLiteSnapshotInterleavesSettlementAndReport(t *testing.T) {
	f := newFixture(t)
	f.put(t, f.a, "2026-12-30", 1, item("alpha", 7))
	f.put(t, f.a, "2026-12-31", 1, item("alpha", 11))
	f.put(t, f.b, "2026-12-31", 1, item("alpha", 3))
	if err := reports.AdvanceWindow(context.Background(), f.db.Writer, time.Date(2027, 1, 1, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	r, db, d := pausedReader(t, f, time.Second*5)
	type outcome struct {
		s   statistics.Snapshot
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		s, e := r.Read(context.Background(), statistics.Selection{Kind: periods.All, Day: "2026-12-31"})
		done <- outcome{s, e}
	}()
	<-d.ready
	f.closeDay(t, "2026-12-30", f.a)
	f.put(t, f.b, "2026-12-31", 2, item("alpha", 9))
	close(d.release)
	got := <-done
	if got.err != nil {
		t.Fatal(got.err)
	}
	check(t, got.s, f.a, 18)
	check(t, got.s, f.b, 3)
	s := read(t, f.reader(t, 100), periods.All, "2026-12-31", 0)
	check(t, s, f.a, 18)
	check(t, s, f.b, 9)
	if db.Stats().InUse != 0 {
		t.Fatal("snapshot read retained connection")
	}
	// 持有返回的候选数据也不阻止 WAL checkpoint。
	if _, err := f.db.Writer.Exec(`UPDATE clients SET display_name='changed' WHERE id=?`, f.a); err != nil {
		t.Fatal(err)
	}
	var busy, log, checkpointed int
	if err := f.db.Writer.QueryRow(`PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &log, &checkpointed); err != nil || busy != 0 {
		t.Fatal(busy, log, checkpointed, err)
	}
}
func TestYearUsesSettledMonthsAndAllPendingDays(t *testing.T) {
	f := newFixture(t)
	f.put(t, f.a, "2025-12-31", 1, item("alpha", 101))
	f.put(t, f.a, "2026-01-01", 1, item("alpha", 2))
	f.put(t, f.a, "2026-05-03", 1, item("alpha", 3))
	f.put(t, f.a, "2026-12-31", 1, item("alpha", 5))
	f.put(t, f.a, "2027-01-01", 1, item("alpha", 103))
	if err := reports.AdvanceWindow(context.Background(), f.db.Writer, time.Date(2027, 1, 3, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	f.closeDay(t, "2026-01-01", f.a)
	r := f.reader(t, 10)
	checkYear := func() {
		s := read(t, r, periods.Year, "2026-12-31", 0)
		if s.Start != "2026-01-01" || s.End != "2027-01-01" {
			t.Fatal(s.Start, s.End)
		}
		check(t, s, f.a, 10)
	}
	checkYear()
	f.closeDay(t, "2026-05-03", f.a)
	f.closeDay(t, "2026-12-31", f.a)
	checkYear()
}

func TestPublicYearSnapshotInterleavesSettlementProfilesAndDisable(t *testing.T) {
	f := newFixture(t)
	f.put(t, f.a, "2026-01-01", 1, item("alpha", 2))
	f.put(t, f.a, "2026-12-30", 1, item("alpha", 5))
	f.put(t, f.b, "2026-12-31", 1, item("alpha", 11))
	if err := reports.AdvanceWindow(context.Background(), f.db.Writer, time.Date(2027, 1, 2, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	f.closeDay(t, "2026-01-01", f.a)
	r, db, d := pausedReader(t, f, 5*time.Second)
	type outcome struct {
		s   statistics.Snapshot
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		s, err := r.Read(context.Background(), statistics.Selection{Kind: periods.Year, Day: "2026-12-31", Public: true})
		done <- outcome{s, err}
	}()
	<-d.ready
	f.closeDay(t, "2026-12-30", f.a)
	if _, err := f.db.Writer.Exec(`UPDATE clients SET display_name='new',disabled=1 WHERE id=?`, f.b); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Writer.Exec(`UPDATE clients SET display_name='new-a',avatar_id=2 WHERE id=?`, f.a); err != nil {
		t.Fatal(err)
	}
	close(d.release)
	got := <-done
	if got.err != nil {
		t.Fatal(got.err)
	}
	check(t, got.s, f.a, 7)
	check(t, got.s, f.b, 11)
	if got.s.Profiles[f.a].DisplayName != "client" || got.s.Profiles[f.a].AvatarID != 1 || got.s.Profiles[f.b].DisplayName != "client" {
		t.Fatal(got.s.Profiles)
	}
	if db.Stats().InUse != 0 {
		t.Fatal("reader retained connection")
	}
	fresh, err := f.reader(t, 1).Read(context.Background(), statistics.Selection{Kind: periods.Year, Day: "2026-12-31", Public: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(fresh.Rows) != 1 || fresh.Profiles[f.a].DisplayName != "new-a" || fresh.Profiles[f.a].AvatarID != 2 {
		t.Fatal(fresh)
	}
	check(t, fresh, f.a, 7)
}

func TestReadTimeoutInsideSnapshotReleasesTransaction(t *testing.T) {
	f := newFixture(t)
	f.put(t, f.a, "2026-12-31", 1, item("alpha", 1))
	r, db, d := pausedReader(t, f, 30*time.Millisecond)
	_, err := r.Read(context.Background(), statistics.Selection{Kind: periods.All, Day: "2026-12-31"})
	<-d.ready
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	// database/sql 在取消时可异步回滚，等待连接实际归还而非假定同步调度。
	wait, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	conn, err := db.Conn(wait)
	if err != nil {
		t.Fatalf("timed-out transaction retained connection: %v", err)
	}
	conn.Close()
	close(d.release)
	if _, err := f.db.Writer.Exec(`UPDATE clients SET display_name='changed' WHERE id=?`, f.a); err != nil {
		t.Fatal(err)
	}
	var busy, log, checkpointed int
	if err := f.db.Writer.QueryRow(`PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &log, &checkpointed); err != nil || busy != 0 {
		t.Fatal(busy, log, checkpointed, err)
	}
}
