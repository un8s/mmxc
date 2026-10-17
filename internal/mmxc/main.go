package mmxc

import (
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type app struct {
	db         *sql.DB
	dbFiles    []databaseFile
	config     config
	configPath string
	compat     compatibilityReport
	tmpl       *template.Template
	version    string
	history    []versionEntry
	auth       *oidcAuth   // nil unless the OIDC login gate is enabled
	tr         *translator // nil model = pass-through (English)
	llm        *llmClient  // OpenAI-compatible chat-completions transport

	// audits is the audit log's own database (nil when auditing is off or the
	// store could not be opened); auditStatus carries the reason in that case.
	audits      *auditStore
	auditStatus string

	currFmt currencyFormat // base currency display format

	// rt is the shared, replaceable runtime state (runtime.go). The fields
	// above that mirror runtimeState are this request's copy of it.
	rt *runtimeHolder
	ai *aiCache // latest result of the short LLM availability check
}

// versionEntry is one row of the embedded git history shown in the version modal.
type versionEntry struct {
	Date    string
	Subject string
}

// parseHistory turns the embedded "date<TAB>subject" lines (see ui.go and the
// `gen` justfile recipe) into entries, skipping blanks and malformed rows.
func parseHistory(raw string) []versionEntry {
	var entries []versionEntry
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		date, subject, found := strings.Cut(line, "\t")
		if !found {
			continue
		}
		entries = append(entries, versionEntry{Date: date, Subject: subject})
	}
	return entries
}

type config struct {
	DBPath       string `yaml:"db_path"`
	DBScanPath   string `yaml:"-"`
	SelectedDB   string `yaml:"selected_db"` // file name of the database last opened from Settings
	HTTPAddr     string `yaml:"http_addr"`
	WriteEnabled bool   `yaml:"-"`
	// DBKey unlocks an encrypted (.mmbx) database. It is deliberately never
	// persisted: it comes from MMXC_DB_PASSWORD or the unlock page and lives
	// only in memory, so the password is not stored beside the file it protects.
	DBKey                   string        `yaml:"-"`
	BackupDir               string        `yaml:"backup_dir"`
	BackupPruneEnabled      bool          `yaml:"backup_prune_enabled"`
	BackupRetentionDays     int           `yaml:"backup_retention_days"`
	BackupRetentionMinCount int           `yaml:"backup_retention_min_count"`
	StatementDir            string        `yaml:"statement_dir"`
	AuditLog                string        `yaml:"audit_log"`            // off | actions | detailed
	AuditDir                string        `yaml:"audit_dir"`            // holds the audit database, not scanned for .mmb files
	AuditRetentionDays      int           `yaml:"audit_retention_days"` // 0 keeps every entry
	LLMBaseURL              string        `yaml:"llm_base_url"`
	LLMAPIKey               string        `yaml:"llm_api_key"`
	LLMModel                string        `yaml:"llm_model"`
	LLMTimeout              time.Duration `yaml:"-"`
	LLMReasoningEffort      string        `yaml:"llm_reasoning_effort"`
	LLMResolveEnabled       bool          `yaml:"llm_resolve"`
	PromptDir               string        `yaml:"prompt_dir"` // optional overrides of the Ask and statement prompt guidance
	LogLLM                  bool          `yaml:"log_llm"`
	AssistantQueryDetails   bool          `yaml:"assistant_query_details"`
	AppStartPage            string        `yaml:"app_start_page"`
	Theme                   string        `yaml:"theme"`
	ThemeMode               string        `yaml:"theme_mode"`
	AllowTopLevelCategories bool          `yaml:"allow_top_level_categories"`
	AllowTransactionDelete  bool          `yaml:"allow_transaction_delete"` // opt-in hard delete next to the default soft void
	HideInactiveItems       bool          `yaml:"hide_inactive_items"`
	ShowAccountBalance      bool          `yaml:"show_account_balance"`

	// --- Translation ---
	TranslationLanguage string `yaml:"translation_language"`
	TranslationModel    string `yaml:"translation_model"`
	TranslationDir      string `yaml:"translation_dir"`

	// --- Authentication (Pocket ID / OIDC login gate) ---
	AuthEnabled         bool   `yaml:"auth_enabled"` // opt-in; default false
	OIDCIssuer          string `yaml:"oidc_issuer"`  // e.g. https://id.example.com
	OIDCClientID        string `yaml:"oidc_client_id"`
	OIDCClientSecret    string `yaml:"oidc_client_secret"`
	OIDCRedirectURL     string `yaml:"oidc_redirect_url"`     // .../auth/callback
	OIDCScopes          string `yaml:"oidc_scopes"`           // default "openid profile email"
	OIDCAuthURL         string `yaml:"oidc_auth_url"`         // optional: skip discovery
	OIDCTokenURL        string `yaml:"oidc_token_url"`        // optional: skip discovery
	SessionSecret       string `yaml:"session_secret"`        // HMAC key; empty => ephemeral
	SessionTTLHours     int    `yaml:"session_ttl_hours"`     // default 24 (1 day)
	TrustForwardedProto bool   `yaml:"trust_forwarded_proto"` // honor X-Forwarded-Proto for Secure flag
}

// defaultConfig returns the built-in defaults that a YAML config file overrides.
func defaultConfig() config {
	return config{
		DBPath:                  "data",
		HTTPAddr:                ":8080",
		BackupDir:               "data/backups",
		BackupPruneEnabled:      true,
		BackupRetentionDays:     30,
		BackupRetentionMinCount: 25,
		StatementDir:            "data/statements",
		AuditLog:                auditActions,
		AuditDir:                "data/audit",
		AuditRetentionDays:      365,
		LLMBaseURL:              "http://localhost:11434/v1",
		LLMModel:                "qwen3:30b",
		LLMTimeout:              600 * time.Second,
		LLMReasoningEffort:      "none", // structured JSON output; thinking rarely helps here
		LLMResolveEnabled:       true,
		PromptDir:               "data/prompts",
		AppStartPage:            "ask",
		Theme:                   defaultThemeName,
		ThemeMode:               defaultThemeMode,
		HideInactiveItems:       true,
		ShowAccountBalance:      true,

		TranslationLanguage: "en",
		TranslationDir:      "data/translations",

		OIDCScopes:      "openid profile email",
		SessionTTLHours: 24,
	}
}

// configFile is the on-disk YAML shape. It embeds config so every tagged field
// decodes directly, and adds LLMTimeoutSeconds because time.Duration has no
// natural YAML representation.
type configFile struct {
	config            `yaml:",inline"`
	LLMTimeoutSeconds int `yaml:"llm_timeout_seconds"`
}

// loadConfig builds the runtime config from the built-in defaults overlaid with
// the YAML file at path. A missing file is only an error when path was
// explicitly requested (via -config or MMXC_CONFIG).
func loadConfig(path string, explicit bool) (config, error) {
	cf := configFile{config: defaultConfig(), LLMTimeoutSeconds: 600}

	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := yaml.Unmarshal(data, &cf); err != nil {
			return config{}, fmt.Errorf("parse config %s: %w", path, err)
		}
	case errors.Is(err, os.ErrNotExist) && !explicit:
		// No config file present at the default path; defaults are enough.
	default:
		return config{}, fmt.Errorf("read config %s: %w", path, err)
	}

	cfg := cf.config
	cfg.LLMTimeout = time.Duration(cf.LLMTimeoutSeconds) * time.Second
	if err := normalizeConfig(&cfg); err != nil {
		return config{}, err
	}
	return cfg, nil
}

func normalizeConfig(cfg *config) error {
	startPage, err := normalizeAppStartPage(cfg.AppStartPage)
	if err != nil {
		return err
	}
	cfg.AppStartPage = startPage
	theme, err := normalizeThemeName(cfg.Theme)
	if err != nil {
		return err
	}
	cfg.Theme = theme
	themeMode, err := normalizeThemeMode(cfg.ThemeMode)
	if err != nil {
		return err
	}
	cfg.ThemeMode = themeMode
	effort, err := normalizeReasoningEffort(cfg.LLMReasoningEffort)
	if err != nil {
		return err
	}
	cfg.LLMReasoningEffort = effort
	auditLog, err := normalizeAuditLog(cfg.AuditLog)
	if err != nil {
		return err
	}
	cfg.AuditLog = auditLog
	cfg.LLMBaseURL = llmBaseURL(cfg.LLMBaseURL)
	return nil
}

// normalizeReasoningEffort validates llm_reasoning_effort. An empty value omits
// the field entirely, letting the endpoint apply its own default.
func normalizeReasoningEffort(value string) (string, error) {
	effort := strings.ToLower(strings.TrimSpace(value))
	switch effort {
	case "", "none", "low", "medium", "high", "max":
		return effort, nil
	default:
		return "", fmt.Errorf("llm_reasoning_effort must be none, low, medium, high, or max")
	}
}

func normalizeAppStartPage(value string) (string, error) {
	startPage := strings.ToLower(strings.TrimSpace(value))
	if startPage == "" {
		startPage = "ask"
	}
	switch startPage {
	case "ask", "reports", "accounts":
		return startPage, nil
	default:
		return "", fmt.Errorf("app_start_page must be ask, reports, or accounts")
	}
}

func mustNormalizeAppStartPage(value string) string {
	startPage, err := normalizeAppStartPage(value)
	if err != nil {
		return "ask"
	}
	return startPage
}

type compatibilityReport struct {
	OK      bool
	Status  string
	Details string
}

type dbInfo struct {
	Version                 string
	History                 []versionEntry
	ConfigPath              string
	Path                    string
	DBScanPath              string
	DBFiles                 []databaseFile
	SeedCurrencies          []seedCurrency // base-currency choices when creating a database
	HTTPAddr                string
	BackupDir               string
	BackupPruneEnabled      bool
	BackupRetentionDays     int
	BackupRetentionMinCount int
	BackupCount             int
	BackupSize              string
	MMEXVersion             string
	DataVersion             string
	BaseCurrency            string
	WriteEnabled            bool
	AllowTopLevelCategories bool
	AllowTransactionDelete  bool
	HideInactiveItems       bool
	ShowAccountBalance      bool
	StatementDir            string
	AuditLog                string
	AuditDir                string
	AuditRetentionDays      int
	AuditCount              int
	AuditPath               string
	AuditEncrypted          bool
	AuditStatus             string
	LLMBaseURL              string
	LLMAPIKeySet            bool
	LLMModel                string
	LLMTimeoutSeconds       int
	LLMReasoningEffort      string
	AIConfigured            bool
	AIAvailable             bool
	AIStatus                string
	AIDetail                string
	AssistantAvailable      bool
	LLMResolveEnabled       bool
	PromptDir               string
	Prompts                 []PromptStatus
	AssistantQueryDetails   bool
	AppStartPage            string
	Theme                   string
	ThemeMode               string
	Themes                  []themeOption
	AccountCount            int
	TransCount              int
	CategoryCount           int
	PayeeCount              int
	BudgetCount             int
	Status                  string
	Compatible              bool // compat.OK; the header only shows Status when this is false
	CompatibilityDetail     string
	AuthEnabled             bool

	TranslationLanguage string
	TranslationModel    string
	TranslationDir      string
}

type databaseFile struct {
	Path         string
	Name         string
	WriteEnabled bool
	// Encrypted marks an MMXC-encrypted (.mmbx) database, which needs a password
	// to open and cannot be read by MMEX.
	Encrypted bool
}

type pageData struct {
	Title              string
	Active             string
	Info               dbInfo
	Accounts           []account
	AccountTypes       []string
	Currencies         []currency
	Selected           *account
	EditTransaction    *transaction
	Scroll             string
	ReturnTo           string
	CancelTo           string // where the transaction editor's Cancel goes back to
	SelectedCategory   *category
	SelectedPayee      *payee
	SelectedBudget     *budgetYear
	Transactions       []transaction
	TransactionSummary transactionSummary
	Filter             transactionFilter
	Categories         []category
	Payees             []payee
	BudgetYears        []budgetYear
	BudgetEntries      []budgetEntry
	BudgetCategoryIDs  map[int]bool
	MonthlyReview      monthlyReviewData
	ImportRules        []importRuleRow
	Reports            reportData
	Assistant          assistantPageData
	Audit              auditPageData
	Error              string

	// unexported — bound by render() so templates can call {{.T "source"}}
	lang string
	tr   *translator
}

// T returns the translation of source into the active UI language. An optional
// second argument provides guidance to the model when the same English string
// must translate differently depending on context (e.g. "noun, not verb").
// Falls back to source (English) when no translation model is configured or on
// any error. Templates call this as {{.T "source"}} or {{.T "source" "context"}}.
func (p pageData) T(source string, guidance ...string) string {
	if p.tr == nil || p.lang == "" || p.lang == "en" {
		return source
	}
	g := ""
	if len(guidance) > 0 {
		g = guidance[0]
	}
	return p.tr.get(p.lang, source, g)
}

// DateFmt formats a YYYY-MM-DD date string for display in the active locale.
// Input values for form fields should still use the raw ISO value; this is
// only for human-readable display in table cells and prose.
// Falls back to the original string if parsing fails.
func (p pageData) DateFmt(date string) string {
	if p.lang == "" || p.lang == "en" {
		return date
	}
	layout := localDateLayout(p.lang)
	if layout == "2006-01-02" {
		return date // ISO — no reformatting needed
	}
	t, err := time.Parse("2006-01-02", date)
	if err != nil {
		return date
	}
	return t.Format(layout)
}

func Main() {
	// `mmxc init` creates a new database and exits, so a fresh install doesn't
	// need Money Manager Ex desktop just to produce an empty .mmb.
	if len(os.Args) > 1 && os.Args[1] == "init" {
		os.Exit(runInit(os.Args[2:]))
	}

	defaultConfigPath := getenv("MMXC_CONFIG", "mmxc.yaml")
	configPath := flag.String("config", defaultConfigPath, "path to the YAML config file")
	flag.Parse()

	// The file is required only if the user asked for a specific one (via the
	// flag or MMXC_CONFIG); the default path is allowed to be absent.
	explicit := *configPath != "mmxc.yaml" || os.Getenv("MMXC_CONFIG") != ""
	cfg, err := loadConfig(*configPath, explicit)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	// Deployment-time settings may be overridden by environment variables before
	// startup. DB path points to the directory/file scanned once for .mmb files.
	cfg = applyEnvOverrides(cfg)
	cfg, dbFiles, err := resolveStartupDatabase(cfg)

	if err != nil && errors.Is(err, fs.ErrNotExist) {
		// No database yet: serve the first-run setup UI until one is created,
		// then open it normally.
		if serveErr := serveSetup(cfg); serveErr != nil {
			log.Fatalf("setup: %v", serveErr)
		}
		cfg, dbFiles, err = resolveStartupDatabase(cfg)
	}
	if err != nil {
		log.Fatalf("scan databases: %v", err)
	}
	// An encrypted database needs a password before it can be opened. Ask for it
	// on the unlock page unless MMXC_DB_PASSWORD already supplied a working one.
	if databaseEncrypted(cfg.DBPath) {
		if cfg.DBKey == "" || checkDatabaseKey(cfg.DBPath, cfg.DBKey) != nil {
			key, unlockErr := serveUnlock(cfg)
			if unlockErr != nil {
				log.Fatalf("unlock: %v", unlockErr)
			}
			cfg.DBKey = key
		}
	}
	a, err := newApp(cfg)
	if err != nil {
		log.Fatalf("start mmxc: %v", err)
	}
	defer a.closeRuntime()
	a.configPath = *configPath
	a.publish(func(s *runtimeState) { s.dbFiles = dbFiles })

	handler := a.routes()

	log.Printf("MMXC listening on %s using %s", listenURL(cfg.HTTPAddr), cfg.DBPath)
	if err := http.ListenAndServe(cfg.HTTPAddr, handler); err != nil {
		log.Fatal(err)
	}
}

// routes registers every page and action. Each handler runs through handle, on
// its own view of the runtime state (runtime.go), behind the optional sign-in
// gate.
func (a *app) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", a.handle((*app).startPage))
	mux.HandleFunc("/accounts", a.handle((*app).accounts))
	mux.HandleFunc("/accounts/create", a.handle((*app).createAccount))
	mux.HandleFunc("/accounts/update", a.handle((*app).updateAccount))
	mux.HandleFunc("/transactions/create", a.handle((*app).createTransaction))
	mux.HandleFunc("/transactions/update", a.handle((*app).updateTransaction))
	mux.HandleFunc("/transactions/void", a.handle((*app).voidTransaction))
	mux.HandleFunc("/transactions/delete", a.handle((*app).deleteTransaction))
	mux.HandleFunc("/review", a.handle((*app).monthlyReview))
	mux.HandleFunc("/review/extract", a.handle((*app).extractStatement))
	mux.HandleFunc("/review/statements/attach", a.handle((*app).confirmStatementSections))
	mux.HandleFunc("/review/import", a.handle((*app).reviewImport))
	mux.HandleFunc("/review/import/commit", a.handle((*app).commitReviewImport))
	mux.HandleFunc("/review/import/validate", a.handle((*app).validateReviewImport))
	mux.HandleFunc("/review/candidate/dismiss", a.handle((*app).dismissReviewCandidate))
	mux.HandleFunc("/review/statement", a.handle((*app).downloadStatement))
	mux.HandleFunc("/review/statement/reassign", a.handle((*app).reassignStatement))
	mux.HandleFunc("/review/statement/delete", a.handle((*app).deleteStatement))
	mux.HandleFunc("/review/statement/document/delete", a.handle((*app).deleteSharedStatement))
	mux.HandleFunc("/review/reviewed", a.handle((*app).markReviewed))
	mux.HandleFunc("/review/rules/save", a.handle((*app).saveImportRuleHandler))
	mux.HandleFunc("/review/rules/delete", a.handle((*app).deleteImportRuleHandler))
	mux.HandleFunc("/budgets", a.handle((*app).budgets))
	mux.HandleFunc("/budgets/create-year", a.handle((*app).createBudgetYear))
	mux.HandleFunc("/budgets/create", a.handle((*app).createBudgetEntry))
	mux.HandleFunc("/budgets/update", a.handle((*app).updateBudgetEntry))
	mux.HandleFunc("/budgets/delete", a.handle((*app).deleteBudgetEntry))
	mux.HandleFunc("/categories", a.handle((*app).categories))
	mux.HandleFunc("/categories/create", a.handle((*app).createCategory))
	mux.HandleFunc("/categories/update", a.handle((*app).updateCategory))
	mux.HandleFunc("/categories/delete", a.handle((*app).deleteCategory))
	mux.HandleFunc("/payees", a.handle((*app).payees))
	mux.HandleFunc("/payees/create", a.handle((*app).createPayee))
	mux.HandleFunc("/payees/update", a.handle((*app).updatePayee))
	mux.HandleFunc("/payees/delete", a.handle((*app).deletePayee))
	mux.HandleFunc("/assistant", a.handle((*app).assistant))
	mux.HandleFunc("/assistant/ask", a.handle((*app).askAssistant))
	mux.HandleFunc("/assistant/feedback", a.handle((*app).assistantFeedback))
	mux.HandleFunc("/assistant/tools", a.handle((*app).askAssistantTools))
	mux.HandleFunc("/reports", a.handle((*app).reports))
	mux.HandleFunc("/audit", a.handle((*app).auditPage))
	mux.HandleFunc("/settings", a.handle((*app).settings))
	mux.HandleFunc("/settings/update", a.handle((*app).updateSettings))
	mux.HandleFunc("/settings/database/create", a.handle((*app).createDatabaseFile))
	mux.HandleFunc("/settings/database/convert", a.handle((*app).convertDatabaseFile))
	mux.HandleFunc("/settings/llm-models", a.handle((*app).llmModels))
	mux.HandleFunc("/settings/prompt", a.handle((*app).downloadBuiltinPrompt))
	mux.HandleFunc("/static/app.css", a.handle((*app).css))
	mux.HandleFunc("/static/theme.css", a.handle((*app).themeCSS))
	mux.HandleFunc("/static/app.js", a.handle((*app).js))
	mux.HandleFunc("/favicon.svg", a.handle((*app).favicon))
	mux.HandleFunc("/auth/login", a.handle((*app).authLogin))
	mux.HandleFunc("/auth/callback", a.handle((*app).authCallback))
	mux.HandleFunc("/auth/logout", a.handle((*app).authLogout))
	return a.requireAuth(mux) // pass-through when the gate is disabled
}

func newApp(cfg config) (*app, error) {
	if err := ensureThemeFiles(defaultThemeDir); err != nil {
		return nil, err
	}
	db, compat, currFmt, err := openRuntimeDB(cfg)
	if err != nil {
		return nil, err
	}
	a := &app{db: db, config: cfg, version: strings.TrimSpace(versionRaw), history: parseHistory(historyRaw), compat: compat, currFmt: currFmt, ai: &aiCache{}}
	a.installAuditStore(cfg)
	initial := cfg
	a.llm = newLLMClient(&initial)
	a.tr = newTranslator(&initial, a.llm)
	a.tr.aiAvailable = func() bool { return a.current().aiRuntimeStatus(false).Available }
	tmpl, err := template.New("app").Funcs(template.FuncMap{
		"money":     func(v float64) string { return formatMoney(v, a.currency()) },
		"short":     short,
		"payee":     payeeHTML,
		"today":     formatDate,
		"yearMonth": yearMonth,
		"yearOnly":  yearOnly,
		"isPDF":     isPDFName,
		"dict":      templateDict,
		"highlight": func(dest string, id int) string {
			return appendHighlight(dest, strconv.Itoa(id))
		},
		"blankSplits": func(n int) []splitLine {
			if n < 0 {
				n = 0
			}
			return make([]splitLine, n)
		},
		"showCategoryNav": func(info dbInfo, c category) bool {
			return !info.HideInactiveItems || c.Active
		},
		"showPayeeNav": func(info dbInfo, p payee) bool {
			return !info.HideInactiveItems || p.Active
		},
		"selectableCategory": func(info dbInfo, c category) bool {
			return c.Active && (info.AllowTopLevelCategories || c.Parent != "")
		},
		"selectablePayee": func(p payee) bool {
			return p.Active
		},
	}).Parse(templates)
	if err != nil {
		db.Close()
		return nil, err
	}
	a.tmpl = tmpl
	if cfg.AuthEnabled {
		auth, err := newOIDCAuth(cfg)
		if err != nil {
			db.Close()
			return nil, fmt.Errorf("init auth: %w", err)
		}
		a.auth = auth
	}
	a.startRuntime()
	return a, nil
}

func openRuntimeDB(cfg config) (*sql.DB, compatibilityReport, currencyFormat, error) {
	if strings.TrimSpace(cfg.DBPath) == "" {
		return nil, compatibilityReport{}, currencyFormat{}, errors.New("database path is required")
	}
	if _, err := os.Stat(cfg.DBPath); err != nil {
		return nil, compatibilityReport{}, currencyFormat{}, err
	}
	mode := "ro"
	if cfg.WriteEnabled {
		mode = "rw"
	}
	db, err := openSQLite(cfg.DBPath, mode, cfg.DBKey)
	if err != nil {
		return nil, compatibilityReport{}, currencyFormat{}, err
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, compatibilityReport{}, currencyFormat{}, err
	}
	// SQLite reads the file lazily, so a wrong key surfaces on the first real
	// query rather than at open. Force it here to fail with a clear message.
	if databaseEncrypted(cfg.DBPath) {
		var n int
		if err := db.QueryRow("SELECT count(*) FROM sqlite_schema").Scan(&n); err != nil {
			db.Close()
			if wrongDatabaseKey(err) {
				err = errWrongDatabaseKey
			}
			return nil, compatibilityReport{}, currencyFormat{}, err
		}
	}
	a := &app{db: db, config: cfg}
	compat := a.checkCompatibility()
	if cfg.WriteEnabled && !compat.OK {
		db.Close()
		return nil, compatibilityReport{}, currencyFormat{}, errors.New(compat.Details)
	}
	return db, compat, a.loadBaseCurrencyFormat(), nil
}

func resolveStartupDatabase(cfg config) (config, []databaseFile, error) {
	scanPath := strings.TrimSpace(cfg.DBPath)
	if scanPath == "" {
		return cfg, nil, errors.New("database path is required")
	}
	scanDir, preferred, setupPath, missing := databaseScanPaths(scanPath)
	cfg.DBScanPath = scanDir
	if missing != nil && !errors.Is(missing, fs.ErrNotExist) {
		cfg.DBPath = setupPath
		cfg.WriteEnabled = false
		return cfg, nil, missing
	}
	// A db_path naming a file that has since been deleted or renamed is not a
	// first run as long as the folder around it still holds databases: scan and
	// pick from what is actually there rather than dropping into setup.
	files, err := scanDatabaseFiles(scanDir)
	if missing != nil && (err != nil || len(files) == 0) {
		cfg.DBPath = setupPath
		cfg.WriteEnabled = false
		return cfg, nil, fs.ErrNotExist
	}
	if err != nil {
		return cfg, nil, err
	}
	if len(files) == 0 {
		cfg.DBPath = setupPath
		cfg.WriteEnabled = false
		return cfg, nil, fs.ErrNotExist
	}
	// A db_path naming one specific file is an explicit pin (it can come from
	// MMXC_DB_PATH), so it outranks the selection Settings remembered; the first
	// file in the folder is the last resort.
	selected := files[0]
	if file, ok := findDatabaseFile(files, preferred, cfg.SelectedDB); ok {
		selected = file
	}
	cfg.DBPath = selected.Path
	cfg.WriteEnabled = selected.WriteEnabled
	cfg.SelectedDB = selected.Name
	return cfg, files, nil
}

// findDatabaseFile returns the first candidate that names one of the scanned
// databases. A candidate may be a full path or a bare file name, so a
// remembered selection survives the whole folder moving (a container mount
// path changing, say) and a candidate naming a file that is gone is simply
// skipped.
func findDatabaseFile(files []databaseFile, candidates ...string) (databaseFile, bool) {
	for _, candidate := range candidates {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		name := filepath.Base(candidate)
		for _, file := range files {
			if samePath(file.Path, candidate) || file.Name == name {
				return file, true
			}
		}
	}
	return databaseFile{}, false
}

func databaseScanPaths(scanPath string) (scanDir, preferred, setupPath string, err error) {
	clean := filepath.Clean(scanPath)
	info, statErr := os.Stat(clean)
	if statErr != nil {
		if errors.Is(statErr, os.ErrNotExist) {
			if isDatabaseFileName(clean) {
				dir := filepath.Dir(clean)
				return dir, clean, clean, fs.ErrNotExist
			}
			return clean, "", filepath.Join(clean, "mmxc.mmb"), fs.ErrNotExist
		}
		return "", "", "", statErr
	}
	if info.IsDir() {
		return clean, "", filepath.Join(clean, "mmxc.mmb"), nil
	}
	return filepath.Dir(clean), clean, clean, nil
}

func scanDatabaseFiles(scanDir string) ([]databaseFile, error) {
	entries, err := os.ReadDir(scanDir)
	if err != nil {
		return nil, err
	}
	var files []databaseFile
	for _, entry := range entries {
		if entry.IsDir() || !isDatabaseFileName(entry.Name()) {
			continue
		}
		path := filepath.Join(scanDir, entry.Name())
		files = append(files, databaseFile{
			Path:         path,
			Name:         entry.Name(),
			WriteEnabled: databaseFileWritable(path),
			Encrypted:    databaseEncrypted(path),
		})
	}
	sort.Slice(files, func(i, j int) bool {
		return strings.ToLower(files[i].Name) < strings.ToLower(files[j].Name)
	})
	return files, nil
}

func databaseFileWritable(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.Mode().Perm()&0o222 != 0
}

func samePath(a, b string) bool {
	aa, errA := filepath.Abs(a)
	bb, errB := filepath.Abs(b)
	if errA == nil && errB == nil {
		return aa == bb
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

// installRuntimeDB makes db the open database. The previous pool and audit
// store close once no request still uses them (runtime.go).
func (a *app) installRuntimeDB(cfg config, db *sql.DB, compat compatibilityReport, currFmt currencyFormat) {
	// The audit store follows the database: switching files, or encrypting one,
	// changes both its path and its key.
	audits, auditStatus := openAuditState(cfg)
	a.publish(func(s *runtimeState) {
		s.db, s.config, s.compat, s.currFmt = db, cfg, compat, currFmt
		s.audits, s.auditStatus = audits, auditStatus
	})
}

func (a *app) settings(w http.ResponseWriter, _ *http.Request) {
	a.aiRuntimeStatus(true)
	a.render(w, "settings", pageData{Title: "Settings", Active: "settings", Info: a.info()})
}

func (a *app) startPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, a.startPagePath(), http.StatusSeeOther)
}

func (a *app) startPagePath() string {
	startPage, _ := normalizeAppStartPage(a.config.AppStartPage)
	switch {
	case startPage == "accounts":
		return "/accounts"
	case startPage == "reports", !a.assistantReady():
		return "/reports"
	}
	return "/assistant"
}

func (a *app) css(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(css))
}

func (a *app) js(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(js))
}

func (a *app) favicon(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(favicon))
}

func (a *app) render(w http.ResponseWriter, name string, data pageData) {
	data.lang = a.config.TranslationLanguage
	data.tr = a.tr
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := a.tmpl.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("render %s: %v", name, err)
	}
}

func (a *app) renderError(w http.ResponseWriter, title, active string, err error) {
	w.WriteHeader(http.StatusInternalServerError)
	a.render(w, "error", pageData{Title: title, Active: active, Info: a.info(), Error: err.Error()})
}

// httpError writes a plain-text HTTP error translated into the active UI
// language. Use it for fixed, user-facing validation/guard messages (not
// dynamic err.Error() text). Pass-through (English) when no translation model
// is configured, so it is safe everywhere.
func (a *app) httpError(w http.ResponseWriter, msg string, status int, guidance ...string) {
	g := ""
	if len(guidance) > 0 {
		g = guidance[0]
	} else {
		g = httpErrorGuidance(msg)
	}
	http.Error(w, a.translateUI(msg, g), status)
}

func (a *app) translateUI(source, guidance string) string {
	if a.tr == nil {
		return source
	}
	return a.tr.get(a.config.TranslationLanguage, source, guidance)
}

func httpErrorGuidance(msg string) string {
	switch {
	case msg == "method not allowed":
		return "HTTP error for an unsupported request method"
	case msg == "write mode is disabled":
		return "HTTP error: the personal-finance database is currently read-only"
	case strings.Contains(msg, "amount"):
		return "HTTP validation error for a money amount field"
	case strings.Contains(msg, "budget"):
		return "HTTP validation error for budget settings or budget entries"
	case strings.Contains(msg, "category"):
		return "HTTP validation error for transaction categories"
	case strings.Contains(msg, "payee"):
		return "HTTP validation error for transaction payees"
	case strings.Contains(msg, "account"):
		return "HTTP validation error for a financial account"
	case strings.Contains(msg, "transaction"):
		return "HTTP validation error for a financial transaction"
	case strings.Contains(msg, "import candidate"):
		return "HTTP validation error for statement transaction import candidates"
	case strings.Contains(msg, "prompt") || strings.Contains(msg, "clarification"):
		return "HTTP validation error for the AI assistant question form"
	}
	return "HTTP validation or guard error message in a personal-finance web app"
}

func (a *app) info() dbInfo {
	ai := a.aiRuntimeStatus(false)
	info := dbInfo{
		Version:                 a.version,
		History:                 a.history,
		ConfigPath:              a.configPath,
		Path:                    a.config.DBPath,
		DBScanPath:              a.config.DBScanPath,
		DBFiles:                 a.dbFiles,
		SeedCurrencies:          seedCurrencies,
		HTTPAddr:                a.config.HTTPAddr,
		BackupDir:               a.config.BackupDir,
		BackupPruneEnabled:      a.config.BackupPruneEnabled,
		BackupRetentionDays:     a.config.BackupRetentionDays,
		BackupRetentionMinCount: a.config.BackupRetentionMinCount,
		MMEXVersion:             a.infoValue("MMEXVERSION"),
		DataVersion:             a.infoValue("DATAVERSION"),
		BaseCurrency:            a.infoValue("BASECURRENCYNAME"),
		WriteEnabled:            a.config.WriteEnabled,
		AllowTopLevelCategories: a.config.AllowTopLevelCategories,
		AllowTransactionDelete:  a.config.AllowTransactionDelete,
		HideInactiveItems:       a.config.HideInactiveItems,
		ShowAccountBalance:      a.config.ShowAccountBalance,
		StatementDir:            a.config.StatementDir,
		AuditLog:                a.config.AuditLog,
		AuditDir:                a.config.AuditDir,
		AuditRetentionDays:      a.config.AuditRetentionDays,
		AuditStatus:             a.auditStatus,
		LLMBaseURL:              a.config.LLMBaseURL,
		LLMAPIKeySet:            strings.TrimSpace(a.config.LLMAPIKey) != "",
		LLMModel:                a.config.LLMModel,
		LLMTimeoutSeconds:       int(a.config.LLMTimeout / time.Second),
		LLMReasoningEffort:      a.config.LLMReasoningEffort,
		AIConfigured:            ai.Configured,
		AIAvailable:             ai.Available,
		AIStatus:                ai.Status,
		AIDetail:                ai.Detail,
		AssistantAvailable:      ai.AssistantAvailable,
		LLMResolveEnabled:       a.config.LLMResolveEnabled,
		PromptDir:               a.config.PromptDir,
		Prompts:                 a.prompts().Status(),
		AssistantQueryDetails:   a.config.AssistantQueryDetails,
		AppStartPage:            mustNormalizeAppStartPage(a.config.AppStartPage),
		Theme:                   a.config.Theme,
		ThemeMode:               a.config.ThemeMode,
		Themes:                  themeOptions(defaultThemeDir),
		Status:                  a.compat.Status,
		Compatible:              a.compat.OK,
		CompatibilityDetail:     a.compat.Details,
		AuthEnabled:             a.auth != nil && a.auth.enabled,

		TranslationLanguage: a.config.TranslationLanguage,
		TranslationModel:    a.config.TranslationModel,
		TranslationDir:      a.config.TranslationDir,
	}
	info.AccountCount = a.count("ACCOUNTLIST_V1")
	info.TransCount = a.count("CHECKINGACCOUNT_V1")
	info.CategoryCount = a.count("CATEGORY_V1")
	info.PayeeCount = a.count("PAYEE_V1")
	info.BudgetCount = a.count("BUDGETYEAR_V1")
	if count, size, err := a.backupStats(); err == nil {
		info.BackupCount = count
		info.BackupSize = humanBytes(size)
	}
	if a.audits != nil {
		info.AuditCount = a.audits.count()
		info.AuditPath = a.audits.path
		info.AuditEncrypted = databaseEncrypted(a.audits.path)
	}
	return info
}

// requiredSchema lists the MMEX tables and columns MMXC reads or writes. It is
// the single source of truth for the local-database compatibility check
// (checkCompatibility).
func requiredSchema() map[string][]string {
	return map[string][]string{
		"ACCOUNTLIST_V1": {
			"ACCOUNTID", "ACCOUNTNAME", "ACCOUNTTYPE", "ACCOUNTNUM", "STATUS", "NOTES",
			"HELDAT", "WEBSITE", "CONTACTINFO", "ACCESSINFO", "INITIALBAL", "FAVORITEACCT",
			"CURRENCYID", "STATEMENTDATE", "MINIMUMBALANCE", "CREDITLIMIT", "INTERESTRATE",
			"PAYMENTDUEDATE", "MINIMUMPAYMENT", "INITIALDATE",
		},
		"BUDGETTABLE_V1":       {"BUDGETENTRYID", "CATEGID"},
		"CATEGORY_V1":          {"CATEGID", "CATEGNAME", "ACTIVE", "PARENTID"},
		"CHECKINGACCOUNT_V1":   {"TRANSID", "ACCOUNTID", "TOACCOUNTID", "PAYEEID", "TRANSCODE", "TRANSAMOUNT", "STATUS", "TRANSACTIONNUMBER", "NOTES", "CATEGID", "TRANSDATE", "FOLLOWUPID", "TOTRANSAMOUNT", "LASTUPDATEDTIME", "DELETEDTIME", "COLOR"},
		"CURRENCYFORMATS_V1":   {"CURRENCYID", "CURRENCYNAME", "CURRENCY_SYMBOL"},
		"INFOTABLE_V1":         {"INFONAME", "INFOVALUE"},
		"PAYEE_V1":             {"PAYEEID", "PAYEENAME", "CATEGID", "NUMBER", "WEBSITE", "NOTES", "ACTIVE", "PATTERN"},
		"SPLITTRANSACTIONS_V1": {"SPLITTRANSID", "TRANSID", "CATEGID", "SPLITTRANSAMOUNT", "NOTES"},
	}
}

// minDataVersion is the MMEX INFOTABLE DATAVERSION whose *_V1 schema this app
// targets. Databases older than this use an incompatible layout and must be
// upgraded in MMEX desktop before MMXC will write to them.
const minDataVersion = 3

func (a *app) checkCompatibility() compatibilityReport {
	// Reject databases older than the schema generation we target. A genuinely
	// ancient file may lack INFOTABLE_V1 entirely, in which case the table check
	// below reports the missing tables; when the table is present we can give the
	// clearer "upgrade in MMEX desktop" message.
	if a.tableExists("INFOTABLE_V1") {
		raw := strings.TrimSpace(a.infoValue("DATAVERSION"))
		if v, err := strconv.Atoi(raw); err == nil && v < minDataVersion {
			return compatibilityReport{
				OK:     false,
				Status: "Unsupported MMEX database version",
				Details: fmt.Sprintf("Database DATAVERSION %d is older than the supported version %d. "+
					"Open and upgrade this file in MMEX desktop first.", v, minDataVersion),
			}
		}
	}
	required := requiredSchema()
	var missing []string
	for table, columns := range required {
		if !a.tableExists(table) {
			missing = append(missing, table)
			continue
		}
		existing, err := a.tableColumns(table)
		if err != nil {
			missing = append(missing, table+" columns")
			continue
		}
		for _, column := range columns {
			if !existing[column] {
				missing = append(missing, table+"."+column)
			}
		}
	}
	if len(missing) > 0 {
		return compatibilityReport{
			OK:      false,
			Status:  "Incompatible MMEX schema",
			Details: "Missing required schema elements: " + strings.Join(missing, ", "),
		}
	}
	return compatibilityReport{
		OK:      true,
		Status:  "MMEX schema compatible",
		Details: "Required manual-edit tables and columns are present.",
	}
}

func getenv(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

// applyEnvOverrides overlays deployment environment variables onto cfg before
// startup. DB path is the directory/file scanned once for .mmb databases.
func applyEnvOverrides(cfg config) config {
	envStr("MMXC_DB_PATH", &cfg.DBPath)
	// Unattended restarts of an encrypted database; otherwise the unlock page asks.
	envStr("MMXC_DB_PASSWORD", &cfg.DBKey)
	envSecretFile("MMXC_DB_PASSWORD_FILE", "MMXC_DB_PASSWORD", &cfg.DBKey)
	envStr("MMXC_HTTP_ADDR", &cfg.HTTPAddr)

	envBool("MMXC_AUTH_ENABLED", &cfg.AuthEnabled)
	envStr("MMXC_OIDC_ISSUER", &cfg.OIDCIssuer)
	envStr("MMXC_OIDC_CLIENT_ID", &cfg.OIDCClientID)
	envStr("MMXC_OIDC_CLIENT_SECRET", &cfg.OIDCClientSecret)
	envStr("MMXC_OIDC_REDIRECT_URL", &cfg.OIDCRedirectURL)
	envStr("MMXC_OIDC_SCOPES", &cfg.OIDCScopes)
	envStr("MMXC_OIDC_AUTH_URL", &cfg.OIDCAuthURL)
	envStr("MMXC_OIDC_TOKEN_URL", &cfg.OIDCTokenURL)
	envStr("MMXC_SESSION_SECRET", &cfg.SessionSecret)
	envInt("MMXC_SESSION_TTL_HOURS", &cfg.SessionTTLHours)
	envBool("MMXC_TRUST_FORWARDED_PROTO", &cfg.TrustForwardedProto)
	return cfg
}

func envStr(name string, dst *string) {
	if value, ok := os.LookupEnv(name); ok {
		*dst = strings.TrimSpace(value)
	}
}

func envBool(name string, dst *bool) {
	if value, ok := os.LookupEnv(name); ok {
		if b, err := strconv.ParseBool(strings.TrimSpace(value)); err == nil {
			*dst = b
		}
	}
}

func envInt(name string, dst *int) {
	if value, ok := os.LookupEnv(name); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(value)); err == nil {
			*dst = n
		}
	}
}

// envSecretFile overlays a secret read from the file named by the fileVar
// environment variable — the Docker/systemd convention (`/run/secrets/…`,
// `$CREDENTIALS_DIRECTORY/…`), where the file lives on tmpfs. That keeps the
// secret out of the environment, out of `docker inspect`, and off the disk it
// protects. It wins over the inline value in envVar (named here only so the
// message can say which won), because pointing at a secret file is the more
// deliberate choice.
//
// A secret that cannot be read is logged and skipped rather than fatal: for the
// database password the unlock page then asks for it, which is a safe fallback.
func envSecretFile(fileVar, envVar string, dst *string) {
	path := strings.TrimSpace(os.Getenv(fileVar))
	if path == "" {
		return
	}
	secret, err := readSecretFile(path)
	if err != nil {
		log.Printf("%s: %v", fileVar, err)
		return
	}
	if secret == "" {
		log.Printf("%s: %s is empty", fileVar, path)
		return
	}
	if *dst != "" && *dst != secret {
		log.Printf("%s and %s are both set; using %s", envVar, fileVar, fileVar)
	}
	*dst = secret
}

// readSecretFile reads a secret from path. Only a trailing newline is stripped
// (echo and most secret managers add one); other whitespace is kept, because a
// password may legitimately contain spaces.
func readSecretFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	// Docker mounts secrets world-readable (0444), so only writability is worth
	// warning about: anyone who can rewrite the file can choose the secret.
	if info, statErr := os.Stat(path); statErr == nil && info.Mode().Perm()&0o022 != 0 {
		log.Printf("secret file %s is writable by other users (mode %04o)", path, info.Mode().Perm())
	}
	return strings.TrimRight(string(data), "\r\n"), nil
}
