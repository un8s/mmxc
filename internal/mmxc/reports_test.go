package mmxc

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAdvancedReportData(t *testing.T) {
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
  (300, 12, NULL, 1, 'Withdrawal', 350.0, '', '', 'large utilities', 66, '2025-10-20T00:00:00', NULL, NULL, '2025-10-20T00:00:00', NULL, -1),
  (303, 12, NULL, 3, 'Withdrawal', 11.0,  '', '', 'coffee aug',       1,  '2025-08-19T00:00:00', NULL, NULL, '2025-08-19T00:00:00', NULL, -1),
  (301, 12, NULL, 3, 'Withdrawal', 10.0,  '', '', 'coffee sep',       1,  '2025-09-20T00:00:00', NULL, NULL, '2025-09-20T00:00:00', NULL, -1),
  (302, 12, NULL, 3, 'Withdrawal', 75.0,  '', '', 'coffee oct',       1,  '2025-10-21T00:00:00', NULL, NULL, '2025-10-21T00:00:00', NULL, -1),
  (304, 12, NULL, 2, 'Withdrawal', 20.0,  '', '', 'grocery sep',      30, '2025-09-25T00:00:00', NULL, NULL, '2025-09-25T00:00:00', NULL, -1),
  (305, 12, NULL, 2, 'Withdrawal', 21.0,  '', '', 'grocery oct',      30, '2025-10-25T00:00:00', NULL, NULL, '2025-10-25T00:00:00', NULL, -1),
  (306, 12, NULL, 2, 'Withdrawal', 22.0,  '', '', 'grocery nov',      30, '2025-11-25T00:00:00', NULL, NULL, '2025-11-25T00:00:00', NULL, -1);

INSERT INTO BUDGETYEAR_V1 (BUDGETYEARID, BUDGETYEARNAME) VALUES (1, '2025');
INSERT INTO BUDGETTABLE_V1 (BUDGETENTRYID, BUDGETYEARID, CATEGID, PERIOD, AMOUNT, NOTES, ACTIVE)
VALUES (1, 1, 66, 'Monthly', -25.0, '', 1);`); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", "/reports?from=2025-08-01&to=2025-11-30&report=anomalies", nil)
	report, err := a.reportData(req)
	if err != nil {
		t.Fatal(err)
	}

	if report.SelectedID != "anomalies" {
		t.Fatalf("selected report = %q, want anomalies", report.SelectedID)
	}
	if len(report.Anomalies) == 0 {
		t.Fatal("expected anomaly rows")
	}
	if report.Anomalies[0].Amount != 350 {
		t.Fatalf("top anomaly amount = %.2f, want 350.00", report.Anomalies[0].Amount)
	}
	if report.Anomalies[0].Notes != "large utilities" {
		t.Fatalf("top anomaly notes = %q, want large utilities", report.Anomalies[0].Notes)
	}
	if report.Anomalies[0].TransID != 300 {
		t.Fatalf("top anomaly trans id = %d, want 300", report.Anomalies[0].TransID)
	}
	if report.Anomalies[0].AccountID != 12 {
		t.Fatalf("top anomaly account id = %d, want 12", report.Anomalies[0].AccountID)
	}
	if len(report.Anomalies[0].Rows) != 2 {
		t.Fatalf("anomaly contributor count = %d, want 2", len(report.Anomalies[0].Rows))
	}
	if report.Anomalies[0].Rows[0].Date != "2025-10-20" {
		t.Fatalf("first anomaly contributor date = %q, want 2025-10-20", report.Anomalies[0].Rows[0].Date)
	}
	if report.Anomalies[0].Rows[0].Notes != "large utilities" {
		t.Fatalf("first anomaly contributor notes = %q, want large utilities", report.Anomalies[0].Rows[0].Notes)
	}
	if len(report.Trends) == 0 {
		t.Fatal("expected spending trend rows")
	}
	// Every non-void transaction in the range: seed 100 and 186 (187 is July) plus
	// the seven inserted above. The DB total is larger, so a wrong query shows.
	if report.Transactions != 9 {
		t.Fatalf("period transaction count = %d, want 9", report.Transactions)
	}
	if len(report.TopCategories) == 0 {
		t.Fatal("expected top category rows")
	}
	if len(report.TopCategories[0].Rows) != report.TopCategories[0].Count {
		t.Fatalf("top category contributor count = %d, want %d", len(report.TopCategories[0].Rows), report.TopCategories[0].Count)
	}
	if report.TopCategories[0].Rows[0].Date != "2025-10-20" {
		t.Fatalf("top category first contributor date = %q, want 2025-10-20", report.TopCategories[0].Rows[0].Date)
	}
	if len(report.TopPayees) == 0 {
		t.Fatal("expected top payee rows")
	}
	if len(report.TopPayees[0].Rows) != report.TopPayees[0].Count {
		t.Fatalf("top payee contributor count = %d, want %d", len(report.TopPayees[0].Rows), report.TopPayees[0].Count)
	}
	if report.TopPayees[0].Rows[0].Date != "2025-10-20" {
		t.Fatalf("top payee first contributor date = %q, want 2025-10-20", report.TopPayees[0].Rows[0].Date)
	}
	if len(report.BudgetOverruns) == 0 {
		t.Fatal("expected budget overrun rows")
	}
	if report.BudgetOverruns[0].Category != "Hobbies" {
		t.Fatalf("budget overrun category = %q, want Hobbies", report.BudgetOverruns[0].Category)
	}
	if len(report.Recurring) != 2 {
		t.Fatalf("recurring row count = %d, want 2", len(report.Recurring))
	}
	if report.Recurring[0].Repeat != "Monthly" {
		t.Fatalf("recurring repeat = %q, want Monthly", report.Recurring[0].Repeat)
	}
	if report.Recurring[0].Payee != "Grocery Mart" {
		t.Fatalf("first recurring payee = %q, want Grocery Mart", report.Recurring[0].Payee)
	}
	if report.Recurring[0].NextDate != "2025-12-25" {
		t.Fatalf("first recurring expected date = %q, want 2025-12-25", report.Recurring[0].NextDate)
	}
	if report.Recurring[0].Count != 3 {
		t.Fatalf("recurring count = %d, want 3", report.Recurring[0].Count)
	}
	if report.Recurring[0].Transactions[0].Date != "2025-11-25" {
		t.Fatalf("first transaction date = %q, want 2025-11-25", report.Recurring[0].Transactions[0].Date)
	}

	shortRange, err := a.recurringReview("2025-09-01", "2025-10-31")
	if err != nil {
		t.Fatal(err)
	}
	if len(shortRange) != 0 {
		t.Fatalf("short range recurring rows = %d, want 0", len(shortRange))
	}
}

func TestBudgetAmountForRange(t *testing.T) {
	got, err := budgetAmountForRange("Monthly", 25, "2025-09-01", "2025-10-31")
	if err != nil {
		t.Fatal(err)
	}
	if got != 50 {
		t.Fatalf("monthly budget = %.2f, want 50.00", got)
	}

	got, err = budgetAmountForRange("Weekly", 7, "2025-01-01", "2025-01-07")
	if err != nil {
		t.Fatal(err)
	}
	if got != 7 {
		t.Fatalf("weekly budget = %.2f, want 7.00", got)
	}

	got, err = budgetAmountForRange("Fortnightly", 14, "2025-01-01", "2025-01-14")
	if err != nil {
		t.Fatal(err)
	}
	if got != 14 {
		t.Fatalf("fortnightly budget = %.2f, want 14.00", got)
	}
}

func TestSnapRangeBound(t *testing.T) {
	cases := []struct {
		value string
		end   bool
		want  string
	}{
		{"2025", false, "2025-01-01"},
		{"2025", true, "2025-12-31"},
		{"2025-02", false, "2025-02-01"},
		{"2025-02", true, "2025-02-28"},
		{"2024-02", true, "2024-02-29"}, // leap year
		{"2025-06-15", false, "2025-06-15"},
		{"2025-06-15", true, "2025-06-15"},
	}
	for _, tc := range cases {
		got, err := snapRangeBound(tc.value, tc.end)
		if err != nil {
			t.Fatalf("snapRangeBound(%q, %v): %v", tc.value, tc.end, err)
		}
		if got != tc.want {
			t.Fatalf("snapRangeBound(%q, %v) = %q, want %q", tc.value, tc.end, got, tc.want)
		}
	}
}

func TestSpendingTrends(t *testing.T) {
	a, err := newApp(config{DBPath: fixtureDB(t), WriteEnabled: true, BackupDir: "backups"})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	// Category 66 ("Hobbies" under "Leisure"): a big month-over-month jump.
	// Category 30: a tiny change that stays below the noteworthy threshold.
	if _, err := a.db.Exec(`
INSERT INTO CHECKINGACCOUNT_V1
  (TRANSID, ACCOUNTID, TOACCOUNTID, PAYEEID, TRANSCODE, TRANSAMOUNT, STATUS,
   TRANSACTIONNUMBER, NOTES, CATEGID, TRANSDATE, FOLLOWUPID, TOTRANSAMOUNT,
   LASTUPDATEDTIME, DELETEDTIME, COLOR)
VALUES
  (500, 12, NULL, 1, 'Withdrawal', 20.0,  '', '', 'hobby sep', 66, '2025-09-10T00:00:00', NULL, NULL, '2025-09-10T00:00:00', NULL, -1),
  (501, 12, NULL, 1, 'Withdrawal', 200.0, '', '', 'hobby oct', 66, '2025-10-10T00:00:00', NULL, NULL, '2025-10-10T00:00:00', NULL, -1),
  (502, 12, NULL, 2, 'Withdrawal', 30.0,  '', '', 'grok sep',  30, '2025-09-12T00:00:00', NULL, NULL, '2025-09-12T00:00:00', NULL, -1),
  (503, 12, NULL, 2, 'Withdrawal', 31.0,  '', '', 'grok oct',  30, '2025-10-12T00:00:00', NULL, NULL, '2025-10-12T00:00:00', NULL, -1);`); err != nil {
		t.Fatal(err)
	}

	trends, prevMonths, current, err := a.spendingTrends("2025-08-01", "2025-10-31")
	if err != nil {
		t.Fatal(err)
	}
	if current != "2025-10" {
		t.Fatalf("current month = %q, want 2025-10", current)
	}
	if len(prevMonths) != 2 || prevMonths[0] != "2025-08" || prevMonths[1] != "2025-09" {
		t.Fatalf("prev months = %v, want [2025-08 2025-09]", prevMonths)
	}
	if len(trends) != 1 {
		t.Fatalf("trend rows = %d, want 1 (only the noteworthy change)", len(trends))
	}
	top := trends[0]
	if top.Category != "Leisure: Hobbies" {
		t.Fatalf("trend category = %q, want Leisure: Hobbies", top.Category)
	}
	// October cat 66 = our 200 + the seed's 50 (TRANSID 100) = 250; September = 20.
	if top.Amount != 250 {
		t.Fatalf("trend amount = %.2f, want 250.00", top.Amount)
	}
	if top.Change != 230 {
		t.Fatalf("trend change = %.2f, want 230.00 (250 - 20)", top.Change)
	}
	if len(top.Previous) != 2 || top.Previous[0] != 0 || top.Previous[1] != 20 {
		t.Fatalf("trend previous = %v, want [0 20]", top.Previous)
	}
	if len(top.Rows) != 2 {
		t.Fatalf("trend contributors = %d, want 2 (our hobby + seed utilities in October)", len(top.Rows))
	}
}

func TestSpendingTrendsAttributeSplits(t *testing.T) {
	a, err := newApp(config{DBPath: fixtureDB(t), WriteEnabled: true, BackupDir: "backups"})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	// An October split (CATEGID -1) of 250.00 Subscriptions + 50.00 Groceries.
	// The jump belongs to those categories, not to an "Uncategorized" row.
	if _, err := a.db.Exec(`
INSERT INTO CHECKINGACCOUNT_V1
  (TRANSID, ACCOUNTID, TOACCOUNTID, PAYEEID, TRANSCODE, TRANSAMOUNT, STATUS,
   TRANSACTIONNUMBER, NOTES, CATEGID, TRANSDATE, FOLLOWUPID, TOTRANSAMOUNT,
   LASTUPDATEDTIME, DELETEDTIME, COLOR)
VALUES
  (600, 12, NULL, 14, 'Withdrawal', 300.0, '', '', 'october split', -1, '2025-10-08T00:00:00', NULL, NULL, '2025-10-08T00:00:00', NULL, -1);

INSERT INTO SPLITTRANSACTIONS_V1 (SPLITTRANSID, TRANSID, CATEGID, SPLITTRANSAMOUNT, NOTES) VALUES
  (600, 600, 102, 250.0, 'annual plan'),
  (601, 600, 30,  50.0,  '');`); err != nil {
		t.Fatal(err)
	}

	trends, _, current, err := a.spendingTrends("2025-09-01", "2025-10-31")
	if err != nil {
		t.Fatal(err)
	}
	if current != "2025-10" {
		t.Fatalf("current month = %q, want 2025-10", current)
	}
	byName := make(map[string]trendReportRow, len(trends))
	for _, row := range trends {
		byName[row.Category] = row
	}
	if _, ok := byName["Uncategorized"]; ok {
		t.Fatalf("split spending still reported as Uncategorized: %+v", trends)
	}
	subs, ok := byName["Food: Subscriptions"]
	if !ok {
		t.Fatalf("missing Food: Subscriptions trend, got %+v", trends)
	}
	if subs.Amount != 250 || subs.Change != 250 {
		t.Fatalf("subscriptions amount/change = %.2f/%.2f, want 250.00/250.00", subs.Amount, subs.Change)
	}
	// The contributor shows the split line, not the 300.00 parent.
	if len(subs.Rows) != 1 {
		t.Fatalf("subscriptions contributors = %d, want 1", len(subs.Rows))
	}
	if subs.Rows[0].Amount != 250 {
		t.Fatalf("contributor amount = %.2f, want 250.00", subs.Rows[0].Amount)
	}
	if subs.Rows[0].Notes != "annual plan" {
		t.Fatalf("contributor notes = %q, want annual plan", subs.Rows[0].Notes)
	}
	if subs.Rows[0].Category != "Food: Subscriptions" {
		t.Fatalf("contributor category = %q, want Food: Subscriptions", subs.Rows[0].Category)
	}
	// A split line with no note of its own falls back to the parent's.
	groceries, ok := byName["Food: Groceries"]
	if !ok {
		t.Fatalf("missing Food: Groceries trend, got %+v", trends)
	}
	if len(groceries.Rows) != 1 || groceries.Rows[0].Notes != "october split" {
		t.Fatalf("groceries contributors = %+v, want one row noted october split", groceries.Rows)
	}
}

func TestTopExpenseCategoriesAttributeSplits(t *testing.T) {
	a, err := newApp(config{DBPath: fixtureDB(t), WriteEnabled: true, BackupDir: "backups"})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	// The seed's only August withdrawal is split parent 186 (100.00, CATEGID -1)
	// splitting into 40.00 Groceries (30) and 60.00 Subscriptions (102). Its
	// spending must land on those categories, not in an "Uncategorized" bucket.
	ranked, err := a.topExpenseCategories("2025-08-01", "2025-08-31", 10)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.addRankedCategoryContributors("2025-08-01", "2025-08-31", ranked); err != nil {
		t.Fatal(err)
	}
	byName := make(map[string]rankedReportRow, len(ranked))
	for _, row := range ranked {
		byName[row.Name] = row
	}
	if _, ok := byName["Uncategorized"]; ok {
		t.Fatalf("split spending still reported as Uncategorized: %+v", ranked)
	}
	subs, ok := byName["Food: Subscriptions"]
	if !ok {
		t.Fatalf("missing Food: Subscriptions row, got %+v", ranked)
	}
	if subs.Amount != 60 || subs.Count != 1 {
		t.Fatalf("subscriptions = %.2f across %d, want 60.00 across 1", subs.Amount, subs.Count)
	}
	if len(subs.Rows) != 1 {
		t.Fatalf("subscriptions contributors = %d, want 1", len(subs.Rows))
	}
	// The contributor shows the split line's amount and note, not the parent's.
	if subs.Rows[0].Amount != 60 {
		t.Fatalf("contributor amount = %.2f, want 60.00 (the split line)", subs.Rows[0].Amount)
	}
	if subs.Rows[0].Notes != "subs" {
		t.Fatalf("contributor notes = %q, want subs", subs.Rows[0].Notes)
	}
	if subs.Rows[0].Payee != "Amazon" {
		t.Fatalf("contributor payee = %q, want Amazon", subs.Rows[0].Payee)
	}
	groceries, ok := byName["Food: Groceries"]
	if !ok {
		t.Fatalf("missing Food: Groceries row, got %+v", ranked)
	}
	if groceries.Amount != 40 {
		t.Fatalf("groceries = %.2f, want 40.00", groceries.Amount)
	}
	// Ordinary transactions keep contributing their own amount once.
	ranked, err = a.topExpenseCategories("2025-10-01", "2025-10-31", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ranked) != 1 || ranked[0].Name != "Leisure: Hobbies" || ranked[0].Amount != 50 || ranked[0].Count != 1 {
		t.Fatalf("october categories = %+v, want one Leisure: Hobbies row of 50.00", ranked)
	}
}

func TestReportPagesRenderActionableContributors(t *testing.T) {
	a, err := newApp(config{DBPath: fixtureDB(t), WriteEnabled: true, BackupDir: "backups"})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	// Enough history for every report to produce rows: a recurring monthly
	// grocery run, an outlier against it, and a budget to overrun.
	if _, err := a.db.Exec(`
INSERT INTO CHECKINGACCOUNT_V1
  (TRANSID, ACCOUNTID, TOACCOUNTID, PAYEEID, TRANSCODE, TRANSAMOUNT, STATUS,
   TRANSACTIONNUMBER, NOTES, CATEGID, TRANSDATE, FOLLOWUPID, TOTRANSAMOUNT,
   LASTUPDATEDTIME, DELETEDTIME, COLOR)
VALUES
  (700, 12, NULL, 2, 'Withdrawal', 40.0,  '', '', 'grocery aug', 30, '2025-08-05T00:00:00', NULL, NULL, '2025-08-05T00:00:00', NULL, -1),
  (701, 12, NULL, 2, 'Withdrawal', 42.0,  '', '', 'grocery sep', 30, '2025-09-05T00:00:00', NULL, NULL, '2025-09-05T00:00:00', NULL, -1),
  (702, 12, NULL, 2, 'Withdrawal', 41.0,  '', '', 'grocery oct', 30, '2025-10-05T00:00:00', NULL, NULL, '2025-10-05T00:00:00', NULL, -1),
  (703, 12, NULL, 2, 'Withdrawal', 400.0, '', '', 'grocery nov', 30, '2025-11-05T00:00:00', NULL, NULL, '2025-11-05T00:00:00', NULL, -1);

INSERT INTO BUDGETYEAR_V1 (BUDGETYEARID, BUDGETYEARNAME) VALUES (1, '2025');
INSERT INTO BUDGETTABLE_V1 (BUDGETENTRYID, BUDGETYEARID, CATEGID, PERIOD, AMOUNT, NOTES, ACTIVE)
VALUES (1, 1, 30, 'Monthly', -5.0, '', 1);`); err != nil {
		t.Fatal(err)
	}

	// The contributing lists live in the per-report detail divs, so each report
	// must carry both the row wiring and the menu those rows drive.
	for _, report := range []string{"overview", "trends", "categories", "payees", "anomalies", "budget-overruns", "recurring"} {
		rr := httptest.NewRecorder()
		a.reports(rr, httptest.NewRequest("GET", "/reports?from=2025-08-01&to=2025-11-30&report="+report, nil))
		body := rr.Body.String()
		if rr.Code != 200 {
			t.Fatalf("%s: status = %d, want 200", report, rr.Code)
		}
		if !strings.Contains(body, `id="row-menu"`) {
			t.Fatalf("%s: page has no transaction context menu", report)
		}
		if !strings.Contains(body, `data-trans-id="703"`) {
			t.Fatalf("%s: no contributing row wired to transaction 703", report)
		}
		if !strings.Contains(body, `data-edit-url="/accounts?account_id=12"`) {
			t.Fatalf("%s: contributing row has no edit target", report)
		}
		// Saving from the editor must land back on this report and period.
		if !strings.Contains(body, `data-return-to="/reports?from=2025-08-01&amp;report=`+report) {
			t.Fatalf("%s: contributing row has no return path back to the report", report)
		}
	}

	// Read-only mode offers no editing affordance at all.
	a.config.WriteEnabled = false
	rr := httptest.NewRecorder()
	a.reports(rr, httptest.NewRequest("GET", "/reports?from=2025-08-01&to=2025-11-30&report=categories", nil))
	body := rr.Body.String()
	for _, unwanted := range []string{`id="row-menu"`, `data-trans-id=`, `data-edit-url=`, `class="actionable"`} {
		if strings.Contains(body, unwanted) {
			t.Fatalf("read-only report page still renders %s", unwanted)
		}
	}
}

func TestReportReturnURL(t *testing.T) {
	got := reportReturnURL("categories", "2025-08-01", "2025-11-30", 50)
	want := "/reports?from=2025-08-01&report=categories&to=2025-11-30&top_n=50"
	if got != want {
		t.Fatalf("reportReturnURL = %q, want %q", got, want)
	}
	if sanitizeLocalPath(got) != want {
		t.Fatalf("reportReturnURL is not accepted as a local return path: %q", got)
	}
}

func TestParseTopN(t *testing.T) {
	cases := map[string]int{"10": 10, "15": 15, "25": 25, "50": 50, "": 25, "999": 25, "abc": 25}
	for raw, want := range cases {
		if got := parseTopN(raw); got != want {
			t.Fatalf("parseTopN(%q) = %d, want %d", raw, got, want)
		}
	}
}

func TestReportNavOrder(t *testing.T) {
	nav := reportNav()
	if nav[0].ID != "overview" {
		t.Fatalf("first report = %q, want overview", nav[0].ID)
	}
	for i := 2; i < len(nav); i++ {
		if nav[i-1].Name > nav[i].Name {
			t.Fatalf("reports not alphabetical after overview: %q before %q", nav[i-1].Name, nav[i].Name)
		}
	}
}

func TestDefaultReportRange(t *testing.T) {
	a, err := newApp(config{DBPath: fixtureDB(t), BackupDir: "backups"})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	wantTo := time.Now().Format("2006-01-02")
	cases := []struct {
		report   string
		wantFrom string
	}{
		{"overview", time.Now().AddDate(-1, 0, 0).Format("2006-01-02")},
		{"recurring", time.Now().AddDate(-10, 0, 0).Format("2006-01-02")},
	}
	for _, tc := range cases {
		from, to, err := a.defaultReportRange(tc.report)
		if err != nil {
			t.Fatal(err)
		}
		if to != wantTo {
			t.Fatalf("%s default to = %q, want %q", tc.report, to, wantTo)
		}
		if from != tc.wantFrom {
			t.Fatalf("%s default from = %q, want %q", tc.report, from, tc.wantFrom)
		}
	}
}

func TestBudgetOverrunsArePerBudgetYear(t *testing.T) {
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
  (400, 12, NULL, 1, 'Withdrawal', 1250.0, '', '', 'dec toiletries', 86, '2024-12-15T00:00:00', NULL, NULL, '2024-12-15T00:00:00', NULL, -1),
  (401, 12, NULL, 1, 'Withdrawal', 350.0,  '', '', 'jan toiletries', 86, '2025-01-15T00:00:00', NULL, NULL, '2025-01-15T00:00:00', NULL, -1),
  (402, 12, NULL, 1, 'Deposit',    20.0,   '', '', 'jan rebate',     86, '2025-01-20T00:00:00', NULL, NULL, '2025-01-20T00:00:00', NULL, -1);

INSERT INTO BUDGETYEAR_V1 (BUDGETYEARID, BUDGETYEARNAME) VALUES
  (1, '2024'),
  (2, '2025');
INSERT INTO BUDGETTABLE_V1 (BUDGETENTRYID, BUDGETYEARID, CATEGID, PERIOD, AMOUNT, NOTES, ACTIVE)
VALUES
  (1, 1, 86, 'Monthly', -100.0, '', 1),
  (2, 2, 86, 'Monthly', -25.0,  '', 1);`); err != nil {
		t.Fatal(err)
	}

	rows, err := a.budgetOverruns("2024-12-01", "2025-01-31")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("budget overrun rows = %d, want 2", len(rows))
	}
	if rows[0].Year != "2025" {
		t.Fatalf("first row year = %q, want 2025", rows[0].Year)
	}
	if rows[0].Category != "Toiletries" {
		t.Fatalf("first row category = %q, want Toiletries", rows[0].Category)
	}
	if rows[0].Budget != 300 {
		t.Fatalf("2025 budget = %.2f, want 300.00", rows[0].Budget)
	}
	if rows[0].Actual != 330 {
		t.Fatalf("2025 actual = %.2f, want 330.00", rows[0].Actual)
	}
	if rows[0].Overrun != 30 {
		t.Fatalf("2025 overrun = %.2f, want 30.00", rows[0].Overrun)
	}
	if len(rows[0].Rows) != 2 {
		t.Fatalf("2025 actual contributor count = %d, want 2", len(rows[0].Rows))
	}
	if rows[0].Rows[0].Date != "2025-01-20" {
		t.Fatalf("first 2025 contributor date = %q, want 2025-01-20", rows[0].Rows[0].Date)
	}
	if rows[0].Rows[0].Amount != -20 {
		t.Fatalf("first 2025 contributor amount = %.2f, want -20.00", rows[0].Rows[0].Amount)
	}
	if rows[1].Year != "2024" {
		t.Fatalf("second row year = %q, want 2024", rows[1].Year)
	}
	if rows[1].Budget != 1200 {
		t.Fatalf("2024 budget = %.2f, want 1200.00", rows[1].Budget)
	}
	if rows[1].Actual != 1250 {
		t.Fatalf("2024 actual = %.2f, want 1250.00", rows[1].Actual)
	}
	if rows[1].Overrun != 50 {
		t.Fatalf("2024 overrun = %.2f, want 50.00", rows[1].Overrun)
	}
	if len(rows[1].Rows) != 1 {
		t.Fatalf("2024 actual contributor count = %d, want 1", len(rows[1].Rows))
	}
}

func TestBucketPayeesRenderStrippedButRoundTripRaw(t *testing.T) {
	a, err := newApp(config{DBPath: fixtureDB(t), WriteEnabled: true, BackupDir: "backups"})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	// "*Gas" is a bucket payee: a class of spending rather than a merchant.
	if _, err := a.db.Exec(`
INSERT INTO PAYEE_V1 (PAYEEID, PAYEENAME, CATEGID, NUMBER, WEBSITE, NOTES, ACTIVE, PATTERN)
VALUES (900, '*Gas', NULL, '', '', '', 1, '');

INSERT INTO CHECKINGACCOUNT_V1
  (TRANSID, ACCOUNTID, TOACCOUNTID, PAYEEID, TRANSCODE, TRANSAMOUNT, STATUS,
   TRANSACTIONNUMBER, NOTES, CATEGID, TRANSDATE, FOLLOWUPID, TOTRANSAMOUNT,
   LASTUPDATEDTIME, DELETEDTIME, COLOR)
VALUES
  (800, 12, NULL, 900, 'Withdrawal', 60.0, '', '', 'fill up', 30, '2025-10-09T00:00:00', NULL, NULL, '2025-10-09T00:00:00', NULL, -1);`); err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	// An empty "from" shows the whole ledger, so the fixture's 2025 dates are
	// visible regardless of when the test runs.
	a.payees(rr, httptest.NewRequest("GET", "/payees?payee_id=900&from=", nil))
	body := rr.Body.String()
	// Display: prefix dropped, name italicised, nav entry marked for the divider.
	if strings.Count(body, `<i class="payee-bucket">Gas</i>`) < 3 {
		t.Fatalf("payees page should show the stripped name in nav, heading and ledger:\n%s", body)
	}
	if !strings.Contains(body, `class="entity-link bucket `) {
		t.Fatal("bucket payee nav entry is not marked for the group divider")
	}
	// Round trip: the raw name must survive anywhere it feeds back into resolution.
	if !strings.Contains(body, `<input required name="name" value="*Gas"`) {
		t.Fatal("payee edit form dropped the bucket prefix")
	}
	if !strings.Contains(body, `<option value="*Gas" data-id="900">`) {
		t.Fatal("payee datalist dropped the bucket prefix")
	}

	rr = httptest.NewRecorder()
	a.reports(rr, httptest.NewRequest("GET", "/reports?from=2025-10-01&to=2025-10-31&report=payees", nil))
	body = rr.Body.String()
	if !strings.Contains(body, `<i class="payee-bucket">Gas</i>`) {
		t.Fatal("payee report does not render the bucket payee stripped")
	}
	if strings.Contains(body, ">*Gas<") {
		t.Fatal("payee report still displays the raw bucket prefix")
	}
}

func TestAccountEditorCancelReturnsWhereItWasOpened(t *testing.T) {
	a, err := newApp(config{DBPath: fixtureDB(t), WriteEnabled: true, BackupDir: "backups"})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	// Opened from a report contributing-row: Cancel goes back to the report, not
	// just Save — otherwise cancelling strands the user in Accounts.
	rr := httptest.NewRecorder()
	a.accounts(rr, httptest.NewRequest("GET",
		"/accounts?account_id=12&from=&edit_trans_id=100&scroll=120&return_to=%2Freports%3Freport%3Dpayees", nil))
	body := rr.Body.String()
	if !strings.Contains(body, `href="/reports?report=payees&amp;highlight=100"`) {
		t.Fatalf("Cancel does not return to the report:\n%s", body)
	}
	if !strings.Contains(body, `name="return_to" value="/reports?report=payees"`) {
		t.Fatal("Save does not return to the report")
	}

	// Opened from the ledger itself: Cancel stays put and keeps the scroll offset.
	rr = httptest.NewRecorder()
	a.accounts(rr, httptest.NewRequest("GET", "/accounts?account_id=12&from=&edit_trans_id=100&scroll=120", nil))
	body = rr.Body.String()
	if !strings.Contains(body, `href="/accounts?account_id=12&amp;scroll=120&amp;highlight=100"`) {
		t.Fatalf("Cancel does not return to the account ledger at the same offset:\n%s", body)
	}
}
