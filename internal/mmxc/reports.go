package mmxc

import (
	"database/sql"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

type reportNavItem struct {
	ID          string
	Name        string
	Description string // short blurb for the nav sidebar
	Help        string // fuller explanation shown in the report header
	Granularity string // "day", "month", or "year" — drives the range picker
	Href        string
}

type monthlyReportRow struct {
	Month    string
	Income   float64
	Expenses float64
	Net      float64
}

type rankedReportRow struct {
	Name    string
	Amount  float64
	Count   int
	Rows    []rankedTransactionRow
	categID int // category rows only; -1 for the "Uncategorized" bucket
}

// reportTransactionRef ties a contributing row back to its transaction so the
// report modals can offer the same right-click Edit/Void as the account ledger.
// Editable follows transaction.Editable: MMEX investment and share accounts have
// no editor here.
type reportTransactionRef struct {
	TransID   int
	AccountID int
	Editable  bool
}

type rankedTransactionRow struct {
	reportTransactionRef
	Date     string
	Account  string
	Payee    string
	Category string
	Amount   float64
	Notes    string
}

type anomalyReportRow struct {
	reportTransactionRef
	Date     string
	Account  string
	Payee    string
	Category string
	Notes    string
	Amount   float64
	Average  float64
	Count    int
	Rows     []anomalyTransactionRow
	payeeID  int
	categID  int
}

type anomalyTransactionRow struct {
	reportTransactionRef
	Date    string
	Account string
	Amount  float64
	Notes   string
}

type trendReportRow struct {
	Category string                 // "<Parent>: <Category>"
	Previous []float64              // earlier months, oldest → newest (aligns with TrendPrevMonths)
	Change   float64                // current month minus the immediately preceding month
	Amount   float64                // current (last selected) month
	Rows     []rankedTransactionRow // transactions contributing to Amount
	categID  int
}

type budgetOverrunRow struct {
	Year     string
	Category string
	Parent   string
	Budget   float64
	Actual   float64
	Overrun  float64
	Usage    float64
	Rows     []budgetActualRow
	categID  int
}

type budgetActualRow struct {
	reportTransactionRef
	Date    string
	Account string
	Type    string
	Amount  float64
	Notes   string
}

type recurringReportRow struct {
	LastDate     string
	NextDate     string
	Account      string
	Payee        string
	Category     string
	Type         string
	Amount       float64
	Repeat       string
	Count        int
	Transactions []recurringTransactionRow
}

type recurringTransactionRow struct {
	reportTransactionRef
	Date   string
	Amount float64
	Notes  string
}

type recurringObservation struct {
	ref      reportTransactionRef
	date     time.Time
	account  string
	payee    string
	category string
	txType   string
	amount   float64
	notes    string
}

type reportData struct {
	Nav                 []reportNavItem
	SelectedID          string
	SelectedName        string
	SelectedDescription string
	SelectedHelp        string
	Granularity         string
	TopN                int
	From                string
	To                  string
	ReturnTo            string
	Years               []int
	Income              float64
	Expenses            float64
	Net                 float64
	Transactions        int
	Monthly             []monthlyReportRow
	TopCategories       []rankedReportRow
	TopPayees           []rankedReportRow
	Anomalies           []anomalyReportRow
	Trends              []trendReportRow
	TrendPrevMonths     []string // column labels for the previous months, oldest → newest
	TrendCurrentMonth   string   // column label for the current (Amount) month
	BudgetOverruns      []budgetOverrunRow
	Recurring           []recurringReportRow
}

func (a *app) reports(w http.ResponseWriter, r *http.Request) {
	report, err := a.reportData(r)
	if err != nil {
		a.renderError(w, "Reports", "reports", err)
		return
	}
	a.render(w, "reports", pageData{Title: "Reports", Active: "reports", Info: a.info(), Reports: report})
}

func (a *app) reportData(r *http.Request) (reportData, error) {
	from := strings.TrimSpace(r.URL.Query().Get("from"))
	to := strings.TrimSpace(r.URL.Query().Get("to"))
	selectedReport := strings.TrimSpace(r.URL.Query().Get("report"))

	nav := reportNav()
	selected := nav[0]
	for i := range nav {
		if nav[i].ID == selectedReport {
			selected = nav[i]
			break
		}
	}

	if from == "" || to == "" {
		defaultFrom, defaultTo, err := a.defaultReportRange(selected.ID)
		if err != nil {
			return reportData{}, err
		}
		if from == "" {
			from = defaultFrom
		}
		if to == "" {
			to = defaultTo
		}
	}
	// The picker may submit a year (YYYY) or month (YYYY-MM); snap each bound to a
	// full calendar date so every query downstream keeps working in YYYY-MM-DD.
	from, err := snapRangeBound(from, false)
	if err != nil {
		return reportData{}, err
	}
	to, err = snapRangeBound(to, true)
	if err != nil {
		return reportData{}, err
	}

	years, err := a.reportYears()
	if err != nil {
		return reportData{}, err
	}

	topN := parseTopN(r.URL.Query().Get("top_n"))
	nav = a.translateReportNav(nav)
	selected = translateReportNavItem(a, selected)
	report := reportData{
		Nav:                 nav,
		SelectedID:          selected.ID,
		SelectedName:        selected.Name,
		SelectedDescription: selected.Description,
		SelectedHelp:        selected.Help,
		Granularity:         selected.Granularity,
		TopN:                topN,
		From:                from,
		To:                  to,
		ReturnTo:            reportReturnURL(selected.ID, from, to, topN),
		Years:               years,
	}
	// The Overview side panels stay compact; the dedicated reports honor Top N.
	catLimit, payeeLimit := topN, topN
	if selected.ID == "overview" {
		catLimit, payeeLimit = 10, 10
	}
	// Transfers are counted but contribute nothing to income or expenses, so the
	// count reflects all activity in the period while the totals stay clean.
	if err := a.db.QueryRow(`
SELECT COALESCE(SUM(CASE WHEN TRANSCODE = 'Deposit' THEN TRANSAMOUNT ELSE 0 END), 0) AS INCOME,
       COALESCE(SUM(CASE WHEN TRANSCODE = 'Withdrawal' THEN TRANSAMOUNT ELSE 0 END), 0) AS EXPENSES,
       COUNT(*) AS N
  FROM CHECKINGACCOUNT_V1
 WHERE COALESCE(STATUS, '') <> 'Void'
   AND date(TRANSDATE) BETWEEN date(?) AND date(?)`, from, to).Scan(&report.Income, &report.Expenses, &report.Transactions); err != nil {
		return reportData{}, err
	}
	report.Net = report.Income - report.Expenses

	monthly, err := a.monthlyReport(from, to)
	if err != nil {
		return reportData{}, err
	}
	report.Monthly = monthly

	categories, err := a.topExpenseCategories(from, to, catLimit)
	if err != nil {
		return reportData{}, err
	}
	if err := a.addRankedCategoryContributors(from, to, categories); err != nil {
		return reportData{}, err
	}
	report.TopCategories = categories

	payees, err := a.topExpensePayees(from, to, payeeLimit)
	if err != nil {
		return reportData{}, err
	}
	if err := a.addRankedPayeeContributors(from, to, payees); err != nil {
		return reportData{}, err
	}
	report.TopPayees = payees

	anomalies, err := a.anomalyReport(from, to)
	if err != nil {
		return reportData{}, err
	}
	report.Anomalies = anomalies

	trends, prevMonths, currentMonth, err := a.spendingTrends(from, to)
	if err != nil {
		return reportData{}, err
	}
	report.Trends = trends
	report.TrendPrevMonths = prevMonths
	report.TrendCurrentMonth = currentMonth

	overruns, err := a.budgetOverruns(from, to)
	if err != nil {
		return reportData{}, err
	}
	report.BudgetOverruns = overruns

	recurring, err := a.recurringReview(from, to)
	if err != nil {
		return reportData{}, err
	}
	report.Recurring = recurring

	return report, nil
}

func (a *app) translateReportNav(items []reportNavItem) []reportNavItem {
	out := make([]reportNavItem, len(items))
	for i := range items {
		out[i] = translateReportNavItem(a, items[i])
	}
	return out
}

func translateReportNavItem(a *app, item reportNavItem) reportNavItem {
	item.Name = a.translateUI(item.Name, "financial report name")
	item.Description = a.translateUI(item.Description, "short navigation description for a financial report")
	item.Help = a.translateUI(item.Help, "help text explaining a financial report")
	return item
}

func reportNav() []reportNavItem {
	overview := reportNavItem{
		ID: "overview", Name: "Overview", Granularity: "day",
		Description: "Income, expenses, net, and top spending summaries.",
		Help:        "Totals for the selected period, plus the largest expense categories and payees. Transfers between your own accounts are excluded so they never inflate income or expenses.",
	}
	rest := []reportNavItem{
		{ID: "trends", Name: "Spending Trends", Granularity: "month",
			Description: "Month-by-month change in category spending.",
			Help:        "Compares each category's spending across recent months to surface what is rising or falling. Split transactions are counted per split line, so their spending lands on the real categories. Always computed over whole months, so day-level selection is ignored."},
		{ID: "categories", Name: "Expense Categories", Granularity: "day",
			Description: "Largest expense categories in the selected period.",
			Help:        "Ranks expense categories by total spending in the selected period. Split transactions are counted per split line, so their spending lands on the real categories. Use Top N to widen or narrow the list; click a row to see the contributing transactions."},
		{ID: "payees", Name: "Expense Payees", Granularity: "day",
			Description: "Largest expense payees in the selected period.",
			Help:        "Ranks payees by total spending in the selected period. Use Top N to widen or narrow the list; click a row to see the contributing transactions."},
		{ID: "anomalies", Name: "Anomalies", Granularity: "day",
			Description: "Unusually large withdrawals versus their norm.",
			Help:        "Flags individual withdrawals that are well above the usual amount for their payee and category — at least 1.75× the average for that pairing and $100 or more."},
		{ID: "budget-overruns", Name: "Budget Overruns", Granularity: "year",
			Description: "Categories running above their budget.",
			Help:        "Categories whose actual spending in a budget year exceeded the budgeted amount. Each budget period is normalized to the full calendar year before comparison."},
		{ID: "recurring", Name: "Recurring Review", Granularity: "day",
			Description: "Repeating deposits, withdrawals, and transfers.",
			Help:        "Detects deposits, withdrawals, and transfers that repeat on a regular cadence (weekly through yearly) and projects the next expected date."},
	}
	// Overview stays pinned at the top; everything else is listed alphabetically.
	sort.Slice(rest, func(i, j int) bool { return rest[i].Name < rest[j].Name })
	items := append([]reportNavItem{overview}, rest...)
	for i := range items {
		// The link only pins the report; the range is restored from localStorage
		// (per report) or falls back to defaultReportRange server-side.
		items[i].Href = "/reports?report=" + items[i].ID
	}
	return items
}

// reportReturnURL rebuilds the URL of the report view currently on screen. The
// transaction editor takes it as return_to, so an edit started by right-clicking
// a contributing transaction lands back on the same report and period.
func reportReturnURL(reportID, from, to string, topN int) string {
	q := url.Values{}
	q.Set("report", reportID)
	q.Set("from", from)
	q.Set("to", to)
	q.Set("top_n", strconv.Itoa(topN))
	return "/reports?" + q.Encode()
}

// parseTopN clamps the Top N selector to one of the offered choices, defaulting
// to 25 for anything unrecognized.
func parseTopN(raw string) int {
	switch strings.TrimSpace(raw) {
	case "10":
		return 10
	case "15":
		return 15
	case "50":
		return 50
	default:
		return 25
	}
}

// snapRangeBound accepts a year (YYYY), month (YYYY-MM), or full date
// (YYYY-MM-DD) and snaps it to a calendar date: the start (end=false) or end
// (end=true) of the implied period. Full dates pass through unchanged.
func snapRangeBound(value string, end bool) (string, error) {
	value = strings.TrimSpace(value)
	switch len(value) {
	case 4: // YYYY
		y, err := time.Parse("2006", value)
		if err != nil {
			return "", err
		}
		if end {
			return time.Date(y.Year(), 12, 31, 0, 0, 0, 0, time.UTC).Format("2006-01-02"), nil
		}
		return time.Date(y.Year(), 1, 1, 0, 0, 0, 0, time.UTC).Format("2006-01-02"), nil
	case 7: // YYYY-MM
		m, err := time.Parse("2006-01", value)
		if err != nil {
			return "", err
		}
		if end {
			return m.AddDate(0, 1, -1).Format("2006-01-02"), nil
		}
		return m.Format("2006-01-02"), nil
	default:
		return value, nil
	}
}

// reportYears lists the calendar years that carry non-void transactions, newest
// first, to populate the "full year" range shortcut.
func (a *app) reportYears() ([]int, error) {
	var minYear, maxYear sql.NullString
	if err := a.db.QueryRow(`
SELECT strftime('%Y', MIN(TRANSDATE)), strftime('%Y', MAX(TRANSDATE))
  FROM CHECKINGACCOUNT_V1
 WHERE COALESCE(STATUS, '') <> 'Void'`).Scan(&minYear, &maxYear); err != nil {
		return nil, err
	}
	if !minYear.Valid || !maxYear.Valid {
		return nil, nil
	}
	lo, err := strconv.Atoi(minYear.String)
	if err != nil {
		return nil, err
	}
	hi, err := strconv.Atoi(maxYear.String)
	if err != nil {
		return nil, err
	}
	var years []int
	for y := hi; y >= lo; y-- {
		years = append(years, y)
	}
	return years, nil
}

func (a *app) defaultReportRange(report string) (string, string, error) {
	to := time.Now()
	// Recurring detection needs a long history to spot repeating transactions;
	// every other report defaults to the trailing 12 months.
	years := 1
	if report == "recurring" {
		years = 10
	}
	from := to.AddDate(-years, 0, 0)
	return from.Format("2006-01-02"), to.Format("2006-01-02"), nil
}

func (a *app) monthlyReport(from, to string) ([]monthlyReportRow, error) {
	rows, err := a.db.Query(`
SELECT strftime('%Y-%m', TRANSDATE) AS MONTH,
       COALESCE(SUM(CASE WHEN TRANSCODE = 'Deposit' THEN TRANSAMOUNT ELSE 0 END), 0) AS INCOME,
       COALESCE(SUM(CASE WHEN TRANSCODE = 'Withdrawal' THEN TRANSAMOUNT ELSE 0 END), 0) AS EXPENSES
  FROM CHECKINGACCOUNT_V1
 WHERE COALESCE(STATUS, '') <> 'Void'
   AND TRANSCODE IN ('Deposit', 'Withdrawal')
   AND date(TRANSDATE) BETWEEN date(?) AND date(?)
 GROUP BY strftime('%Y-%m', TRANSDATE)
 ORDER BY MONTH`, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []monthlyReportRow
	for rows.Next() {
		var row monthlyReportRow
		if err := rows.Scan(&row.Month, &row.Income, &row.Expenses); err != nil {
			return nil, err
		}
		row.Net = row.Income - row.Expenses
		result = append(result, row)
	}
	return result, rows.Err()
}

// expenseLinesCTE flattens withdrawals in a period into one row per category
// contribution: an ordinary transaction contributes its own amount, while a
// split transaction contributes one row per split line. Without this, splits —
// whose parent row carries no category — all land in "Uncategorized", which
// hides real spending behind a bucket that looks like a data-entry problem.
// It takes the period bounds twice (once per arm of the UNION).
const expenseLinesCTE = `
WITH expense_lines AS (
	SELECT t.TRANSID,
	       CASE WHEN COALESCE(t.CATEGID, -1) <= 0 THEN -1 ELSE t.CATEGID END AS CATEGID,
	       date(t.TRANSDATE) AS TXDATE,
	       t.TRANSAMOUNT AS AMOUNT,
	       COALESCE(t.NOTES, '') AS NOTES
	  FROM CHECKINGACCOUNT_V1 t
	 WHERE COALESCE(t.STATUS, '') <> 'Void'
	   AND t.TRANSCODE = 'Withdrawal'
	   AND date(t.TRANSDATE) BETWEEN date(?) AND date(?)
	   AND NOT EXISTS (SELECT 1 FROM SPLITTRANSACTIONS_V1 s WHERE s.TRANSID = t.TRANSID)
	UNION ALL
	SELECT t.TRANSID,
	       CASE WHEN COALESCE(s.CATEGID, -1) <= 0 THEN -1 ELSE s.CATEGID END AS CATEGID,
	       date(t.TRANSDATE) AS TXDATE,
	       s.SPLITTRANSAMOUNT AS AMOUNT,
	       COALESCE(NULLIF(s.NOTES, ''), t.NOTES, '') AS NOTES
	  FROM CHECKINGACCOUNT_V1 t
	  JOIN SPLITTRANSACTIONS_V1 s ON s.TRANSID = t.TRANSID
	 WHERE COALESCE(t.STATUS, '') <> 'Void'
	   AND t.TRANSCODE = 'Withdrawal'
	   AND date(t.TRANSDATE) BETWEEN date(?) AND date(?)
)`

func (a *app) topExpenseCategories(from, to string, limit int) ([]rankedReportRow, error) {
	rows, err := a.db.Query(expenseLinesCTE+`
SELECT l.CATEGID,
       COALESCE(c.CATEGNAME, 'Uncategorized') AS CATEGORY,
       COALESCE(parent.CATEGNAME, '') AS PARENT,
       COALESCE(SUM(l.AMOUNT), 0) AS AMOUNT,
       COUNT(*) AS N
  FROM expense_lines l
  LEFT JOIN CATEGORY_V1 c ON c.CATEGID = l.CATEGID
  LEFT JOIN CATEGORY_V1 parent ON parent.CATEGID = c.PARENTID
 GROUP BY l.CATEGID
 ORDER BY AMOUNT DESC
 LIMIT ?`, from, to, from, to, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []rankedReportRow
	for rows.Next() {
		var row rankedReportRow
		var parent string
		if err := rows.Scan(&row.categID, &row.Name, &parent, &row.Amount, &row.Count); err != nil {
			return nil, err
		}
		// Child category names repeat across parents ("Food: Misc" vs
		// "Leisure: Misc"), so qualify them the way Spending Trends does.
		if parent != "" {
			row.Name = parent + ": " + row.Name
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

func (a *app) topExpensePayees(from, to string, limit int) ([]rankedReportRow, error) {
	rows, err := a.db.Query(`
SELECT COALESCE(p.PAYEENAME, 'No payee') AS PAYEE,
       COALESCE(SUM(t.TRANSAMOUNT), 0) AS AMOUNT,
       COUNT(*) AS N
  FROM CHECKINGACCOUNT_V1 t
  LEFT JOIN PAYEE_V1 p ON p.PAYEEID = t.PAYEEID
 WHERE COALESCE(t.STATUS, '') <> 'Void'
   AND t.TRANSCODE = 'Withdrawal'
   AND date(t.TRANSDATE) BETWEEN date(?) AND date(?)
 GROUP BY COALESCE(p.PAYEENAME, 'No payee')
 ORDER BY AMOUNT DESC
 LIMIT ?`, from, to, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanRankedRows(rows)
}

func scanRankedRows(rows *sql.Rows) ([]rankedReportRow, error) {
	var result []rankedReportRow
	for rows.Next() {
		var row rankedReportRow
		if err := rows.Scan(&row.Name, &row.Amount, &row.Count); err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

// categoryContributors lists the expense lines making up one category's total in
// a period — one row per contributing line, so a split shows only the part that
// landed in this category and the rows sum to the total shown above them. label
// is echoed back as the row's category so the detail table matches the
// parent-qualified name used in the ranking.
func (a *app) categoryContributors(from, to string, categID int, label string) ([]rankedTransactionRow, error) {
	rows, err := a.db.Query(expenseLinesCTE+`
SELECT l.TXDATE,
       a.ACCOUNTNAME,
       COALESCE(p.PAYEENAME, 'No payee') AS PAYEE,
       ? AS CATEGORY,
       l.AMOUNT,
       l.NOTES`+reportRefColumns+`
  FROM expense_lines l
  JOIN CHECKINGACCOUNT_V1 t ON t.TRANSID = l.TRANSID
  JOIN ACCOUNTLIST_V1 a ON a.ACCOUNTID = t.ACCOUNTID
  LEFT JOIN PAYEE_V1 p ON p.PAYEEID = t.PAYEEID`+reportRefJoin+`
 WHERE l.CATEGID = ?
 ORDER BY l.TXDATE DESC, t.TRANSID DESC`, from, to, from, to, label, categID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanRankedContributors(rows)
}

func (a *app) addRankedCategoryContributors(from, to string, ranked []rankedReportRow) error {
	for i := range ranked {
		contributors, err := a.categoryContributors(from, to, ranked[i].categID, ranked[i].Name)
		if err != nil {
			return err
		}
		ranked[i].Rows = contributors
	}
	return nil
}

func (a *app) addRankedPayeeContributors(from, to string, ranked []rankedReportRow) error {
	for i := range ranked {
		rows, err := a.db.Query(`
SELECT date(t.TRANSDATE) AS TXDATE,
       a.ACCOUNTNAME,
       COALESCE(p.PAYEENAME, 'No payee') AS PAYEE,
       COALESCE(c.CATEGNAME, 'Uncategorized') AS CATEGORY,
       t.TRANSAMOUNT,
       COALESCE(t.NOTES, '') AS NOTES`+reportRefColumns+`
  FROM CHECKINGACCOUNT_V1 t
  JOIN ACCOUNTLIST_V1 a ON a.ACCOUNTID = t.ACCOUNTID
  LEFT JOIN PAYEE_V1 p ON p.PAYEEID = t.PAYEEID
  LEFT JOIN CATEGORY_V1 c ON c.CATEGID = t.CATEGID`+reportRefJoin+`
 WHERE COALESCE(t.STATUS, '') <> 'Void'
   AND t.TRANSCODE = 'Withdrawal'
   AND date(t.TRANSDATE) BETWEEN date(?) AND date(?)
   AND COALESCE(p.PAYEENAME, 'No payee') = ?
 ORDER BY date(t.TRANSDATE) DESC, t.TRANSID DESC`, from, to, ranked[i].Name)
		if err != nil {
			return err
		}
		contributors, err := scanRankedContributors(rows)
		rows.Close()
		if err != nil {
			return err
		}
		ranked[i].Rows = contributors
	}
	return nil
}

// reportRefColumns and reportRefJoin append the transaction identity and the
// account types that decide editability to a contributor query. They assume the
// transaction is aliased `t` and its account `a`, and must stay last in the
// SELECT list so scanReportRef can read them positionally.
const reportRefColumns = `,
       t.TRANSID,
       t.ACCOUNTID,
       a.ACCOUNTTYPE,
       COALESCE(toacct.ACCOUNTTYPE, '') AS TOACCOUNTTYPE`

const reportRefJoin = `
  LEFT JOIN ACCOUNTLIST_V1 toacct ON toacct.ACCOUNTID = t.TOACCOUNTID`

// scanTargets returns the destinations for the reportRefColumns tail, so each
// contributor scan can splice them onto its own column list. The caller resolves
// Editable from the two account types afterwards.
func (ref *reportTransactionRef) scanTargets(accountType, toAccountType *string) []any {
	return []any{&ref.TransID, &ref.AccountID, accountType, toAccountType}
}

func scanRankedContributors(rows *sql.Rows) ([]rankedTransactionRow, error) {
	var result []rankedTransactionRow
	for rows.Next() {
		var row rankedTransactionRow
		var accountType, toAccountType string
		targets := append([]any{&row.Date, &row.Account, &row.Payee, &row.Category, &row.Amount, &row.Notes},
			row.scanTargets(&accountType, &toAccountType)...)
		if err := rows.Scan(targets...); err != nil {
			return nil, err
		}
		row.Editable = transactionEditable(accountType, toAccountType)
		result = append(result, row)
	}
	return result, rows.Err()
}

func (a *app) anomalyReport(from, to string) ([]anomalyReportRow, error) {
	rows, err := a.db.Query(`
WITH expense_rows AS (
	SELECT t.TRANSID,
	       t.ACCOUNTID,
	       date(t.TRANSDATE) AS TXDATE,
	       a.ACCOUNTNAME,
	       COALESCE(p.PAYEENAME, 'No payee') AS PAYEE,
	       COALESCE(c.CATEGNAME, 'Uncategorized') AS CATEGORY,
	       COALESCE(t.NOTES, '') AS NOTES,
	       t.TRANSAMOUNT AS AMOUNT,
	       COALESCE(t.PAYEEID, -1) AS PAYEEID,
	       COALESCE(t.CATEGID, -1) AS CATEGID,
	       a.ACCOUNTTYPE AS ACCOUNTTYPE,
	       COALESCE(toacct.ACCOUNTTYPE, '') AS TOACCOUNTTYPE
	  FROM CHECKINGACCOUNT_V1 t
	  JOIN ACCOUNTLIST_V1 a ON a.ACCOUNTID = t.ACCOUNTID
	  LEFT JOIN PAYEE_V1 p ON p.PAYEEID = t.PAYEEID
	  LEFT JOIN CATEGORY_V1 c ON c.CATEGID = t.CATEGID
	  LEFT JOIN ACCOUNTLIST_V1 toacct ON toacct.ACCOUNTID = t.TOACCOUNTID
	 WHERE COALESCE(t.STATUS, '') <> 'Void'
	   AND t.TRANSCODE = 'Withdrawal'
	   AND COALESCE(t.CATEGID, -1) <> -1
	   AND date(t.TRANSDATE) BETWEEN date(?) AND date(?)
),
baselines AS (
	SELECT PAYEEID, CATEGID, AVG(AMOUNT) AS AVG_AMOUNT, COUNT(*) AS N
	  FROM expense_rows
	 GROUP BY PAYEEID, CATEGID
)
SELECT e.TXDATE, e.ACCOUNTNAME, e.ACCOUNTID, e.TRANSID, e.PAYEE, e.CATEGORY, e.NOTES, e.AMOUNT, b.AVG_AMOUNT, b.N, e.PAYEEID, e.CATEGID, e.ACCOUNTTYPE, e.TOACCOUNTTYPE
  FROM expense_rows e
  JOIN baselines b ON b.PAYEEID = e.PAYEEID AND b.CATEGID = e.CATEGID
 WHERE b.N >= 2
   AND e.AMOUNT >= 100
   AND e.AMOUNT >= b.AVG_AMOUNT * 1.75
 ORDER BY e.AMOUNT - b.AVG_AMOUNT DESC, e.TXDATE DESC
 LIMIT 25`, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []anomalyReportRow
	for rows.Next() {
		var row anomalyReportRow
		var accountType, toAccountType string
		if err := rows.Scan(&row.Date, &row.Account, &row.AccountID, &row.TransID, &row.Payee, &row.Category, &row.Notes, &row.Amount, &row.Average, &row.Count, &row.payeeID, &row.categID, &accountType, &toAccountType); err != nil {
			return nil, err
		}
		row.Editable = transactionEditable(accountType, toAccountType)
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range result {
		contributors, err := a.anomalyContributors(from, to, result[i].payeeID, result[i].categID)
		if err != nil {
			return nil, err
		}
		result[i].Rows = contributors
	}
	return result, nil
}

func (a *app) anomalyContributors(from, to string, payeeID, categID int) ([]anomalyTransactionRow, error) {
	rows, err := a.db.Query(`
SELECT date(t.TRANSDATE) AS TXDATE,
       a.ACCOUNTNAME,
       t.TRANSAMOUNT,
       COALESCE(t.NOTES, '') AS NOTES`+reportRefColumns+`
  FROM CHECKINGACCOUNT_V1 t
  JOIN ACCOUNTLIST_V1 a ON a.ACCOUNTID = t.ACCOUNTID`+reportRefJoin+`
 WHERE COALESCE(t.STATUS, '') <> 'Void'
   AND t.TRANSCODE = 'Withdrawal'
   AND COALESCE(t.CATEGID, -1) <> -1
   AND date(t.TRANSDATE) BETWEEN date(?) AND date(?)
   AND COALESCE(t.PAYEEID, -1) = ?
   AND COALESCE(t.CATEGID, -1) = ?
 ORDER BY date(t.TRANSDATE) DESC, t.TRANSID DESC`, from, to, payeeID, categID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []anomalyTransactionRow
	for rows.Next() {
		var row anomalyTransactionRow
		var accountType, toAccountType string
		targets := append([]any{&row.Date, &row.Account, &row.Amount, &row.Notes},
			row.scanTargets(&accountType, &toAccountType)...)
		if err := rows.Scan(targets...); err != nil {
			return nil, err
		}
		row.Editable = transactionEditable(accountType, toAccountType)
		result = append(result, row)
	}
	return result, rows.Err()
}

// trendChangeThreshold is the minimum absolute month-over-month swing (in the
// base currency) for a category to count as a "noteworthy" trend.
const trendChangeThreshold = 50.0

// spendingTrends compares each expense category's spending in the last selected
// month against up to three preceding months. The report is inherently
// month-by-month, so the bounds are taken as full calendar months. It returns
// the rows (sorted by Change descending), the previous-month column labels
// (oldest → newest), and the current-month label.
func (a *app) spendingTrends(from, to string) ([]trendReportRow, []string, string, error) {
	toT, err := time.Parse("2006-01-02", to)
	if err != nil {
		return nil, nil, "", err
	}
	fromT, err := time.Parse("2006-01-02", from)
	if err != nil {
		return nil, nil, "", err
	}
	current := time.Date(toT.Year(), toT.Month(), 1, 0, 0, 0, 0, time.UTC)
	fromMonth := time.Date(fromT.Year(), fromT.Month(), 1, 0, 0, 0, 0, time.UTC)
	currentLabel := current.Format("2006-01")

	prevCount := (current.Year()-fromMonth.Year())*12 + int(current.Month()) - int(fromMonth.Month())
	if prevCount < 0 {
		return nil, nil, currentLabel, nil
	}
	if prevCount > 3 {
		prevCount = 3
	}
	// Previous columns run oldest → newest: PREVIOUS-2, PREVIOUS-1, PREVIOUS.
	prevLabels := make([]string, 0, prevCount)
	for off := prevCount; off >= 1; off-- {
		prevLabels = append(prevLabels, current.AddDate(0, -off, 0).Format("2006-01"))
	}
	immediatePrev := ""
	if prevCount > 0 {
		immediatePrev = current.AddDate(0, -1, 0).Format("2006-01")
	}
	// Whole-month bounds covering every column: the oldest previous month (or the
	// current one when it stands alone) through the end of the current month.
	windowStart := current.AddDate(0, -prevCount, 0).Format("2006-01-02")
	curStart := current.Format("2006-01-02")
	curEnd := current.AddDate(0, 1, -1).Format("2006-01-02")

	rows, err := a.db.Query(expenseLinesCTE+`
SELECT l.CATEGID,
       COALESCE(c.CATEGNAME, 'Uncategorized') AS CATEGORY,
       COALESCE(parent.CATEGNAME, '') AS PARENT,
       strftime('%Y-%m', l.TXDATE) AS MONTH,
       COALESCE(SUM(l.AMOUNT), 0) AS AMOUNT
  FROM expense_lines l
  LEFT JOIN CATEGORY_V1 c ON c.CATEGID = l.CATEGID
  LEFT JOIN CATEGORY_V1 parent ON parent.CATEGID = c.PARENTID
 GROUP BY l.CATEGID, MONTH`, windowStart, curEnd, windowStart, curEnd)
	if err != nil {
		return nil, nil, "", err
	}
	defer rows.Close()

	type catAgg struct {
		label  string
		months map[string]float64
	}
	aggs := make(map[int]*catAgg)
	var order []int
	for rows.Next() {
		var categID int
		var name, parent, month string
		var amount float64
		if err := rows.Scan(&categID, &name, &parent, &month, &amount); err != nil {
			return nil, nil, "", err
		}
		agg := aggs[categID]
		if agg == nil {
			label := name
			if parent != "" {
				label = parent + ": " + name
			}
			agg = &catAgg{label: label, months: make(map[string]float64)}
			aggs[categID] = agg
			order = append(order, categID)
		}
		agg.months[month] = amount
	}
	if err := rows.Err(); err != nil {
		return nil, nil, "", err
	}

	var result []trendReportRow
	for _, categID := range order {
		agg := aggs[categID]
		amount := agg.months[currentLabel]
		var prev float64
		if immediatePrev != "" {
			prev = agg.months[immediatePrev]
		}
		change := amount - prev
		if math.Abs(change) < trendChangeThreshold {
			continue
		}
		previous := make([]float64, len(prevLabels))
		for i, lbl := range prevLabels {
			previous[i] = agg.months[lbl]
		}
		result = append(result, trendReportRow{
			Category: agg.label,
			Previous: previous,
			Change:   change,
			Amount:   amount,
			categID:  categID,
		})
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Change == result[j].Change {
			return result[i].Category < result[j].Category
		}
		return result[i].Change > result[j].Change
	})

	for i := range result {
		contributors, err := a.categoryContributors(curStart, curEnd, result[i].categID, result[i].Category)
		if err != nil {
			return nil, nil, "", err
		}
		result[i].Rows = contributors
	}
	return result, prevLabels, currentLabel, nil
}

func (a *app) budgetOverruns(from, to string) ([]budgetOverrunRow, error) {
	start, err := time.Parse("2006-01-02", from)
	if err != nil {
		return nil, err
	}
	end, err := time.Parse("2006-01-02", to)
	if err != nil {
		return nil, err
	}
	if end.Before(start) {
		return nil, nil
	}
	fromYear := fmt.Sprintf("%04d", start.Year())
	toYear := fmt.Sprintf("%04d", end.Year())

	actualRows, err := a.db.Query(`
WITH actuals AS (
	SELECT strftime('%Y', t.TRANSDATE) AS BUDGET_YEAR,
	       t.CATEGID,
	       COALESCE(SUM(CASE WHEN t.TRANSCODE = 'Deposit' THEN -t.TRANSAMOUNT ELSE t.TRANSAMOUNT END), 0) AS ACTUAL
	  FROM CHECKINGACCOUNT_V1 t
	 WHERE COALESCE(t.STATUS, '') <> 'Void'
	   AND t.TRANSCODE IN ('Deposit', 'Withdrawal')
	   AND COALESCE(t.CATEGID, -1) <> -1
	   AND strftime('%Y', t.TRANSDATE) BETWEEN ? AND ?
	 GROUP BY strftime('%Y', t.TRANSDATE), t.CATEGID
	UNION ALL
	SELECT strftime('%Y', t.TRANSDATE) AS BUDGET_YEAR,
	       s.CATEGID,
	       COALESCE(SUM(CASE WHEN t.TRANSCODE = 'Deposit' THEN -s.SPLITTRANSAMOUNT ELSE s.SPLITTRANSAMOUNT END), 0) AS ACTUAL
	  FROM CHECKINGACCOUNT_V1 t
	  JOIN SPLITTRANSACTIONS_V1 s ON s.TRANSID = t.TRANSID
	 WHERE COALESCE(t.STATUS, '') <> 'Void'
	   AND t.TRANSCODE IN ('Deposit', 'Withdrawal')
	   AND strftime('%Y', t.TRANSDATE) BETWEEN ? AND ?
	 GROUP BY strftime('%Y', t.TRANSDATE), s.CATEGID
),
actual_totals AS (
	SELECT BUDGET_YEAR, CATEGID, COALESCE(SUM(ACTUAL), 0) AS ACTUAL
	  FROM actuals
	 GROUP BY BUDGET_YEAR, CATEGID
)
SELECT a.BUDGET_YEAR,
       c.CATEGNAME,
       COALESCE(parent.CATEGNAME, '') AS PARENT,
       c.CATEGID,
       COALESCE(a.ACTUAL, 0) AS ACTUAL
  FROM CATEGORY_V1 c
  LEFT JOIN CATEGORY_V1 parent ON parent.CATEGID = c.PARENTID
  JOIN actual_totals a ON a.CATEGID = c.CATEGID
 ORDER BY a.BUDGET_YEAR, COALESCE(parent.CATEGNAME, c.CATEGNAME), c.PARENTID IS NOT NULL, c.CATEGNAME`, fromYear, toYear, fromYear, toYear)
	if err != nil {
		return nil, err
	}
	defer actualRows.Close()

	type budgetOverrunKey struct {
		year       string
		categoryID int
	}

	byCategory := make(map[budgetOverrunKey]*budgetOverrunRow)
	for actualRows.Next() {
		var categoryID int
		row := budgetOverrunRow{}
		if err := actualRows.Scan(&row.Year, &row.Category, &row.Parent, &categoryID, &row.Actual); err != nil {
			return nil, err
		}
		row.categID = categoryID
		key := budgetOverrunKey{year: row.Year, categoryID: categoryID}
		byCategory[key] = &row
	}
	if err := actualRows.Err(); err != nil {
		return nil, err
	}

	budgetRows, err := a.db.Query(`
SELECT c.CATEGID,
       c.CATEGNAME,
       COALESCE(parent.CATEGNAME, '') AS PARENT,
       y.BUDGETYEARNAME,
       b.PERIOD,
       b.AMOUNT
  FROM BUDGETTABLE_V1 b
  JOIN BUDGETYEAR_V1 y ON y.BUDGETYEARID = b.BUDGETYEARID
  JOIN CATEGORY_V1 c ON c.CATEGID = b.CATEGID
  LEFT JOIN CATEGORY_V1 parent ON parent.CATEGID = c.PARENTID
 WHERE COALESCE(b.ACTIVE, 1) <> 0
   AND b.AMOUNT < 0
   AND y.BUDGETYEARNAME BETWEEN ? AND ?
 ORDER BY y.BUDGETYEARNAME, COALESCE(parent.CATEGNAME, c.CATEGNAME), c.PARENTID IS NOT NULL, c.CATEGNAME`, fromYear, toYear)
	if err != nil {
		return nil, err
	}
	defer budgetRows.Close()

	for budgetRows.Next() {
		var categoryID int
		var category, parent, budgetYear, period string
		var amount float64
		if err := budgetRows.Scan(&categoryID, &category, &parent, &budgetYear, &period, &amount); err != nil {
			return nil, err
		}
		year, err := time.Parse("2006", budgetYear)
		if err != nil {
			continue
		}
		yearStart := time.Date(year.Year(), 1, 1, 0, 0, 0, 0, time.UTC)
		yearEnd := time.Date(year.Year(), 12, 31, 0, 0, 0, 0, time.UTC)
		budget, err := budgetAmountForRange(period, amount, yearStart.Format("2006-01-02"), yearEnd.Format("2006-01-02"))
		if err != nil {
			return nil, err
		}
		if budget < 0 {
			budget = -budget
		}
		if budget <= 0 {
			continue
		}
		key := budgetOverrunKey{year: budgetYear, categoryID: categoryID}
		row := byCategory[key]
		if row == nil {
			row = &budgetOverrunRow{Year: budgetYear, Category: category, Parent: parent, categID: categoryID}
			byCategory[key] = row
		}
		row.Budget += budget
	}
	if err := budgetRows.Err(); err != nil {
		return nil, err
	}

	var result []budgetOverrunRow
	for _, row := range byCategory {
		if row.Budget <= 0 || row.Actual <= row.Budget {
			continue
		}
		row.Overrun = row.Actual - row.Budget
		row.Usage = row.Actual / row.Budget * 100
		contributors, err := a.budgetActualContributors(row.Year, row.categID)
		if err != nil {
			return nil, err
		}
		row.Rows = contributors
		result = append(result, *row)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Year != result[j].Year {
			return result[i].Year > result[j].Year
		}
		if result[i].Overrun == result[j].Overrun {
			return result[i].Category < result[j].Category
		}
		return result[i].Overrun > result[j].Overrun
	})
	return result, nil
}

func (a *app) budgetActualContributors(year string, categID int) ([]budgetActualRow, error) {
	rows, err := a.db.Query(`
SELECT date(TXDATE) AS TXDATE, ACCOUNTNAME, TRANSCODE, AMOUNT, NOTES, SORT_ID, ACCOUNTID, ACCOUNTTYPE, TOACCOUNTTYPE
  FROM (
	SELECT t.TRANSDATE AS TXDATE,
	       a.ACCOUNTNAME AS ACCOUNTNAME,
	       t.TRANSCODE AS TRANSCODE,
	       CASE WHEN t.TRANSCODE = 'Deposit' THEN -t.TRANSAMOUNT ELSE t.TRANSAMOUNT END AS AMOUNT,
	       COALESCE(t.NOTES, '') AS NOTES,
	       t.TRANSID AS SORT_ID,
	       t.ACCOUNTID AS ACCOUNTID,
	       a.ACCOUNTTYPE AS ACCOUNTTYPE,
	       COALESCE(toacct.ACCOUNTTYPE, '') AS TOACCOUNTTYPE
	  FROM CHECKINGACCOUNT_V1 t
	  JOIN ACCOUNTLIST_V1 a ON a.ACCOUNTID = t.ACCOUNTID
	  LEFT JOIN ACCOUNTLIST_V1 toacct ON toacct.ACCOUNTID = t.TOACCOUNTID
	 WHERE COALESCE(t.STATUS, '') <> 'Void'
	   AND t.TRANSCODE IN ('Deposit', 'Withdrawal')
	   AND COALESCE(t.CATEGID, -1) <> -1
	   AND strftime('%Y', t.TRANSDATE) = ?
	   AND t.CATEGID = ?
	UNION ALL
	SELECT t.TRANSDATE AS TXDATE,
	       a.ACCOUNTNAME AS ACCOUNTNAME,
	       t.TRANSCODE AS TRANSCODE,
	       CASE WHEN t.TRANSCODE = 'Deposit' THEN -s.SPLITTRANSAMOUNT ELSE s.SPLITTRANSAMOUNT END AS AMOUNT,
	       COALESCE(s.NOTES, t.NOTES, '') AS NOTES,
	       t.TRANSID AS SORT_ID,
	       t.ACCOUNTID AS ACCOUNTID,
	       a.ACCOUNTTYPE AS ACCOUNTTYPE,
	       COALESCE(toacct.ACCOUNTTYPE, '') AS TOACCOUNTTYPE
	  FROM CHECKINGACCOUNT_V1 t
	  JOIN ACCOUNTLIST_V1 a ON a.ACCOUNTID = t.ACCOUNTID
	  JOIN SPLITTRANSACTIONS_V1 s ON s.TRANSID = t.TRANSID
	  LEFT JOIN ACCOUNTLIST_V1 toacct ON toacct.ACCOUNTID = t.TOACCOUNTID
	 WHERE COALESCE(t.STATUS, '') <> 'Void'
	   AND t.TRANSCODE IN ('Deposit', 'Withdrawal')
	   AND strftime('%Y', t.TRANSDATE) = ?
	   AND s.CATEGID = ?
  )
 ORDER BY date(TXDATE) DESC, SORT_ID DESC`, year, categID, year, categID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []budgetActualRow
	for rows.Next() {
		var row budgetActualRow
		var accountType, toAccountType string
		if err := rows.Scan(&row.Date, &row.Account, &row.Type, &row.Amount, &row.Notes,
			&row.TransID, &row.AccountID, &accountType, &toAccountType); err != nil {
			return nil, err
		}
		row.Editable = transactionEditable(accountType, toAccountType)
		result = append(result, row)
	}
	return result, rows.Err()
}

func budgetAmountForRange(period string, amount float64, from, to string) (float64, error) {
	start, err := time.Parse("2006-01-02", from)
	if err != nil {
		return 0, err
	}
	end, err := time.Parse("2006-01-02", to)
	if err != nil {
		return 0, err
	}
	if end.Before(start) {
		return 0, nil
	}
	days := end.Sub(start).Hours()/24 + 1
	months := float64((end.Year()-start.Year())*12 + int(end.Month()) - int(start.Month()) + 1)
	switch strings.ToLower(strings.TrimSpace(period)) {
	case "", "none":
		return amount, nil
	case "daily":
		return amount * days, nil
	case "weekly":
		return amount * days / 7, nil
	case "bi-weekly", "fortnightly":
		return amount * days / 14, nil
	case "monthly":
		return amount * months, nil
	case "bi-monthly":
		return amount * months / 2, nil
	case "quarterly":
		return amount * months / 3, nil
	case "half-yearly":
		return amount * months / 6, nil
	case "yearly":
		return amount * months / 12, nil
	default:
		return 0, fmt.Errorf("unsupported budget period %q", period)
	}
}

func (a *app) recurringReview(from, to string) ([]recurringReportRow, error) {
	rows, err := a.db.Query(`
SELECT date(t.TRANSDATE) AS TXDATE,
       a.ACCOUNTNAME,
       COALESCE(p.PAYEENAME, 'No payee') AS PAYEE,
       COALESCE(c.CATEGNAME, 'Uncategorized') AS CATEGORY,
       t.TRANSCODE,
       t.TRANSAMOUNT,
       COALESCE(t.NOTES, '') AS NOTES`+reportRefColumns+`
  FROM CHECKINGACCOUNT_V1 t
  JOIN ACCOUNTLIST_V1 a ON a.ACCOUNTID = t.ACCOUNTID
  LEFT JOIN PAYEE_V1 p ON p.PAYEEID = t.PAYEEID
  LEFT JOIN CATEGORY_V1 c ON c.CATEGID = t.CATEGID`+reportRefJoin+`
 WHERE COALESCE(t.STATUS, '') <> 'Void'
   AND t.TRANSCODE IN ('Deposit', 'Withdrawal')
   AND COALESCE(t.CATEGID, -1) <> -1
   AND date(t.TRANSDATE) BETWEEN date(?) AND date(?)
 ORDER BY a.ACCOUNTNAME, p.PAYEENAME, c.CATEGNAME, t.TRANSCODE, date(t.TRANSDATE)`, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	groups := make(map[string][]recurringObservation)
	for rows.Next() {
		var dateText string
		var obs recurringObservation
		var accountType, toAccountType string
		targets := append([]any{&dateText, &obs.account, &obs.payee, &obs.category, &obs.txType, &obs.amount, &obs.notes},
			obs.ref.scanTargets(&accountType, &toAccountType)...)
		if err := rows.Scan(targets...); err != nil {
			return nil, err
		}
		obs.ref.Editable = transactionEditable(accountType, toAccountType)
		parsed, err := time.Parse("2006-01-02", dateText)
		if err != nil {
			continue
		}
		obs.date = parsed
		key := strings.Join([]string{obs.account, obs.payee, obs.category, obs.txType}, "\x00")
		groups[key] = append(groups[key], obs)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var result []recurringReportRow
	for _, group := range groups {
		row, ok := recurringFromObservations(group)
		if ok {
			result = append(result, row)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].NextDate == result[j].NextDate {
			if result[i].Payee == result[j].Payee {
				return result[i].Category < result[j].Category
			}
			return result[i].Payee < result[j].Payee
		}
		return result[i].NextDate > result[j].NextDate
	})
	if len(result) > 50 {
		result = result[:50]
	}
	return result, nil
}

func recurringFromObservations(group []recurringObservation) (recurringReportRow, bool) {
	if len(group) < 3 {
		return recurringReportRow{}, false
	}
	sort.Slice(group, func(i, j int) bool {
		return group[i].date.Before(group[j].date)
	})

	var gaps []int
	for i := 1; i < len(group); i++ {
		gap := int(group[i].date.Sub(group[i-1].date).Hours()/24 + 0.5)
		if gap > 0 {
			gaps = append(gaps, gap)
		}
	}
	if len(gaps) < 2 {
		return recurringReportRow{}, false
	}

	cadence, ok := recurringCadence(gaps)
	if !ok {
		return recurringReportRow{}, false
	}

	var total float64
	for i := range group {
		total += group[i].amount
	}
	last := group[len(group)-1]
	transactions := make([]recurringTransactionRow, 0, len(group))
	for i := len(group) - 1; i >= 0; i-- {
		transactions = append(transactions, recurringTransactionRow{
			reportTransactionRef: group[i].ref,
			Date:                 group[i].date.Format("2006-01-02"),
			Amount:               group[i].amount,
			Notes:                group[i].notes,
		})
	}
	return recurringReportRow{
		LastDate:     last.date.Format("2006-01-02"),
		NextDate:     nextRecurringDate(last.date, cadence).Format("2006-01-02"),
		Account:      last.account,
		Payee:        last.payee,
		Category:     last.category,
		Type:         last.txType,
		Amount:       total / float64(len(group)),
		Repeat:       cadence,
		Count:        len(group),
		Transactions: transactions,
	}, true
}

func recurringCadence(gaps []int) (string, bool) {
	sorted := append([]int(nil), gaps...)
	sort.Ints(sorted)
	median := sorted[len(sorted)/2]

	candidates := []struct {
		name string
		min  int
		max  int
	}{
		{name: "Weekly", min: 6, max: 8},
		{name: "Bi-weekly", min: 12, max: 16},
		{name: "Monthly", min: 25, max: 35},
		{name: "Quarterly", min: 80, max: 100},
		{name: "Yearly", min: 330, max: 400},
	}
	for _, candidate := range candidates {
		if median < candidate.min || median > candidate.max {
			continue
		}
		matches := 0
		for _, gap := range gaps {
			if gap >= candidate.min && gap <= candidate.max {
				matches++
			}
		}
		if matches >= len(gaps)-1 {
			return candidate.name, true
		}
	}
	return "", false
}

func nextRecurringDate(last time.Time, cadence string) time.Time {
	switch cadence {
	case "Weekly":
		return last.AddDate(0, 0, 7)
	case "Bi-weekly":
		return last.AddDate(0, 0, 14)
	case "Monthly":
		return last.AddDate(0, 1, 0)
	case "Quarterly":
		return last.AddDate(0, 3, 0)
	case "Yearly":
		return last.AddDate(1, 0, 0)
	default:
		return last
	}
}
