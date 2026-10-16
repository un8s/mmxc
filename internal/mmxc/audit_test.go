package mmxc

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// auditTestApp builds an app whose audit store lives in a temp directory. The
// MMEX file itself never has to exist: only its name decides what the audit
// database is called and whether it is encrypted.
func auditTestApp(t *testing.T, level string) *app {
	t.Helper()
	dir := t.TempDir()
	cfg := config{
		DBPath:   filepath.Join(dir, "household.mmb"),
		AuditDir: filepath.Join(dir, "audit"),
		AuditLog: level,
	}
	a := &app{config: cfg}
	a.installAuditStore(cfg)
	if level != auditOff && a.audits == nil {
		t.Fatalf("audit store did not open: %s", a.auditStatus)
	}
	t.Cleanup(func() { _ = a.audits.Close() })
	return a
}

func TestNormalizeAuditLog(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", auditActions},
		{"  Detailed ", auditDetailed},
		{"OFF", auditOff},
		{"actions", auditActions},
	}
	for _, tc := range cases {
		got, err := normalizeAuditLog(tc.in)
		if err != nil {
			t.Fatalf("normalizeAuditLog(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Errorf("normalizeAuditLog(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if _, err := normalizeAuditLog("verbose"); err == nil {
		t.Fatal("expected an error for an unknown audit level")
	}
}

// The actions level is the default, so this is the property that decides
// whether the audit log leaks: the sensitive half must never reach the store.
func TestAuditActionsLevelDropsDetail(t *testing.T) {
	a := auditTestApp(t, auditActions)
	a.audit(nil, auditEntry{
		Action: "transaction delete id=421 account_id=12",
		Detail: `account="Chequing" payee="Amazon" amount=42.10`,
	})

	records, err := a.audits.recent(10, "")
	if err != nil {
		t.Fatalf("recent: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("got %d records, want 1", len(records))
	}
	if records[0].Action != "transaction delete id=421 account_id=12" {
		t.Errorf("Action = %q", records[0].Action)
	}
	if records[0].Detail != "" {
		t.Errorf("detail leaked at the actions level: %q", records[0].Detail)
	}
}

func TestAuditDetailedLevelKeepsDetail(t *testing.T) {
	a := auditTestApp(t, auditDetailed)
	a.audit(nil, auditEntry{Action: "payee delete id=14", Detail: `name="Amazon"`})

	records, err := a.audits.recent(10, "")
	if err != nil {
		t.Fatalf("recent: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("got %d records, want 1", len(records))
	}
	if records[0].Detail != `name="Amazon"` {
		t.Errorf("Detail = %q, want the recorded detail", records[0].Detail)
	}
}

func TestAuditOffOpensNoStoreAndWritesNothing(t *testing.T) {
	dir := t.TempDir()
	cfg := config{
		DBPath:   filepath.Join(dir, "household.mmb"),
		AuditDir: filepath.Join(dir, "audit"),
		AuditLog: auditOff,
	}
	a := &app{config: cfg}
	a.installAuditStore(cfg)
	if a.audits != nil {
		t.Fatal("expected no audit store when auditing is off")
	}
	// Must not panic, and must leave nothing behind on disk.
	a.audit(nil, auditEntry{Action: "category delete id=66", Detail: `name="Hobb"`})
	if entries, err := os.ReadDir(cfg.AuditDir); err == nil && len(entries) > 0 {
		t.Errorf("audit directory has %d entries with auditing off", len(entries))
	}
}

func TestAuditPruneDropsOldEntries(t *testing.T) {
	a := auditTestApp(t, auditActions)
	now := time.Now()
	if err := a.audits.write(now.AddDate(0, 0, -40), "", "old entry", ""); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := a.audits.write(now, "", "new entry", ""); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := a.audits.prune(now, 30); err != nil {
		t.Fatalf("prune: %v", err)
	}
	records, err := a.audits.recent(10, "")
	if err != nil {
		t.Fatalf("recent: %v", err)
	}
	if len(records) != 1 || records[0].Action != "new entry" {
		t.Fatalf("records = %+v, want only the new entry", records)
	}
}

func TestAuditPruneKeepsEverythingAtZero(t *testing.T) {
	a := auditTestApp(t, auditActions)
	now := time.Now()
	if err := a.audits.write(now.AddDate(0, -5, 0), "", "ancient entry", ""); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := a.audits.prune(now, auditRetentionForever); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if got := a.audits.count(); got != 1 {
		t.Fatalf("count = %d, want the entry kept forever", got)
	}
}

func TestAuditRecentFiltersByQuery(t *testing.T) {
	a := auditTestApp(t, auditActions)
	for _, action := range []string{"payee delete id=14", "category add id=66", "payee rename id=15"} {
		if err := a.audits.write(time.Now(), "", action, ""); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	records, err := a.audits.recent(10, "payee")
	if err != nil {
		t.Fatalf("recent: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("got %d records, want the 2 payee entries", len(records))
	}
}

func TestAuditStorePathFollowsDatabaseEncryption(t *testing.T) {
	plain := auditStorePath(config{DBPath: filepath.Join("data", "household.mmb"), AuditDir: "audit"})
	if got := filepath.Base(plain); got != "household.audit.db" {
		t.Errorf("plain audit file = %q", got)
	}
	encrypted := auditStorePath(config{DBPath: filepath.Join("data", "household.mmbx"), AuditDir: "audit"})
	if got := filepath.Base(encrypted); got != "household.audit.mmbx" {
		t.Errorf("encrypted audit file = %q", got)
	}
}

// An encrypted audit log necessarily carries the .mmbx extension that marks an
// MMXC-encrypted database, so it must live outside the scanned folder or it
// would be offered as a database to open.
func TestAuditStoreStaysOutOfDatabasePicker(t *testing.T) {
	dir := t.TempDir()
	cfg := config{
		DBPath:   filepath.Join(dir, "household.mmbx"),
		AuditDir: filepath.Join(dir, "audit"),
		AuditLog: auditActions,
		DBKey:    "correct horse battery staple",
	}
	a := &app{config: cfg}
	a.installAuditStore(cfg)
	if a.audits == nil {
		t.Fatalf("audit store did not open: %s", a.auditStatus)
	}
	defer a.audits.Close()

	files, err := scanDatabaseFiles(dir)
	if err != nil {
		t.Fatalf("scanDatabaseFiles: %v", err)
	}
	for _, file := range files {
		if strings.Contains(file.Name, "audit") {
			t.Fatalf("audit database %q showed up in the database picker", file.Name)
		}
	}
}

// Encrypting a database must not leave its plaintext audit history sitting in
// the folder next to the encrypted file.
func TestAuditStoreAbsorbsPlaintextLogAfterEncryption(t *testing.T) {
	dir := t.TempDir()
	auditDir := filepath.Join(dir, "audit")
	plain := config{
		DBPath:   filepath.Join(dir, "household.mmb"),
		AuditDir: auditDir,
		AuditLog: auditActions,
	}
	a := &app{config: plain}
	a.installAuditStore(plain)
	if a.audits == nil {
		t.Fatalf("audit store did not open: %s", a.auditStatus)
	}
	a.audit(nil, auditEntry{Action: "category add id=66"})
	if err := a.audits.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	encrypted := plain
	encrypted.DBPath = filepath.Join(dir, "household.mmbx")
	encrypted.DBKey = "correct horse battery staple"
	b := &app{config: encrypted}
	b.installAuditStore(encrypted)
	if b.audits == nil {
		t.Fatalf("encrypted audit store did not open: %s", b.auditStatus)
	}
	defer b.audits.Close()

	records, err := b.audits.recent(10, "")
	if err != nil {
		t.Fatalf("recent: %v", err)
	}
	if len(records) != 1 || records[0].Action != "category add id=66" {
		t.Fatalf("records = %+v, want the entry carried over", records)
	}
	if _, err := os.Stat(filepath.Join(auditDir, "household.audit.db")); !os.IsNotExist(err) {
		t.Error("the plaintext audit log survived encryption")
	}
}

// The audit template is only parsed by the rest of the suite. render() logs a
// template error instead of failing, so without executing the page a bad field
// reference would silently produce broken HTML.
func TestAuditPageRendersEntries(t *testing.T) {
	dir := t.TempDir()
	a, err := newApp(config{
		DBPath:       fixtureDB(t),
		WriteEnabled: false,
		BackupDir:    filepath.Join(dir, "backups"),
		AuditDir:     filepath.Join(dir, "audit"),
		AuditLog:     auditDetailed,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	defer a.audits.Close()
	if a.audits == nil {
		t.Fatalf("audit store did not open: %s", a.auditStatus)
	}
	a.audit(nil, auditEntry{Action: "payee delete id=14", Detail: `name="Amazon"`})

	req := httptest.NewRequest(http.MethodGet, "/audit", nil)
	rec := httptest.NewRecorder()
	a.auditPage(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("audit status = %d", rec.Code)
	}
	html := rec.Body.String()
	for _, want := range []string{"Audit Log", "payee delete id=14", `name=&#34;Amazon&#34;`} {
		if !strings.Contains(html, want) {
			t.Fatalf("audit page missing %q", want)
		}
	}
	// A rendering failure truncates the page, so check it reached the end.
	if !strings.Contains(html, "</html>") {
		t.Fatalf("audit page did not render to completion:\n%s", html)
	}
}

// The search box narrows the table, and the detail column must stay absent when
// the level does not record it.
func TestAuditPageHidesDetailAtActionsLevel(t *testing.T) {
	dir := t.TempDir()
	a, err := newApp(config{
		DBPath:       fixtureDB(t),
		WriteEnabled: false,
		BackupDir:    filepath.Join(dir, "backups"),
		AuditDir:     filepath.Join(dir, "audit"),
		AuditLog:     auditActions,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	defer a.audits.Close()
	a.audit(nil, auditEntry{Action: "payee delete id=14", Detail: `name="Amazon"`})

	req := httptest.NewRequest(http.MethodGet, "/audit", nil)
	rec := httptest.NewRecorder()
	a.auditPage(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("audit status = %d", rec.Code)
	}
	html := rec.Body.String()
	if !strings.Contains(html, "payee delete id=14") {
		t.Fatal("audit page missing the recorded action")
	}
	if strings.Contains(html, "Amazon") {
		t.Fatal("audit page showed detail that the actions level must not record")
	}
}

func TestConfigChangedKeysDropsValues(t *testing.T) {
	keys := configChangedKeys([]string{
		"llm_base_url:http://old->http://new",
		"audit_log:actions->off",
	})
	want := []string{"llm_base_url", "audit_log"}
	if len(keys) != len(want) {
		t.Fatalf("keys = %v, want %v", keys, want)
	}
	for i := range want {
		if keys[i] != want[i] {
			t.Errorf("keys[%d] = %q, want %q", i, keys[i], want[i])
		}
	}
	for _, key := range keys {
		if strings.Contains(key, "->") {
			t.Errorf("key %q still carries a value", key)
		}
	}
}

func TestSplitAuditTimestamp(t *testing.T) {
	date, clock := splitAuditTimestamp("2026-09-16T14:35:02Z")
	if date != "2026-09-16" || clock != "14:35:02" {
		t.Fatalf("split = %q %q", date, clock)
	}
	// Anything unparseable passes through as the date so the page still renders.
	if date, clock := splitAuditTimestamp("not-a-time"); date != "not-a-time" || clock != "" {
		t.Fatalf("split of invalid input = %q %q", date, clock)
	}
}
