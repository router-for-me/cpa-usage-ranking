package test

import (
	"strings"
	"testing"
	"time"

	"cpa-usage-ranking/internal/config"
	"cpa-usage-ranking/internal/registration"
)

func TestRegistrationRequestConfiguration(t *testing.T) {
	clearEnvironment(t)
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RegistrationRequests != registration.DefaultRequestLimits() || cfg.GitHub != nil || !cfg.RegistrationEnabled || cfg.RegistrationMaxBodyBytes != 4096 {
		t.Fatalf("defaults: %+v", cfg)
	}
	t.Setenv("RANKING_REGISTRATION_SOURCE_REQUESTS", "2")
	t.Setenv("RANKING_REGISTRATION_GLOBAL_REQUESTS", "3")
	t.Setenv("RANKING_REGISTRATION_REQUEST_MAX_SOURCES", "4")
	t.Setenv("RANKING_REGISTRATION_REQUEST_WINDOW", "5s")
	t.Setenv("RANKING_REGISTRATION_ENABLED", "false")
	t.Setenv("RANKING_REGISTRATION_MAX_BODY_BYTES", "2048")
	cfg, err = config.Load()
	if err != nil {
		t.Fatal(err)
	}
	want := registration.RequestLimits{SourceRequests: 2, GlobalRequests: 3, MaxSources: 4, Window: 5 * time.Second}
	if cfg.RegistrationRequests != want || cfg.RegistrationEnabled || cfg.RegistrationMaxBodyBytes != 2048 {
		t.Fatalf("overrides: %+v", cfg)
	}
}

func TestOAuthConfigurationIsOptionalAllOrNoneAndHTTPS(t *testing.T) {
	clearEnvironment(t)
	const fake = "fixture-value-never-log"
	t.Setenv("RANKING_GITHUB_CLIENT_SECRET", fake)
	if _, err := config.Load(); err == nil || !strings.Contains(err.Error(), "RANKING_PUBLIC_ORIGIN") || strings.Contains(err.Error(), fake) {
		t.Fatal("partial configuration or error redaction failed")
	}
	t.Setenv("RANKING_GITHUB_CLIENT_ID", "fixture-id")
	for _, origin := range []string{"", "http://ranking.example", "https://ranking.example/auth", "https://ranking.example?", "https://ranking.example/#", "https://user@ranking.example"} {
		t.Setenv("RANKING_PUBLIC_ORIGIN", origin)
		if _, err := config.Load(); err == nil || !strings.Contains(err.Error(), "RANKING_PUBLIC_ORIGIN") {
			t.Fatalf("non-origin accepted: %q", origin)
		}
	}
	t.Setenv("RANKING_PUBLIC_ORIGIN", "https://ranking.example:8443")
	cfg, err := config.Load()
	if err != nil || cfg.GitHub == nil || cfg.GitHub.PublicOrigin != "https://ranking.example:8443" {
		t.Fatalf("valid OAuth configuration rejected: %v", err)
	}
	t.Setenv("RANKING_GITHUB_CLIENT_ID", "")
	if _, err := config.Load(); err == nil || strings.Contains(err.Error(), fake) {
		t.Fatal("empty credential configuration accepted or secret leaked")
	}
}
