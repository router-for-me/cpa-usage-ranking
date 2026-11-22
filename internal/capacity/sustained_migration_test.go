package capacity

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cpa-usage-ranking/internal/models"
)

// This exercises the actual CLI with a synthetic, explicitly pinned two-segment
// journal. The production binary remains pinned to the one audited C54 prefix.
func TestReviewedRetryMigrationRealCLI(t *testing.T) {
	if testing.Short() {
		t.Skip("builds CLI and exercises actual replay")
	}
	dir := t.TempDir()
	catalog := filepath.Join("..", "..", "config", "models.json")
	c, err := models.Load(catalog)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, m := range c.List() {
		names = append(names, m.Name)
	}
	day := time.Now().UTC().Format(time.DateOnly)
	var seed uint64
	for i := uint64(1); i < 1000; i++ {
		m, e := NewSustainedModel(day, names, 5, i)
		if e != nil {
			t.Fatal(e)
		}
		a, e := m.Next()
		if e != nil {
			t.Fatal(e)
		}
		b, e := m.Next()
		if e != nil {
			t.Fatal(e)
		}
		otherPending, e := m.Next()
		if e != nil {
			t.Fatal(e)
		}
		if a.Client != b.Client && otherPending.Client != a.Client && otherPending.Client != b.Client {
			seed = i
			break
		}
	}
	if seed == 0 {
		t.Fatal("no suitable model seed")
	}
	seedDB := filepath.Join(dir, "seed.sqlite")
	fixture, err := SeedSustained(context.Background(), seedDB, catalog, 5, 2, 0, day, seed)
	if err != nil {
		t.Fatal(err)
	}
	fixturePath := filepath.Join(dir, "fixture.json")
	data, _ := json.Marshal(fixture)
	if err = os.WriteFile(fixturePath, data, 0600); err != nil {
		t.Fatal(err)
	}
	model, err := NewSustainedModel(day, names, fixture.Clients, seed)
	if err != nil {
		t.Fatal(err)
	}
	first, err := model.Next()
	if err != nil {
		t.Fatal(err)
	}
	retained, err := model.Next()
	if err != nil {
		t.Fatal(err)
	}
	otherPending, err := model.Next()
	if err != nil {
		t.Fatalf("missing second pending report: %+v %v", otherPending, err)
	}
	received := make(chan error, 1)
	retried := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, e := io.ReadAll(r.Body)
		if e == nil && r.Method == http.MethodPut && bytes.Equal(body, otherPending.Body) &&
			r.Header.Get("Authorization") == "Bearer "+fixture.Secret(otherPending.Client) {
			select {
			case retried <- nil:
			default:
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"revision": fmt.Sprint(otherPending.Revision)})
			return
		}
		if e == nil && (r.Method != http.MethodPut || !bytes.Equal(body, retained.Body) || r.Header.Get("Authorization") != "Bearer "+fixture.Secret(retained.Client)) {
			e = fmt.Errorf("retry changed identity, method or body")
		}
		select {
		case received <- e:
		default:
		}
		if e != nil {
			w.WriteHeader(400)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"revision": fmt.Sprint(retained.Revision)})
	}))
	defer server.Close()
	journal := filepath.Join(dir, "old.jsonl.gz")
	opts := SustainedOptions{URL: server.URL, Journal: journal, Models: names, ModelSeed: seed, ArrivalSeed: 23,
		MaxPending: 125000, MaxInFlight: 4, MaxAttempts: 30, MaxJournalBytes: 4 << 30,
		RequestTimeout: 20 * time.Second, MaxArrivalLag: 5 * time.Second, MaxActive: time.Hour}
	file, err := os.Create(journal)
	if err != nil {
		t.Fatal(err)
	}
	z := gzip.NewWriter(file)
	enc := json.NewEncoder(z)
	now := time.Now().Add(-time.Minute).UnixNano()
	write := func(r sustainedRecord) {
		t.Helper()
		if err := enc.Encode(r); err != nil {
			t.Fatal(err)
		}
		now += int64(time.Millisecond)
	}
	write(sustainedRecord{Kind: "header", TimeUnixNS: now, Header: ptrHeader(sustainedHeaderFor(fixture, opts))})
	write(sustainedRecord{Kind: "stage", TimeUnixNS: now, Command: &SustainedCommand{Operation: "stage", Stage: "old-1", Rate: 0.6}})
	write(sustainedRecord{Kind: "created", TimeUnixNS: now, PlannedUnixNS: now, Source: "debt", Task: &first})
	write(sustainedRecord{Kind: "created", TimeUnixNS: now, PlannedUnixNS: now, Source: "debt", Task: &retained})
	write(sustainedRecord{Kind: "created", TimeUnixNS: now, PlannedUnixNS: now, Source: "debt", Task: &otherPending})
	write(sustainedRecord{Kind: "start", TimeUnixNS: now, Sequence: otherPending.Sequence, Client: otherPending.Client, Action: "report", Attempt: 1})
	write(sustainedRecord{Kind: "finish", TimeUnixNS: now, Sequence: otherPending.Sequence, Client: otherPending.Client, Action: "report", Attempt: 1, Error: "canceled"})
	write(sustainedRecord{Kind: "start", TimeUnixNS: now, Sequence: first.Sequence, Client: first.Client, Action: "report", Attempt: 1})
	write(sustainedRecord{Kind: "finish", TimeUnixNS: now, Sequence: first.Sequence, Client: first.Client, Action: "report", Attempt: 1, Status: 200, Confirmed: true, AckRevision: fmt.Sprint(first.Revision)})
	var lastFinishNS int64
	attempt := func(i int) {
		write(sustainedRecord{Kind: "start", TimeUnixNS: now, Sequence: retained.Sequence, Client: retained.Client, Action: "report", Attempt: i})
		r := sustainedRecord{Kind: "finish", TimeUnixNS: now, Sequence: retained.Sequence, Client: retained.Client, Action: "report", Attempt: i, Status: 503, ErrorCode: "report_overloaded"}
		if i == 30 {
			r.Error = "retry_attempt_limit"
			r.RetryAfterNS = int64(20 * time.Second)
			lastFinishNS = r.TimeUnixNS
		}
		write(r)
	}
	for i := 1; i <= 9; i++ {
		attempt(i)
	}
	write(sustainedRecord{Kind: "stage", TimeUnixNS: now, Command: &SustainedCommand{Operation: "pause", Stage: "old-1"}})
	write(sustainedRecord{Kind: "end", TimeUnixNS: now, Reason: "context_canceled"})
	write(sustainedRecord{Kind: "stage", TimeUnixNS: now, Command: &SustainedCommand{Operation: "stage", Stage: "old-2", Rate: 0.01}})
	for i := 10; i <= 30; i++ {
		attempt(i)
	}
	write(sustainedRecord{Kind: "stage", TimeUnixNS: now, Command: &SustainedCommand{Operation: "pause", Stage: "old-2"}})
	write(sustainedRecord{Kind: "end", TimeUnixNS: now, Reason: "http_or_journal_failure"})
	if err = z.Close(); err != nil {
		t.Fatal(err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	old, err := os.ReadFile(journal)
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(old)
	digest := hex.EncodeToString(h[:])
	previousPin := c54RetryPrefixSHA
	c54RetryPrefixSHA = digest
	defer func() { c54RetryPrefixSHA = previousPin }()
	m := sustainedRetryMigration{Approval: c54RetryApproval, PreviousJournalSHA: digest, PreviousBytes: int64(len(old)),
		PreviousBudgetSHA: c54RetryBudgetSHA, PreviousConfigHash: c54RetryOldConfigHash, NextConfigHash: c54PendingOldConfigHash,
		PreviousBinarySHA: c54RetryOldBinarySHA, NextBinarySHA: c54PendingOldBinarySHA, PreviousAttempts: 30, NextAttempts: 512,
		PreviousPending: 125000, NextPending: 125000, TaskSequence: retained.Sequence, TaskClient: retained.Client,
		TaskRevision: retained.Revision, TaskDay: retained.Day, TaskBodySHA: retained.BodyHash, TaskAttempts: 30, NextTaskAttempt: 31,
		TaskFinishNS: lastFinishNS, TaskRetryAfterNS: int64(20 * time.Second), TaskNotBeforeNS: lastFinishNS + int64(20*time.Second)}
	manifest := filepath.Join(dir, "manifest.json")
	data, _ = json.Marshal(m)
	if err = os.WriteFile(manifest, data, 0600); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(dir, "ranking-capacity")
	build := exec.Command("go", "build", "-ldflags", "-X cpa-usage-ranking/internal/capacity.c54RetryPrefixSHA="+digest, "-o", binary, "../../cmd/ranking-capacity")
	if output, e := build.CombinedOutput(); e != nil {
		t.Fatalf("build real CLI: %v %s", e, output)
	}
	args := func(path string, maxPending, maxAttempts string, extra ...string) []string {
		return append([]string{"sustained", "--fixture", fixturePath, "--models", catalog, "--journal", path, "--url", server.URL,
			"--model-seed", fmt.Sprint(seed), "--arrival-seed", "23", "--max-pending", maxPending, "--max-in-flight", "4",
			"--max-attempts", maxAttempts, "--max-journal-bytes", fmt.Sprint(int64(4) << 30), "--active-budget", "1h", "--resume"}, extra...)
	}
	// No migration flag or a changed/truncated prefix must fail without network.
	bad := exec.Command(binary, args(journal, "100000", "512")...)
	bad.Stdin = strings.NewReader("")
	if out, e := bad.CombinedOutput(); e == nil || !bytes.Contains(out, []byte("configuration mismatch")) {
		t.Fatalf("unauthorized change accepted: %v %s", e, out)
	}
	truncated := filepath.Join(dir, "truncated.jsonl.gz")
	if err = os.WriteFile(truncated, old[:len(old)-1], 0600); err != nil {
		t.Fatal(err)
	}
	bad = exec.Command(binary, args(truncated, "125000", "512", "--reviewed-retry-migration", manifest)...)
	bad.Stdin = strings.NewReader("")
	if out, e := bad.CombinedOutput(); e == nil || !bytes.Contains(out, []byte("prefix")) {
		t.Fatalf("truncated prefix accepted: %v %s", e, out)
	}
	bad = exec.Command(binary, args(journal, "125000", "512", "--reviewed-retry-migration", manifest, "--max-in-flight", "5")...)
	bad.Stdin = strings.NewReader("")
	if out, e := bad.CombinedOutput(); e == nil || !bytes.Contains(out, []byte("configuration mismatch")) {
		t.Fatalf("unapproved non-retry config change accepted: %v %s", e, out)
	}
	// Old 30 mode keeps the exhausted count and must not issue a 31st HTTP call.
	oldCopy := filepath.Join(dir, "old30.jsonl.gz")
	if err = os.WriteFile(oldCopy, old, 0600); err != nil {
		t.Fatal(err)
	}
	oldCmd := exec.Command(binary, args(oldCopy, "125000", "30")...)
	oldIn, e := oldCmd.StdinPipe()
	if e != nil {
		t.Fatal(e)
	}
	var oldOut bytes.Buffer
	oldCmd.Stdout = &oldOut
	oldCmd.Stderr = &oldOut
	if e = oldCmd.Start(); e != nil {
		t.Fatal(e)
	}
	fmt.Fprintln(oldIn, `{"operation":"stage","stage":"old-limit","rate":0.01}`)
	if e = oldCmd.Wait(); e == nil || !bytes.Contains(oldOut.Bytes(), []byte("retry attempt limit")) {
		t.Fatalf("old limit not enforced: %v %s", e, oldOut.String())
	}
	_ = oldIn.Close()
	select {
	case e := <-received:
		t.Fatalf("old limit sent a request: %v", e)
	default:
	}
	// The approved path restores the same task and makes exactly attempt 31.
	samples := filepath.Join(dir, "samples.jsonl")
	cmd := exec.Command(binary, args(journal, "125000", "512", "--reviewed-retry-migration", manifest, "--samples", samples)...)
	stdin, e := cmd.StdinPipe()
	if e != nil {
		t.Fatal(e)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	}()
	fmt.Fprintln(stdin, `{"operation":"stage","stage":"new","rate":0.01}`)
	select {
	case e = <-received:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no 31st request")
	}
	select {
	case e = <-retried:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ambiguous report was not retried with the same secret and body")
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if b, e := os.ReadFile(samples); e == nil && bytes.Contains(b, []byte(`"confirmed":3`)) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	fmt.Fprintln(stdin, `{"operation":"stop"}`)
	_ = stdin.Close()
	if err = cmd.Wait(); err != nil {
		t.Fatalf("CLI stop: %v stderr=%s", err, stderr.String())
	}
	var result SustainedResult
	if err = json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Sample.Produced != 3 || result.Sample.Confirmed != 3 || result.Sample.Pending != 0 {
		t.Fatalf("replay lost or duplicated ACK: %+v", result)
	}
	f, err := os.Open(journal)
	if err != nil {
		t.Fatal(err)
	}
	source, _, err := sustainedJournalReader(f)
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(source)
	var starts, confirmed int
	for {
		var r sustainedRecord
		e := dec.Decode(&r)
		if e == io.EOF {
			break
		}
		if e != nil {
			t.Fatal(e)
		}
		if r.Kind == "start" && r.Sequence == retained.Sequence && r.Attempt == 31 {
			starts++
		}
		if r.Kind == "finish" && r.Sequence == retained.Sequence && r.Attempt == 31 && r.Confirmed {
			confirmed++
		}
	}
	_ = source.Close()
	_ = f.Close()
	if starts != 1 || confirmed != 1 {
		t.Fatalf("expected one attempt 31 and ACK: %d/%d", starts, confirmed)
	}
	if _, err := SummarizeSustained(journal, names, 100000); err == nil {
		t.Fatal("summary accepted migrated journal without approved manifest")
	}
	stats, err := SummarizeSustained(journal, names, 100000, manifest)
	if err != nil || stats.Confirmed != 3 {
		t.Fatalf("summary migration: %+v %v", stats, err)
	}
	seedConn, err := readDB(seedDB)
	if err != nil {
		t.Fatal(err)
	}
	defer seedConn.Close()
	state, err := newVerificationState(context.Background(), fixture, seedConn)
	if err != nil {
		t.Fatal(err)
	}
	if err = readSustainedJournal(journal, fixture, SustainedVerifyOptions{Models: names, ModelSeed: seed, RetryMigrationPath: manifest}, state); err != nil || state.confirmed != 3 {
		t.Fatalf("verify migrated journal: confirmed=%d %v", state.confirmed, err)
	}
	// A forged marker and an attempt 31 without a marker are rejected by
	// both descriptive summary and the independent verification reader.
	for _, tc := range []struct {
		name   string
		record sustainedRecord
	}{
		{name: "forged", record: sustainedRecord{Kind: "retry_migration", RetryMigration: func() *sustainedRetryMigration { x := m; x.NextAttempts = 513; return &x }()}},
		{name: "different-config-hash", record: sustainedRecord{Kind: "retry_migration", RetryMigration: func() *sustainedRetryMigration { x := m; x.NextConfigHash = strings.Repeat("c", 64); return &x }()}},
		{name: "shortened-retry-after", record: sustainedRecord{Kind: "retry_migration", RetryMigration: func() *sustainedRetryMigration { x := m; x.TaskNotBeforeNS--; return &x }()}},
		{name: "no-marker", record: sustainedRecord{Kind: "start", Sequence: retained.Sequence, Client: retained.Client, Action: "report", Attempt: 31}},
	} {
		path := filepath.Join(dir, tc.name+".jsonl.gz")
		f, e := os.Create(path)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = f.Write(old); e != nil {
			t.Fatal(e)
		}
		z := gzip.NewWriter(f)
		enc := json.NewEncoder(z)
		if e = enc.Encode(tc.record); e != nil {
			t.Fatal(e)
		}
		if e = enc.Encode(sustainedRecord{Kind: "end"}); e != nil {
			t.Fatal(e)
		}
		if e = z.Close(); e != nil {
			t.Fatal(e)
		}
		if e = f.Close(); e != nil {
			t.Fatal(e)
		}
		if _, e = SummarizeSustained(path, names, 125000, manifest); e == nil {
			t.Fatalf("summary accepted %s", tc.name)
		}
		v, e := newVerificationState(context.Background(), fixture, seedConn)
		if e != nil {
			t.Fatal(e)
		}
		if e = readSustainedJournal(path, fixture, SustainedVerifyOptions{Models: names, ModelSeed: seed, RetryMigrationPath: manifest}, v); e == nil {
			t.Fatalf("verification accepted %s", tc.name)
		}
	}

	// The second, separate change keeps the 512 retry count and every task,
	// while reducing only the pending ceiling at the third clean end.
	prefix, err := os.ReadFile(journal)
	if err != nil {
		t.Fatal(err)
	}
	ph := sha256.Sum256(prefix)
	prefixDigest := hex.EncodeToString(ph[:])
	previousPendingPin := c54PendingPrefixSHA
	c54PendingPrefixSHA = prefixDigest
	defer func() { c54PendingPrefixSHA = previousPendingPin }()
	pm := sustainedPendingMigration{Approval: c54PendingApproval, PreviousJournalSHA: prefixDigest, PreviousBytes: int64(len(prefix)),
		PreviousBudgetSHA: c54PendingBudgetSHA, ReservedBudgetSHA: c54PendingReservedSHA,
		PreviousConfigHash: c54PendingOldConfigHash, NextConfigHash: strings.Repeat("c", 64),
		PreviousBinarySHA: c54PendingOldBinarySHA, NextBinarySHA: strings.Repeat("d", 64),
		PreviousPending: 125000, NextPending: 100000, MaxAttempts: 512, Produced: 3, Confirmed: 3, Pending: 0}
	pendingManifest := filepath.Join(dir, "pending-manifest.json")
	data, _ = json.Marshal(pm)
	if err = os.WriteFile(pendingManifest, data, 0600); err != nil {
		t.Fatal(err)
	}
	ldflags := "-X cpa-usage-ranking/internal/capacity.c54RetryPrefixSHA=" + digest + " -X cpa-usage-ranking/internal/capacity.c54PendingPrefixSHA=" + prefixDigest
	build = exec.Command("go", "build", "-ldflags", ldflags, "-o", binary, "../../cmd/ranking-capacity")
	if output, e := build.CombinedOutput(); e != nil {
		t.Fatalf("build pending CLI: %v %s", e, output)
	}
	pendingArgs := args(journal, "100000", "512", "--reviewed-retry-migration", manifest, "--reviewed-pending-migration", pendingManifest)
	bad = exec.Command(binary, args(journal, "100000", "512", "--reviewed-retry-migration", manifest)...)
	bad.Stdin = strings.NewReader("")
	if out, e := bad.CombinedOutput(); e == nil {
		t.Fatalf("unapproved pending decrease accepted: %s", out)
	}
	bad = exec.Command(binary, append(append([]string{}, pendingArgs...), "--max-in-flight", "5")...)
	bad.Stdin = strings.NewReader("")
	if out, e := bad.CombinedOutput(); e == nil || !bytes.Contains(out, []byte("configuration mismatch")) {
		t.Fatalf("unapproved pending-era header change accepted: %v %s", e, out)
	}
	badManifest := filepath.Join(dir, "bad-pending.json")
	forgedManifest := pm
	forgedManifest.PreviousBudgetSHA = strings.Repeat("e", 64)
	data, _ = json.Marshal(forgedManifest)
	if err = os.WriteFile(badManifest, data, 0600); err != nil {
		t.Fatal(err)
	}
	bad = exec.Command(binary, args(journal, "100000", "512", "--reviewed-retry-migration", manifest, "--reviewed-pending-migration", badManifest)...)
	bad.Stdin = strings.NewReader("")
	if out, e := bad.CombinedOutput(); e == nil {
		t.Fatalf("unapproved budget accepted: %s", out)
	}
	paused := exec.Command(binary, pendingArgs...)
	paused.Stdin = strings.NewReader("")
	if out, e := paused.CombinedOutput(); e != nil {
		t.Fatalf("pending paused replay: %v %s", e, out)
	}
	if _, e := SummarizeSustained(journal, names, 100000, manifest); e == nil {
		t.Fatal("summary accepted unapproved pending marker")
	}
	stats, err = SummarizeSustained(journal, names, 100000, manifest, pendingManifest)
	if err != nil || stats.Produced != 3 || stats.Confirmed != 3 || stats.Pending != 0 {
		t.Fatalf("pending summary: %+v %v", stats, err)
	}
	v, err := newVerificationState(context.Background(), fixture, seedConn)
	if err != nil {
		t.Fatal(err)
	}
	if err = readSustainedJournal(journal, fixture, SustainedVerifyOptions{Models: names, ModelSeed: seed, RetryMigrationPath: manifest, PendingMigrationPath: pendingManifest}, v); err != nil || v.confirmed != 3 {
		t.Fatalf("pending verification: %v", err)
	}
	forgedPath := filepath.Join(dir, "forged-pending.jsonl.gz")
	f, err = os.Create(forgedPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.Write(prefix); err != nil {
		t.Fatal(err)
	}
	z = gzip.NewWriter(f)
	enc = json.NewEncoder(z)
	forged := pm
	forged.NextPending = 99999
	if err = enc.Encode(sustainedRecord{Kind: "pending_migration", PendingMigration: &forged}); err != nil {
		t.Fatal(err)
	}
	if err = enc.Encode(sustainedRecord{Kind: "end"}); err != nil {
		t.Fatal(err)
	}
	if err = z.Close(); err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = SummarizeSustained(forgedPath, names, 100000, manifest, pendingManifest); err == nil {
		t.Fatal("summary accepted forged pending marker")
	}
	v, err = newVerificationState(context.Background(), fixture, seedConn)
	if err != nil {
		t.Fatal(err)
	}
	if err = readSustainedJournal(forgedPath, fixture, SustainedVerifyOptions{Models: names, ModelSeed: seed, RetryMigrationPath: manifest, PendingMigrationPath: pendingManifest}, v); err == nil {
		t.Fatal("verification accepted forged pending marker")
	}
}

func ptrHeader(h sustainedHeader) *sustainedHeader { return &h }
