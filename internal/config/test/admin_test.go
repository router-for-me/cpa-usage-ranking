package test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cpa-usage-ranking/internal/config"
)

func TestAdminConfigurationCannotBeServedAsStaticAsset(t *testing.T) {
	root := t.TempDir()
	assets := filepath.Join(root, "assets")
	if err := os.Mkdir(assets, 0700); err != nil {
		t.Fatal(err)
	}
	private := filepath.Join(assets, "admin.json")
	if err := os.WriteFile(private, []byte(`{"username":"owner","password":"isolated-test-password"}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RANKING_WEB_DIR", root)
	t.Setenv("RANKING_ADMIN_CONFIG_FILE", private)
	if _, err := config.Load(); err == nil {
		t.Fatal("private file under public assets accepted")
	}
	link := filepath.Join(t.TempDir(), "admin.json")
	if err := os.Symlink(private, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RANKING_ADMIN_CONFIG_FILE", link)
	if _, err := config.Load(); err == nil {
		t.Fatal("symlink into public assets accepted")
	}
}

func TestAdminConfiguration(t *testing.T) {
	clearEnvironment(t)
	cfg, err := config.Load()
	if err != nil || cfg.AdminOwner != nil || cfg.AdminAllowLocalHTTP || cfg.AdminConfigInsecurePerm {
		t.Fatalf("disabled defaults: %v", err)
	}
	t.Setenv("RANKING_ADMIN_CONFIG_FILE", "   ")
	t.Setenv("RANKING_ADMIN_ALLOW_INSECURE_LOCAL_HTTP", "   ")
	cfg, err = config.Load()
	if err != nil || cfg.AdminOwner != nil || cfg.AdminAllowLocalHTTP {
		t.Fatalf("blank admin env should disable admin: %+v, %v", cfg, err)
	}
	path := filepath.Join(t.TempDir(), "admin.json")
	t.Setenv("RANKING_ADMIN_CONFIG_FILE", path)
	if _, err := config.Load(); err == nil {
		t.Fatal("missing file accepted")
	}
	for _, body := range []string{`{`, `{}`, `{"username":"owner","password":"short"}`, `{"username":"owner","password":"long-test-password","extra":1}`, `{"username":"owner","password":"long-test-password"} {}`} {
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := config.Load(); err == nil || strings.Contains(err.Error(), "long-test-password") {
			t.Fatal("invalid file accepted or leaked password")
		}
	}
	if err := os.WriteFile(path, []byte(`{"username":"owner","password":"long-test-password"}`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err = config.Load()
	if err != nil || cfg.AdminOwner == nil || cfg.AdminOwner.Username != "owner" || cfg.AdminConfigInsecurePerm {
		t.Fatalf("0600 config: %+v, %v", cfg, err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err = config.Load()
	if err != nil || cfg.AdminOwner == nil || !cfg.AdminConfigInsecurePerm {
		t.Fatalf("0644 config should warn instead of failing startup: %+v, %v", cfg, err)
	}
	if err := os.Chmod(path, 0640); err != nil {
		t.Fatal(err)
	}
	cfg, err = config.Load()
	if err != nil || cfg.AdminConfigInsecurePerm {
		t.Fatalf("0640 config: %+v, %v", cfg, err)
	}
	t.Setenv("RANKING_ADMIN_ALLOW_INSECURE_LOCAL_HTTP", "true")
	t.Setenv("RANKING_HTTP_ADDR", "0.0.0.0:8080")
	cfg, err = config.Load()
	if err != nil || !cfg.AdminAllowLocalHTTP {
		t.Fatalf("container listen addr with insecure local HTTP should succeed: %+v, %v", cfg, err)
	}
	t.Setenv("RANKING_HTTP_ADDR", "127.0.0.1:8080")
	if _, err := config.Load(); err != nil {
		t.Fatal(err)
	}
}
