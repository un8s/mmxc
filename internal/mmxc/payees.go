package mmxc

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

type payee struct {
	ID   int
	Name string
	// Category/CategoryID are the payee's default category (PAYEE_V1.CATEGID),
	// the one MMEX pre-fills when you pick the payee. CategoryID is 0 when the
	// payee has no default; the transaction forms use it to fill an empty
	// category on selection (see app.js).
	Category   string
	CategoryID int
	Notes      string
	Active     bool
	Count      int
	Bucket     bool // name starts with bucketPayeePrefix; see displayPayee
}

func (a *app) payees(w http.ResponseWriter, r *http.Request) {
	payees, err := a.listPayees()
	if err != nil {
		a.renderError(w, "Payees", "payees", err)
		return
	}
	cats, err := a.listCategories()
	if err != nil {
		a.renderError(w, "Payees", "payees", err)
		return
	}
	accounts, err := a.listAccounts()
	if err != nil {
		a.renderError(w, "Payees", "payees", err)
		return
	}
	var selected *payee
	if idText := r.URL.Query().Get("payee_id"); idText != "" {
		id, _ := strconv.Atoi(idText)
		for i := range payees {
			if payees[i].ID == id {
				selected = &payees[i]
				break
			}
		}
	}
	if selected == nil {
		for i := range payees {
			if !a.config.HideInactiveItems || payees[i].Active {
				selected = &payees[i]
				break
			}
		}
	}
	filter := parseRecentTransactionFilter(r)
	var txs []transaction
	if selected != nil {
		filter.PayeeID = selected.ID
		txs, err = a.listTransactions(filter)
		if err != nil {
			a.renderError(w, "Payees", "payees", err)
			return
		}
	}
	var editTx *transaction
	if idText := r.URL.Query().Get("edit_trans_id"); idText != "" && a.config.WriteEnabled {
		if id, err := strconv.Atoi(idText); err == nil {
			if tx, err := a.getTransaction(id); err == nil {
				editTx = &tx
			}
		}
	}
	a.render(w, "payees", pageData{
		Title:              "Payees",
		Active:             "payees",
		Info:               a.info(),
		Accounts:           accounts,
		Payees:             payees,
		Categories:         cats,
		EditTransaction:    editTx,
		Scroll:             scrollParam(r.URL.Query().Get("scroll")),
		ReturnTo:           localReturnURL(r),
		SelectedPayee:      selected,
		Transactions:       txs,
		TransactionSummary: summarizeTransactions(txs),
		Filter:             filter,
	})
}

func (a *app) createPayee(w http.ResponseWriter, r *http.Request) {
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
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		a.httpError(w, "payee name is required", http.StatusBadRequest)
		return
	}
	categoryID := 0
	if categoryText := strings.TrimSpace(r.FormValue("category_text")); categoryText != "" {
		id, err := a.resolveCategoryID(categoryText)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if !a.categoryActive(id) {
			a.httpError(w, "default category is inactive; choose an active category", http.StatusBadRequest)
			return
		}
		categoryID = id
	}
	id, err := a.insertPayee(name, categoryID, strings.TrimSpace(r.FormValue("notes")), formBoolDefault(r, "active", true))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	a.audit(r, auditEntry{
		Action: fmt.Sprintf("payee add id=%d category_id=%d active=%t", id, categoryID, formBoolDefault(r, "active", true)),
		Detail: fmt.Sprintf("name=%q", name),
	})
	http.Redirect(w, r, "/payees?payee_id="+strconv.Itoa(id), http.StatusSeeOther)
}

func (a *app) updatePayee(w http.ResponseWriter, r *http.Request) {
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
	id, err := strconv.Atoi(r.FormValue("payee_id"))
	if err != nil || id <= 0 {
		a.httpError(w, "payee id is required", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		a.httpError(w, "payee name is required", http.StatusBadRequest)
		return
	}
	categoryID := 0
	if categoryText := strings.TrimSpace(r.FormValue("category_text")); categoryText != "" {
		categoryID, err = a.resolveCategoryID(categoryText)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if !a.categoryActive(categoryID) && categoryID != a.payeeCategoryID(id) {
			a.httpError(w, "default category is inactive; choose an active category", http.StatusBadRequest)
			return
		}
	}
	oldName := a.payeeName(id)
	oldActive := a.payeeActive(id)
	active := formBoolDefault(r, "active", false)
	if err := a.updatePayeeRecord(id, name, categoryID, strings.TrimSpace(r.FormValue("notes")), active); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if oldName != "" && oldName != name {
		a.audit(r, auditEntry{
			Action: fmt.Sprintf("payee rename id=%d", id),
			Detail: fmt.Sprintf("old_name=%q new_name=%q", oldName, name),
		})
	}
	if oldActive != active {
		action := "deactivate"
		if active {
			action = "activate"
		}
		a.audit(r, auditEntry{
			Action: fmt.Sprintf("payee %s id=%d", action, id),
			Detail: fmt.Sprintf("name=%q", name),
		})
	}
	http.Redirect(w, r, "/payees?payee_id="+strconv.Itoa(id), http.StatusSeeOther)
}

func (a *app) deletePayee(w http.ResponseWriter, r *http.Request) {
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
	id, err := strconv.Atoi(r.FormValue("payee_id"))
	if err != nil || id <= 0 {
		a.httpError(w, "payee id is required", http.StatusBadRequest)
		return
	}
	name := a.payeeName(id)
	if err := a.deleteUnusedPayee(id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	a.audit(r, auditEntry{
		Action: fmt.Sprintf("payee delete id=%d", id),
		Detail: fmt.Sprintf("name=%q", name),
	})
	http.Redirect(w, r, "/payees", http.StatusSeeOther)
}

// payeeNotFoundError is resolvePayeeID's "nothing matched" failure, kept
// distinct from an ambiguous match so the transaction form can offer to create
// the payee only when the name is genuinely new.
type payeeNotFoundError struct{ text string }

func (e *payeeNotFoundError) Error() string {
	return fmt.Sprintf("payee %q was not found", e.text)
}

func (a *app) resolvePayeeID(text string) (int, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return 0, errors.New("payee is required")
	}
	rows, err := a.db.Query(`
SELECT PAYEEID,
       CASE
         WHEN PAYEENAME = ? THEN 0
         WHEN COALESCE(PATTERN, '') LIKE ? THEN 1
         ELSE 2
       END AS MATCH_RANK
  FROM PAYEE_V1
 WHERE PAYEENAME = ?
    OR COALESCE(PATTERN, '') LIKE ?
    OR PAYEENAME LIKE ?
 ORDER BY CASE
   WHEN PAYEENAME = ? THEN 0
   WHEN COALESCE(PATTERN, '') LIKE ? THEN 1
   ELSE 2
 END, PAYEENAME
 LIMIT 10`, text, "%"+text+"%", text, "%"+text+"%", "%"+text+"%", text, "%"+text+"%")
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	var ids []int
	bestRank := 99
	for rows.Next() {
		var id int
		var rank int
		if err := rows.Scan(&id, &rank); err != nil {
			return 0, err
		}
		if rank > bestRank {
			continue
		}
		if rank < bestRank {
			bestRank = rank
			ids = nil
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(ids) == 1 {
		return ids[0], nil
	}
	if len(ids) == 0 {
		return 0, &payeeNotFoundError{text: text}
	}
	return 0, fmt.Errorf("payee %q matches multiple payees; choose one from the list", text)
}

func (a *app) resolveFuzzyPayeeID(text string) (int, error) {
	text = normalizeSearchText(text)
	if text == "" {
		return 0, errors.New("payee is required")
	}
	rows, err := a.db.Query("SELECT PAYEEID, PAYEENAME, COALESCE(PATTERN, '') FROM PAYEE_V1")
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	bestID := 0
	bestScore := 0
	tie := false
	for rows.Next() {
		var id int
		var name, pattern string
		if err := rows.Scan(&id, &name, &pattern); err != nil {
			return 0, err
		}
		score := fuzzyTextScore(text, normalizeSearchText(name))
		for _, part := range strings.Split(pattern, "\n") {
			if s := fuzzyTextScore(text, normalizeSearchText(part)); s > score {
				score = s
			}
		}
		if score > bestScore {
			bestScore = score
			bestID = id
			tie = false
		} else if score == bestScore && score > 0 {
			tie = true
		}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if bestID > 0 && !tie && bestScore >= 82 {
		return bestID, nil
	}
	return 0, fmt.Errorf("payee %q was not found", text)
}

func (a *app) payeeName(id int) string {
	var name string
	if err := a.db.QueryRow("SELECT PAYEENAME FROM PAYEE_V1 WHERE PAYEEID = ?", id).Scan(&name); err != nil {
		return ""
	}
	return name
}

func (a *app) payeeCategoryID(id int) int {
	var categoryID int
	if err := a.db.QueryRow("SELECT COALESCE(CATEGID, 0) FROM PAYEE_V1 WHERE PAYEEID = ?", id).Scan(&categoryID); err != nil {
		return 0
	}
	return categoryID
}

func ensurePayeeInTx(tx *sql.Tx, name string, categoryID int) (int, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return 0, errors.New("payee is required")
	}
	var id int
	err := tx.QueryRow("SELECT PAYEEID FROM PAYEE_V1 WHERE PAYEENAME = ? COLLATE NOCASE", name).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	id, err = nextIntID(tx, "PAYEE_V1", "PAYEEID")
	if err != nil {
		return 0, err
	}
	var category any
	if categoryID > 0 {
		category = categoryID
	}
	if _, err := tx.Exec("INSERT INTO PAYEE_V1 (PAYEEID, PAYEENAME, CATEGID, NUMBER, WEBSITE, NOTES, ACTIVE, PATTERN) VALUES (?, ?, ?, '', '', '', 1, '')", id, name, category); err != nil {
		return 0, err
	}
	return id, nil
}

func appendPayeePatternInTx(tx *sql.Tx, payeeID int, raw string) error {
	pattern := generalizePayeePattern(raw)
	if pattern == "" {
		return nil
	}
	var existing string
	if err := tx.QueryRow("SELECT COALESCE(PATTERN, '') FROM PAYEE_V1 WHERE PAYEEID = ?", payeeID).Scan(&existing); err != nil {
		return err
	}
	var lines []string
	for _, part := range strings.Split(existing, "\n") {
		if p := strings.TrimSpace(part); p != "" {
			lines = append(lines, p)
		}
	}
	for _, line := range lines {
		if strings.EqualFold(line, pattern) {
			return nil
		}
	}
	// Merge with an existing entry for the same merchant — sharing >= 2
	// significant tokens — by collapsing both to their common core, so the
	// pattern generalizes as more statement variants are seen (e.g. the city or
	// store number that differs between visits drops out). A 2-token floor keeps
	// the core specific enough not to over-match. Otherwise keep it as a new
	// entry (distinct merchants under the same payee stay separate).
	merged := false
	for i, line := range lines {
		if common := sharedSignificantTokens(line, pattern); len(common) >= 2 {
			lines[i] = strings.Join(common, " ")
			merged = true
			break
		}
	}
	if !merged {
		lines = append(lines, pattern)
	}
	lines = dedupFold(lines)
	result, err := tx.Exec("UPDATE PAYEE_V1 SET PATTERN = ? WHERE PAYEEID = ?", strings.Join(lines, "\n"), payeeID)
	if err != nil {
		return err
	}
	return requireRowsAffected(result, "payee", payeeID)
}

// dedupFold removes case-insensitive duplicate entries, preserving first-seen
// order. A merge can leave two entries collapsed to the same core.
func dedupFold(items []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, it := range items {
		key := strings.ToLower(strings.TrimSpace(it))
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, it)
	}
	return out
}

func (a *app) insertPayee(name string, categoryID int, notes string, active bool) (int, error) {
	if !a.config.WriteEnabled {
		return 0, errors.New("write mode disabled")
	}
	if !a.payeeNameAvailable(0, name) {
		return 0, fmt.Errorf("payee name %q already exists", name)
	}
	if err := a.probeWriteLock(); err != nil {
		return 0, fmt.Errorf("database write lock unavailable: %w", err)
	}
	if _, err := a.backupDatabase("payee"); err != nil {
		return 0, fmt.Errorf("backup database: %w", err)
	}

	tx, err := a.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	id, err := nextIntID(tx, "PAYEE_V1", "PAYEEID")
	if err != nil {
		return 0, err
	}
	var category any
	if categoryID > 0 {
		category = categoryID
	}
	if _, err := tx.Exec("INSERT INTO PAYEE_V1 (PAYEEID, PAYEENAME, CATEGID, NUMBER, WEBSITE, NOTES, ACTIVE, PATTERN) VALUES (?, ?, ?, '', '', ?, ?, '')", id, name, category, notes, boolInt(active)); err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

func (a *app) updatePayeeRecord(id int, name string, categoryID int, notes string, active bool) error {
	if !a.config.WriteEnabled {
		return errors.New("write mode disabled")
	}
	if !a.payeeNameAvailable(id, name) {
		return fmt.Errorf("payee name %q already exists", name)
	}
	if err := a.probeWriteLock(); err != nil {
		return fmt.Errorf("database write lock unavailable: %w", err)
	}
	if _, err := a.backupDatabase("payee-update"); err != nil {
		return fmt.Errorf("backup database: %w", err)
	}
	var category any
	if categoryID > 0 {
		category = categoryID
	}
	result, err := a.db.Exec("UPDATE PAYEE_V1 SET PAYEENAME = ?, CATEGID = ?, NOTES = ?, ACTIVE = ? WHERE PAYEEID = ?", name, category, notes, boolInt(active), id)
	if err != nil {
		return err
	}
	return requireRowsAffected(result, "payee", id)
}

func (a *app) deleteUnusedPayee(id int) error {
	if !a.config.WriteEnabled {
		return errors.New("write mode disabled")
	}
	if n := a.countWhere("CHECKINGACCOUNT_V1", "PAYEEID = ?", id); n > 0 {
		return fmt.Errorf("payee is used in %d transactions", n)
	}
	if err := a.probeWriteLock(); err != nil {
		return fmt.Errorf("database write lock unavailable: %w", err)
	}
	if _, err := a.backupDatabase("payee-delete"); err != nil {
		return fmt.Errorf("backup database: %w", err)
	}
	result, err := a.db.Exec("DELETE FROM PAYEE_V1 WHERE PAYEEID = ?", id)
	if err != nil {
		return err
	}
	return requireRowsAffected(result, "payee", id)
}

func (a *app) listPayees() ([]payee, error) {
	rows, err := a.db.Query(`
SELECT p.PAYEEID, p.PAYEENAME, COALESCE(c.CATEGNAME, ''), COALESCE(c.CATEGID, 0), COALESCE(p.NOTES, ''), COALESCE(p.ACTIVE, 1),
       COUNT(t.TRANSID) AS TRANSACTION_COUNT
  FROM PAYEE_V1 p
  LEFT JOIN CATEGORY_V1 c ON c.CATEGID = p.CATEGID
  LEFT JOIN CHECKINGACCOUNT_V1 t ON t.PAYEEID = p.PAYEEID
 GROUP BY p.PAYEEID, p.PAYEENAME, c.CATEGNAME, c.CATEGID, p.NOTES, p.ACTIVE
 -- Binary collation sorts "*" ahead of digits and letters, so bucket payees
 -- cluster at the top of the list on their own. The nav draws a divider after
 -- the last one (see .entity-link.bucket in app.css).
 ORDER BY p.PAYEENAME`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var payees []payee
	for rows.Next() {
		var p payee
		if err := rows.Scan(&p.ID, &p.Name, &p.Category, &p.CategoryID, &p.Notes, &p.Active, &p.Count); err != nil {
			return nil, err
		}
		p.Bucket = isBucketPayee(p.Name)
		payees = append(payees, p)
	}
	return payees, rows.Err()
}
