package mmxc

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// budgetStatusApp seeds a 2024 budget on top of the synthetic fixture: two
// expense lines with spending (Hobbies over budget, Groceries under with a
// refund), one untouched expense line (Dining) and one income line (Misc).
func budgetStatusApp(t *testing.T) *app {
	t.Helper()
	a, err := newApp(config{DBPath: fixtureDB(t), WriteEnabled: true, BackupDir: "backups"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.db.Close() })
	if _, err := a.db.Exec(`
INSERT INTO CHECKINGACCOUNT_V1
  (TRANSID, ACCOUNTID, TOACCOUNTID, PAYEEID, TRANSCODE, TRANSAMOUNT, STATUS,
   TRANSACTIONNUMBER, NOTES, CATEGID, TRANSDATE, FOLLOWUPID, TOTRANSAMOUNT,
   LASTUPDATEDTIME, DELETEDTIME, COLOR)
VALUES
  (400, 12, NULL, 14, 'Withdrawal', 1300.0, '', '', 'kit',      66, '2024-03-01T00:00:00', NULL, NULL, '2024-03-01T00:00:00', NULL, -1),
  (401, 12, NULL, 2,  'Withdrawal',  250.0, '', '', 'food',     30, '2024-02-01T00:00:00', NULL, NULL, '2024-02-01T00:00:00', NULL, -1),
  (402, 12, NULL, 2,  'Deposit',      10.0, '', '', 'refund',   30, '2024-02-05T00:00:00', NULL, NULL, '2024-02-05T00:00:00', NULL, -1),
  (403, 12, NULL, 1,  'Deposit',     500.0, '', '', 'side job', 16, '2024-04-01T00:00:00', NULL, NULL, '2024-04-01T00:00:00', NULL, -1);

INSERT INTO BUDGETYEAR_V1 (BUDGETYEARID, BUDGETYEARNAME) VALUES (1, '2024');
INSERT INTO BUDGETTABLE_V1 (BUDGETENTRYID, BUDGETYEARID, CATEGID, PERIOD, AMOUNT, NOTES, ACTIVE) VALUES
  (1, 1, 66, 'Monthly', -100.0, '', 1),
  (2, 1, 30, 'Monthly',  -50.0, '', 1),
  (3, 1, 1,  'Yearly',  -300.0, '', 1),
  (4, 1, 16, 'Monthly',  200.0, '', 1);`); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestAssistantBudgetStatusWholeYear(t *testing.T) {
	a := budgetStatusApp(t)
	today := time.Date(2024, 6, 15, 0, 0, 0, 0, time.UTC)

	status, err := a.assistantToolBudgetStatus(assistantBudgetStatusArgs{Year: "2024"}, today)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Found || status.EntryCount != 4 || status.OverBudgetCount != 1 {
		t.Fatalf("status = found %v entries %d over %d, want true/4/1", status.Found, status.EntryCount, status.OverBudgetCount)
	}
	first := status.Entries[0]
	if first.Category != "Leisure: Hobbies" || first.Budget != 1200 || first.Actual != 1300 || first.Remaining != -100 || first.OverBy != 100 || first.UsedPercent != 108.3 {
		t.Fatalf("first entry = %+v, want the Hobbies overrun on top", first)
	}
	if first.BudgetToDate != 600 {
		t.Fatalf("hobbies budget_to_date = %.2f, want 600 (six months of 100)", first.BudgetToDate)
	}
	if last := status.Entries[len(status.Entries)-1]; last.Kind != "income" {
		t.Fatalf("last entry = %+v, want the income line last", last)
	}
	want := assistantBudgetTotals{Entries: 3, Budget: 2100, BudgetToDate: 1050, Actual: 1540, Remaining: 560, OverBy: 0}
	if status.Expenses == nil || *status.Expenses != want {
		t.Fatalf("expenses = %+v, want %+v", status.Expenses, want)
	}
	if status.Income == nil || status.Income.Budget != 2400 || status.Income.Actual != 500 || status.Income.Remaining != 1900 {
		t.Fatalf("income = %+v, want budget 2400 actual 500 remaining 1900", status.Income)
	}
	if status.Primary() != status.Expenses {
		t.Fatal("primary totals should be the expense totals")
	}
}

func TestAssistantBudgetStatusCategoryIncludesSubcategories(t *testing.T) {
	a := budgetStatusApp(t)
	today := time.Date(2024, 6, 15, 0, 0, 0, 0, time.UTC)

	status, err := a.assistantToolBudgetStatus(assistantBudgetStatusArgs{Year: "2024", Category: "Food"}, today)
	if err != nil {
		t.Fatal(err)
	}
	if status.Category != "Food" || status.EntryCount != 3 {
		t.Fatalf("status = category %q entries %d, want Food/3", status.Category, status.EntryCount)
	}
	for _, entry := range status.Entries {
		if entry.Category == "Leisure: Hobbies" {
			t.Fatal("Food budget status included a Leisure entry")
		}
	}

	status, err = a.assistantToolBudgetStatus(assistantBudgetStatusArgs{Year: "2024", Category: "Food: Groceries"}, today)
	if err != nil {
		t.Fatal(err)
	}
	if status.EntryCount != 1 || status.Income != nil {
		t.Fatalf("groceries status = %+v, want one expense entry", status)
	}
	// The refund reduces net spending: 250 - 10.
	want := assistantBudgetTotals{Entries: 1, Budget: 600, BudgetToDate: 300, Actual: 240, Remaining: 360}
	if *status.Expenses != want {
		t.Fatalf("groceries totals = %+v, want %+v", *status.Expenses, want)
	}
}

func TestAssistantBudgetStatusReportsMissingBudgets(t *testing.T) {
	a := budgetStatusApp(t)
	today := time.Date(2024, 6, 15, 0, 0, 0, 0, time.UTC)

	status, err := a.assistantToolBudgetStatus(assistantBudgetStatusArgs{Year: "2023"}, today)
	if err != nil {
		t.Fatal(err)
	}
	if status.Found || len(status.AvailableYears) != 1 || status.AvailableYears[0] != "2024" || status.Note == "" {
		t.Fatalf("status = %+v, want not found with 2024 available", status)
	}

	status, err = a.assistantToolBudgetStatus(assistantBudgetStatusArgs{Year: "2024", Category: "Food: Toiletries"}, today)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Found || status.EntryCount != 0 || !strings.Contains(status.Note, "Food: Toiletries") {
		t.Fatalf("status = %+v, want found year with no entry and a note", status)
	}

	if _, err := a.assistantToolBudgetStatus(assistantBudgetStatusArgs{Year: "last year"}, today); err == nil {
		t.Fatal("non-numeric year was accepted")
	}
	if _, err := a.assistantToolBudgetStatus(assistantBudgetStatusArgs{Year: "2024", Category: "Groceries"}, today); err == nil {
		t.Fatal("inexact category path was accepted")
	}
}

func TestExecuteAssistantBudgetStatusTool(t *testing.T) {
	a := budgetStatusApp(t)

	result, txs, _, _, err := a.executeAssistantTool("How much over budget was I in 2024 for hobbies?", "budget_status",
		json.RawMessage(`{"year":"2024","category":"Leisure: Hobbies"}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(txs) != 0 {
		t.Fatalf("budget_status returned %d transactions, want none", len(txs))
	}
	status := assistantBudgetResult(result)
	if status == nil {
		t.Fatalf("result type = %T, want *assistantBudgetStatus", result)
	}
	// 2024 is a past year, so the prorated budget is the whole budget.
	if status.Expenses.OverBy != 100 || status.Expenses.BudgetToDate != 1200 {
		t.Fatalf("expenses = %+v, want over_by 100 and budget_to_date 1200", status.Expenses)
	}
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"over_by":100`) || strings.Contains(string(data), "budgetID") {
		t.Fatalf("result JSON = %s", data)
	}

	if _, _, _, _, err := a.executeAssistantTool("", "budget_status", json.RawMessage(`{"year":"2024","account":"Main"}`)); err == nil {
		t.Fatal("budget_status accepted an unknown argument")
	}
}

func TestAssistantCarryForwardBudgetStatusUsesCategoryOnly(t *testing.T) {
	transcript := []map[string]any{
		{"tool": "find_payee", "result": []map[string]any{{"id": 2, "name": "Greenleaf Grocers"}}},
		{"tool": "find_category", "result": []map[string]any{{"id": 30, "name": "Food: Groceries"}}},
	}
	raw, warnings := assistantCarryForwardLookupArguments("budget_status", json.RawMessage(`{"year":"2024","category":""}`), transcript)
	var args assistantBudgetStatusArgs
	if err := decodeAssistantToolArgs(raw, &args); err != nil {
		t.Fatalf("carried arguments do not decode for budget_status: %v (%s)", err, raw)
	}
	if args.Category != "Food: Groceries" || len(warnings) != 1 {
		t.Fatalf("args = %+v warnings = %v, want the category carried forward only", args, warnings)
	}
}

func TestAssistantPageRendersBudgetStatus(t *testing.T) {
	a := budgetStatusApp(t)
	status, err := a.assistantToolBudgetStatus(assistantBudgetStatusArgs{Year: "2024"}, time.Date(2024, 6, 15, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	data := pageData{Title: "Ask", Active: "assistant", Info: a.info()}
	applyAssistantResult(&data, assistantToolRunResult{Answer: "You are 100.00 over budget on Hobbies.", Tool: "budget_status", Budget: status})
	rec := httptest.NewRecorder()
	a.render(rec, "assistant", data)
	body := rec.Body.String()
	for _, want := range []string{"Leisure: Hobbies", "Remaining", "/budgets?budget_id=1", "108%"} {
		if !strings.Contains(body, want) {
			t.Fatalf("Ask page is missing %q", want)
		}
	}
	if rec.Code != 200 {
		t.Fatalf("render status = %d, want 200", rec.Code)
	}
}
