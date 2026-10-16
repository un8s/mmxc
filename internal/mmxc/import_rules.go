package mmxc

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// importRuleRow is one learned import rule as the Review "Learned Mappings"
// modal shows it. Key identifies the rule for edit/delete: saveImportRulesInTx
// keeps at most one rule per (generalized pattern, account), so that pair is a
// stable handle that survives the list being reordered by a concurrent import.
type importRuleRow struct {
	Pattern     string
	AccountID   int
	AccountName string
	Type        string
	Payee       string
	Category    string
	Counterpart string // transfer counterparty account name
	Direction   string // "in" or "out" for transfers
	// Ambiguous marks a pattern-only tombstone: the same description meant
	// different transactions, so nothing is learned until it is deleted or filled in.
	Ambiguous bool
	// Scoped rules (transfers and their tombstones) belong to one reviewed
	// account and are delete-only here; their counterparty is chosen in Review.
	Scoped bool
}

func (a *app) listImportRuleRows() []importRuleRow {
	rules := a.loadImportRules()
	rows := make([]importRuleRow, 0, len(rules))
	for _, rule := range rules {
		row := importRuleRow{
			Pattern:   rule.Pattern,
			AccountID: rule.AccountID,
			Type:      rule.Type,
			Payee:     rule.Payee,
			Category:  rule.Category,
			Ambiguous: importRuleEmpty(rule),
			Scoped:    rule.AccountID != 0 || rule.Type == "Transfer",
		}
		if rule.AccountID != 0 {
			row.AccountName = a.accountName(rule.AccountID)
		}
		if rule.CounterpartyAccountID > 0 {
			row.Counterpart = a.accountName(rule.CounterpartyAccountID)
		}
		if rule.Type == "Transfer" {
			row.Direction = importRuleTransferDirection(rule)
		}
		rows = append(rows, row)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		return strings.ToLower(rows[i].Pattern) < strings.ToLower(rows[j].Pattern)
	})
	return rows
}

// mutateImportRules applies fn to the stored rules under the standard write
// dance (write mode, lock probe, backup, transaction).
func (a *app) mutateImportRules(reason string, fn func([]importRule) ([]importRule, error)) error {
	if !a.config.WriteEnabled {
		return errors.New("write mode disabled")
	}
	if err := a.probeWriteLock(); err != nil {
		return err
	}
	if _, err := a.backupDatabase(reason); err != nil {
		return err
	}
	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rules, err := loadImportRulesInTx(tx)
	if err != nil {
		return err
	}
	rules, err = fn(rules)
	if err != nil {
		return err
	}
	if err := storeImportRulesInTx(tx, rules); err != nil {
		return err
	}
	return tx.Commit()
}

func findImportRule(rules []importRule, pattern string, accountID int) int {
	for i, rule := range rules {
		if strings.EqualFold(strings.TrimSpace(rule.Pattern), strings.TrimSpace(pattern)) && rule.AccountID == accountID {
			return i
		}
	}
	return -1
}

// canonicalCategoryName resolves typed category text to the "Parent: Child"
// form candidates carry, so a rule can never name a category that doesn't exist.
func (a *app) canonicalCategoryName(text string) (string, error) {
	id, err := a.resolveCategoryID(text)
	if err != nil {
		return "", err
	}
	var name, parent string
	err = a.db.QueryRow(`
SELECT c.CATEGNAME, COALESCE(p.CATEGNAME, '')
  FROM CATEGORY_V1 c LEFT JOIN CATEGORY_V1 p ON p.CATEGID = c.PARENTID
 WHERE c.CATEGID = ?`, id).Scan(&name, &parent)
	if err != nil {
		return "", err
	}
	if parent != "" {
		return parent + ": " + name, nil
	}
	return name, nil
}

// saveImportRule adds a rule (empty keyPattern) or edits the account-independent
// rule identified by keyPattern. Editing replaces the whole mapping, which also
// turns an ambiguity tombstone back into a usable rule.
func (a *app) saveImportRule(keyPattern, pattern, typ, payee, category string) (string, error) {
	pattern = strings.ToLower(strings.Join(strings.Fields(pattern), " "))
	if normalizePayeePattern(pattern) == "" {
		return "", errors.New("pattern needs at least 3 characters")
	}
	if typ != "" && typ != "Withdrawal" && typ != "Deposit" {
		return "", errors.New("type must be Withdrawal or Deposit")
	}
	payee = strings.TrimSpace(payee)
	category = strings.TrimSpace(category)
	if category != "" {
		canonical, err := a.canonicalCategoryName(category)
		if err != nil {
			return "", err
		}
		category = canonical
	}
	if payee == "" && category == "" && typ == "" {
		return "", errors.New("a mapping needs a payee, a category or a type")
	}
	rule := importRule{Pattern: pattern, Type: typ, Payee: payee, Category: category}
	err := a.mutateImportRules("import-rule-save", func(rules []importRule) ([]importRule, error) {
		at := -1
		if keyPattern != "" {
			if at = findImportRule(rules, keyPattern, 0); at < 0 {
				return nil, errors.New("mapping no longer exists")
			}
		}
		if other := findImportRule(rules, pattern, 0); other >= 0 && other != at {
			return nil, fmt.Errorf("a mapping for %q already exists; edit that one", pattern)
		}
		if at >= 0 {
			rules[at] = rule
		} else {
			rules = append(rules, rule)
		}
		return rules, nil
	})
	return pattern, err
}

func (a *app) deleteImportRule(pattern string, accountID int) error {
	return a.mutateImportRules("import-rule-delete", func(rules []importRule) ([]importRule, error) {
		at := findImportRule(rules, pattern, accountID)
		if at < 0 {
			return nil, errors.New("mapping no longer exists")
		}
		return append(rules[:at], rules[at+1:]...), nil
	})
}

// importRuleRedirect returns to Review with the modal reopened, so several
// mappings can be fixed in one visit.
func importRuleRedirect(w http.ResponseWriter, r *http.Request) {
	accountID, _ := strconv.Atoi(r.FormValue("account_id"))
	http.Redirect(w, r, reviewURL(sanitizeMonth(r.FormValue("month")), accountID)+"&rules=1", http.StatusSeeOther)
}

func (a *app) saveImportRuleHandler(w http.ResponseWriter, r *http.Request) {
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
	pattern, err := a.saveImportRule(
		strings.TrimSpace(r.FormValue("key_pattern")), r.FormValue("pattern"),
		r.FormValue("type"), r.FormValue("payee"), r.FormValue("category"))
	if err != nil {
		a.httpError(w, err.Error(), http.StatusBadRequest)
		return
	}
	a.audit(r, auditEntry{
		Action: "import rule save",
		Detail: fmt.Sprintf("pattern=%q payee=%q category=%q", pattern, r.FormValue("payee"), r.FormValue("category")),
	})
	importRuleRedirect(w, r)
}

func (a *app) deleteImportRuleHandler(w http.ResponseWriter, r *http.Request) {
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
	pattern := strings.TrimSpace(r.FormValue("key_pattern"))
	ruleAccount, _ := strconv.Atoi(r.FormValue("key_account_id"))
	if pattern == "" {
		a.httpError(w, "pattern is required", http.StatusBadRequest)
		return
	}
	if err := a.deleteImportRule(pattern, ruleAccount); err != nil {
		a.httpError(w, err.Error(), http.StatusBadRequest)
		return
	}
	a.audit(r, auditEntry{
		Action: "import rule delete",
		Detail: fmt.Sprintf("pattern=%q", pattern),
	})
	importRuleRedirect(w, r)
}
