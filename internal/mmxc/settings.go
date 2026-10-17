package mmxc

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

func (a *app) updateSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		a.httpError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	next, updates, err := a.configFromSettingsForm(r)
	if err != nil {
		a.renderError(w, "Settings", "settings", err)
		return
	}
	var runtimeDB *sql.DB
	var runtimeCompat compatibilityReport
	var runtimeCurrency currencyFormat
	runtimeChanged := next.DBPath != a.config.DBPath || next.WriteEnabled != a.config.WriteEnabled
	if runtimeChanged {
		runtimeDB, runtimeCompat, runtimeCurrency, err = openRuntimeDB(next)
		if err != nil {
			a.renderError(w, "Settings", "settings", err)
			return
		}
	}
	if err := writeConfigUpdates(a.configPath, updates); err != nil {
		if runtimeDB != nil {
			runtimeDB.Close()
		}
		a.renderError(w, "Settings", "settings", err)
		return
	}
	if changes := configAuditChanges(a.config, updates); len(changes) > 0 {
		// Which settings changed is safe to keep; the values are not — they
		// carry filesystem paths and endpoint URLs.
		a.audit(r, auditEntry{
			Action: fmt.Sprintf("settings update keys=%q", strings.Join(configChangedKeys(changes), ", ")),
			Detail: fmt.Sprintf("changes=%q", strings.Join(changes, ", ")),
		})
	}
	if runtimeChanged {
		a.installRuntimeDB(next, runtimeDB, runtimeCompat, runtimeCurrency)
	} else {
		a.publish(func(s *runtimeState) { s.config = next })
	}
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

func (a *app) configFromSettingsForm(r *http.Request) (config, map[string]any, error) {
	if err := r.ParseForm(); err != nil {
		return config{}, nil, err
	}
	next := a.config
	selectedDB := strings.TrimSpace(r.FormValue("selected_db_path"))
	if selectedDB == "" {
		selectedDB = a.config.DBPath
	}
	dbFile, ok := a.databaseFile(selectedDB)
	if !ok {
		return config{}, nil, fmt.Errorf("database %q is not in the startup scan list", selectedDB)
	}
	next.DBPath = dbFile.Path
	next.WriteEnabled = dbFile.WriteEnabled
	next.DBScanPath = a.config.DBScanPath
	// db_path stays the folder that is scanned; the file picked here is
	// remembered separately so the next start opens it again.
	next.SelectedDB = dbFile.Name
	// The password never reaches the YAML file: switching to an encrypted
	// database takes it from this form (or keeps the one already unlocked), and
	// switching to a plain one drops it.
	next.DBKey = ""
	if dbFile.Encrypted {
		next.DBKey = a.config.DBKey
		if password := strings.TrimSpace(r.FormValue("db_password")); password != "" {
			next.DBKey = password
		}
	}
	next.BackupDir = strings.TrimSpace(r.FormValue("backup_dir"))
	next.BackupPruneEnabled = formBoolDefault(r, "backup_prune_enabled", false)
	next.StatementDir = strings.TrimSpace(r.FormValue("statement_dir"))
	next.AuditLog = strings.ToLower(strings.TrimSpace(r.FormValue("audit_log")))
	next.AuditDir = strings.TrimSpace(r.FormValue("audit_dir"))
	next.AllowTopLevelCategories = formBoolDefault(r, "allow_top_level_categories", false)
	next.AllowTransactionDelete = formBoolDefault(r, "allow_transaction_delete", false)
	next.HideInactiveItems = formBoolDefault(r, "hide_inactive_items", false)
	next.ShowAccountBalance = formBoolDefault(r, "show_account_balance", false)
	next.LLMBaseURL = strings.TrimRight(strings.TrimSpace(r.FormValue("llm_base_url")), "/")
	next.LLMModel = strings.TrimSpace(r.FormValue("llm_model"))
	next.LLMReasoningEffort = strings.TrimSpace(r.FormValue("llm_reasoning_effort"))
	// The stored key is never rendered back into the form, so a blank field
	// means "unchanged" rather than "clear it". Clearing is an explicit tick.
	if apiKey := strings.TrimSpace(r.FormValue("llm_api_key")); apiKey != "" {
		next.LLMAPIKey = apiKey
	}
	if formBoolDefault(r, "llm_api_key_clear", false) {
		next.LLMAPIKey = ""
	}
	next.LLMResolveEnabled = formBoolDefault(r, "llm_resolve", false)
	next.PromptDir = strings.TrimSpace(r.FormValue("prompt_dir"))
	// log_llm and assistant_query_details are debug-only and have no Settings
	// control: next inherits them from the running config, and leaving them out
	// of updates keeps a hand-edited value in the YAML file untouched.
	next.AppStartPage = strings.ToLower(strings.TrimSpace(r.FormValue("app_start_page")))
	next.Theme = strings.ToLower(strings.TrimSpace(r.FormValue("theme")))
	next.ThemeMode = strings.ToLower(strings.TrimSpace(r.FormValue("theme_mode")))
	next.TranslationLanguage = strings.TrimSpace(r.FormValue("translation_language"))
	next.TranslationModel = strings.TrimSpace(r.FormValue("translation_model"))
	next.TranslationDir = strings.TrimSpace(r.FormValue("translation_dir"))

	timeout, err := parsePositiveSetting(r.FormValue("llm_timeout_seconds"), "LLM timeout")
	if err != nil {
		return config{}, nil, err
	}
	next.LLMTimeout = time.Duration(timeout) * time.Second
	retentionDays, err := parsePositiveSetting(r.FormValue("backup_retention_days"), "Backup retention days")
	if err != nil {
		return config{}, nil, err
	}
	next.BackupRetentionDays = retentionDays
	retentionMin, err := parsePositiveSetting(r.FormValue("backup_retention_min_count"), "Backup retention minimum count")
	if err != nil {
		return config{}, nil, err
	}
	next.BackupRetentionMinCount = retentionMin
	// Zero is meaningful here: it keeps every entry forever.
	auditRetention, err := parseNonNegativeSetting(r.FormValue("audit_retention_days"), "Audit retention days")
	if err != nil {
		return config{}, nil, err
	}
	next.AuditRetentionDays = auditRetention

	if next.AuditDir == "" {
		return config{}, nil, errors.New("audit directory is required")
	}
	if next.DBScanPath == "" {
		return config{}, nil, errors.New("database scan path is required")
	}
	if next.BackupDir == "" {
		return config{}, nil, errors.New("backup directory is required")
	}
	if next.StatementDir == "" {
		return config{}, nil, errors.New("statement directory is required")
	}
	if next.TranslationDir == "" {
		return config{}, nil, errors.New("translation cache directory is required")
	}
	if next.TranslationLanguage == "" {
		next.TranslationLanguage = "en"
	}
	if next.LLMBaseURL != "" {
		if err := validateHTTPURL(next.LLMBaseURL, "LLM base URL"); err != nil {
			return config{}, nil, err
		}
	}
	if err := normalizeConfig(&next); err != nil {
		return config{}, nil, err
	}
	if !themeAvailable(next.Theme, themeOptions(defaultThemeDir)) {
		return config{}, nil, fmt.Errorf("theme %q was not found in %s", next.Theme, defaultThemeDir)
	}

	updates := map[string]any{
		"db_path":                    next.DBScanPath,
		"selected_db":                next.SelectedDB,
		"backup_dir":                 next.BackupDir,
		"backup_prune_enabled":       next.BackupPruneEnabled,
		"backup_retention_days":      next.BackupRetentionDays,
		"backup_retention_min_count": next.BackupRetentionMinCount,
		"statement_dir":              next.StatementDir,
		"audit_log":                  next.AuditLog,
		"audit_dir":                  next.AuditDir,
		"audit_retention_days":       next.AuditRetentionDays,
		"allow_top_level_categories": next.AllowTopLevelCategories,
		"allow_transaction_delete":   next.AllowTransactionDelete,
		"hide_inactive_items":        next.HideInactiveItems,
		"show_account_balance":       next.ShowAccountBalance,
		"llm_base_url":               next.LLMBaseURL,
		"llm_api_key":                next.LLMAPIKey,
		"llm_model":                  next.LLMModel,
		"llm_timeout_seconds":        timeout,
		"llm_reasoning_effort":       next.LLMReasoningEffort,
		"llm_resolve":                next.LLMResolveEnabled,
		"prompt_dir":                 next.PromptDir,
		"app_start_page":             next.AppStartPage,
		"theme":                      next.Theme,
		"theme_mode":                 next.ThemeMode,
		"translation_language":       next.TranslationLanguage,
		"translation_model":          next.TranslationModel,
		"translation_dir":            next.TranslationDir,
	}
	return next, updates, nil
}

func parsePositiveSetting(raw, label string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", label)
	}
	return n, nil
}

func parseNonNegativeSetting(raw, label string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s must be zero or a positive integer", label)
	}
	return n, nil
}

// configChangedKeys reduces the "key:old->new" strings from configAuditChanges
// to their key halves, so an audit entry can name what changed without
// recording the values.
func configChangedKeys(changes []string) []string {
	keys := make([]string, 0, len(changes))
	for _, change := range changes {
		key, _, _ := strings.Cut(change, ":")
		keys = append(keys, key)
	}
	return keys
}

func validateHTTPURL(raw, label string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("%s must be an http or https URL", label)
	}
	return nil
}

func configAuditChanges(current config, updates map[string]any) []string {
	keys := make([]string, 0, len(updates))
	for key := range updates {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var changes []string
	for _, key := range keys {
		oldValue := configAuditValue(current, key)
		newValue := fmt.Sprint(updates[key])
		if secretSettings[key] {
			newValue = redactedSecret(newValue)
		}
		if oldValue != newValue {
			changes = append(changes, fmt.Sprintf("%s:%s->%s", key, oldValue, newValue))
		}
	}
	return changes
}

func configAuditValue(c config, key string) string {
	switch key {
	case "db_path":
		if c.DBScanPath != "" {
			return c.DBScanPath
		}
		return c.DBPath
	case "selected_db":
		return c.SelectedDB
	case "backup_dir":
		return c.BackupDir
	case "backup_prune_enabled":
		return strconv.FormatBool(c.BackupPruneEnabled)
	case "backup_retention_days":
		return strconv.Itoa(c.BackupRetentionDays)
	case "backup_retention_min_count":
		return strconv.Itoa(c.BackupRetentionMinCount)
	case "statement_dir":
		return c.StatementDir
	case "audit_log":
		return c.AuditLog
	case "audit_dir":
		return c.AuditDir
	case "audit_retention_days":
		return strconv.Itoa(c.AuditRetentionDays)
	case "allow_top_level_categories":
		return strconv.FormatBool(c.AllowTopLevelCategories)
	case "allow_transaction_delete":
		return strconv.FormatBool(c.AllowTransactionDelete)
	case "hide_inactive_items":
		return strconv.FormatBool(c.HideInactiveItems)
	case "show_account_balance":
		return strconv.FormatBool(c.ShowAccountBalance)
	case "llm_base_url":
		return c.LLMBaseURL
	case "llm_api_key":
		return redactedSecret(c.LLMAPIKey)
	case "llm_model":
		return c.LLMModel
	case "llm_timeout_seconds":
		return strconv.Itoa(int(c.LLMTimeout / time.Second))
	case "llm_reasoning_effort":
		return c.LLMReasoningEffort
	case "llm_resolve":
		return strconv.FormatBool(c.LLMResolveEnabled)
	case "prompt_dir":
		return c.PromptDir
	case "app_start_page":
		return c.AppStartPage
	case "theme":
		return c.Theme
	case "theme_mode":
		return c.ThemeMode
	case "translation_language":
		return c.TranslationLanguage
	case "translation_model":
		return c.TranslationModel
	case "translation_dir":
		return c.TranslationDir
	default:
		return ""
	}
}

func (a *app) databaseFile(path string) (databaseFile, bool) {
	for _, file := range a.dbFiles {
		if samePath(file.Path, path) {
			return file, true
		}
	}
	return databaseFile{}, false
}

func writeConfigUpdates(path string, updates map[string]any) error {
	if strings.TrimSpace(path) == "" {
		path = "mmxc.yaml"
	}
	var doc yaml.Node
	if data, err := os.ReadFile(path); err == nil {
		if len(strings.TrimSpace(string(data))) > 0 {
			if err := yaml.Unmarshal(data, &doc); err != nil {
				return fmt.Errorf("parse config %s: %w", path, err)
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read config %s: %w", path, err)
	}
	root := ensureYAMLMap(&doc)
	if root.Kind != yaml.MappingNode {
		return fmt.Errorf("config %s must contain a YAML mapping", path)
	}
	keys := make([]string, 0, len(updates))
	for key := range updates {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		setYAMLScalar(root, key, updates[key])
	}
	data, err := yaml.Marshal(&doc)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return err
	}
	// WriteFile's mode only applies when it creates the file, and this one can
	// carry llm_api_key, so tighten an existing file's permissions too.
	return os.Chmod(path, 0o600)
}

func ensureYAMLMap(doc *yaml.Node) *yaml.Node {
	if doc.Kind == 0 {
		doc.Kind = yaml.DocumentNode
		doc.Content = []*yaml.Node{{Kind: yaml.MappingNode}}
	}
	if doc.Kind != yaml.DocumentNode {
		return doc
	}
	if len(doc.Content) == 0 {
		doc.Content = []*yaml.Node{{Kind: yaml.MappingNode}}
	}
	return doc.Content[0]
}

func setYAMLScalar(root *yaml.Node, key string, value any) {
	valueNode := yamlScalar(value)
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == key {
			root.Content[i+1] = valueNode
			return
		}
	}
	root.Content = append(root.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: key}, valueNode)
}

func yamlScalar(value any) *yaml.Node {
	node := &yaml.Node{Kind: yaml.ScalarNode}
	switch v := value.(type) {
	case bool:
		node.Tag = "!!bool"
		node.Value = strconv.FormatBool(v)
	case int:
		node.Tag = "!!int"
		node.Value = strconv.Itoa(v)
	default:
		node.Tag = "!!str"
		node.Value = fmt.Sprint(v)
	}
	return node
}

func (a *app) llmModels(w http.ResponseWriter, r *http.Request) {
	rawURL := strings.TrimSpace(r.URL.Query().Get("url"))
	if rawURL == "" {
		rawURL = a.config.LLMBaseURL
	}
	models, err := a.llm.listModels(rawURL, 10*time.Second)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": err.Error()})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"models": models})
}

// secretSettings are config keys whose value must never reach the audit log.
var secretSettings = map[string]bool{"llm_api_key": true}

// redactedSecret renders a secret for the settings audit log: whether one is
// set, never what it is.
func redactedSecret(value string) string {
	if strings.TrimSpace(value) == "" {
		return "(unset)"
	}
	return "(set)"
}
