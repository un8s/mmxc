package mmxc

import (
	"path/filepath"
	"testing"
)

func TestImportRuleEditDeleteAdd(t *testing.T) {
	a, err := newApp(config{
		DBPath:       fixtureDB(t),
		WriteEnabled: true,
		BackupDir:    filepath.Join(t.TempDir(), "backups"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	if _, err := a.db.Exec(`INSERT INTO INFOTABLE_V1 (INFONAME, INFOVALUE) VALUES (?, ?)`,
		importRulesInfoName,
		`[{"pattern":"generic card purchase"},{"pattern":"wrong shop","type":"Withdrawal","payee":"Amazon","category":"Food: Groceries"}]`); err != nil {
		t.Fatal(err)
	}
	rows := a.listImportRuleRows()
	if len(rows) != 2 || !rows[0].Ambiguous || rows[1].Ambiguous {
		t.Fatalf("unexpected rows: %+v", rows)
	}

	// Edit: fix the category and rename the pattern; the tombstone stays put.
	if _, err := a.saveImportRule("wrong shop", "Right Shop", "Withdrawal", "Amazon", "Hobb"); err != nil {
		t.Fatal(err)
	}
	// A typo'd category is rejected rather than stored.
	if _, err := a.saveImportRule("right shop", "right shop", "", "Amazon", "No Such Category"); err == nil {
		t.Fatal("unknown category should be rejected")
	}
	// Filling in the tombstone teaches it; a duplicate pattern is refused.
	if _, err := a.saveImportRule("generic card purchase", "generic card purchase", "Withdrawal", "Amazon", "Hobb"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.saveImportRule("", "right shop", "", "Amazon", ""); err == nil {
		t.Fatal("duplicate pattern should be rejected")
	}
	if _, err := a.saveImportRule("", "ab", "", "Amazon", ""); err == nil {
		t.Fatal("too-short pattern should be rejected")
	}
	// Add.
	if _, err := a.saveImportRule("", "Corner Cafe", "", "Amazon", ""); err != nil {
		t.Fatal(err)
	}
	// Delete.
	if err := a.deleteImportRule("right shop", 0); err != nil {
		t.Fatal(err)
	}
	if err := a.deleteImportRule("right shop", 0); err == nil {
		t.Fatal("deleting a missing rule should fail")
	}

	rules := a.loadImportRules()
	if len(rules) != 2 {
		t.Fatalf("want 2 rules, got %+v", rules)
	}
	if rules[0].Category != "Leisure: Hobbies" || rules[0].Payee != "Amazon" {
		t.Fatalf("tombstone was not taught with the canonical category: %+v", rules[0])
	}
	if rules[1].Pattern != "corner cafe" {
		t.Fatalf("added rule missing: %+v", rules[1])
	}
}

func TestImportCreatesNewSubcategory(t *testing.T) {
	a, err := newApp(config{
		DBPath:       fixtureDB(t),
		WriteEnabled: true,
		BackupDir:    filepath.Join(t.TempDir(), "backups"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	validate := func(text string, reviewed bool) (importCandidate, []candidateIssue) {
		c := importCandidate{Date: "2025-12-05", Type: "Withdrawal", Amount: 9, Payee: "Amazon", Category: text, Reviewed: reviewed}
		issues := a.validateImportCandidateIssues(12, &c)
		return c, issues
	}
	hasError := func(issues []candidateIssue) bool {
		for _, is := range issues {
			if is.IsError {
				return true
			}
		}
		return false
	}

	// Parent exists, Sub does not: creatable, with the parent's own spelling.
	c, issues := validate("leisure: Board Games", true)
	if hasError(issues) || c.NewCategoryName != "Board Games" || c.Category != "Leisure: Board Games" || c.NewCategoryParentID != 3 {
		t.Fatalf("expected a creatable category, got %+v %+v", c, issues)
	}
	// Model output (not Reviewed) never creates categories.
	if _, issues := validate("Leisure: Board Games", false); !hasError(issues) {
		t.Fatal("unreviewed unknown category must stay an error")
	}
	// Missing parent, and an existing subcategory, are not creatable.
	if _, issues := validate("Nonexistent: Thing", true); !hasError(issues) {
		t.Fatal("unknown parent must be an error")
	}
	if c, issues := validate("Leisure: Hobbies", true); hasError(issues) || c.NewCategoryName != "" {
		t.Fatalf("existing category must resolve normally: %+v %+v", c, issues)
	}

	// Two rows naming the same new category share one created row.
	c, _ = validate("Leisure: Board Games", true)
	inputs := []transactionInput{
		{AccountID: 12, Date: "2025-12-05", Type: "Withdrawal", Amount: 9, PayeeID: 14, PayeeName: "Amazon", OriginalPayee: "Toy shop",
			CategoryName: c.Category, NewCategoryParentID: c.NewCategoryParentID, NewCategoryName: c.NewCategoryName},
		{AccountID: 12, Date: "2025-12-06", Type: "Withdrawal", Amount: 5, PayeeID: 14, PayeeName: "Amazon", OriginalPayee: "Toy shop",
			CategoryName: c.Category, NewCategoryParentID: c.NewCategoryParentID, NewCategoryName: c.NewCategoryName},
	}
	if _, err := a.insertImportedTransactions(inputs, "", "", 12); err != nil {
		t.Fatal(err)
	}
	if n := a.countWhere("CATEGORY_V1", "CATEGNAME = ? AND PARENTID = ?", "Board Games", 3); n != 1 {
		t.Fatalf("want exactly one new category, got %d", n)
	}
	if n := a.countWhere("CHECKINGACCOUNT_V1 t JOIN CATEGORY_V1 c ON c.CATEGID = t.CATEGID", "c.CATEGNAME = ?", "Board Games"); n != 2 {
		t.Fatalf("both transactions should use the new category, got %d", n)
	}
}
