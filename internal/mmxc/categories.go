package mmxc

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

type category struct {
	ID        int
	Name      string
	ParentID  int
	Parent    string
	Active    bool
	Count     int
	Depth     int
	CanDelete bool
}

func (a *app) categories(w http.ResponseWriter, r *http.Request) {
	cats, err := a.listCategories()
	if err != nil {
		a.renderError(w, "Categories", "categories", err)
		return
	}
	var selected *category
	if idText := r.URL.Query().Get("category_id"); idText != "" {
		id, _ := strconv.Atoi(idText)
		for i := range cats {
			if cats[i].ID == id {
				selected = &cats[i]
				break
			}
		}
	}
	if selected == nil {
		for i := range cats {
			if !a.config.HideInactiveItems || cats[i].Active {
				selected = &cats[i]
				break
			}
		}
	}
	filter := parseRecentTransactionFilter(r)
	var txs []transaction
	if selected != nil {
		filter.CategoryID = selected.ID
		filter.IncludeCategoryChildren = true
		txs, err = a.listTransactions(filter)
		if err != nil {
			a.renderError(w, "Categories", "categories", err)
			return
		}
	}
	accounts, err := a.listAccounts()
	if err != nil {
		a.renderError(w, "Categories", "categories", err)
		return
	}
	payees, err := a.listPayees()
	if err != nil {
		a.renderError(w, "Categories", "categories", err)
		return
	}
	var editTx *transaction
	if idText := r.URL.Query().Get("edit_trans_id"); idText != "" && a.config.WriteEnabled {
		if id, err := strconv.Atoi(idText); err == nil {
			if tx, err := a.getTransaction(id); err == nil {
				editTx = &tx
			}
		}
	}
	a.render(w, "categories", pageData{
		Title:              "Categories",
		Active:             "categories",
		Info:               a.info(),
		Accounts:           accounts,
		Categories:         cats,
		Payees:             payees,
		EditTransaction:    editTx,
		Scroll:             scrollParam(r.URL.Query().Get("scroll")),
		ReturnTo:           localReturnURL(r),
		SelectedCategory:   selected,
		Transactions:       txs,
		TransactionSummary: summarizeTransactions(txs),
		Filter:             filter,
	})
}

func (a *app) createCategory(w http.ResponseWriter, r *http.Request) {
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
		a.httpError(w, "category name is required", http.StatusBadRequest)
		return
	}
	parentID := -1
	if parentText := strings.TrimSpace(r.FormValue("parent_text")); parentText != "" {
		id, err := a.resolveCategoryIDWithTopLevel(parentText, true)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if !a.categoryActive(id) {
			a.httpError(w, "parent category is inactive; choose an active category", http.StatusBadRequest)
			return
		}
		parentID = id
	}
	if !a.categoryNameAvailable(0, name, parentID) {
		a.httpError(w, "category name already exists under that parent", http.StatusBadRequest)
		return
	}
	id, err := a.insertCategory(name, parentID, formBoolDefault(r, "active", true))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	a.audit(r, auditEntry{
		Action: fmt.Sprintf("category add id=%d parent_id=%d active=%t", id, parentID, formBoolDefault(r, "active", true)),
		Detail: fmt.Sprintf("name=%q", name),
	})
	http.Redirect(w, r, "/categories?category_id="+strconv.Itoa(id), http.StatusSeeOther)
}

func (a *app) updateCategory(w http.ResponseWriter, r *http.Request) {
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
	id, err := strconv.Atoi(r.FormValue("category_id"))
	if err != nil || id <= 0 {
		a.httpError(w, "category id is required", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		a.httpError(w, "category name is required", http.StatusBadRequest)
		return
	}
	parentID := -1
	if parentText := strings.TrimSpace(r.FormValue("parent_text")); parentText != "" {
		parentID, err = a.resolveCategoryIDWithTopLevel(parentText, true)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if parentID == id {
			a.httpError(w, "category cannot be its own parent", http.StatusBadRequest)
			return
		}
		if a.categoryParentCreatesLoop(id, parentID) {
			a.httpError(w, "category parent would create a hierarchy loop", http.StatusBadRequest)
			return
		}
		if !a.categoryActive(parentID) && parentID != a.categoryParentID(id) {
			a.httpError(w, "parent category is inactive; choose an active category", http.StatusBadRequest)
			return
		}
	}
	if !a.categoryNameAvailable(id, name, parentID) {
		a.httpError(w, "category name already exists under that parent", http.StatusBadRequest)
		return
	}
	oldName := a.categoryName(id)
	oldActive := a.categoryActive(id)
	active := formBoolDefault(r, "active", false)
	if err := a.updateCategoryRecord(id, name, parentID, active); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if oldName != "" && oldName != name {
		a.audit(r, auditEntry{
			Action: fmt.Sprintf("category rename id=%d", id),
			Detail: fmt.Sprintf("old_name=%q new_name=%q", oldName, name),
		})
	}
	if oldActive != active {
		action := "deactivate"
		if active {
			action = "activate"
		}
		a.audit(r, auditEntry{
			Action: fmt.Sprintf("category %s id=%d", action, id),
			Detail: fmt.Sprintf("name=%q", name),
		})
	}
	http.Redirect(w, r, "/categories?category_id="+strconv.Itoa(id), http.StatusSeeOther)
}

func (a *app) deleteCategory(w http.ResponseWriter, r *http.Request) {
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
	id, err := strconv.Atoi(r.FormValue("category_id"))
	if err != nil || id <= 0 {
		a.httpError(w, "category id is required", http.StatusBadRequest)
		return
	}
	name := a.categoryName(id)
	if err := a.deleteUnusedCategory(id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	a.audit(r, auditEntry{
		Action: fmt.Sprintf("category delete id=%d", id),
		Detail: fmt.Sprintf("name=%q", name),
	})
	http.Redirect(w, r, "/categories", http.StatusSeeOther)
}

// categoryNotFoundError is resolveCategoryID's "no such category" failure, kept
// distinct from an ambiguous or top-level match so the import form can offer to
// create a new "Parent: Sub" category only when nothing matched at all.
type categoryNotFoundError struct{ text string }

func (e *categoryNotFoundError) Error() string {
	return fmt.Sprintf("category %q was not found", e.text)
}

// newCategoryFor reports whether text names a category that does not exist yet
// but could be created: "Parent: Sub" where Parent is an existing, active
// top-level category and Sub is not yet used under it. canonical is the
// spelling to store (the parent's real capitalisation).
func (a *app) newCategoryFor(text string) (parentID int, name, canonical string, ok bool) {
	parentText, sub, found := strings.Cut(text, ":")
	parentText, sub = strings.TrimSpace(parentText), strings.TrimSpace(sub)
	if !found || parentText == "" || sub == "" || strings.Contains(sub, ":") {
		return 0, "", "", false
	}
	var parentName string
	err := a.db.QueryRow(`
SELECT CATEGID, CATEGNAME
  FROM CATEGORY_V1
 WHERE CATEGNAME = ? COLLATE NOCASE
   AND COALESCE(PARENTID, -1) <= 0
   AND COALESCE(ACTIVE, 1) <> 0`, parentText).Scan(&parentID, &parentName)
	if err != nil {
		return 0, "", "", false
	}
	if transferID, err := a.transferCategoryID(); err == nil && parentID == transferID {
		return 0, "", "", false
	}
	if !a.categoryNameAvailable(0, sub, parentID) {
		return 0, "", "", false
	}
	return parentID, sub, parentName + ": " + sub, true
}

// ensureCategoryInTx returns the subcategory name under parentID, creating it
// when an earlier row of the same import has not already.
func ensureCategoryInTx(tx *sql.Tx, parentID int, name string) (int, error) {
	var id int
	err := tx.QueryRow("SELECT CATEGID FROM CATEGORY_V1 WHERE CATEGNAME = ? COLLATE NOCASE AND COALESCE(PARENTID, -1) = ?", name, parentID).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	id, err = nextIntID(tx, "CATEGORY_V1", "CATEGID")
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec("INSERT INTO CATEGORY_V1 (CATEGID, CATEGNAME, ACTIVE, PARENTID) VALUES (?, ?, 1, ?)", id, name, parentID); err != nil {
		return 0, err
	}
	return id, nil
}

func (a *app) resolveCategoryID(text string) (int, error) {
	return a.resolveCategoryIDWithTopLevel(text, a.config.AllowTopLevelCategories)
}

func (a *app) resolveCategoryIDWithTopLevel(text string, allowTopLevel bool) (int, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return 0, errors.New("category is required")
	}
	rows, err := a.db.Query(`
SELECT c.CATEGID, COALESCE(c.PARENTID, 0),
       CASE
         WHEN COALESCE(parent.CATEGNAME || ': ', '') || c.CATEGNAME = ? THEN 0
         WHEN c.CATEGNAME = ? THEN 1
         WHEN c.CATEGNAME LIKE ? THEN 2
         ELSE 3
       END AS MATCH_RANK
  FROM CATEGORY_V1 c
  LEFT JOIN CATEGORY_V1 parent ON parent.CATEGID = c.PARENTID
 WHERE c.CATEGNAME = ?
    OR COALESCE(parent.CATEGNAME || ': ', '') || c.CATEGNAME = ?
    OR c.CATEGNAME LIKE ?
    OR COALESCE(parent.CATEGNAME || ': ', '') || c.CATEGNAME LIKE ?
 ORDER BY CASE
   WHEN COALESCE(parent.CATEGNAME || ': ', '') || c.CATEGNAME = ? THEN 0
   WHEN c.CATEGNAME = ? THEN 1
   ELSE 2
 END, COALESCE(parent.CATEGNAME || ': ', '') || c.CATEGNAME
 LIMIT 10`, text, text, "%"+text+"%", text, text, "%"+text+"%", "%"+text+"%", text, text)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	var ids []int
	topLevelMatched := false
	bestRank := 99
	for rows.Next() {
		var id, parentID, rank int
		if err := rows.Scan(&id, &parentID, &rank); err != nil {
			return 0, err
		}
		if rank > bestRank {
			continue
		}
		if rank < bestRank {
			bestRank = rank
			ids = nil
			topLevelMatched = false
		}
		if parentID <= 0 && !allowTopLevel {
			topLevelMatched = true
			continue
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
		if topLevelMatched {
			return 0, fmt.Errorf("category %q is top-level; choose a subcategory", text)
		}
		return 0, &categoryNotFoundError{text: text}
	}
	return 0, fmt.Errorf("category %q matches multiple categories; choose one from the list", text)
}

func (a *app) resolveFuzzyCategoryID(text string) (int, error) {
	text = normalizeSearchText(text)
	if text == "" {
		return 0, errors.New("category is required")
	}
	rows, err := a.db.Query(`
SELECT c.CATEGID, COALESCE(c.PARENTID, 0), c.CATEGNAME,
       COALESCE(parent.CATEGNAME || ': ', '') || c.CATEGNAME AS FULL_NAME
  FROM CATEGORY_V1 c
  LEFT JOIN CATEGORY_V1 parent ON parent.CATEGID = c.PARENTID`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	bestID := 0
	bestScore := 0
	tie := false
	topLevelMatched := false
	for rows.Next() {
		var id, parentID int
		var name, fullName string
		if err := rows.Scan(&id, &parentID, &name, &fullName); err != nil {
			return 0, err
		}
		score := fuzzyTextScore(text, normalizeSearchText(fullName))
		if s := fuzzyTextScore(text, normalizeSearchText(name)); s > score {
			score = s
		}
		if parentID <= 0 && !a.config.AllowTopLevelCategories {
			if score >= 82 {
				topLevelMatched = true
			}
			continue
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
	if topLevelMatched {
		return 0, fmt.Errorf("category %q is top-level; choose a subcategory", text)
	}
	return 0, fmt.Errorf("category %q was not found", text)
}

func (a *app) transferCategoryID() (int, error) {
	var id int
	err := a.db.QueryRow("SELECT CATEGID FROM CATEGORY_V1 WHERE CATEGNAME = 'Transfer' AND COALESCE(PARENTID, -1) <= 0 LIMIT 1").Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, errors.New("MMEX Transfer category was not found")
	}
	return id, err
}

func (a *app) categoryParentID(id int) int {
	var parentID int
	if err := a.db.QueryRow("SELECT COALESCE(PARENTID, -1) FROM CATEGORY_V1 WHERE CATEGID = ?", id).Scan(&parentID); err != nil {
		return -1
	}
	return parentID
}

func (a *app) categoryName(id int) string {
	var name string
	if err := a.db.QueryRow("SELECT CATEGNAME FROM CATEGORY_V1 WHERE CATEGID = ?", id).Scan(&name); err != nil {
		return ""
	}
	return name
}

func (a *app) insertCategory(name string, parentID int, active bool) (int, error) {
	if !a.config.WriteEnabled {
		return 0, errors.New("write mode disabled")
	}
	if !a.categoryNameAvailable(0, name, parentID) {
		return 0, fmt.Errorf("category name %q already exists under that parent", name)
	}
	if err := a.probeWriteLock(); err != nil {
		return 0, fmt.Errorf("database write lock unavailable: %w", err)
	}
	if _, err := a.backupDatabase("category"); err != nil {
		return 0, fmt.Errorf("backup database: %w", err)
	}

	tx, err := a.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	id, err := nextIntID(tx, "CATEGORY_V1", "CATEGID")
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec("INSERT INTO CATEGORY_V1 (CATEGID, CATEGNAME, ACTIVE, PARENTID) VALUES (?, ?, ?, ?)", id, name, boolInt(active), parentID); err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

func (a *app) updateCategoryRecord(id int, name string, parentID int, active bool) error {
	if !a.config.WriteEnabled {
		return errors.New("write mode disabled")
	}
	if !a.categoryNameAvailable(id, name, parentID) {
		return fmt.Errorf("category name %q already exists under that parent", name)
	}
	if err := a.probeWriteLock(); err != nil {
		return fmt.Errorf("database write lock unavailable: %w", err)
	}
	if _, err := a.backupDatabase("category-update"); err != nil {
		return fmt.Errorf("backup database: %w", err)
	}
	result, err := a.db.Exec("UPDATE CATEGORY_V1 SET CATEGNAME = ?, PARENTID = ?, ACTIVE = ? WHERE CATEGID = ?", name, parentID, boolInt(active), id)
	if err != nil {
		return err
	}
	return requireRowsAffected(result, "category", id)
}

func (a *app) deleteUnusedCategory(id int) error {
	if !a.config.WriteEnabled {
		return errors.New("write mode disabled")
	}
	if n := a.countWhere("CATEGORY_V1", "CATEGID = ? AND COALESCE(PARENTID, -1) <= 0", id); n > 0 {
		return errors.New("top-level categories cannot be deleted")
	}
	if n := a.countWhere("CHECKINGACCOUNT_V1", "CATEGID = ?", id); n > 0 {
		return fmt.Errorf("category is used in %d transactions", n)
	}
	if n := a.countWhere("SPLITTRANSACTIONS_V1", "CATEGID = ?", id); n > 0 {
		return fmt.Errorf("category is used in %d split transactions", n)
	}
	if n := a.countWhere("BUDGETTABLE_V1", "CATEGID = ?", id); n > 0 {
		return fmt.Errorf("category is used in %d budget entries", n)
	}
	if n := a.countWhere("CATEGORY_V1", "PARENTID = ?", id); n > 0 {
		return fmt.Errorf("category has %d child categories", n)
	}
	if err := a.probeWriteLock(); err != nil {
		return fmt.Errorf("database write lock unavailable: %w", err)
	}
	if _, err := a.backupDatabase("category-delete"); err != nil {
		return fmt.Errorf("backup database: %w", err)
	}
	result, err := a.db.Exec("DELETE FROM CATEGORY_V1 WHERE CATEGID = ?", id)
	if err != nil {
		return err
	}
	return requireRowsAffected(result, "category", id)
}

func (a *app) listCategories() ([]category, error) {
	rows, err := a.db.Query(`
SELECT c.CATEGID, c.CATEGNAME, COALESCE(c.PARENTID, 0), COALESCE(parent.CATEGNAME, ''), COALESCE(c.ACTIVE, 1),
       COUNT(DISTINCT t.TRANSID) + COUNT(DISTINCT st.TRANSID) AS TRANSACTION_COUNT
  FROM CATEGORY_V1 c
  LEFT JOIN CATEGORY_V1 parent ON parent.CATEGID = c.PARENTID
  LEFT JOIN CHECKINGACCOUNT_V1 t ON t.CATEGID = c.CATEGID
  LEFT JOIN SPLITTRANSACTIONS_V1 st ON st.CATEGID = c.CATEGID
 GROUP BY c.CATEGID, c.CATEGNAME, c.PARENTID, parent.CATEGNAME, c.ACTIVE
 ORDER BY c.CATEGNAME`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var cats []category
	for rows.Next() {
		var c category
		if err := rows.Scan(&c.ID, &c.Name, &c.ParentID, &c.Parent, &c.Active, &c.Count); err != nil {
			return nil, err
		}
		if c.ParentID < 0 {
			c.ParentID = 0
			c.Parent = ""
		}
		c.CanDelete = a.categoryCanDelete(c.ID, c.ParentID, c.Count)
		cats = append(cats, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return categoryTree(cats), nil
}

func (a *app) categoryCanDelete(id, parentID, transactionCount int) bool {
	if parentID <= 0 || transactionCount > 0 {
		return false
	}
	if a.countWhere("BUDGETTABLE_V1", "CATEGID = ?", id) > 0 {
		return false
	}
	if a.countWhere("CATEGORY_V1", "PARENTID = ?", id) > 0 {
		return false
	}
	return true
}

func categoryTree(cats []category) []category {
	byParent := map[int][]category{}
	for _, cat := range cats {
		byParent[cat.ParentID] = append(byParent[cat.ParentID], cat)
	}

	var ordered []category
	seen := map[int]bool{}
	var walk func(parentID, depth int)
	walk = func(parentID, depth int) {
		for _, cat := range byParent[parentID] {
			if seen[cat.ID] {
				continue
			}
			seen[cat.ID] = true
			cat.Depth = depth
			ordered = append(ordered, cat)
			walk(cat.ID, depth+1)
		}
	}
	walk(0, 0)
	for _, cat := range cats {
		if !seen[cat.ID] {
			cat.Depth = 0
			ordered = append(ordered, cat)
		}
	}
	return ordered
}
