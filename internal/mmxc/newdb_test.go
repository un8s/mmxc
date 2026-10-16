package mmxc

import (
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCreateDatabaseProducesUsableDB(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "new.mmb")

	if err := createDatabase(dbPath, "eur"); err != nil { // lower-case on purpose
		t.Fatalf("createDatabase: %v", err)
	}

	// Open in write mode: this only succeeds if the schema is valid and
	// checkCompatibility() passes (which requires DATAVERSION >= minDataVersion).
	a, err := newApp(config{DBPath: dbPath, WriteEnabled: true, BackupDir: filepath.Join(dir, "backups")})
	if err != nil {
		t.Fatalf("newApp on created db: %v", err)
	}
	defer a.db.Close()

	if got := a.infoValue("DATAVERSION"); got != newDBDataVersion {
		t.Errorf("DATAVERSION = %q, want %q", got, newDBDataVersion)
	}
	if got := a.infoValue("BASECURRENCYNAME"); got != "Euro" {
		t.Errorf("BASECURRENCYNAME = %q, want Euro", got)
	}

	// Every seeded currency should be present, base first.
	currencies, err := a.listCurrencies()
	if err != nil {
		t.Fatalf("listCurrencies: %v", err)
	}
	if len(currencies) != len(seedCurrencies) {
		t.Errorf("got %d currencies, want %d", len(currencies), len(seedCurrencies))
	}
	if len(currencies) > 0 && currencies[0].Symbol != "EUR" {
		t.Errorf("base currency not listed first: %q", currencies[0].Symbol)
	}

	// Transfers need a top-level "Transfer" category.
	transferID, err := a.transferCategoryID()
	if err != nil {
		t.Fatalf("transferCategoryID: %v", err)
	}
	if transferID != 1 {
		t.Errorf("transfer category id = %d, want 1", transferID)
	}

	var categoryCount int
	if err := a.db.QueryRow("SELECT COUNT(*) FROM CATEGORY_V1").Scan(&categoryCount); err != nil {
		t.Fatalf("count categories: %v", err)
	}
	if categoryCount < 20 {
		t.Errorf("default category tree too small: %d rows", categoryCount)
	}
}

func TestCreateDatabaseDefaultsToTheFirstSeedCurrency(t *testing.T) {
	// The head of seedCurrencies is what every currency list offers first, so it
	// is also what a create with no currency named must produce.
	if got := defaultCurrency().Code; got != "USD" {
		t.Fatalf("default currency = %q, want USD at the head of seedCurrencies", got)
	}
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "default.mmb")
	if err := createDatabase(dbPath, ""); err != nil {
		t.Fatalf("createDatabase: %v", err)
	}
	a, err := newApp(config{DBPath: dbPath, BackupDir: filepath.Join(dir, "backups")})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	if got := a.infoValue("BASECURRENCYNAME"); got != "US Dollar" {
		t.Errorf("BASECURRENCYNAME = %q, want US Dollar", got)
	}
}

func TestCreateDatabaseRefusesOverwrite(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "exists.mmb")
	if err := os.WriteFile(dbPath, []byte("not really a db"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := createDatabase(dbPath, "USD"); err == nil {
		t.Fatal("expected createDatabase to refuse overwriting an existing file")
	}
}

func TestCreateDatabaseRejectsUnknownCurrency(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "bad.mmb")
	if err := createDatabase(dbPath, "ZZZ"); err == nil {
		t.Fatal("expected unknown currency to be rejected")
	}
	if _, err := os.Stat(dbPath); err == nil {
		t.Fatal("no file should be created when the currency is invalid")
	}
}

func TestSetupServerCreatesDatabase(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "setup.mmb")
	s := &setupServer{cfg: config{DBPath: dbPath}, done: make(chan struct{})}

	// Only the currency comes from the form; the path is fixed to cfg.DBPath.
	form := url.Values{"currency": {"USD"}}
	req := httptest.NewRequest("POST", "/", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()

	s.handle(rec, req)

	if rec.Code != 200 {
		t.Fatalf("POST /setup status = %d, want 200", rec.Code)
	}
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("database not created: %v", err)
	}
	select {
	case <-s.done:
	default:
		t.Fatal("done channel should be closed after successful creation")
	}
	// The created file must open cleanly.
	a, err := newApp(config{DBPath: dbPath})
	if err != nil {
		t.Fatalf("newApp on setup-created db: %v", err)
	}
	a.db.Close()
}

func TestDatabaseFileName(t *testing.T) {
	for _, tc := range []struct {
		in, want string
	}{
		{"household", "household.mmb"},
		{"  household.mmb  ", "household.mmb"},
		{"Household.MMB", "Household.MMB"},
	} {
		got, err := databaseFileName(tc.in)
		if err != nil {
			t.Fatalf("databaseFileName(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Errorf("databaseFileName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	for _, bad := range []string{"", "   ", "sub/dir.mmb", `..\other.mmb`, "..", ".hidden", "bad\nname"} {
		if got, err := databaseFileName(bad); err == nil {
			t.Errorf("databaseFileName(%q) = %q, want an error", bad, got)
		}
	}
}

func TestCreateDatabaseFileFromSettings(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "current.mmb")
	buildSyntheticDB(t, existing)
	dbFiles, err := scanDatabaseFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := defaultConfig()
	cfg.DBPath = existing
	cfg.DBScanPath = dir
	cfg.WriteEnabled = true
	cfg.BackupDir = filepath.Join(dir, "backups")
	cfg.AuditDir = filepath.Join(dir, "audit")
	a, err := newApp(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { a.db.Close() }()
	a.dbFiles = dbFiles
	a.configPath = filepath.Join(dir, "mmxc.yaml")

	form := url.Values{"db_name": {"second"}, "currency": {"EUR"}, "open_now": {"true"}}
	req := httptest.NewRequest("POST", "/settings/database/create", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()

	a.createDatabaseFile(rec, req)

	if rec.Code != 303 {
		t.Fatalf("createDatabaseFile status = %d, body %q", rec.Code, rec.Body.String())
	}
	created := filepath.Join(dir, "second.mmb")
	if _, err := os.Stat(created); err != nil {
		t.Fatalf("database not created: %v", err)
	}
	// The picker must list both files, and the runtime must have switched to the
	// new (writable) one.
	if len(a.dbFiles) != 2 {
		t.Fatalf("dbFiles = %v, want both databases", a.dbFiles)
	}
	if a.config.DBPath != created {
		t.Fatalf("DBPath = %q, want %q", a.config.DBPath, created)
	}
	if !a.config.WriteEnabled {
		t.Error("a freshly created database should be writable")
	}
	if got := a.infoValue("BASECURRENCYNAME"); got != "Euro" {
		t.Errorf("BASECURRENCYNAME = %q, want Euro", got)
	}
	// The switch must survive a restart, so it is written to the config file.
	if a.config.SelectedDB != "second.mmb" {
		t.Errorf("SelectedDB = %q, want second.mmb", a.config.SelectedDB)
	}
	saved, err := os.ReadFile(a.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(saved), "selected_db: second.mmb") {
		t.Errorf("config file did not record the new database:\n%s", saved)
	}

	// A second create with the same name must fail rather than clobber it, and
	// must leave the open database alone.
	req = httptest.NewRequest("POST", "/settings/database/create", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	a.createDatabaseFile(rec, req)
	if rec.Code == 303 {
		t.Fatal("creating over an existing file should not succeed")
	}
	if a.config.DBPath != created {
		t.Fatalf("DBPath changed after a failed create: %q", a.config.DBPath)
	}
}

func TestCreateDatabaseFileKeepsCurrentDatabaseWhenNotOpening(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "current.mmb")
	buildSyntheticDB(t, existing)
	cfg := defaultConfig()
	cfg.DBPath = existing
	cfg.DBScanPath = dir
	cfg.WriteEnabled = true
	cfg.BackupDir = filepath.Join(dir, "backups")
	cfg.AuditDir = filepath.Join(dir, "audit")
	a, err := newApp(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	a.configPath = filepath.Join(dir, "mmxc.yaml")

	form := url.Values{"db_name": {"spare.mmb"}, "currency": {"USD"}}
	req := httptest.NewRequest("POST", "/settings/database/create", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()

	a.createDatabaseFile(rec, req)

	if rec.Code != 303 {
		t.Fatalf("createDatabaseFile status = %d, body %q", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "spare.mmb")); err != nil {
		t.Fatalf("database not created: %v", err)
	}
	if a.config.DBPath != existing {
		t.Fatalf("DBPath = %q, want the database to stay on %q", a.config.DBPath, existing)
	}
	if len(a.dbFiles) != 2 {
		t.Fatalf("dbFiles = %v, want both databases listed", a.dbFiles)
	}
	if _, err := os.Stat(a.configPath); !os.IsNotExist(err) {
		t.Errorf("config should be untouched when the new database is not opened (stat err %v)", err)
	}
}
