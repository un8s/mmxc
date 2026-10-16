package mmxc

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newDeleteTestApp(t *testing.T, allowDelete bool) *app {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.mmb")
	buildSyntheticDB(t, dbPath)
	a, err := newApp(config{
		DBPath:                 dbPath,
		WriteEnabled:           true,
		BackupDir:              filepath.Join(dir, "backups"),
		AllowTransactionDelete: allowDelete,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.db.Close() })
	return a
}

func mustExec(t *testing.T, a *app, stmt string, args ...any) {
	t.Helper()
	if _, err := a.db.Exec(stmt, args...); err != nil {
		t.Fatalf("%s: %v", stmt, err)
	}
}

// Permanent deletion is opt-in: without allow_transaction_delete both the
// handler and the helper refuse, and the row survives.
func TestDeleteTransactionRequiresOptIn(t *testing.T) {
	a := newDeleteTestApp(t, false)

	rec := httptest.NewRecorder()
	a.deleteTransaction(rec, formRequest(url.Values{"trans_id": {"100"}, "account_id": {"12"}}))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("delete status = %d, want %d", rec.Code, http.StatusForbidden)
	}
	if err := a.deleteTransactionRecord(100); err == nil {
		t.Fatal("deleteTransactionRecord succeeded with the setting off")
	}
	if n := a.countWhere("CHECKINGACCOUNT_V1", "TRANSID = ?", 100); n != 1 {
		t.Fatalf("transaction 100 rows = %d, want 1", n)
	}
}

// Deleting a split transaction takes its split lines, tag links (on the
// transaction and on its splits) and custom-field values with it, and leaves
// other transactions' rows alone.
func TestDeleteTransactionRemovesDependentRows(t *testing.T) {
	a := newDeleteTestApp(t, true)

	var splitID int
	if err := a.db.QueryRow("SELECT MIN(SPLITTRANSID) FROM SPLITTRANSACTIONS_V1 WHERE TRANSID = 186").Scan(&splitID); err != nil {
		t.Fatal(err)
	}
	mustExec(t, a, "INSERT INTO TAG_V1 (TAGID, TAGNAME, ACTIVE) VALUES (1, 'synthetic-tag', 1)")
	mustExec(t, a, "INSERT INTO TAGLINK_V1 (TAGLINKID, REFTYPE, REFID, TAGID) VALUES (1, 'Transaction', 186, 1), (2, 'TransactionSplit', ?, 1), (3, 'Transaction', 100, 1)", splitID)
	mustExec(t, a, "INSERT INTO CUSTOMFIELD_V1 (FIELDID, REFTYPE, DESCRIPTION, TYPE, PROPERTIES) VALUES (1, 'Transaction', 'Receipt no.', 'String', '{}')")
	mustExec(t, a, "INSERT INTO CUSTOMFIELDDATA_V1 (FIELDATADID, FIELDID, REFID, CONTENT) VALUES (1, 1, 186, 'A-1'), (2, 1, 100, 'B-2')")

	rec := httptest.NewRecorder()
	a.deleteTransaction(rec, formRequest(url.Values{
		"trans_id":   {"186"},
		"account_id": {"12"},
		"scroll":     {"40"},
		"return_to":  {"/review?account_id=12&month=2025-08"},
	}))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("delete status = %d: %s", rec.Code, rec.Body.String())
	}
	// No highlight parameter: the row it would re-select no longer exists.
	if got := rec.Header().Get("Location"); got != "/review?account_id=12&month=2025-08" {
		t.Fatalf("delete redirect = %q", got)
	}

	for _, check := range []struct {
		table, where string
		args         []any
		want         int
	}{
		{"CHECKINGACCOUNT_V1", "TRANSID = ?", []any{186}, 0},
		{"SPLITTRANSACTIONS_V1", "TRANSID = ?", []any{186}, 0},
		{"TAGLINK_V1", "REFTYPE = 'Transaction' AND REFID = ?", []any{186}, 0},
		{"TAGLINK_V1", "REFTYPE = 'TransactionSplit' AND REFID = ?", []any{splitID}, 0},
		{"CUSTOMFIELDDATA_V1", "REFID = ?", []any{186}, 0},
		{"CHECKINGACCOUNT_V1", "TRANSID = ?", []any{100}, 1},
		{"TAGLINK_V1", "REFTYPE = 'Transaction' AND REFID = ?", []any{100}, 1},
		{"CUSTOMFIELDDATA_V1", "REFID = ?", []any{100}, 1},
	} {
		if n := a.countWhere(check.table, check.where, check.args...); n != check.want {
			t.Errorf("%s where %s %v: %d rows, want %d", check.table, check.where, check.args, n, check.want)
		}
	}

	backups, err := os.ReadDir(a.config.BackupDir)
	if err != nil || len(backups) == 0 {
		t.Fatalf("delete did not back up the database first (entries=%d, err=%v)", len(backups), err)
	}
}

// Attachments and asset/share links belong to files and holdings MMEX
// manages, so MMXC refuses to delete such a transaction instead of orphaning
// them.
func TestDeleteTransactionRefusesAttachmentsAndLinks(t *testing.T) {
	a := newDeleteTestApp(t, true)
	mustExec(t, a, "INSERT INTO ATTACHMENT_V1 (ATTACHMENTID, REFTYPE, REFID, DESCRIPTION, FILENAME) VALUES (1, 'Transaction', 100, '', 'receipt.pdf')")
	mustExec(t, a, "INSERT INTO TRANSLINK_V1 (TRANSLINKID, CHECKINGACCOUNTID, LINKTYPE, LINKRECORDID) VALUES (1, 186, 'Asset', 1)")

	for _, id := range []int64{100, 186} {
		if err := a.deleteTransactionRecord(id); err == nil {
			t.Errorf("transaction %d was deleted despite its MMEX-managed links", id)
		}
		if n := a.countWhere("CHECKINGACCOUNT_V1", "TRANSID = ?", id); n != 1 {
			t.Errorf("transaction %d rows = %d, want 1", id, n)
		}
	}
}

// The row menu only offers "Delete transaction" with the setting on, and only
// then does a voided row become actionable (for deleting it; app.js hides
// Edit/Void on it).
func TestTransactionRowMenuOffersDeleteOnlyWhenEnabled(t *testing.T) {
	render := func(a *app, query string) string {
		t.Helper()
		rec := httptest.NewRecorder()
		a.accounts(rec, httptest.NewRequest(http.MethodGet, query, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("accounts %s status = %d", query, rec.Code)
		}
		return rec.Body.String()
	}
	const query = "/accounts?account_id=12&from=2025-10-01&to=2025-10-31"

	off := newDeleteTestApp(t, false)
	if err := off.markTransactionVoid(100); err != nil {
		t.Fatal(err)
	}
	html := render(off, query)
	if !strings.Contains(html, `data-action="void"`) {
		t.Fatal("ledger row menu is missing")
	}
	if strings.Contains(html, `data-action="delete"`) {
		t.Fatal("row menu offers Delete with the setting off")
	}
	if strings.Contains(html, `data-trans-id="100"`) {
		t.Fatal("voided row is actionable with the setting off")
	}

	on := newDeleteTestApp(t, true)
	if err := on.markTransactionVoid(100); err != nil {
		t.Fatal(err)
	}
	html = render(on, query)
	for _, want := range []string{
		`data-action="delete"`,
		`class="actionable" data-voided="true" data-trans-id="100"`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("ledger with deletion enabled is missing %q", want)
		}
	}

	for _, want := range []string{
		`postRowAction("/transactions/delete"`,
		`appConfirm(i18n("Permanently delete this transaction? This cannot be undone."), { ok: i18n("Delete transaction") })`,
		`row.dataset.voided === "true"`,
	} {
		if !strings.Contains(js, want) {
			t.Fatalf("row menu script missing %q", want)
		}
	}
	if !strings.Contains(templates, `"Permanently delete this transaction? This cannot be undone.": `) {
		t.Fatal("delete confirmation is missing from the i18n table")
	}
}
