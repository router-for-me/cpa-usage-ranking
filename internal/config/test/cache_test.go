package test

import (
	"cpa-usage-ranking/internal/config"
	"testing"
	"time"
)

func TestCacheRefreshInterval(t *testing.T) {
	clearEnvironment(t)
	cfg, err := config.Load()
	if err != nil || cfg.CacheRefreshInterval != 5*time.Minute {
		t.Fatal(cfg.CacheRefreshInterval, err)
	}
	for _, value := range []string{"", "   "} {
		t.Setenv("RANKING_CACHE_REFRESH_INTERVAL", value)
		cfg, err := config.Load()
		if err != nil || cfg.CacheRefreshInterval != 5*time.Minute {
			t.Fatalf("blank value %q should keep default: %v, %v", value, cfg.CacheRefreshInterval, err)
		}
	}
	for _, value := range []string{"0", "-1s", "bad"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("RANKING_CACHE_REFRESH_INTERVAL", value)
			if _, err := config.Load(); err == nil {
				t.Fatalf("accepted %q", value)
			}
		})
	}
	t.Setenv("RANKING_CACHE_REFRESH_INTERVAL", "17s")
	cfg, err = config.Load()
	if err != nil || cfg.CacheRefreshInterval != 17*time.Second {
		t.Fatal(cfg.CacheRefreshInterval, err)
	}
}
