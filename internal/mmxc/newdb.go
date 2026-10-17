package mmxc

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// newDBDataVersion is the MMEX INFOTABLE DATAVERSION written into a freshly
// created database. It must be >= minDataVersion so the new file passes
// checkCompatibility().
const newDBDataVersion = "3"

// seedCurrency is one entry in the built-in currency list used to seed a new
// database and to offer the base-currency choice in the setup UI. Code is the
// ISO 4217 code, stored as CURRENCY_SYMBOL (which MMEX requires to be unique).
type seedCurrency struct {
	Code   string
	Name   string
	Prefix string // PFX_SYMBOL
	Suffix string // SFX_SYMBOL
	Scale  int    // minor units: 100 for cents, 1 for currencies with none
}

// seedCurrencies is a curated set of common world currencies. MMXC has no
// currency-management UI, so this is what a new database ships with; more can be
// added later from MMEX desktop if needed.
//
// The first entry is the default: it heads every currency list the UI and the
// CLI offer, and is what a create with no currency named falls back to. Move a
// different currency to the front to change that — nothing else needs editing.
var seedCurrencies = []seedCurrency{
	{"USD", "US Dollar", "$", "", 100},
	{"CAD", "Canadian Dollar", "$", "", 100},
	{"EUR", "Euro", "€", "", 100},
	{"GBP", "British Pound", "£", "", 100},
	{"JPY", "Japanese Yen", "¥", "", 1},
	{"CHF", "Swiss Franc", "CHF ", "", 100},
	{"AUD", "Australian Dollar", "$", "", 100},
	{"NZD", "New Zealand Dollar", "$", "", 100},
	{"CNY", "Chinese Yuan", "¥", "", 100},
	{"HKD", "Hong Kong Dollar", "HK$", "", 100},
	{"SGD", "Singapore Dollar", "S$", "", 100},
	{"INR", "Indian Rupee", "₹", "", 100},
	{"BRL", "Brazilian Real", "R$", "", 100},
	{"MXN", "Mexican Peso", "$", "", 100},
	{"ZAR", "South African Rand", "R", "", 100},
	{"SEK", "Swedish Krona", "", " kr", 100},
	{"NOK", "Norwegian Krone", "", " kr", 100},
	{"DKK", "Danish Krone", "", " kr", 100},
	{"PLN", "Polish Zloty", "", " zł", 100},
	{"CZK", "Czech Koruna", "", " Kč", 100},
	{"HUF", "Hungarian Forint", "", " Ft", 100},
	{"TRY", "Turkish Lira", "₺", "", 100},
	{"RUB", "Russian Ruble", "₽", "", 100},
	{"KRW", "South Korean Won", "₩", "", 1},
	{"THB", "Thai Baht", "฿", "", 100},
	{"IDR", "Indonesian Rupiah", "Rp", "", 100},
	{"MYR", "Malaysian Ringgit", "RM", "", 100},
	{"PHP", "Philippine Peso", "₱", "", 100},
	{"AED", "UAE Dirham", "AED ", "", 100},
	{"SAR", "Saudi Riyal", "SAR ", "", 100},
	{"ILS", "Israeli Shekel", "₪", "", 100},
}

// defaultCurrency is the base currency offered when none is named: the head of
// seedCurrencies.
func defaultCurrency() seedCurrency { return seedCurrencies[0] }

// currencyByCode resolves a (case-insensitive) ISO code against seedCurrencies.
func currencyByCode(code string) (seedCurrency, bool) {
	code = strings.ToUpper(strings.TrimSpace(code))
	for _, c := range seedCurrencies {
		if c.Code == code {
			return c, true
		}
	}
	return seedCurrency{}, false
}

// createDatabase stamps out a new, empty MMEX-compatible .mmb at path with the
// given base currency: the full canonical schema, the built-in currency list,
// the default category tree, and the minimal INFOTABLE rows MMEX and MMXC need
// to open the file. It refuses to overwrite an existing file.
func createDatabase(path, currencyCode string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return errors.New("database path is required")
	}
	if strings.TrimSpace(currencyCode) == "" {
		currencyCode = defaultCurrency().Code
	}
	base, ok := currencyByCode(currencyCode)
	if !ok {
		return fmt.Errorf("unknown currency %q (run `mmxc init --list-currencies` for the list)", currencyCode)
	}

	switch _, err := os.Stat(path); {
	case err == nil:
		return fmt.Errorf("refusing to overwrite existing file %s", path)
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}

	db, err := openSQLite(path, "rwc", "")
	if err != nil {
		return err
	}
	if err := applySchemaAndSeed(db, base); err != nil {
		db.Close()
		// Don't leave a half-built file behind on failure.
		os.Remove(path)
		return err
	}
	return db.Close()
}

func applySchemaAndSeed(db *sql.DB, base seedCurrency) error {
	if _, err := db.Exec(schemaSQL); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	baseID := 0
	for i, c := range seedCurrencies {
		id := i + 1
		if c.Code == base.Code {
			baseID = id
		}
		if _, err := tx.Exec(`INSERT INTO CURRENCYFORMATS_V1
			(CURRENCYID, CURRENCYNAME, PFX_SYMBOL, SFX_SYMBOL, DECIMAL_POINT, GROUP_SEPARATOR,
			 UNIT_NAME, CENT_NAME, SCALE, BASECONVRATE, CURRENCY_SYMBOL, CURRENCY_TYPE)
			VALUES (?, ?, ?, ?, '.', ',', '', '', ?, 1.0, ?, 'Fiat')`,
			id, c.Name, c.Prefix, c.Suffix, c.Scale, c.Code); err != nil {
			return fmt.Errorf("seed currency %s: %w", c.Code, err)
		}
	}

	if _, err := tx.Exec(seedCategoriesSQL); err != nil {
		return fmt.Errorf("seed categories: %w", err)
	}

	uid, err := newUID()
	if err != nil {
		return err
	}
	now := time.Now().Format("2006-01-02T15:04:05")
	info := [][2]string{
		{"DATAVERSION", newDBDataVersion},
		{"MMEXVERSION", "MMXC"},
		{"DATEFORMAT", "%Y-%m-%d"},
		{"CREATEDATE", now},
		{"UID", uid},
		{"BASECURRENCYID", strconv.Itoa(baseID)},
		{"BASECURRENCYNAME", base.Name},
	}
	for _, kv := range info {
		if _, err := tx.Exec(`INSERT INTO INFOTABLE_V1 (INFONAME, INFOVALUE) VALUES (?, ?)`, kv[0], kv[1]); err != nil {
			return fmt.Errorf("seed info %s: %w", kv[0], err)
		}
	}
	return tx.Commit()
}

// newUID returns a 32-char hex string for the INFOTABLE UID (MMEX stores a
// per-file identifier).
func newUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// runInit implements the `mmxc init` subcommand. It returns a process exit code.
func runInit(args []string) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	dbPath := fs.String("db", "", "path for the new .mmb database (default: db_path/mmxc.mmb when db_path is a directory)")
	currency := fs.String("currency", defaultCurrency().Code, "ISO 4217 base currency code (e.g. USD, EUR, GBP)")
	configPath := fs.String("config", getenv("MMXC_CONFIG", "mmxc.yaml"), "config file used to resolve --db when omitted")
	listCurrencies := fs.Bool("list-currencies", false, "print the available base currency codes and exit")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: mmxc init [--db path] [--currency CODE] [--config path]")
		fmt.Fprintln(fs.Output(), "Create a new, empty MMEX-compatible database.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if *listCurrencies {
		for _, c := range seedCurrencies {
			fmt.Printf("%s  %s\n", c.Code, c.Name)
		}
		return 0
	}

	// Resolve the startup database scan path (config overlaid with env), to both
	// default --db and warn if an explicit --db points outside that scan path.
	explicit := *configPath != "mmxc.yaml" || os.Getenv("MMXC_CONFIG") != ""
	cfg, cfgErr := loadConfig(*configPath, explicit)
	var serverDBPath string
	if cfgErr == nil {
		serverDBPath = defaultDatabaseCreatePath(applyEnvOverrides(cfg).DBPath)
	}

	path := strings.TrimSpace(*dbPath)
	if path == "" {
		if cfgErr != nil {
			fmt.Fprintf(os.Stderr, "mmxc init: %v\n", cfgErr)
			return 1
		}
		path = serverDBPath
	}

	if err := createDatabase(path, *currency); err != nil {
		fmt.Fprintf(os.Stderr, "mmxc init: %v\n", err)
		return 1
	}
	fmt.Printf("Created %s (MMEX DATAVERSION %s, base currency %s).\n", path, newDBDataVersion, strings.ToUpper(*currency))
	if cfgErr == nil && path != serverDBPath {
		fmt.Printf("Note: MMXC scans %s at startup — place the file there to list it in Settings.\n", applyEnvOverrides(cfg).DBPath)
	}
	fmt.Println("Make the .mmb file writable to enable edits, or read-only to run MMXC read-only.")
	return 0
}

func defaultDatabaseCreatePath(scanPath string) string {
	path := strings.TrimSpace(scanPath)
	if path == "" {
		path = defaultConfig().DBPath
	}
	if strings.EqualFold(filepath.Ext(path), ".mmb") {
		return path
	}
	return filepath.Join(path, "mmxc.mmb")
}

// --- Creating a database from Settings ---------------------------------------

// databaseFileName validates the file name typed in Settings and returns it
// with a .mmb extension. It is deliberately a bare file name: the new database
// must land in the folder that is scanned at startup, so it shows up in the
// Database picker.
func databaseFileName(raw string) (string, error) {
	return validateDatabaseName(raw, ".mmb")
}

// validateDatabaseName is databaseFileName for a chosen extension, so an
// encrypted copy can be named .mmbx while new databases stay .mmb. A name that
// already carries one of the database extensions has it swapped rather than
// stacked, so "household.mmb" encrypts to "household.mmbx".
func validateDatabaseName(raw, ext string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", errors.New("database file name is required")
	}
	if strings.ContainsAny(name, `/\`) || name == "." || name == ".." {
		return "", errors.New("database file name must be a plain file name, without a folder path")
	}
	if strings.HasPrefix(name, ".") {
		return "", errors.New("database file name must not start with a dot")
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return "", errors.New("database file name contains an invalid character")
		}
	}
	if current := filepath.Ext(name); isDatabaseFileName(name) && !strings.EqualFold(current, ext) {
		name = strings.TrimSuffix(name, current)
	}
	if !strings.EqualFold(filepath.Ext(name), ext) {
		name += ext
	}
	return name, nil
}

// createDatabaseFile creates an additional database from Settings. The
// first-run setup server only runs while no .mmb exists at all, so this is the
// only way to add another database without leaving the app. It never touches
// the database currently open — read-only mode does not block it — and it
// optionally switches the runtime over to the new file, the same swap the
// Settings database picker performs.
func (a *app) createDatabaseFile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		a.httpError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		a.renderError(w, "Settings", "settings", err)
		return
	}
	scanDir := strings.TrimSpace(a.config.DBScanPath)
	if scanDir == "" {
		a.renderError(w, "Settings", "settings", errors.New("database scan folder is not set"))
		return
	}
	name, err := databaseFileName(r.FormValue("db_name"))
	if err != nil {
		a.renderError(w, "Settings", "settings", err)
		return
	}
	currency := strings.TrimSpace(r.FormValue("currency"))
	path := filepath.Join(scanDir, name)
	if err := createDatabase(path, currency); err != nil {
		a.renderError(w, "Settings", "settings", err)
		return
	}
	a.audit(r, auditEntry{
		Action: fmt.Sprintf("database create currency=%q", strings.ToUpper(currency)),
		Detail: fmt.Sprintf("path=%q", path),
	})

	// Refresh the picker so the new file is selectable even if it is not opened
	// right away.
	files, err := scanDatabaseFiles(scanDir)
	if err != nil {
		a.renderError(w, "Settings", "settings", err)
		return
	}
	a.publish(func(s *runtimeState) { s.dbFiles = files })

	if formBoolDefault(r, "open_now", false) {
		next := a.config
		next.DBPath = path
		next.WriteEnabled = databaseFileWritable(path)
		next.SelectedDB = name
		db, compat, currFmt, err := openRuntimeDB(next)
		if err != nil {
			a.renderError(w, "Settings", "settings", err)
			return
		}
		// Remember the switch the same way the Settings database picker does,
		// so a restart reopens the database that is open now.
		if err := writeConfigUpdates(a.configPath, map[string]any{"selected_db": next.SelectedDB}); err != nil {
			db.Close()
			a.renderError(w, "Settings", "settings", err)
			return
		}
		a.installRuntimeDB(next, db, compat, currFmt)
		a.audit(r, auditEntry{Action: "database switch", Detail: fmt.Sprintf("path=%q", path)})
	}
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

// --- First-run web setup ------------------------------------------------------

// setupServer serves the first-run page when no database exists yet. The POST
// handler creates the database in place; once it succeeds, serve() returns so
// main() can open the new file normally.
type setupServer struct {
	cfg  config
	done chan struct{}
	once sync.Once
}

// serveSetup blocks serving the first-run setup UI on cfg.HTTPAddr until a
// database has been created, then returns nil.
func serveSetup(cfg config) error {
	s := &setupServer{cfg: cfg, done: make(chan struct{})}
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handle)
	srv := &http.Server{Addr: cfg.HTTPAddr, Handler: mux}

	go func() {
		<-s.done
		// Let the success response flush before tearing the listener down.
		time.Sleep(250 * time.Millisecond)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	log.Printf("no database found for %s — open %s to create one", cfg.DBPath, listenURL(cfg.HTTPAddr))
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (s *setupServer) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		// The database is created at the startup resolver's setup path; the user
		// only chooses the base currency. The scan itself is intentionally fixed
		// until the process restarts.
		if err := createDatabase(s.cfg.DBPath, r.FormValue("currency")); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			s.render(w, err)
			return
		}
		s.renderDone(w, s.cfg.DBPath)
		s.once.Do(func() { close(s.done) })
		return
	}
	s.render(w, nil)
}

func (s *setupServer) render(w http.ResponseWriter, formErr error) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	data := struct {
		DBPath     string
		Error      string
		Currencies []seedCurrency
	}{DBPath: s.cfg.DBPath, Currencies: seedCurrencies}
	if formErr != nil {
		data.Error = formErr.Error()
	}
	if err := setupTemplate.Execute(w, data); err != nil {
		log.Printf("render setup: %v", err)
	}
}

func (s *setupServer) renderDone(w http.ResponseWriter, dbPath string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := setupDoneTemplate.Execute(w, dbPath); err != nil {
		log.Printf("render setup done: %v", err)
	}
}

var setupTemplate = template.Must(template.New("setup").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>MMXC — Set up your database</title>
<style>
 :root{color-scheme:light dark;--bg:#faf8f4;--panel:#ffffff;--line:#e7e1d6;
   --line-strong:#d0c7b6;--text:#2a2620;--muted:#7a7264;--accent:#c07828;
   --accent-soft:#f6e9d2;--accent-strong:#a0631d;--field-bg:#ffffff;
   --danger:#b42318;--danger-soft:#fdeceb;--danger-border:#f2a7a2;
   --shadow-card:rgba(60,45,20,.09)}
 @media (prefers-color-scheme:dark){:root{--bg:#18160f;--panel:#221f17;
   --line:#34302a;--line-strong:#443f36;--text:#ece7dd;--muted:#a89f8e;
   --accent:#d9974a;--accent-soft:#352713;--accent-strong:#e4a960;
   --field-bg:#2a261d;--danger:#f07a72;--danger-soft:#361d1b;
   --danger-border:#6e3531;--shadow-card:rgba(0,0,0,.42)}}
 *{box-sizing:border-box}
 body{font-family:system-ui,-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;
   background:var(--bg);color:var(--text);margin:0;display:flex;min-height:100vh;
   align-items:center;justify-content:center;padding:24px}
 .card{background:var(--panel);border:1px solid var(--line);border-radius:8px;
   padding:2rem;max-width:34rem;width:100%;box-shadow:0 6px 22px var(--shadow-card)}
 h1{margin:0 0 .25rem;font-size:1.3rem;letter-spacing:0}
 p{color:var(--muted);line-height:1.5}
 label{display:block;margin:1rem 0 .35rem;font-weight:600}
 select{width:100%;min-height:34px;padding:.6rem;border-radius:6px;
   border:1px solid var(--line-strong);background:var(--field-bg);
   color:var(--text);font:inherit}
 select:focus-visible{outline:none;border-color:var(--accent);
   box-shadow:0 0 0 3px var(--accent-soft)}
 .path{font-family:ui-monospace,monospace;background:var(--field-bg);
   border:1px solid var(--line-strong);
   border-radius:8px;padding:.6rem;margin:.35rem 0 0;word-break:break-all}
 button{margin-top:1.5rem;width:100%;min-height:34px;padding:.7rem;
   border:1px solid var(--accent);border-radius:6px;background:var(--accent);
   color:#fff;font:inherit;font-size:1rem;font-weight:600;cursor:pointer}
 button:hover{background:var(--accent-strong);border-color:var(--accent-strong)}
 button:focus-visible{outline:2px solid var(--accent);outline-offset:2px}
 .err{background:var(--danger-soft);border:1px solid var(--danger-border);
   color:var(--danger);padding:.6rem .8rem;
   border-radius:8px;margin-top:1rem}
 .hint{font-size:.85rem;color:var(--muted)}
</style></head><body>
<div class="card">
 <h1>Welcome to MMXC</h1>
 <p>No database was found yet. Create a new, empty MMEX-compatible database to get
 started — no need to install Money Manager Ex.</p>
 {{if .Error}}<div class="err">{{.Error}}</div>{{end}}
 <form method="post" action="/">
  <label>Database file</label>
  <p class="path">{{.DBPath}}</p>
  <p class="hint">The startup <code>db_path</code> (or <code>MMXC_DB_PATH</code>) is scanned for
  <code>.mmb</code> files. To create it somewhere else, change that and restart.</p>
  <label for="currency">Base currency</label>
  <select id="currency" name="currency">
   {{range .Currencies}}<option value="{{.Code}}">{{.Code}} — {{.Name}}</option>{{end}}
  </select>
  <button type="submit">Create database</button>
 </form>
 <p class="hint">It will be seeded with a default category tree and common
 currencies. Make the file writable to add accounts and transactions afterwards,
 or read-only to keep MMXC read-only.</p>
</div></body></html>`))

var setupDoneTemplate = template.Must(template.New("setupDone").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta http-equiv="refresh" content="3;url=/">
<title>MMXC — Database created</title>
<style>:root{color-scheme:light dark;--bg:#faf8f4;--panel:#ffffff;--line:#e7e1d6;
 --text:#2a2620;--muted:#7a7264;--accent:#c07828;--shadow-card:rgba(60,45,20,.09)}
 @media (prefers-color-scheme:dark){:root{--bg:#18160f;--panel:#221f17;
 --line:#34302a;--text:#ece7dd;--muted:#a89f8e;--accent:#d9974a;
 --shadow-card:rgba(0,0,0,.42)}}*{box-sizing:border-box}
 body{font-family:system-ui,-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;
 background:var(--bg);color:var(--text);margin:0;padding:24px;display:flex;
 min-height:100vh;align-items:center;justify-content:center;text-align:center}
 div{background:var(--panel);border:1px solid var(--line);border-radius:8px;
 padding:2rem;max-width:34rem;width:100%;box-shadow:0 6px 22px var(--shadow-card)}
 h1{margin:0 0 .25rem;font-size:1.3rem;letter-spacing:0}
 p{color:var(--muted);line-height:1.5}code{color:var(--text)}
 a{color:var(--accent);text-decoration:none;font-weight:600}a:hover{text-decoration:underline}</style></head><body>
<div><h1>Database created</h1>
<p>Created <code>{{.}}</code>. Starting MMXC...</p>
<p>If this page does not refresh, <a href="/">reload</a>.</p></div>
</body></html>`))
