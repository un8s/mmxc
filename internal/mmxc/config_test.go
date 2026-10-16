package mmxc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadConfigDefaultsWhenFileMissing(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent.yaml")

	cfg, err := loadConfig(missing, false)
	if err != nil {
		t.Fatalf("loadConfig with missing default file: %v", err)
	}
	want := defaultConfig()
	if cfg != want {
		t.Fatalf("config = %+v, want defaults %+v", cfg, want)
	}
}

func TestLoadConfigMissingExplicitFileErrors(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent.yaml")

	if _, err := loadConfig(missing, true); err == nil {
		t.Fatal("expected error for explicitly requested missing config file")
	}
}

func TestLoadConfigYAMLOverridesDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mmxc.yaml")
	yaml := "" +
		"db_path: /data/custom.mmb\n" +
		"http_addr: \":9000\"\n" +
		"backup_prune_enabled: false\n" +
		"backup_retention_days: 14\n" +
		"backup_retention_min_count: 5\n" +
		"llm_model: llama3\n" +
		"llm_timeout_seconds: 42\n" +
		"llm_reasoning_effort: low\n" +
		"hide_inactive_items: false\n" +
		"show_account_balance: false\n" +
		"llm_resolve: false\n" +
		"assistant_query_details: true\n" +
		"app_start_page: reports\n" +
		"theme: serious\n" +
		"theme_mode: dark\n"
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadConfig(path, true)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}

	if cfg.DBPath != "/data/custom.mmb" {
		t.Errorf("DBPath = %q", cfg.DBPath)
	}
	if cfg.HTTPAddr != ":9000" {
		t.Errorf("HTTPAddr = %q", cfg.HTTPAddr)
	}
	if cfg.WriteEnabled {
		t.Error("WriteEnabled is runtime-derived and should not load from YAML")
	}
	if cfg.BackupPruneEnabled {
		t.Error("BackupPruneEnabled = true, want false")
	}
	if cfg.BackupRetentionDays != 14 {
		t.Errorf("BackupRetentionDays = %d, want 14", cfg.BackupRetentionDays)
	}
	if cfg.BackupRetentionMinCount != 5 {
		t.Errorf("BackupRetentionMinCount = %d, want 5", cfg.BackupRetentionMinCount)
	}
	if cfg.LLMModel != "llama3" {
		t.Errorf("LLMModel = %q", cfg.LLMModel)
	}
	if cfg.LLMTimeout != 42*time.Second {
		t.Errorf("LLMTimeout = %v, want 42s", cfg.LLMTimeout)
	}
	if cfg.LLMReasoningEffort != "low" {
		t.Errorf("LLMReasoningEffort = %q", cfg.LLMReasoningEffort)
	}
	if !cfg.AssistantQueryDetails {
		t.Error("AssistantQueryDetails = false, want true")
	}
	if cfg.AppStartPage != "reports" {
		t.Errorf("AppStartPage = %q, want reports", cfg.AppStartPage)
	}
	if cfg.Theme != "serious" {
		t.Errorf("Theme = %q, want serious", cfg.Theme)
	}
	if cfg.ThemeMode != "dark" {
		t.Errorf("ThemeMode = %q, want dark", cfg.ThemeMode)
	}
	if cfg.LLMResolveEnabled {
		t.Error("LLMResolveEnabled = true, want false")
	}
	if cfg.HideInactiveItems {
		t.Error("HideInactiveItems = true, want false")
	}
	if cfg.ShowAccountBalance {
		t.Error("ShowAccountBalance = true, want false")
	}
	// Unspecified keys keep their defaults.
	if cfg.BackupDir != defaultConfig().BackupDir {
		t.Errorf("BackupDir = %q, want default", cfg.BackupDir)
	}
}

func TestLoadConfigRejectsInvalidThemeMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mmxc.yaml")
	if err := os.WriteFile(path, []byte("theme_mode: noon\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(path, true); err == nil || !strings.Contains(err.Error(), "theme_mode") {
		t.Fatalf("loadConfig error = %v, want theme_mode validation error", err)
	}
}

func TestLoadConfigRejectsInvalidTheme(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mmxc.yaml")
	if err := os.WriteFile(path, []byte("theme: ../bad\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(path, true); err == nil || !strings.Contains(err.Error(), "theme") {
		t.Fatalf("loadConfig error = %v, want theme validation error", err)
	}
}

func TestLoadConfigRejectsInvalidAppStartPage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mmxc.yaml")
	if err := os.WriteFile(path, []byte("app_start_page: budgets\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(path, true); err == nil || !strings.Contains(err.Error(), "app_start_page") {
		t.Fatalf("loadConfig error = %v, want app_start_page validation error", err)
	}
}

func TestStartPagePath(t *testing.T) {
	if got := (&app{}).startPagePath(); got != "/reports" {
		t.Fatalf("unavailable AI start page = %q, want /reports", got)
	}
	if got := (&app{config: config{AppStartPage: "reports"}}).startPagePath(); got != "/reports" {
		t.Fatalf("reports start page = %q, want /reports", got)
	}
	if got := (&app{config: config{AppStartPage: "accounts"}}).startPagePath(); got != "/accounts" {
		t.Fatalf("accounts start page = %q, want /accounts", got)
	}
	cfg := config{AppStartPage: "ask", LLMBaseURL: "http://127.0.0.1:11434/v1", LLMModel: "test-model"}
	available := &app{config: cfg}
	available.ai = aiStatus{
		Configured:         true,
		Available:          true,
		AssistantAvailable: true,
		CheckedAt:          time.Now(),
		ConfigKey:          strings.Join([]string{cfg.LLMBaseURL, cfg.LLMModel, ""}, "\x00"),
	}
	if got := available.startPagePath(); got != "/assistant" {
		t.Fatalf("available AI start page = %q, want /assistant", got)
	}
}
