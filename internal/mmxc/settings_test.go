package mmxc

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestWriteConfigUpdatesPreservesUnrelatedKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mmxc.yaml")
	initial := strings.Join([]string{
		"db_path: data/custom.mmb",
		"oidc_client_secret: keep-me",
		"llm_model: old",
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(initial), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := writeConfigUpdates(path, map[string]any{
		"llm_model":                  "new",
		"hide_inactive_items":        true,
		"show_account_balance":       false,
		"llm_timeout_seconds":        8192,
		"assistant_query_details":    true,
		"app_start_page":             "reports",
		"theme":                      "serious",
		"theme_mode":                 "dark",
		"backup_prune_enabled":       true,
		"backup_retention_days":      30,
		"backup_retention_min_count": 25,
	}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	for _, want := range []string{
		"db_path: data/custom.mmb",
		"oidc_client_secret: keep-me",
		"llm_model: new",
		"hide_inactive_items: true",
		"show_account_balance: false",
		"llm_timeout_seconds: 8192",
		"assistant_query_details: true",
		"app_start_page: reports",
		"theme: serious",
		"theme_mode: dark",
		"backup_prune_enabled: true",
		"backup_retention_days: 30",
		"backup_retention_min_count: 25",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("updated config missing %q:\n%s", want, got)
		}
	}
}

func TestUpdateSettingsSwitchesDatabaseAndWriteMode(t *testing.T) {
	dir := t.TempDir()
	db1 := filepath.Join(dir, "one.mmb")
	db2 := filepath.Join(dir, "two.mmb")
	buildSyntheticDB(t, db1)
	buildSyntheticDB(t, db2)
	if err := os.Chmod(db1, 0o400); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "mmxc.yaml")
	if err := os.WriteFile(configPath, []byte("db_path: "+dir+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dbFiles, err := scanDatabaseFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := defaultConfig()
	cfg.DBPath = db1
	cfg.WriteEnabled = false
	cfg.DBScanPath = dir
	cfg.BackupDir = filepath.Join(dir, "backups")
	cfg.AuditDir = filepath.Join(dir, "audit")
	a, err := newApp(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	a.configPath = configPath
	a.dbFiles = dbFiles

	form := settingsForm(a.config, db2)
	req := httptest.NewRequest(http.MethodPost, "/settings/update", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()

	a.updateSettings(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("updateSettings status = %d, body %q", rec.Code, rec.Body.String())
	}
	if a.config.DBPath != db2 {
		t.Fatalf("DBPath = %q, want %q", a.config.DBPath, db2)
	}
	if !a.config.WriteEnabled {
		t.Fatal("WriteEnabled = false, want true")
	}
	if !a.config.ShowAccountBalance {
		t.Fatal("ShowAccountBalance = false, want true")
	}
	if err := a.probeWriteLock(); err != nil {
		t.Fatalf("probe write lock after switch: %v", err)
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if !strings.Contains(got, "db_path: "+dir) ||
		!strings.Contains(got, "show_account_balance: true") {
		t.Fatalf("config did not persist runtime database settings:\n%s", got)
	}
	// db_path stays the scan folder; the picked file is remembered separately so
	// the next start reopens it instead of the first file in the folder.
	if !strings.Contains(got, "selected_db: two.mmb") {
		t.Fatalf("config did not remember the selected database:\n%s", got)
	}
	restarted, _, err := resolveStartupDatabase(config{DBPath: dir, SelectedDB: a.config.SelectedDB})
	if err != nil {
		t.Fatal(err)
	}
	if restarted.DBPath != db2 {
		t.Fatalf("restart opened %q, want the remembered %q", restarted.DBPath, db2)
	}
}

func TestUpdateSettingsRejectsIncompatibleWriteModeSwitch(t *testing.T) {
	dir := t.TempDir()
	goodDB := filepath.Join(dir, "good.mmb")
	badDB := filepath.Join(dir, "bad.mmb")
	buildSyntheticDB(t, goodDB)
	db, err := openSQLite(badDB, "rwc", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE INFOTABLE_V1 (INFONAME TEXT, INFOVALUE TEXT)"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "mmxc.yaml")
	original := []byte("db_path: " + dir + "\n")
	if err := os.WriteFile(configPath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	dbFiles, err := scanDatabaseFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := defaultConfig()
	cfg.DBPath = goodDB
	cfg.WriteEnabled = false
	cfg.DBScanPath = dir
	cfg.BackupDir = filepath.Join(dir, "backups")
	cfg.AuditDir = filepath.Join(dir, "audit")
	a, err := newApp(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	a.configPath = configPath
	a.dbFiles = dbFiles

	form := settingsForm(a.config, badDB)
	req := httptest.NewRequest(http.MethodPost, "/settings/update", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()

	a.updateSettings(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("updateSettings status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	if a.config.DBPath != goodDB || a.config.WriteEnabled {
		t.Fatalf("runtime config changed after rejected switch: path=%q write=%v", a.config.DBPath, a.config.WriteEnabled)
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	// A rejected switch must leave the file exactly as it was.
	if !bytes.Equal(data, original) {
		t.Fatalf("config was rewritten by a rejected switch:\n%s", string(data))
	}
}

func settingsForm(cfg config, dbPath string) url.Values {
	// The real form always posts these, because Settings renders them from the
	// config and the defaults are never empty. Fall back here so a test that
	// hand-builds a config still produces a form the handler accepts.
	auditLog := cfg.AuditLog
	if auditLog == "" {
		auditLog = defaultConfig().AuditLog
	}
	auditDir := cfg.AuditDir
	if auditDir == "" {
		auditDir = defaultConfig().AuditDir
	}
	form := url.Values{
		"selected_db_path":           {dbPath},
		"backup_dir":                 {cfg.BackupDir},
		"statement_dir":              {cfg.StatementDir},
		"audit_log":                  {auditLog},
		"audit_dir":                  {auditDir},
		"audit_retention_days":       {strconv.Itoa(cfg.AuditRetentionDays)},
		"backup_retention_days":      {strconv.Itoa(cfg.BackupRetentionDays)},
		"backup_retention_min_count": {strconv.Itoa(cfg.BackupRetentionMinCount)},
		"app_start_page":             {cfg.AppStartPage},
		"theme":                      {cfg.Theme},
		"theme_mode":                 {cfg.ThemeMode},
		"llm_base_url":               {cfg.LLMBaseURL},
		"llm_model":                  {cfg.LLMModel},
		"llm_timeout_seconds":        {strconv.Itoa(int(cfg.LLMTimeout / time.Second))},
		"llm_reasoning_effort":       {cfg.LLMReasoningEffort},
		"translation_language":       {cfg.TranslationLanguage},
		"translation_model":          {cfg.TranslationModel},
		"translation_dir":            {cfg.TranslationDir},
		"prompt_dir":                 {cfg.PromptDir},
	}
	if cfg.BackupPruneEnabled {
		form.Set("backup_prune_enabled", "true")
	}
	if cfg.AllowTopLevelCategories {
		form.Set("allow_top_level_categories", "true")
	}
	if cfg.AllowTransactionDelete {
		form.Set("allow_transaction_delete", "true")
	}
	if cfg.HideInactiveItems {
		form.Set("hide_inactive_items", "true")
	}
	if cfg.ShowAccountBalance {
		form.Set("show_account_balance", "true")
	}
	if cfg.LLMResolveEnabled {
		form.Set("llm_resolve", "true")
	}
	return form
}

func TestThemeOptionsLoadsCSSFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "custom-theme.css"), []byte(":root{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bad.name.css"), []byte(":root{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := themeOptions(dir)
	var names []string
	for _, theme := range got {
		names = append(names, theme.Name)
	}
	want := []string{"default", "custom-theme"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("theme names = %#v, want %#v", names, want)
	}
}

func TestThemeCSSServesSelectedTheme(t *testing.T) {
	dir := t.TempDir()
	oldwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(oldwd); err != nil {
			t.Fatalf("restore cwd: %v", err)
		}
	})

	if err := os.MkdirAll(defaultThemeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(defaultThemeDir, "serious.css"), []byte(":root{--accent:#123456;}"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := &app{config: config{Theme: "serious"}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/static/theme.css", nil)

	a.themeCSS(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("themeCSS status = %d", rec.Code)
	}
	if got := rec.Body.String(); !strings.Contains(got, "--accent:#123456") {
		t.Fatalf("themeCSS body = %q", got)
	}
}

func TestApplyThemeModeForcesLightAndDark(t *testing.T) {
	css := []byte(`:root {
  color-scheme: light dark;
  --bg: #fff;
}
@media (prefers-color-scheme: dark) {
  :root {
    --bg: #000;
  }
}`)

	light := string(applyThemeMode(css, "light"))
	if strings.Contains(light, "prefers-color-scheme") || strings.Contains(light, "--bg: #000") {
		t.Fatalf("light theme CSS still contains dark mode:\n%s", light)
	}
	if !strings.Contains(light, "color-scheme: light") {
		t.Fatalf("light theme CSS missing forced color-scheme:\n%s", light)
	}

	dark := string(applyThemeMode(css, "dark"))
	if strings.Contains(dark, "prefers-color-scheme") {
		t.Fatalf("dark theme CSS still contains media query:\n%s", dark)
	}
	if !strings.Contains(dark, "--bg: #000") || !strings.Contains(dark, "color-scheme: dark") {
		t.Fatalf("dark theme CSS missing promoted dark values:\n%s", dark)
	}
}

func TestParseLLMModels(t *testing.T) {
	got, err := parseLLMModels([]byte(`{"object":"list","data":[{"id":"qwen3:30b"},{"id":"qwen3-vl:latest"},{"id":"qwen3:30b"},{"id":""}]}`))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"qwen3-vl:latest", "qwen3:30b"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("models = %#v, want %#v", got, want)
	}
}

func TestPruneBackupsKeepsRecentAndMinimumCount(t *testing.T) {
	dir := t.TempDir()
	a := &app{config: config{
		DBPath:                  filepath.Join(t.TempDir(), "test.mmb"),
		BackupDir:               dir,
		BackupRetentionDays:     30,
		BackupRetentionMinCount: 3,
	}}
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.Local)
	for _, when := range []time.Time{
		now.AddDate(0, 0, -1),
		now.AddDate(0, 0, -10),
		now.AddDate(0, 0, -31),
		now.AddDate(0, 0, -40),
		now.AddDate(0, 0, -50),
	} {
		writeBackupFile(t, dir, when, "manual", "test.mmb")
	}
	if err := os.WriteFile(filepath.Join(dir, "not-an-mmxc-backup.mmb"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := a.pruneBackups(now); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	want := []string{
		"20260515-120000.000000-manual-test.mmb",
		"20260605-120000.000000-manual-test.mmb",
		"20260614-120000.000000-manual-test.mmb",
		"not-an-mmxc-backup.mmb",
	}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("remaining files = %#v, want %#v", names, want)
	}
}

func TestBackupStatsOnlyCountsCurrentDatabaseBackups(t *testing.T) {
	dir := t.TempDir()
	a := &app{config: config{DBPath: "family.mmb", BackupDir: dir}}
	writeBackupFile(t, dir, time.Date(2026, 6, 15, 12, 0, 0, 0, time.Local), "manual", "family.mmb")
	writeBackupFile(t, dir, time.Date(2026, 6, 15, 12, 1, 0, 0, time.Local), "manual", "other.mmb")

	count, size, err := a.backupStats()
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 || size != 6 {
		t.Fatalf("backupStats = count %d size %d, want count 1 size 6", count, size)
	}
}

func writeBackupFile(t *testing.T, dir string, when time.Time, reason, base string) {
	t.Helper()
	name := when.Format("20060102-150405.000000") + "-" + reason + "-" + base
	if err := os.WriteFile(filepath.Join(dir, name), []byte("backup"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestSettingsFormPreservesStoredAPIKey(t *testing.T) {
	// The stored key is never rendered into the form, so a blank field must
	// mean "unchanged" — otherwise every save would silently wipe the key.
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.mmb")
	buildSyntheticDB(t, dbPath)
	dbFiles, err := scanDatabaseFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := defaultConfig()
	cfg.DBPath = dbPath
	cfg.DBScanPath = dir
	cfg.BackupDir = filepath.Join(dir, "backups")
	cfg.AuditDir = filepath.Join(dir, "audit")
	cfg.LLMAPIKey = "sk-secret"
	a, err := newApp(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	a.dbFiles = dbFiles

	post := func(form url.Values) config {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/settings/update", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		next, updates, err := a.configFromSettingsForm(req)
		if err != nil {
			t.Fatal(err)
		}
		// The audit log must never carry the key itself.
		for _, change := range configAuditChanges(a.config, updates) {
			if strings.Contains(change, "sk-secret") || strings.Contains(change, "sk-rotated") {
				t.Fatalf("audit log leaked the API key: %s", change)
			}
		}
		return next
	}

	if got := post(settingsForm(a.config, dbPath)).LLMAPIKey; got != "sk-secret" {
		t.Fatalf("blank field wiped the key: %q", got)
	}

	form := settingsForm(a.config, dbPath)
	form.Set("llm_api_key", "sk-rotated")
	if got := post(form).LLMAPIKey; got != "sk-rotated" {
		t.Fatalf("key not replaced: %q", got)
	}

	form = settingsForm(a.config, dbPath)
	form.Set("llm_api_key_clear", "true")
	if got := post(form).LLMAPIKey; got != "" {
		t.Fatalf("key not cleared: %q", got)
	}
}

// log_llm and assistant_query_details are debug-only YAML settings with no
// Settings control, so a save must leave them exactly as configured instead of
// reading them back as unticked checkboxes.
func TestSettingsFormPreservesDebugFlags(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.mmb")
	buildSyntheticDB(t, dbPath)
	dbFiles, err := scanDatabaseFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := defaultConfig()
	cfg.DBPath = dbPath
	cfg.DBScanPath = dir
	cfg.BackupDir = filepath.Join(dir, "backups")
	cfg.AuditDir = filepath.Join(dir, "audit")
	cfg.LogLLM = true
	cfg.AssistantQueryDetails = true
	a, err := newApp(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	a.dbFiles = dbFiles

	form := settingsForm(a.config, dbPath)
	req := httptest.NewRequest(http.MethodPost, "/settings/update", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	next, updates, err := a.configFromSettingsForm(req)
	if err != nil {
		t.Fatal(err)
	}
	if !next.LogLLM || !next.AssistantQueryDetails {
		t.Fatalf("a save cleared the debug flags: log_llm=%v assistant_query_details=%v", next.LogLLM, next.AssistantQueryDetails)
	}
	for _, key := range []string{"log_llm", "assistant_query_details"} {
		if _, ok := updates[key]; ok {
			t.Errorf("%s must not be rewritten into the config file", key)
		}
	}
}

func TestSettingsFormPersistsTransactionDelete(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.mmb")
	buildSyntheticDB(t, dbPath)
	dbFiles, err := scanDatabaseFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := defaultConfig()
	cfg.DBPath = dbPath
	cfg.DBScanPath = dir
	cfg.BackupDir = filepath.Join(dir, "backups")
	cfg.AuditDir = filepath.Join(dir, "audit")
	a, err := newApp(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	a.dbFiles = dbFiles
	if a.config.AllowTransactionDelete {
		t.Fatal("permanent transaction deletion must default to off")
	}

	for _, want := range []bool{true, false} {
		cfg := a.config
		cfg.AllowTransactionDelete = want
		form := settingsForm(cfg, dbPath)
		req := httptest.NewRequest(http.MethodPost, "/settings/update", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		next, updates, err := a.configFromSettingsForm(req)
		if err != nil {
			t.Fatal(err)
		}
		if next.AllowTransactionDelete != want || updates["allow_transaction_delete"] != want {
			t.Fatalf("allow_transaction_delete = %v (update %v), want %v", next.AllowTransactionDelete, updates["allow_transaction_delete"], want)
		}
	}
}

func TestDetectPromptTruncation(t *testing.T) {
	// 40000 bytes / 3.5 ≈ 11428 estimated tokens.
	cases := []struct {
		name        string
		promptBytes int
		images      int
		reported    int
		wantErr     bool
	}{
		{name: "server processed the whole prompt", promptBytes: 40000, reported: 11000},
		{name: "server truncated to a 4k window", promptBytes: 40000, reported: 4096, wantErr: true},
		{name: "no usage reported means no signal", promptBytes: 40000, reported: 0},
		{name: "short prompts are never checked", promptBytes: 1000, reported: 10},
		{name: "images count toward the estimate", promptBytes: 1000, images: 4, reported: 500, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := detectPromptTruncation(tc.promptBytes, tc.images, tc.reported)
			if tc.wantErr && err == nil {
				t.Fatalf("expected a truncation error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestSettingsPageRendersLLMSection(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.mmb")
	buildSyntheticDB(t, dbPath)
	cfg := defaultConfig()
	cfg.DBPath = dbPath
	cfg.DBScanPath = dir
	cfg.BackupDir = filepath.Join(dir, "backups")
	cfg.AuditDir = filepath.Join(dir, "audit")
	cfg.LLMAPIKey = "sk-super-secret"
	a, err := newApp(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	files, _ := scanDatabaseFiles(dir)
	a.dbFiles = files

	w := httptest.NewRecorder()
	a.settings(w, httptest.NewRequest(http.MethodGet, "/settings", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if strings.Contains(body, "sk-super-secret") {
		t.Fatal("settings page leaked the API key into HTML")
	}
	for _, want := range []string{
		`name="llm_base_url"`, `name="llm_api_key"`, `type="password"`,
		`name="llm_model"`, `list="llm-model-options"`,
		`name="llm_reasoning_effort"`, `id="llm-model-options"`,
		`name="llm_api_key_clear"`, `data-llm-url`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("settings page missing %s", want)
		}
	}
	// Debug-only switches are YAML-only; they must not reappear as checkboxes.
	for _, unwanted := range []string{`name="log_llm"`, `name="assistant_query_details"`} {
		if strings.Contains(body, unwanted) {
			t.Errorf("settings page still renders %s", unwanted)
		}
	}
	if strings.Contains(body, "ollama_") {
		t.Error("settings page still references ollama_ fields")
	}
}
