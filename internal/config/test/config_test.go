package test

import (
	"os"
	"strings"
	"testing"
	"time"

	"cpa-usage-ranking/internal/config"
)

func clearEnvironment(t *testing.T) {
	t.Helper()
	for _, key := range []string{"RANKING_HTTP_ADDR", "RANKING_SHUTDOWN_TIMEOUT", "RANKING_WEB_DIR", "RANKING_SQLITE_PATH", "RANKING_MODELS_PATH", "RANKING_TRUSTED_PROXIES", "RANKING_REGISTRATION_IPV6_64", "RANKING_REGISTRATION_SOURCE_REQUESTS", "RANKING_REGISTRATION_GLOBAL_REQUESTS", "RANKING_REGISTRATION_REQUEST_MAX_SOURCES", "RANKING_REGISTRATION_REQUEST_WINDOW", "RANKING_PUBLIC_ORIGIN", "RANKING_GITHUB_CLIENT_ID", "RANKING_GITHUB_CLIENT_SECRET", "RANKING_REGISTRATION_ENABLED", "RANKING_REGISTRATION_MAX_BODY_BYTES", "RANKING_ADMIN_CONFIG_FILE", "RANKING_ADMIN_ALLOW_INSECURE_LOCAL_HTTP"} {
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
	for _, key := range []string{"RANKING_REPORT_MAX_BODY_BYTES", "RANKING_REPORT_MAX_ITEMS", "RANKING_REPORT_DECODE_CONCURRENCY", "RANKING_REPORT_CLIENT_BURST", "RANKING_REPORT_GLOBAL_BURST", "RANKING_REPORT_MAX_CLIENTS", "RANKING_REPORT_QUEUE_CAPACITY", "RANKING_REPORT_CLIENT_INTERVAL", "RANKING_REPORT_GLOBAL_INTERVAL", "RANKING_REPORT_STATE_TTL", "RANKING_REPORT_TRANSACTION_TIMEOUT", "RANKING_SETTLED_DAY_RETENTION_DAYS", "RANKING_CACHE_REFRESH_INTERVAL"} {
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDefaults(t *testing.T) {
	clearEnvironment(t)
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTPAddr != "127.0.0.1:8080" || cfg.ShutdownTimeout != 10*time.Second || cfg.WebDir != "" || cfg.SQLitePath != "data/ranking.db" || cfg.ModelsPath != "config/models.json" || len(cfg.TrustedProxies) != 0 || !cfg.RegistrationIPv6By64 {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
	if cfg.ReportIngestion.MaxBodyBytes != 512<<10 || cfg.ReportIngestion.MaxItems != 1024 || cfg.ReportIngestion.ConcurrentDecodes != 16 || cfg.ReportIngestion.ClientInterval != 10*time.Second || cfg.ReportIngestion.ClientBurst != 10 || cfg.ReportWriter.QueueCapacity != 128 || cfg.DailyRetentionDays != 31 {
		t.Fatalf("unexpected report defaults: %+v %+v", cfg.ReportIngestion, cfg.ReportWriter)
	}
}

func TestOverrides(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:0", ":8080", "[::1]:65535", "localhost:9090"} {
		t.Run(addr, func(t *testing.T) {
			clearEnvironment(t)
			t.Setenv("RANKING_HTTP_ADDR", " "+addr+" ")
			t.Setenv("RANKING_SHUTDOWN_TIMEOUT", " 250ms ")
			t.Setenv("RANKING_WEB_DIR", " web/dist ")
			t.Setenv("RANKING_SQLITE_PATH", " custom/ranking.db ")
			t.Setenv("RANKING_MODELS_PATH", " custom/models.json ")
			t.Setenv("RANKING_TRUSTED_PROXIES", " 127.0.0.1, , 10.0.0.0/8,::1, ")
			t.Setenv("RANKING_REGISTRATION_IPV6_64", "false")
			cfg, err := config.Load()
			if err != nil || cfg.HTTPAddr != addr || cfg.ShutdownTimeout != 250*time.Millisecond || cfg.WebDir != "web/dist" || cfg.SQLitePath != "custom/ranking.db" || cfg.ModelsPath != "custom/models.json" || strings.Join(cfg.TrustedProxies, ",") != "127.0.0.1,10.0.0.0/8,::1" || cfg.RegistrationIPv6By64 {
				t.Fatalf("configuration = %+v, error = %v", cfg, err)
			}
		})
	}
}

func TestBlankValuesFallBackToDefaults(t *testing.T) {
	allKeys := []string{
		"RANKING_HTTP_ADDR", "RANKING_SHUTDOWN_TIMEOUT", "RANKING_WEB_DIR", "RANKING_SQLITE_PATH",
		"RANKING_MODELS_PATH", "RANKING_TRUSTED_PROXIES", "RANKING_REGISTRATION_IPV6_64",
		"RANKING_REGISTRATION_SOURCE_REQUESTS", "RANKING_REGISTRATION_GLOBAL_REQUESTS",
		"RANKING_REGISTRATION_REQUEST_MAX_SOURCES", "RANKING_REGISTRATION_REQUEST_WINDOW",
		"RANKING_PUBLIC_ORIGIN", "RANKING_GITHUB_CLIENT_ID", "RANKING_GITHUB_CLIENT_SECRET",
		"RANKING_REGISTRATION_ENABLED", "RANKING_REGISTRATION_MAX_BODY_BYTES",
		"RANKING_REPORT_MAX_BODY_BYTES", "RANKING_REPORT_MAX_ITEMS", "RANKING_REPORT_DECODE_CONCURRENCY",
		"RANKING_REPORT_CLIENT_BURST", "RANKING_REPORT_GLOBAL_BURST", "RANKING_REPORT_MAX_CLIENTS",
		"RANKING_REPORT_QUEUE_CAPACITY", "RANKING_REPORT_CLIENT_INTERVAL", "RANKING_REPORT_GLOBAL_INTERVAL",
		"RANKING_REPORT_STATE_TTL", "RANKING_REPORT_TRANSACTION_TIMEOUT",
		"RANKING_SETTLED_DAY_RETENTION_DAYS", "RANKING_CACHE_REFRESH_INTERVAL",
		"RANKING_ADMIN_CONFIG_FILE", "RANKING_ADMIN_ALLOW_INSECURE_LOCAL_HTTP",
	}
	for _, blank := range []string{"", "   \t "} {
		clearEnvironment(t)
		for _, key := range allKeys {
			t.Setenv(key, blank)
		}
		cfg, err := config.Load()
		if err != nil {
			t.Fatalf("blank value %q failed: %v", blank, err)
		}
		if cfg.HTTPAddr != "127.0.0.1:8080" || cfg.SQLitePath != "data/ranking.db" || cfg.ModelsPath != "config/models.json" || cfg.WebDir != "" || len(cfg.TrustedProxies) != 0 || cfg.GitHub != nil || cfg.AdminOwner != nil || cfg.AdminAllowLocalHTTP {
			t.Fatalf("unexpected config for blank value %q: %+v", blank, cfg)
		}
	}
}

func TestInvalidValues(t *testing.T) {
	for key, values := range map[string][]string{
		"RANKING_HTTP_ADDR":                        {"localhost", "http://localhost:8080", "a/b:8080", "a b:8080", "::1:8080", ":", ":http", ":-1", ":+80", ":65536"},
		"RANKING_SHUTDOWN_TIMEOUT":                 {"0s", "-1s", "10", "forever", "999999999999999999999h"},
		"RANKING_TRUSTED_PROXIES":                  {"localhost", "10.0.0.0/33", "https://proxy"},
		"RANKING_REGISTRATION_IPV6_64":             {"yes", "2"},
		"RANKING_REGISTRATION_SOURCE_REQUESTS":     {"0", "-1", "1.5", "999999999999999999999"},
		"RANKING_REGISTRATION_GLOBAL_REQUESTS":     {"0", "invalid"},
		"RANKING_REGISTRATION_REQUEST_MAX_SOURCES": {"0", "-1"},
		"RANKING_REGISTRATION_REQUEST_WINDOW":      {"0s", "-1s", "forever"},
		"RANKING_REGISTRATION_ENABLED":             {"yes", "2"},
		"RANKING_REGISTRATION_MAX_BODY_BYTES":      {"0", "-1", "4096.5"},
		"RANKING_REPORT_MAX_BODY_BYTES":            {"0", "-1"},
		"RANKING_REPORT_MAX_ITEMS":                 {"0", "-1"},
		"RANKING_REPORT_DECODE_CONCURRENCY":        {"0", "-1"},
		"RANKING_REPORT_CLIENT_BURST":              {"0", "-1"},
		"RANKING_REPORT_GLOBAL_BURST":              {"0", "-1"},
		"RANKING_REPORT_MAX_CLIENTS":               {"0", "-1"},
		"RANKING_REPORT_QUEUE_CAPACITY":            {"0", "-1"},
		"RANKING_REPORT_CLIENT_INTERVAL":           {"0s", "-1s"},
		"RANKING_REPORT_GLOBAL_INTERVAL":           {"0s", "-1s"},
		"RANKING_REPORT_STATE_TTL":                 {"0s", "-1s"},
		"RANKING_REPORT_TRANSACTION_TIMEOUT":       {"0s", "-1s"},
		"RANKING_SETTLED_DAY_RETENTION_DAYS":       {"0", "-1", "36501", "1.5"},
	} {
		for _, value := range values {
			t.Run(key+"/"+value, func(t *testing.T) {
				clearEnvironment(t)
				t.Setenv(key, value)
				_, err := config.Load()
				if err == nil || !strings.Contains(err.Error(), key) {
					t.Fatalf("expected an error naming %s, got %v", key, err)
				}
			})
		}
	}
}
