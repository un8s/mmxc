package mmxc

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type budgetYear struct {
	ID       int
	Name     string
	Count    int
	Income   float64
	Expenses float64
	Total    float64
}

type budgetEntry struct {
	ID         int
	CategoryID int
	Category   string
	Parent     string
	Period     string
	Amount     float64
	Annual     float64
	Current    float64
	Previous   float64
	Notes      string
	Active     bool
}

var budgetPeriods = []string{"Daily", "Weekly", "Bi-Weekly", "Fortnightly", "Monthly", "Bi-Monthly", "Quarterly", "Half-Yearly", "Yearly"}

func (a *app) budgets(w http.ResponseWriter, r *http.Request) {
	years, err := a.listBudgetYears()
	if err != nil {
		a.renderError(w, "Budgets", "budgets", err)
		return
	}
	var selected *budgetYear
	if idText := r.URL.Query().Get("budget_id"); idText != "" {
		id, _ := strconv.Atoi(idText)
		for i := range years {
			if years[i].ID == id {
				selected = &years[i]
				break
			}
		}
	}
	if selected == nil && len(years) > 0 {
		selected = &years[0]
	}
	var entries []budgetEntry
	if selected != nil {
		entries, err = a.listBudgetEntries(selected.ID)
		if err != nil {
			a.renderError(w, "Budgets", "budgets", err)
			return
		}
		selected.Income, selected.Expenses, selected.Total = budgetEntryTotals(entries)
	}
	categories, err := a.listCategories()
	if err != nil {
		a.renderError(w, "Budgets", "budgets", err)
		return
	}
	a.render(w, "budgets", pageData{
		Title:             "Budgets",
		Active:            "budgets",
		Info:              a.info(),
		BudgetYears:       years,
		SelectedBudget:    selected,
		BudgetEntries:     entries,
		BudgetCategoryIDs: budgetCategoryIDMap(entries),
		Categories:        categories,
	})
}

func budgetEntryTotals(entries []budgetEntry) (float64, float64, float64) {
	var income, expenses, total float64
	for _, entry := range entries {
		total += entry.Annual
		if entry.Annual > 0 {
			income += entry.Annual
		} else if entry.Annual < 0 {
			expenses += entry.Annual
		}
	}
	return income, expenses, total
}

func (a *app) createBudgetYear(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		a.httpError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !a.config.WriteEnabled {
		a.httpError(w, "write mode is disabled", http.StatusForbidden)
		return
	}
	id, err := a.insertNextBudgetYear()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/budgets?budget_id="+strconv.Itoa(id), http.StatusSeeOther)
}

func (a *app) createBudgetEntry(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		a.httpError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !a.config.WriteEnabled {
		a.httpError(w, "write mode is disabled", http.StatusForbidden)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	budgetYearID, err := strconv.Atoi(r.FormValue("budget_id"))
	if err != nil || budgetYearID <= 0 {
		a.httpError(w, "budget id is required", http.StatusBadRequest)
		return
	}
	categoryID, err := a.resolveCategoryID(strings.TrimSpace(r.FormValue("category_text")))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	period := strings.TrimSpace(r.FormValue("period"))
	if !validBudgetPeriod(period) {
		a.httpError(w, "budget period is not supported", http.StatusBadRequest)
		return
	}
	amount, err := parseFormFloat(r, "amount", false)
	if err != nil {
		a.httpError(w, "amount "+err.Error(), http.StatusBadRequest)
		return
	}
	notes := strings.TrimSpace(r.FormValue("notes"))
	if _, err := a.insertBudgetEntry(budgetYearID, categoryID, period, amount, notes); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/budgets?budget_id="+strconv.Itoa(budgetYearID), http.StatusSeeOther)
}

func (a *app) updateBudgetEntry(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		a.httpError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !a.config.WriteEnabled {
		a.httpError(w, "write mode is disabled", http.StatusForbidden)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	id, err := strconv.Atoi(r.FormValue("budget_entry_id"))
	if err != nil || id <= 0 {
		a.httpError(w, "budget entry id is required", http.StatusBadRequest)
		return
	}
	budgetYearID, _ := strconv.Atoi(r.FormValue("budget_id"))
	period := strings.TrimSpace(r.FormValue("period"))
	if !validBudgetPeriod(period) {
		a.httpError(w, "budget period is not supported", http.StatusBadRequest)
		return
	}
	amount, err := parseFormFloat(r, "amount", false)
	if err != nil {
		a.httpError(w, "amount "+err.Error(), http.StatusBadRequest)
		return
	}
	notes := strings.TrimSpace(r.FormValue("notes"))
	if err := a.updateBudgetEntryRecord(id, period, amount, notes); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, budgetRedirect(budgetYearID), http.StatusSeeOther)
}

func (a *app) deleteBudgetEntry(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		a.httpError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !a.config.WriteEnabled {
		a.httpError(w, "write mode is disabled", http.StatusForbidden)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	id, err := strconv.Atoi(r.FormValue("budget_entry_id"))
	if err != nil || id <= 0 {
		a.httpError(w, "budget entry id is required", http.StatusBadRequest)
		return
	}
	budgetYearID, _ := strconv.Atoi(r.FormValue("budget_id"))
	if err := a.deleteBudgetEntryRecord(id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, budgetRedirect(budgetYearID), http.StatusSeeOther)
}

func (a *app) listBudgetYears() ([]budgetYear, error) {
	rows, err := a.db.Query(`
SELECT y.BUDGETYEARID, y.BUDGETYEARNAME,
       COUNT(CASE WHEN COALESCE(b.ACTIVE, 1) <> 0 AND lower(COALESCE(b.PERIOD, '')) <> 'none' THEN b.BUDGETENTRYID END) AS ENTRY_COUNT,
       COALESCE(SUM(CASE WHEN COALESCE(b.ACTIVE, 1) <> 0 AND lower(COALESCE(b.PERIOD, '')) <> 'none' AND b.AMOUNT > 0 THEN b.AMOUNT ELSE 0 END), 0) AS INCOME,
       COALESCE(SUM(CASE WHEN COALESCE(b.ACTIVE, 1) <> 0 AND lower(COALESCE(b.PERIOD, '')) <> 'none' AND b.AMOUNT < 0 THEN b.AMOUNT ELSE 0 END), 0) AS EXPENSES,
       COALESCE(SUM(CASE WHEN COALESCE(b.ACTIVE, 1) <> 0 AND lower(COALESCE(b.PERIOD, '')) <> 'none' THEN b.AMOUNT ELSE 0 END), 0) AS TOTAL
  FROM BUDGETYEAR_V1 y
  LEFT JOIN BUDGETTABLE_V1 b ON b.BUDGETYEARID = y.BUDGETYEARID
 GROUP BY y.BUDGETYEARID, y.BUDGETYEARNAME
 ORDER BY y.BUDGETYEARNAME DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var years []budgetYear
	for rows.Next() {
		var y budgetYear
		if err := rows.Scan(&y.ID, &y.Name, &y.Count, &y.Income, &y.Expenses, &y.Total); err != nil {
			return nil, err
		}
		years = append(years, y)
	}
	return years, rows.Err()
}

func (a *app) listBudgetEntries(budgetYearID int) ([]budgetEntry, error) {
	rows, err := a.db.Query(`
WITH budget_year AS (
	SELECT BUDGETYEARID, BUDGETYEARNAME
	  FROM BUDGETYEAR_V1
	 WHERE BUDGETYEARID = ?
),
category_totals AS (
	SELECT strftime('%Y', t.TRANSDATE) AS TXYEAR,
	       t.CATEGID,
	       COALESCE(SUM(CASE
	           WHEN t.TRANSCODE = 'Deposit' THEN t.TRANSAMOUNT
	           WHEN t.TRANSCODE IN ('Withdrawal', 'Transfer') THEN -t.TRANSAMOUNT
	           ELSE 0
	       END), 0) AS AMOUNT
	  FROM CHECKINGACCOUNT_V1 t
	 WHERE COALESCE(t.STATUS, '') <> 'Void'
	   AND COALESCE(t.CATEGID, -1) <> -1
	 GROUP BY strftime('%Y', t.TRANSDATE), t.CATEGID
	UNION ALL
	SELECT strftime('%Y', t.TRANSDATE) AS TXYEAR,
	       s.CATEGID,
	       COALESCE(SUM(CASE
	           WHEN t.TRANSCODE = 'Deposit' THEN s.SPLITTRANSAMOUNT
	           WHEN t.TRANSCODE IN ('Withdrawal', 'Transfer') THEN -s.SPLITTRANSAMOUNT
	           ELSE 0
	       END), 0) AS AMOUNT
	  FROM CHECKINGACCOUNT_V1 t
	  JOIN SPLITTRANSACTIONS_V1 s ON s.TRANSID = t.TRANSID
	 WHERE COALESCE(t.STATUS, '') <> 'Void'
	 GROUP BY strftime('%Y', t.TRANSDATE), s.CATEGID
),
category_year_totals AS (
	SELECT TXYEAR, CATEGID, COALESCE(SUM(AMOUNT), 0) AS AMOUNT
	  FROM category_totals
	 GROUP BY TXYEAR, CATEGID
)
SELECT b.BUDGETENTRYID, c.CATEGID, c.CATEGNAME, COALESCE(parent.CATEGNAME, ''),
       b.PERIOD, b.AMOUNT, y.BUDGETYEARNAME,
       COALESCE(current_totals.AMOUNT, 0) AS CURRENT_AMOUNT,
       COALESCE(previous_totals.AMOUNT, 0) AS PREVIOUS_AMOUNT,
       COALESCE(b.NOTES, ''), COALESCE(b.ACTIVE, 1)
  FROM BUDGETTABLE_V1 b
  JOIN budget_year y ON y.BUDGETYEARID = b.BUDGETYEARID
  JOIN CATEGORY_V1 c ON c.CATEGID = b.CATEGID
  LEFT JOIN CATEGORY_V1 parent ON parent.CATEGID = c.PARENTID
  LEFT JOIN category_year_totals current_totals
    ON current_totals.CATEGID = b.CATEGID
   AND current_totals.TXYEAR = y.BUDGETYEARNAME
  LEFT JOIN category_year_totals previous_totals
    ON previous_totals.CATEGID = b.CATEGID
   AND previous_totals.TXYEAR = printf('%04d', CAST(y.BUDGETYEARNAME AS INTEGER) - 1)
 WHERE b.BUDGETYEARID = ?
   AND COALESCE(b.ACTIVE, 1) <> 0
   AND lower(COALESCE(b.PERIOD, '')) <> 'none'
 ORDER BY COALESCE(parent.CATEGNAME, c.CATEGNAME), c.PARENTID IS NOT NULL, c.CATEGNAME`, budgetYearID, budgetYearID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []budgetEntry
	for rows.Next() {
		var e budgetEntry
		var budgetYear string
		if err := rows.Scan(&e.ID, &e.CategoryID, &e.Category, &e.Parent, &e.Period, &e.Amount, &budgetYear, &e.Current, &e.Previous, &e.Notes, &e.Active); err != nil {
			return nil, err
		}
		annual, err := annualBudgetAmount(e.Period, e.Amount, budgetYear)
		if err != nil {
			return nil, err
		}
		e.Annual = annual
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

func budgetCategoryIDMap(entries []budgetEntry) map[int]bool {
	ids := make(map[int]bool, len(entries))
	for _, entry := range entries {
		ids[entry.CategoryID] = true
	}
	return ids
}

func annualBudgetAmount(period string, amount float64, budgetYear string) (float64, error) {
	year, err := time.Parse("2006", strings.TrimSpace(budgetYear))
	if err != nil {
		return 0, err
	}
	start := time.Date(year.Year(), 1, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(year.Year(), 12, 31, 0, 0, 0, 0, time.UTC)
	return budgetAmountForRange(period, amount, start.Format("2006-01-02"), end.Format("2006-01-02"))
}

func validBudgetPeriod(period string) bool {
	for _, candidate := range budgetPeriods {
		if period == candidate {
			return true
		}
	}
	return false
}

func budgetRedirect(budgetYearID int) string {
	if budgetYearID > 0 {
		return "/budgets?budget_id=" + strconv.Itoa(budgetYearID)
	}
	return "/budgets"
}

type budgetEntryCopy struct {
	CategoryID int
	Period     string
	Amount     float64
	Notes      string
	Active     int
}

func (a *app) insertNextBudgetYear() (int, error) {
	if !a.config.WriteEnabled {
		return 0, errors.New("write mode disabled")
	}
	if err := a.probeWriteLock(); err != nil {
		return 0, fmt.Errorf("database write lock unavailable: %w", err)
	}
	if _, err := a.backupDatabase("budget-year"); err != nil {
		return 0, fmt.Errorf("backup database: %w", err)
	}

	tx, err := a.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	// The new year follows the latest numeric one and starts as its copy. With
	// none yet (every new database), it is the current calendar year, empty.
	var sourceID int
	var sourceName string
	err = tx.QueryRow(`
SELECT BUDGETYEARID, BUDGETYEARNAME
  FROM BUDGETYEAR_V1
 WHERE BUDGETYEARNAME GLOB '[0-9][0-9][0-9][0-9]'
 ORDER BY CAST(BUDGETYEARNAME AS INTEGER) DESC
 LIMIT 1`).Scan(&sourceID, &sourceName)
	var newName string
	switch {
	case errors.Is(err, sql.ErrNoRows):
		sourceID = 0
		newName = strconv.Itoa(time.Now().Year())
	case err != nil:
		return 0, err
	default:
		sourceYear, err := strconv.Atoi(sourceName)
		if err != nil {
			return 0, err
		}
		newName = strconv.Itoa(sourceYear + 1)
	}
	var existing int
	if err := tx.QueryRow("SELECT COUNT(*) FROM BUDGETYEAR_V1 WHERE BUDGETYEARNAME = ?", newName).Scan(&existing); err != nil {
		return 0, err
	}
	if existing > 0 {
		return 0, fmt.Errorf("budget year %s already exists", newName)
	}

	rows, err := tx.Query("SELECT CATEGID, PERIOD, AMOUNT, COALESCE(NOTES, ''), COALESCE(ACTIVE, 1) FROM BUDGETTABLE_V1 WHERE BUDGETYEARID = ? ORDER BY BUDGETENTRYID", sourceID)
	if err != nil {
		return 0, err
	}
	var copies []budgetEntryCopy
	for rows.Next() {
		var copy budgetEntryCopy
		if err := rows.Scan(&copy.CategoryID, &copy.Period, &copy.Amount, &copy.Notes, &copy.Active); err != nil {
			rows.Close()
			return 0, err
		}
		copies = append(copies, copy)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()

	newID, err := nextIntID(tx, "BUDGETYEAR_V1", "BUDGETYEARID")
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec("INSERT INTO BUDGETYEAR_V1 (BUDGETYEARID, BUDGETYEARNAME) VALUES (?, ?)", newID, newName); err != nil {
		return 0, err
	}
	for _, copy := range copies {
		entryID, err := nextIntID(tx, "BUDGETTABLE_V1", "BUDGETENTRYID")
		if err != nil {
			return 0, err
		}
		if _, err := tx.Exec("INSERT INTO BUDGETTABLE_V1 (BUDGETENTRYID, BUDGETYEARID, CATEGID, PERIOD, AMOUNT, NOTES, ACTIVE) VALUES (?, ?, ?, ?, ?, ?, ?)", entryID, newID, copy.CategoryID, copy.Period, copy.Amount, copy.Notes, copy.Active); err != nil {
			return 0, err
		}
	}
	return newID, tx.Commit()
}

func (a *app) insertBudgetEntry(budgetYearID, categoryID int, period string, amount float64, notes string) (int, error) {
	if !a.config.WriteEnabled {
		return 0, errors.New("write mode disabled")
	}
	if !validBudgetPeriod(period) {
		return 0, fmt.Errorf("unsupported budget period %q", period)
	}
	if n := a.countWhere("BUDGETYEAR_V1", "BUDGETYEARID = ?", budgetYearID); n == 0 {
		return 0, fmt.Errorf("budget year %d was not found", budgetYearID)
	}
	if n := a.countWhere("CATEGORY_V1", "CATEGID = ?", categoryID); n == 0 {
		return 0, fmt.Errorf("category %d was not found", categoryID)
	}
	if n := a.countWhere("BUDGETTABLE_V1", "BUDGETYEARID = ? AND CATEGID = ? AND COALESCE(ACTIVE, 1) <> 0 AND lower(COALESCE(PERIOD, '')) <> 'none'", budgetYearID, categoryID); n > 0 {
		return 0, errors.New("category is already active in this budget")
	}
	if err := a.probeWriteLock(); err != nil {
		return 0, fmt.Errorf("database write lock unavailable: %w", err)
	}
	if _, err := a.backupDatabase("budget"); err != nil {
		return 0, fmt.Errorf("backup database: %w", err)
	}

	tx, err := a.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	var id int
	err = tx.QueryRow("SELECT BUDGETENTRYID FROM BUDGETTABLE_V1 WHERE BUDGETYEARID = ? AND CATEGID = ? AND lower(COALESCE(PERIOD, '')) = 'none' ORDER BY BUDGETENTRYID LIMIT 1", budgetYearID, categoryID).Scan(&id)
	if err == nil {
		if _, err := tx.Exec("UPDATE BUDGETTABLE_V1 SET PERIOD = ?, AMOUNT = ?, NOTES = ?, ACTIVE = 1 WHERE BUDGETENTRYID = ?", period, amount, notes, id); err != nil {
			return 0, err
		}
		return id, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	id, err = nextIntID(tx, "BUDGETTABLE_V1", "BUDGETENTRYID")
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec("INSERT INTO BUDGETTABLE_V1 (BUDGETENTRYID, BUDGETYEARID, CATEGID, PERIOD, AMOUNT, NOTES, ACTIVE) VALUES (?, ?, ?, ?, ?, ?, 1)", id, budgetYearID, categoryID, period, amount, notes); err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

func (a *app) updateBudgetEntryRecord(id int, period string, amount float64, notes string) error {
	if !a.config.WriteEnabled {
		return errors.New("write mode disabled")
	}
	if !validBudgetPeriod(period) {
		return fmt.Errorf("unsupported budget period %q", period)
	}
	if err := a.probeWriteLock(); err != nil {
		return fmt.Errorf("database write lock unavailable: %w", err)
	}
	if _, err := a.backupDatabase("budget-update"); err != nil {
		return fmt.Errorf("backup database: %w", err)
	}
	result, err := a.db.Exec("UPDATE BUDGETTABLE_V1 SET PERIOD = ?, AMOUNT = ?, NOTES = ?, ACTIVE = 1 WHERE BUDGETENTRYID = ?", period, amount, notes, id)
	if err != nil {
		return err
	}
	return requireRowsAffected(result, "budget entry", id)
}

func (a *app) deleteBudgetEntryRecord(id int) error {
	if !a.config.WriteEnabled {
		return errors.New("write mode disabled")
	}
	if err := a.probeWriteLock(); err != nil {
		return fmt.Errorf("database write lock unavailable: %w", err)
	}
	if _, err := a.backupDatabase("budget-delete"); err != nil {
		return fmt.Errorf("backup database: %w", err)
	}
	result, err := a.db.Exec("DELETE FROM BUDGETTABLE_V1 WHERE BUDGETENTRYID = ?", id)
	if err != nil {
		return err
	}
	return requireRowsAffected(result, "budget entry", id)
}
