package mmxc

import (
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestListBudgetEntriesIncludesCurrentAndPreviousTotals(t *testing.T) {
	a, err := newApp(config{DBPath: fixtureDB(t), WriteEnabled: true, BackupDir: "backups"})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	if _, err := a.db.Exec(`
INSERT INTO CHECKINGACCOUNT_V1
  (TRANSID, ACCOUNTID, TOACCOUNTID, PAYEEID, TRANSCODE, TRANSAMOUNT, STATUS,
   TRANSACTIONNUMBER, NOTES, CATEGID, TRANSDATE, FOLLOWUPID, TOTRANSAMOUNT,
   LASTUPDATEDTIME, DELETEDTIME, COLOR)
VALUES
  (300, 12, NULL, 1, 'Withdrawal', 20.0, '', '', 'previous hobbies', 66, '2024-04-10T00:00:00', NULL, NULL, '2024-04-10T00:00:00', NULL, -1),
  (301, 12, NULL, 2, 'Withdrawal', 99.0, '', '', 'split groceries', -1, '2025-03-01T00:00:00', NULL, NULL, '2025-03-01T00:00:00', NULL, -1),
  (302, 12, NULL, 3, 'Deposit',    12.0, '', '', 'dining refund',    1, '2025-02-01T00:00:00', NULL, NULL, '2025-02-01T00:00:00', NULL, -1),
  (303, 12, NULL, 3, 'Withdrawal',  4.0, 'Void', '', 'void dining',   1, '2025-02-02T00:00:00', NULL, NULL, '2025-02-02T00:00:00', NULL, -1);

INSERT INTO SPLITTRANSACTIONS_V1 (SPLITTRANSID, TRANSID, CATEGID, SPLITTRANSAMOUNT, NOTES) VALUES
  (10, 301, 30, 25.0, 'more groceries'),
  (11, 301, 86, 74.0, 'care');

INSERT INTO BUDGETYEAR_V1 (BUDGETYEARID, BUDGETYEARNAME) VALUES (1, '2025');
INSERT INTO BUDGETTABLE_V1 (BUDGETENTRYID, BUDGETYEARID, CATEGID, PERIOD, AMOUNT, NOTES, ACTIVE) VALUES
  (1, 1, 66, 'Monthly', -100.0, '', 1),
  (2, 1, 86, 'Monthly', -200.0, '', 1),
  (3, 1, 1,  'Monthly', -50.0,  '', 1),
  (4, 1, 102, 'Monthly', -5.0,  'inactive', 0),
  (5, 1, 30, 'None', 0, 'not budgeted', 1);`); err != nil {
		t.Fatal(err)
	}

	entries, err := a.listBudgetEntries(1)
	if err != nil {
		t.Fatal(err)
	}

	byCategory := make(map[string]budgetEntry)
	for _, entry := range entries {
		byCategory[entry.Category] = entry
	}

	hobbies := byCategory["Hobbies"]
	if hobbies.Current != -50 || hobbies.Previous != -20 {
		t.Fatalf("hobbies totals = current %.2f previous %.2f, want -50.00/-20.00", hobbies.Current, hobbies.Previous)
	}
	if hobbies.Annual != -1200 {
		t.Fatalf("hobbies annual = %.2f, want -1200.00", hobbies.Annual)
	}

	toiletries := byCategory["Toiletries"]
	if toiletries.Current != -74 || toiletries.Previous != 0 {
		t.Fatalf("toiletries totals = current %.2f previous %.2f, want -74.00/0.00", toiletries.Current, toiletries.Previous)
	}

	dining := byCategory["Dining"]
	if dining.Current != 12 || dining.Previous != 0 {
		t.Fatalf("dining totals = current %.2f previous %.2f, want 12.00/0.00", dining.Current, dining.Previous)
	}

	if _, ok := byCategory["Subscriptions"]; ok {
		t.Fatal("inactive budget entry was displayed")
	}
	if _, ok := byCategory["Groceries"]; ok {
		t.Fatal("None-period budget entry was displayed")
	}
}

func TestListBudgetYearsSplitsIncomeExpensesAndNet(t *testing.T) {
	a, err := newApp(config{DBPath: fixtureDB(t), WriteEnabled: true, BackupDir: "backups"})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	if _, err := a.db.Exec(`
INSERT INTO BUDGETYEAR_V1 (BUDGETYEARID, BUDGETYEARNAME) VALUES (1, '2025');
INSERT INTO BUDGETTABLE_V1 (BUDGETENTRYID, BUDGETYEARID, CATEGID, PERIOD, AMOUNT, NOTES, ACTIVE) VALUES
  (1, 1, 66, 'Monthly', 100.0, '', 1),
  (2, 1, 86, 'Monthly', -40.0, '', 1),
  (3, 1, 1,  'Monthly', -20.0, '', 0);`); err != nil {
		t.Fatal(err)
	}

	years, err := a.listBudgetYears()
	if err != nil {
		t.Fatal(err)
	}
	if len(years) == 0 {
		t.Fatal("expected budget years")
	}
	got := years[0]
	if got.Income != 100 || got.Expenses != -40 || got.Total != 60 || got.Count != 2 {
		t.Fatalf("budget totals = income %.2f expenses %.2f net %.2f count %d, want 100/-40/60/2", got.Income, got.Expenses, got.Total, got.Count)
	}
}

func TestBudgetEntryTotalsUseAnnualAmounts(t *testing.T) {
	income, expenses, total := budgetEntryTotals([]budgetEntry{
		{Annual: 1200},
		{Annual: -480},
		{Annual: -52},
	})
	if income != 1200 || expenses != -532 || total != 668 {
		t.Fatalf("annual totals = income %.2f expenses %.2f net %.2f, want 1200/-532/668", income, expenses, total)
	}
}

func TestBudgetEntryWriteHelpers(t *testing.T) {
	dir := t.TempDir()
	a, err := newApp(config{
		DBPath:       fixtureDB(t),
		WriteEnabled: true,
		BackupDir:    filepath.Join(dir, "backups"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	if _, err := a.db.Exec("INSERT INTO BUDGETYEAR_V1 (BUDGETYEARID, BUDGETYEARNAME) VALUES (1, '2025')"); err != nil {
		t.Fatal(err)
	}

	id, err := a.insertBudgetEntry(1, 66, "Monthly", -25.50, "original notes")
	if err != nil {
		t.Fatalf("insert budget entry: %v", err)
	}
	if _, err := a.insertBudgetEntry(1, 86, "None", -10, "none period"); err == nil {
		t.Fatal("expected None budget period to fail")
	}
	if _, err := a.insertBudgetEntry(1, 66, "Monthly", -25.50, "duplicate"); err == nil {
		t.Fatal("expected duplicate active budget category to fail")
	}
	if _, err := a.db.Exec("INSERT INTO BUDGETTABLE_V1 (BUDGETENTRYID, BUDGETYEARID, CATEGID, PERIOD, AMOUNT, NOTES, ACTIVE) VALUES (2, 1, 86, 'None', 0, 'placeholder', 1)"); err != nil {
		t.Fatal(err)
	}
	reusedID, err := a.insertBudgetEntry(1, 86, "Monthly", -15, "reused")
	if err != nil {
		t.Fatalf("insert budget entry over None placeholder: %v", err)
	}
	if reusedID != 2 {
		t.Fatalf("reused budget entry id = %d, want 2", reusedID)
	}

	if err := a.updateBudgetEntryRecord(id, "Yearly", -300, "updated notes"); err != nil {
		t.Fatalf("update budget entry: %v", err)
	}
	var period, notes string
	var amount float64
	if err := a.db.QueryRow("SELECT PERIOD, AMOUNT, COALESCE(NOTES, '') FROM BUDGETTABLE_V1 WHERE BUDGETENTRYID = ?", id).Scan(&period, &amount, &notes); err != nil {
		t.Fatal(err)
	}
	if period != "Yearly" || amount != -300 || notes != "updated notes" {
		t.Fatalf("unexpected budget entry after update: period=%q amount=%.2f notes=%q", period, amount, notes)
	}

	if err := a.deleteBudgetEntryRecord(id); err != nil {
		t.Fatalf("delete budget entry: %v", err)
	}
	if got := a.countWhere("BUDGETTABLE_V1", "BUDGETENTRYID = ?", id); got != 0 {
		t.Fatalf("budget entry count after delete = %d, want 0", got)
	}
}

func TestInsertNextBudgetYearCopiesPreviousBudget(t *testing.T) {
	dir := t.TempDir()
	a, err := newApp(config{
		DBPath:       fixtureDB(t),
		WriteEnabled: true,
		BackupDir:    filepath.Join(dir, "backups"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	if _, err := a.db.Exec(`
INSERT INTO BUDGETYEAR_V1 (BUDGETYEARID, BUDGETYEARNAME) VALUES (10, '2024'), (11, '2025');
INSERT INTO BUDGETTABLE_V1 (BUDGETENTRYID, BUDGETYEARID, CATEGID, PERIOD, AMOUNT, NOTES, ACTIVE) VALUES
  (20, 10, 66, 'Monthly', -1.0, 'old', 1),
  (21, 11, 66, 'Monthly', -25.0, 'active copy', 1),
  (22, 11, 86, 'None', 0, 'placeholder copy', 1),
  (23, 11, 1, 'Weekly', -5.0, 'inactive copy', 0);`); err != nil {
		t.Fatal(err)
	}

	id, err := a.insertNextBudgetYear()
	if err != nil {
		t.Fatalf("insert next budget year: %v", err)
	}

	var name string
	if err := a.db.QueryRow("SELECT BUDGETYEARNAME FROM BUDGETYEAR_V1 WHERE BUDGETYEARID = ?", id).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name != "2026" {
		t.Fatalf("new budget year name = %q, want 2026", name)
	}

	var count int
	if err := a.db.QueryRow("SELECT COUNT(*) FROM BUDGETTABLE_V1 WHERE BUDGETYEARID = ?", id).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Fatalf("copied budget rows = %d, want 3", count)
	}

	var amount float64
	var notes string
	var active int
	if err := a.db.QueryRow("SELECT AMOUNT, COALESCE(NOTES, ''), COALESCE(ACTIVE, 1) FROM BUDGETTABLE_V1 WHERE BUDGETYEARID = ? AND CATEGID = 1", id).Scan(&amount, &notes, &active); err != nil {
		t.Fatal(err)
	}
	if amount != -5 || notes != "inactive copy" || active != 0 {
		t.Fatalf("copied inactive row = amount %.2f notes %q active %d, want -5/inactive copy/0", amount, notes, active)
	}

	if _, err := a.insertNextBudgetYear(); err != nil {
		t.Fatalf("insert second next budget year: %v", err)
	}
	var exists int
	if err := a.db.QueryRow("SELECT COUNT(*) FROM BUDGETYEAR_V1 WHERE BUDGETYEARNAME = '2027'").Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists != 1 {
		t.Fatalf("2027 budget count = %d, want 1", exists)
	}
}

func TestBudgetsPageRendersEditableRows(t *testing.T) {
	a, err := newApp(config{DBPath: fixtureDB(t), WriteEnabled: true, BackupDir: filepath.Join(t.TempDir(), "backups")})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	if _, err := a.db.Exec(`
INSERT INTO BUDGETYEAR_V1 (BUDGETYEARID, BUDGETYEARNAME) VALUES (1, '2025');
INSERT INTO BUDGETTABLE_V1 (BUDGETENTRYID, BUDGETYEARID, CATEGID, PERIOD, AMOUNT, NOTES, ACTIVE)
VALUES
  (1, 1, 66, 'Monthly', -25.0, 'render notes', 1),
  (2, 1, 86, 'None', 0, 'not budgeted', 1);`); err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	a.budgets(rr, httptest.NewRequest("GET", "/budgets?budget_id=1", nil))
	body := rr.Body.String()
	for _, want := range []string{`action="/budgets/create-year"`, `action="/budgets/create"`, `form="budget-update-1"`, `name="notes"`, `data-budget-autosave`, `Income`, `Expenses`, `Net`, `Annual`, `Current`, `Previous`} {
		if !strings.Contains(body, want) {
			t.Fatalf("budgets page body missing %q", want)
		}
	}
	if strings.Contains(body, `<option value="Leisure: Hobbies">`) {
		t.Fatal("new budget category list includes a category already in the budget")
	}
	if !strings.Contains(body, `<option value="Food: Toiletries">`) {
		t.Fatal("new budget category list excludes a None-period category")
	}
	if strings.Contains(body, `<option value="None"`) {
		t.Fatal("budgets page still offers None as a period")
	}
	if strings.Contains(body, ">Status</th>") {
		t.Fatal("budgets page still renders the active/status column")
	}
	if strings.Contains(body, ">Save</button>") {
		t.Fatal("budgets page still renders row save buttons")
	}
	headerIndex := strings.Index(body, `<div class="detail-head">`)
	createIndex := strings.Index(body, `action="/budgets/create"`)
	tableIndex := strings.Index(body, `<table class="budget-table">`)
	if headerIndex < 0 || createIndex < 0 || tableIndex < 0 || createIndex < headerIndex || createIndex > tableIndex {
		t.Fatal("new budget category form is not below the budget header before the table")
	}
}

// A database with no budget year yet (every new one) must still be able to
// start budgeting: the first year is the current calendar year, empty, and the
// next press continues from it as usual.
func TestInsertNextBudgetYearStartsAtCurrentYearWhenNoneExists(t *testing.T) {
	a, err := newApp(config{DBPath: fixtureDB(t), WriteEnabled: true, BackupDir: filepath.Join(t.TempDir(), "backups")})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	if _, err := a.db.Exec("DELETE FROM BUDGETTABLE_V1; DELETE FROM BUDGETYEAR_V1;"); err != nil {
		t.Fatal(err)
	}

	first, err := a.insertNextBudgetYear()
	if err != nil {
		t.Fatalf("first budget year on an empty database: %v", err)
	}
	thisYear := strconv.Itoa(time.Now().Year())
	var name string
	var entries int
	if err := a.db.QueryRow("SELECT BUDGETYEARNAME FROM BUDGETYEAR_V1 WHERE BUDGETYEARID = ?", first).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if err := a.db.QueryRow("SELECT COUNT(*) FROM BUDGETTABLE_V1 WHERE BUDGETYEARID = ?", first).Scan(&entries); err != nil {
		t.Fatal(err)
	}
	if name != thisYear || entries != 0 {
		t.Fatalf("first budget year = %q with %d entries, want %q with none", name, entries, thisYear)
	}

	second, err := a.insertNextBudgetYear()
	if err != nil {
		t.Fatalf("second budget year: %v", err)
	}
	if err := a.db.QueryRow("SELECT BUDGETYEARNAME FROM BUDGETYEAR_V1 WHERE BUDGETYEARID = ?", second).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if want := strconv.Itoa(time.Now().Year() + 1); name != want {
		t.Fatalf("second budget year = %q, want %q", name, want)
	}
}
