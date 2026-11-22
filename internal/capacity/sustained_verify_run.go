package capacity

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"time"
)

type SustainedVerifyOptions struct {
	SeedDB, FinalDB, Journal, ExpectedDB     string
	RetryMigrationPath, PendingMigrationPath string
	Models                                   []string
	ModelSeed                                uint64
}

func noLiveWAL(path string) error {
	info, err := os.Stat(path + "-wal")
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil && info.Size() != 0 {
		return errors.New("database has a nonempty WAL; take a stopped, consistent snapshot first")
	}
	return nil
}

func (s *verificationState) stageRecord(r sustainedRecord) error {
	if r.Command == nil || r.TimeUnixNS == 0 {
		return errors.New("missing stage command")
	}
	c := r.Command
	switch c.Operation {
	case "stage":
		if s.debtRemaining != 0 || c.Stage == "" || c.Rate <= 0 || c.Rate > 100000 || math.IsNaN(c.Rate) || math.IsInf(c.Rate, 0) {
			return errors.New("invalid stage transition")
		}
		if s.rate == 0 {
			s.activeSince = r.TimeUnixNS
		}
		s.stage, s.rate = c.Stage, c.Rate
		s.debtRemaining = int(math.Round(5 * c.Rate))
	case "pause":
		if s.rate == 0 || r.TimeUnixNS < s.activeSince {
			return errors.New("invalid pause")
		}
		s.activeElapsed += time.Duration(r.TimeUnixNS - s.activeSince)
		s.activeSince = 0
		s.rate = 0
	case "resume":
		if s.rate != 0 || c.Stage != s.stage || c.Rate <= 0 || c.Rate > 100000 {
			return errors.New("invalid resume")
		}
		s.activeSince = r.TimeUnixNS
		s.rate = c.Rate
	default:
		return errors.New("unknown stage command")
	}
	return nil
}

func readSustainedJournal(path string, f Fixture, o SustainedVerifyOptions, state *verificationState) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	authorized, err := loadSustainedRetryMigration(o.RetryMigrationPath, file)
	if err != nil {
		return err
	}
	pendingAuthorized, err := loadSustainedPendingMigration(o.PendingMigrationPath, file)
	if err != nil {
		return err
	}
	if pendingAuthorized != nil && (authorized == nil || authorized.NextPending != pendingAuthorized.PreviousPending || authorized.NextAttempts != pendingAuthorized.MaxAttempts ||
		authorized.NextConfigHash != pendingAuthorized.PreviousConfigHash || authorized.NextBinarySHA != pendingAuthorized.PreviousBinarySHA) {
		return errors.New("pending verification manifest does not follow retry manifest")
	}
	source, _, err := sustainedJournalReader(file)
	if err != nil {
		return err
	}
	defer source.Close()
	reader := bufio.NewReaderSize(source, 256*1024)
	line, endCount := 0, 0
	lastKind := ""
	var header *sustainedHeader
	migrated, pendingMigrated := false, false
	for {
		data, readErr := readSustainedLine(reader)
		if len(data) == 0 && readErr == io.EOF {
			break
		}
		line++
		if readErr != nil || len(data) > 2<<20 {
			return fmt.Errorf("incomplete or oversized journal record at line %d: %v", line, readErr)
		}
		var r sustainedRecord
		if json.Unmarshal(data, &r) != nil {
			return fmt.Errorf("invalid journal JSON at line %d", line)
		}
		previousKind := lastKind
		lastKind = r.Kind
		if line == 1 {
			catalog, _ := json.Marshal(o.Models)
			catalogHash := sha256.Sum256(catalog)
			credentialHash := sha256.Sum256([]byte(f.Seed))
			if r.Kind != "header" || r.Header == nil || r.Header.Version != SustainedModelVersion || r.Header.Day != f.Day || r.Header.Stock != f.Clients || r.Header.ModelSeed != o.ModelSeed || r.Header.CatalogHash != hex.EncodeToString(catalogHash[:]) || r.Header.CredentialSeedHash != hex.EncodeToString(credentialHash[:]) || r.Header.MaxAttempts < 1 || r.Header.MaxAttempts > 512 || r.Header.MaxActiveNS <= 0 || r.Header.MaxActiveNS > int64(time.Hour) {
				return errors.New("journal header does not match seed, model, catalog, or safety limit")
			}
			header = r.Header
			state.maxAttempts = header.MaxAttempts
			continue
		}
		var e error
		switch r.Kind {
		case "stage":
			e = state.stageRecord(r)
		case "created":
			e = state.createdTask(r)
		case "start":
			e = state.start(r)
		case "finish":
			e = state.finish(r)
		case "sample":
			e = state.verifySample(r.Sample)
		case "recover":
			a := state.actors[r.Client]
			if a == nil || !a.inFlight || len(a.queue) == 0 || a.queue[0].task.Sequence != r.Sequence {
				e = errors.New("invalid ambiguous recovery")
			} else {
				a.queue[0].ambiguous = true
				a.inFlight = false
				a.notBefore = 0
			}
		case "end":
			endCount++
			if state.rate != 0 || state.activeSince != 0 {
				e = errors.New("active input not closed before end")
			}
		case "retry_migration":
			m := r.RetryMigration
			if migrated || previousKind != "end" || endCount != 2 || state.rate != 0 || state.activeSince != 0 || m == nil ||
				authorized == nil || *m != *authorized || header.MaxAttempts != m.PreviousAttempts || header.MaxPending != m.PreviousPending {
				e = errors.New("invalid retry migration boundary")
			} else if e = m.validate(file); e == nil {
				for _, a := range state.actors {
					if a.inFlight {
						e = errors.New("retry migration with in-flight attempts")
					}
				}
				if e == nil {
					a := state.actors[m.TaskClient]
					if a == nil || a.inFlight || len(a.queue) == 0 ||
						a.queue[0].task.Sequence != m.TaskSequence || a.queue[0].task.Revision != m.TaskRevision ||
						a.queue[0].task.Day != m.TaskDay || a.queue[0].task.BodyHash != m.TaskBodySHA || a.queue[0].attempts != m.TaskAttempts {
						e = errors.New("reviewed retry task identity or cumulative attempts changed")
					} else {
						if a.notBefore < m.TaskNotBeforeNS {
							a.notBefore = m.TaskNotBeforeNS
						}
						state.maxAttempts, migrated = m.NextAttempts, true
					}
				}
			}
		case "pending_migration":
			m := r.PendingMigration
			if pendingMigrated || !migrated || previousKind != "end" || endCount != 3 || state.rate != 0 || state.activeSince != 0 ||
				m == nil || pendingAuthorized == nil || *m != *pendingAuthorized || state.created != m.Produced ||
				state.confirmed != m.Confirmed || state.created-state.confirmed != m.Pending {
				e = errors.New("invalid pending migration boundary")
			} else if e = m.validate(file); e == nil {
				for _, a := range state.actors {
					if a.inFlight {
						e = errors.New("pending migration with in-flight attempts")
					}
				}
				if e == nil {
					pendingMigrated = true
				}
			}
		default:
			e = errors.New("unexpected journal record")
		}
		if e != nil {
			return fmt.Errorf("journal line %d (%s): %w", line, r.Kind, e)
		}
	}
	if authorized != nil && !migrated {
		return errors.New("authorized retry migration record missing from journal")
	}
	if pendingAuthorized != nil && !pendingMigrated {
		return errors.New("authorized pending migration record missing from journal")
	}
	if line == 0 || lastKind != "end" || state.rate != 0 || state.activeSince != 0 {
		return errors.New("journal not cleanly closed")
	}
	if state.activeElapsed > time.Hour {
		return errors.New("formal active-input budget exceeded")
	}
	for _, a := range state.actors {
		if a.inFlight {
			return errors.New("unfinished HTTP attempt at journal end")
		}
	}
	return nil
}

func syntheticHash(f Fixture, logical int) ([32]byte, error) {
	raw, err := base64.RawURLEncoding.DecodeString(f.Secret(logical))
	if err != nil || len(raw) != 32 {
		return [32]byte{}, errors.New("invalid synthetic credential")
	}
	return sha256.Sum256(raw), nil
}

func checkSyntheticWriters(ctx context.Context, seed *sql.DB, fixture Fixture) error {
	rows, err := seed.QueryContext(ctx, `SELECT id,client_id,token_hash FROM client_writers ORDER BY id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	logical := 0
	for rows.Next() {
		logical++
		var id, seat int
		var hash []byte
		if err := rows.Scan(&id, &seat, &hash); err != nil {
			return err
		}
		expected, err := syntheticHash(fixture, logical)
		if err != nil || id != logical || seat != logical || !bytes.Equal(hash, expected[:]) {
			return errors.New("invalid synthetic writer in seed")
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if logical != fixture.Clients {
		return errors.New("synthetic writer count mismatch")
	}
	return nil
}

// VerifySustained checks every journaled delta and attempt, then reconstructs
// the expected database in a NEW local file and compares all versions, daily
// metrics and period metrics row by row with a stopped consistent snapshot.
// Ambiguous unconfirmed commits are permitted only for the active front task
// of an identity and are reported separately. No existing DB is overwritten.
func VerifySustained(ctx context.Context, f Fixture, o SustainedVerifyOptions) (SustainedVerification, error) {
	var out SustainedVerification
	if err := f.Validate(); err != nil {
		return out, err
	}
	if o.SeedDB == "" || o.FinalDB == "" || o.Journal == "" || o.ExpectedDB == "" {
		return out, errors.New("verification needs seed, final, journal and new expected DB paths")
	}
	if err := noLiveWAL(o.SeedDB); err != nil {
		return out, fmt.Errorf("seed: %w", err)
	}
	if err := noLiveWAL(o.FinalDB); err != nil {
		return out, fmt.Errorf("final: %w", err)
	}
	seed, err := readDB(o.SeedDB)
	if err != nil {
		return out, err
	}
	defer seed.Close()
	final, err := readDB(o.FinalDB)
	if err != nil {
		return out, err
	}
	defer final.Close()
	var integrity string
	if err = final.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil || integrity != "ok" {
		return out, fmt.Errorf("final integrity check failed: %s %v", integrity, err)
	}
	state, err := newVerificationState(ctx, f, seed)
	if err != nil {
		return out, err
	}
	if err = readSustainedJournal(o.Journal, f, o, state); err != nil {
		return out, err
	}
	if err := checkSyntheticWriters(ctx, seed, f); err != nil {
		return out, err
	}
	var unconfirmed int64
	for logical, a := range state.actors {
		if len(a.queue) == 0 || a.queue[0].attempts == 0 || !a.queue[0].ambiguous {
			continue
		}
		job := a.queue[0]
		var rev int64
		var digest []byte
		e := final.QueryRowContext(ctx, "SELECT revision,digest FROM daily_writer_versions WHERE writer_id=? AND day=?", logical, job.task.Day).Scan(&rev, &digest)
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return out, e
		}
		if e == nil && rev == job.task.Revision {
			snapshot, _, e := snapshotRows(job.task.Body, state.modelIDs)
			if e != nil {
				return out, e
			}
			if !bytes.Equal(digest, snapshot.digest[:]) {
				return out, errors.New("ambiguous revision digest mismatch")
			}
			state.acks[verificationKey{client: logical, day: job.task.Day}] = snapshot
			unconfirmed++
		}
	}
	counts, err := compareSustainedDatabases(ctx, o.SeedDB, o.ExpectedDB, final, state)
	if err != nil {
		return out, err
	}
	out = SustainedVerification{Produced: state.created, Confirmed: state.confirmed, Pending: state.created - state.confirmed, UnconfirmedCommits: unconfirmed, CheckedClients: counts["clients"], CheckedDays: counts["daily_reports"], CheckedDailyRows: counts["daily_stats"], CheckedPeriodRows: counts["period_stats"], ClosedSourcesRemaining: counts["closed"], ExpectedDB: o.ExpectedDB}
	return out, nil
}
