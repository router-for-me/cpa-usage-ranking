package test

import (
	"reflect"
	"testing"
	"time"

	"cpa-usage-ranking/internal/config"
)

// Only the explicitly authorized admission rates differ. In particular the
// queue, decoding, item/body, TTL, cache and registration window remain the
// production defaults. This checks parsing; it does not claim a deployment ran.
func TestSustainedTestOnlyRateConfiguration(t *testing.T) {
	baseline, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{
		"RANKING_REPORT_GLOBAL_INTERVAL": "10us",
		"RANKING_REPORT_GLOBAL_BURST":    "100000",
		"RANKING_REPORT_CLIENT_INTERVAL": "100us",
		"RANKING_REPORT_CLIENT_BURST":    "1000",
	} {
		t.Setenv(k, v)
	}
	actual, err := config.Load()
	if err != nil {
		t.Fatalf("test-only rates rejected: %v", err)
	}
	if actual.ReportIngestion.GlobalInterval != 10*time.Microsecond || actual.ReportIngestion.GlobalBurst != 100000 || actual.ReportIngestion.ClientInterval != 100*time.Microsecond || actual.ReportIngestion.ClientBurst != 1000 {
		t.Fatal("test-only rates were not parsed")
	}
	actual.ReportIngestion.GlobalInterval = baseline.ReportIngestion.GlobalInterval
	actual.ReportIngestion.GlobalBurst = baseline.ReportIngestion.GlobalBurst
	actual.ReportIngestion.ClientInterval = baseline.ReportIngestion.ClientInterval
	actual.ReportIngestion.ClientBurst = baseline.ReportIngestion.ClientBurst
	if !reflect.DeepEqual(actual, baseline) {
		t.Fatalf("unexpected non-rate configuration difference: actual=%+v baseline=%+v", actual, baseline)
	}
}
