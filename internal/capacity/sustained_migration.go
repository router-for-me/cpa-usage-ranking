package capacity

// This is a single explicitly reviewed C54 journal transition, not automatic
// recovery of failed budgets or a general permission to change replay options.
import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"time"
)

const c54RetryApproval = "c54-721cb71e50f60575-retry-30-to-512"
const c54RetryBudgetSHA = "dc40ff7f5adb81424a5dacd948ecddb9edfac6528106b04f27f4643a2a9768ae"
const c54RetryOldConfigHash = "c9fe089a5bfe2ab3fa0079dfe30d07aeeb247a1f55eaea6f9203ec19bfe77bf8"
const c54RetryOldBinarySHA = "fbebd0bca029514e0a3728e238fe0986301ea7a23ecd3d704dbb8443e062546a"

// A variable solely to let the isolated CLI integration test build its own
// pinned synthetic prefix. The normal binary pins the parent-audited journal.
const c54RealRetryPrefixSHA = "d08cb32d0c2b0dfc7bf0589d80ba6142a6d9be44c8a84c54bab922830a851c6e"

var c54RetryPrefixSHA = c54RealRetryPrefixSHA

const c54PendingApproval = "c54-721cb71e50f60575-pending-125000-to-100000"
const c54PendingBudgetSHA = "aef1d32c67ba9ee27c6a024a0c62ccdfa1d6d358f1b95e4e878911fa206d9abd"
const c54PendingReservedSHA = "76f82e013e26318ee23f7161e3e40187ac927f7e95dcede9c85f4e115199af9d"
const c54PendingOldConfigHash = "ef9b5227da220e555079b1dd2d09bacd11b2af3e80ffafa75da796ea8e72cd5b"
const c54PendingOldBinarySHA = "a56cfa16fba285e31e4d364138d6569838ecea682e8096e0964a5291302b3f1b"

var c54PendingPrefixSHA = "190c540a1b691be86e8d3f4183ceaf362c22cdd42fe8fe6ab5586d48ed6018f1"

type sustainedRetryMigration struct {
	Approval           string `json:"approval"`
	PreviousJournalSHA string `json:"previous_journal_sha256"`
	PreviousBytes      int64  `json:"previous_journal_bytes"`
	PreviousBudgetSHA  string `json:"previous_budget_sha256"`
	PreviousConfigHash string `json:"previous_config_hash"`
	NextConfigHash     string `json:"next_config_hash"`
	PreviousBinarySHA  string `json:"previous_binary_sha256"`
	NextBinarySHA      string `json:"next_binary_sha256"`
	PreviousAttempts   int    `json:"previous_max_attempts"`
	NextAttempts       int    `json:"next_max_attempts"`
	PreviousPending    int    `json:"previous_max_pending"`
	NextPending        int    `json:"next_max_pending"`
	TaskSequence       int64  `json:"task_sequence"`
	TaskClient         int    `json:"task_client"`
	TaskRevision       int64  `json:"task_revision"`
	TaskDay            string `json:"task_day"`
	TaskBodySHA        string `json:"task_body_sha256"`
	TaskAttempts       int    `json:"task_attempts"`
	NextTaskAttempt    int    `json:"next_task_attempt"`
	TaskFinishNS       int64  `json:"task_finish_unix_ns"`
	TaskRetryAfterNS   int64  `json:"task_retry_after_ns"`
	TaskNotBeforeNS    int64  `json:"task_not_before_unix_ns"`
}

type sustainedPendingMigration struct {
	Approval           string `json:"approval"`
	PreviousJournalSHA string `json:"previous_journal_sha256"`
	PreviousBytes      int64  `json:"previous_journal_bytes"`
	PreviousBudgetSHA  string `json:"previous_budget_sha256"`
	ReservedBudgetSHA  string `json:"reserved_budget_sha256"`
	PreviousConfigHash string `json:"previous_config_hash"`
	NextConfigHash     string `json:"next_config_hash"`
	PreviousBinarySHA  string `json:"previous_binary_sha256"`
	NextBinarySHA      string `json:"next_binary_sha256"`
	PreviousPending    int    `json:"previous_max_pending"`
	NextPending        int    `json:"next_max_pending"`
	MaxAttempts        int    `json:"max_attempts"`
	Produced           int64  `json:"produced"`
	Confirmed          int64  `json:"confirmed"`
	Pending            int64  `json:"pending"`
}

func (m *sustainedPendingMigration) validate(file *os.File) error {
	if m == nil || m.Approval != c54PendingApproval || m.PreviousJournalSHA != c54PendingPrefixSHA ||
		m.PreviousBudgetSHA != c54PendingBudgetSHA || m.ReservedBudgetSHA != c54PendingReservedSHA ||
		m.PreviousConfigHash != c54PendingOldConfigHash || m.PreviousBinarySHA != c54PendingOldBinarySHA ||
		!validSHA(m.NextConfigHash) || !validSHA(m.NextBinarySHA) || m.PreviousPending != 125000 ||
		m.NextPending != 100000 || m.MaxAttempts != 512 || m.PreviousBytes <= 0 ||
		m.Produced < 1 || m.Confirmed < 0 || m.Pending != m.Produced-m.Confirmed || m.Pending > int64(m.NextPending) {
		return errors.New("unapproved C54 pending-limit migration")
	}
	if c54PendingPrefixSHA == "190c540a1b691be86e8d3f4183ceaf362c22cdd42fe8fe6ab5586d48ed6018f1" &&
		(m.Produced != 456569 || m.Confirmed != 440239 || m.Pending != 16330) {
		return errors.New("reviewed pending-limit totals differ from the audited transition")
	}
	h := sha256.New()
	n, err := io.Copy(h, io.NewSectionReader(file, 0, m.PreviousBytes))
	if err != nil || n != m.PreviousBytes || hex.EncodeToString(h.Sum(nil)) != m.PreviousJournalSHA {
		return errors.New("reviewed pending-limit journal prefix does not match")
	}
	return nil
}

func loadSustainedPendingMigration(path string, file *os.File) (*sustainedPendingMigration, error) {
	if path == "" {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	dec := json.NewDecoder(io.LimitReader(f, 8193))
	dec.DisallowUnknownFields()
	var m sustainedPendingMigration
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("invalid pending migration manifest: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, errors.New("trailing pending migration manifest data")
	}
	if err := m.validate(file); err != nil {
		return nil, err
	}
	return &m, nil
}

func validSHA(s string) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == 32 && hex.EncodeToString(b) == s
}

func (m *sustainedRetryMigration) validate(file *os.File) error {
	if m == nil || m.Approval != c54RetryApproval || m.PreviousJournalSHA != c54RetryPrefixSHA ||
		m.PreviousBudgetSHA != c54RetryBudgetSHA || m.PreviousConfigHash != c54RetryOldConfigHash ||
		m.PreviousBinarySHA != c54RetryOldBinarySHA || !validSHA(m.NextConfigHash) || !validSHA(m.NextBinarySHA) ||
		m.PreviousAttempts != 30 || m.NextAttempts != 512 || m.PreviousPending != 125000 ||
		(m.NextPending != 125000 && m.NextPending != 100000) || m.PreviousBytes <= 0 ||
		m.TaskSequence <= 0 || m.TaskClient <= 0 || m.TaskRevision <= 0 || m.TaskDay == "" || !validSHA(m.TaskBodySHA) ||
		m.TaskAttempts != 30 || m.NextTaskAttempt != 31 || m.TaskFinishNS <= 0 || m.TaskRetryAfterNS < 0 {
		return errors.New("unapproved C54 retry migration")
	}
	delay := sustainedBackoff(0, m.TaskClient, m.TaskAttempts, time.Duration(m.TaskRetryAfterNS))
	if int64(delay) < 0 || m.TaskFinishNS > math.MaxInt64-int64(delay) || m.TaskNotBeforeNS != m.TaskFinishNS+int64(delay) {
		return errors.New("reviewed retry backoff or Retry-After was changed")
	}
	if c54RetryPrefixSHA == c54RealRetryPrefixSHA &&
		(m.TaskSequence != 130422 || m.TaskClient != 9034 || m.TaskRevision != 54 || m.TaskDay != "2026-11-13" ||
			m.TaskBodySHA != "ad6e280211e23aa21566bdad0074a0045452d71dbb1ced4d748aea1b21448a1a" ||
			m.TaskFinishNS != 1794560887575625142 || m.TaskRetryAfterNS != 1000000000) {
		return errors.New("reviewed retry task identity differs from audited task")
	}
	h := sha256.New()
	n, err := io.Copy(h, io.NewSectionReader(file, 0, m.PreviousBytes))
	if err != nil || n != m.PreviousBytes || hex.EncodeToString(h.Sum(nil)) != m.PreviousJournalSHA {
		return errors.New("reviewed journal prefix does not match")
	}
	return nil
}

func verifyReviewedRetryTask(m *sustainedRetryMigration, state *sustainedState) error {
	a := state.actors[m.TaskClient]
	if a == nil || a.inFlight || len(a.queue) == 0 {
		return errors.New("reviewed retry task is not an idle report queue head")
	}
	job := a.queue[0]
	if job.task.Sequence != m.TaskSequence || job.task.Revision != m.TaskRevision || job.task.Day != m.TaskDay ||
		job.task.BodyHash != m.TaskBodySHA || job.attempts != m.TaskAttempts {
		return errors.New("reviewed retry task identity, body or cumulative attempts changed")
	}
	if a.retryAt.IsZero() || a.retryAt.UnixNano() < m.TaskNotBeforeNS {
		a.retryAt = time.Unix(0, m.TaskNotBeforeNS)
	}
	return nil
}

func loadSustainedRetryMigration(path string, file *os.File) (*sustainedRetryMigration, error) {
	if path == "" {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	dec := json.NewDecoder(io.LimitReader(f, 8193))
	dec.DisallowUnknownFields()
	var m sustainedRetryMigration
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("invalid retry migration manifest: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, errors.New("trailing retry migration manifest data")
	}
	if err := m.validate(file); err != nil {
		return nil, err
	}
	return &m, nil
}
