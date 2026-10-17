package mmxc

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"math"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestWriteModeSafetyOnTempDatabase(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.mmb")
	buildSyntheticDB(t, dbPath)

	a, err := newApp(config{
		DBPath:       dbPath,
		WriteEnabled: true,
		BackupDir:    filepath.Join(dir, "backups"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	if err := a.probeWriteLock(); err != nil {
		t.Fatalf("probe write lock: %v", err)
	}

	backupPath, err := a.backupDatabase("test")
	if err != nil {
		t.Fatalf("backup database: %v", err)
	}
	if _, err := os.Stat(backupPath); err != nil {
		t.Fatalf("backup stat: %v", err)
	}

	before := countRows(t, a, "CHECKINGACCOUNT_V1")
	if err := a.insertManualTransaction(transactionInput{
		AccountID:  12,
		Date:       "2025-10-31",
		Type:       "Withdrawal",
		Amount:     12.34,
		PayeeID:    1,
		CategoryID: 1,
		Status:     "",
		Notes:      "mmxc test transaction",
	}); err != nil {
		t.Fatalf("insert manual transaction: %v", err)
	}
	after := countRows(t, a, "CHECKINGACCOUNT_V1")
	if after != before+1 {
		t.Fatalf("transaction count after insert = %d, want %d", after, before+1)
	}

	if err := a.insertManualTransaction(transactionInput{
		AccountID:   2,
		ToAccountID: 12,
		Date:        "2025-10-31",
		Type:        "Transfer",
		Amount:      56.78,
		PayeeID:     -1,
		CategoryID:  16,
		Status:      "",
		Notes:       "mmxc test transfer",
	}); err != nil {
		t.Fatalf("insert transfer transaction: %v", err)
	}
	afterTransfer := countRows(t, a, "CHECKINGACCOUNT_V1")
	if afterTransfer != before+2 {
		t.Fatalf("transaction count after transfer = %d, want %d", afterTransfer, before+2)
	}

	var transID int64
	if err := a.db.QueryRow("SELECT TRANSID FROM CHECKINGACCOUNT_V1 WHERE NOTES = 'mmxc test transfer'").Scan(&transID); err != nil {
		t.Fatal(err)
	}
	if err := a.markTransactionVoid(transID); err != nil {
		t.Fatalf("void transaction: %v", err)
	}
	var status string
	if err := a.db.QueryRow("SELECT STATUS FROM CHECKINGACCOUNT_V1 WHERE TRANSID = ?", transID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "Void" {
		t.Fatalf("voided transaction status = %q, want Void", status)
	}

	categoryID, err := a.insertCategory("MMXC Test Subcategory", 3, true)
	if err != nil {
		t.Fatalf("insert category: %v", err)
	}
	var categoryParent int
	if err := a.db.QueryRow("SELECT PARENTID FROM CATEGORY_V1 WHERE CATEGID = ?", categoryID).Scan(&categoryParent); err != nil {
		t.Fatal(err)
	}
	if categoryParent != 3 {
		t.Fatalf("category parent = %d, want 3", categoryParent)
	}

	payeeID, err := a.insertPayee("MMXC Test Payee", 66, "created by test", true)
	if err != nil {
		t.Fatalf("insert payee: %v", err)
	}
	var payeeCategory int
	if err := a.db.QueryRow("SELECT CATEGID FROM PAYEE_V1 WHERE PAYEEID = ?", payeeID).Scan(&payeeCategory); err != nil {
		t.Fatal(err)
	}
	if payeeCategory != 66 {
		t.Fatalf("payee category = %d, want 66", payeeCategory)
	}

	if err := a.updateCategoryRecord(categoryID, "MMXC Test Subcategory Updated", 3, false); err != nil {
		t.Fatalf("update category: %v", err)
	}
	var categoryName string
	var categoryActive int
	if err := a.db.QueryRow("SELECT CATEGNAME, ACTIVE FROM CATEGORY_V1 WHERE CATEGID = ?", categoryID).Scan(&categoryName, &categoryActive); err != nil {
		t.Fatal(err)
	}
	if categoryName != "MMXC Test Subcategory Updated" {
		t.Fatalf("category name = %q", categoryName)
	}
	if categoryActive != 0 {
		t.Fatalf("category active = %d, want 0", categoryActive)
	}

	if err := a.updatePayeeRecord(payeeID, "MMXC Test Payee Updated", 66, "updated by test", false); err != nil {
		t.Fatalf("update payee: %v", err)
	}
	var payeeName string
	var payeeNotes string
	var payeeActive int
	if err := a.db.QueryRow("SELECT PAYEENAME, NOTES, ACTIVE FROM PAYEE_V1 WHERE PAYEEID = ?", payeeID).Scan(&payeeName, &payeeNotes, &payeeActive); err != nil {
		t.Fatal(err)
	}
	if payeeName != "MMXC Test Payee Updated" {
		t.Fatalf("payee name = %q", payeeName)
	}
	if payeeNotes != "updated by test" {
		t.Fatalf("payee notes = %q", payeeNotes)
	}
	if payeeActive != 0 {
		t.Fatalf("payee active = %d, want 0", payeeActive)
	}
	if _, err := a.insertPayee("MMXC Test Payee Updated", 66, "duplicate", true); err == nil {
		t.Fatal("expected duplicate payee name to fail")
	}

	if err := a.deleteUnusedPayee(1); err == nil {
		t.Fatal("expected used payee deletion to fail")
	}
	if err := a.deleteUnusedCategory(3); err == nil {
		t.Fatal("expected top-level category deletion to fail")
	}
	if err := a.deleteUnusedCategory(66); err == nil {
		t.Fatal("expected used category deletion to fail")
	}
	if err := a.deleteUnusedPayee(payeeID); err != nil {
		t.Fatalf("delete unused payee: %v", err)
	}
	if err := a.deleteUnusedCategory(categoryID); err != nil {
		t.Fatalf("delete unused category: %v", err)
	}
}

func TestUpdateTransactionOnTempDatabase(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.mmb")
	buildSyntheticDB(t, dbPath)

	a, err := newApp(config{
		DBPath:       dbPath,
		WriteEnabled: true,
		BackupDir:    filepath.Join(dir, "backups"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	if err := a.insertManualTransaction(transactionInput{
		AccountID:  12,
		Date:       "2025-10-31",
		Type:       "Withdrawal",
		Amount:     12.34,
		PayeeID:    1,
		CategoryID: 1,
		Notes:      "mmxc edit seed",
	}); err != nil {
		t.Fatalf("insert manual transaction: %v", err)
	}

	var transID int64
	if err := a.db.QueryRow("SELECT TRANSID FROM CHECKINGACCOUNT_V1 WHERE NOTES = 'mmxc edit seed'").Scan(&transID); err != nil {
		t.Fatal(err)
	}

	if err := a.updateTransactionRecord(transID, transactionInput{
		AccountID:  12,
		Date:       "2025-11-15",
		Type:       "Deposit",
		Amount:     99.99,
		PayeeID:    14,
		CategoryID: 66,
		Status:     "Reconciled",
		Notes:      "mmxc edit updated",
	}); err != nil {
		t.Fatalf("update transaction: %v", err)
	}

	got, err := a.getTransaction(int(transID))
	if err != nil {
		t.Fatalf("get transaction: %v", err)
	}
	if got.Type != "Deposit" || got.Amount != 99.99 || got.Date != "2025-11-15" ||
		got.Status != "Reconciled" || got.PayeeID != 14 || got.CategoryID != 66 ||
		got.Notes != "mmxc edit updated" {
		t.Fatalf("unexpected transaction after update: %+v", got)
	}

	if err := a.updateTransactionRecord(999999999, transactionInput{
		AccountID:  12,
		Date:       "2025-11-15",
		Type:       "Deposit",
		Amount:     1,
		PayeeID:    14,
		CategoryID: 66,
	}); err == nil {
		t.Fatal("expected update of missing transaction to fail")
	}
}

func TestSummarizeTransactions(t *testing.T) {
	got := summarizeTransactions([]transaction{
		{Type: "Withdrawal", Amount: 12.34},
		{Type: "Deposit", Amount: 50.00},
		{Type: "Transfer", Amount: 99.00},
		{Type: "Withdrawal", Amount: 7.66},
	})
	if got.Count != 4 || got.Withdrawals != 20 || got.Deposits != 50 || got.Net != 30 {
		t.Fatalf("summary = count %d withdrawals %.2f deposits %.2f net %.2f, want 4/20/50/30", got.Count, got.Withdrawals, got.Deposits, got.Net)
	}
}

func TestCreateAndUpdateAccountOnTempDatabase(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.mmb")
	buildSyntheticDB(t, dbPath)

	a, err := newApp(config{
		DBPath:       dbPath,
		WriteEnabled: true,
		BackupDir:    filepath.Join(dir, "backups"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	before := countRows(t, a, "ACCOUNTLIST_V1")
	accountID, err := a.insertAccount(accountInput{
		Name:           "MMXC Test Account",
		Type:           "Checking",
		Status:         "Open",
		AccountNum:     "1234",
		Notes:          "created by test",
		HeldAt:         "Test Bank",
		Website:        "https://example.test",
		ContactInfo:    "contact",
		AccessInfo:     "access",
		Initial:        123.45,
		CurrencyID:     35,
		Favorite:       true,
		StatementDate:  "2025-10-31",
		MinimumBalance: 10,
		CreditLimit:    1000,
		InterestRate:   1.25,
		PaymentDueDate: "2025-11-15",
		MinimumPayment: 25,
		InitialDate:    "2025-01-01",
	})
	if err != nil {
		t.Fatalf("insert account: %v", err)
	}
	after := countRows(t, a, "ACCOUNTLIST_V1")
	if after != before+1 {
		t.Fatalf("account count after insert = %d, want %d", after, before+1)
	}

	var name, typ, status, favorite string
	var initial float64
	if err := a.db.QueryRow("SELECT ACCOUNTNAME, ACCOUNTTYPE, STATUS, INITIALBAL, FAVORITEACCT FROM ACCOUNTLIST_V1 WHERE ACCOUNTID = ?", accountID).Scan(&name, &typ, &status, &initial, &favorite); err != nil {
		t.Fatal(err)
	}
	if name != "MMXC Test Account" || typ != "Checking" || status != "Open" || initial != 123.45 || favorite != "TRUE" {
		t.Fatalf("unexpected account after insert: name=%q type=%q status=%q initial=%v favorite=%q", name, typ, status, initial, favorite)
	}

	if err := a.updateAccountRecord(accountID, accountInput{
		Name:       "MMXC Test Account Updated",
		Type:       "Credit Card",
		Status:     "Closed",
		Initial:    -50,
		CurrencyID: 35,
		Favorite:   false,
		Notes:      "updated by test",
	}); err != nil {
		t.Fatalf("update account: %v", err)
	}
	if err := a.db.QueryRow("SELECT ACCOUNTNAME, ACCOUNTTYPE, STATUS, INITIALBAL, FAVORITEACCT FROM ACCOUNTLIST_V1 WHERE ACCOUNTID = ?", accountID).Scan(&name, &typ, &status, &initial, &favorite); err != nil {
		t.Fatal(err)
	}
	if name != "MMXC Test Account Updated" || typ != "Credit Card" || status != "Closed" || initial != -50 || favorite != "FALSE" {
		t.Fatalf("unexpected account after update: name=%q type=%q status=%q initial=%v favorite=%q", name, typ, status, initial, favorite)
	}

	if _, err := a.insertAccount(accountInput{
		Name:       "MMXC Test Account Updated",
		Type:       "Checking",
		Status:     "Open",
		CurrencyID: 35,
	}); err == nil {
		t.Fatal("expected duplicate account name to fail")
	}

	if err := a.updateAccountRecord(999999999, accountInput{
		Name:       "Missing",
		Type:       "Checking",
		Status:     "Open",
		CurrencyID: 35,
	}); err == nil {
		t.Fatal("expected update of missing account to fail")
	}
}

func TestAccountListIncludesClosedAndCurrencyDefault(t *testing.T) {
	a, err := newApp(config{
		DBPath:       fixtureDB(t),
		WriteEnabled: false,
		BackupDir:    "backups",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	accounts, err := a.listAccounts()
	if err != nil {
		t.Fatal(err)
	}
	foundClosed := false
	for _, account := range accounts {
		if account.Status == "Closed" {
			foundClosed = true
			break
		}
	}
	if !foundClosed {
		t.Fatal("closed account was not returned")
	}

	currencies, err := a.listCurrencies()
	if err != nil {
		t.Fatal(err)
	}
	if len(currencies) == 0 || currencies[0].ID != 35 {
		t.Fatalf("first currency = %+v, want base currency id 35", currencies)
	}
}

func TestUnsupportedAccountTypesAreReadOnly(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.mmb")
	buildSyntheticDB(t, dbPath)

	a, err := newApp(config{
		DBPath:       dbPath,
		WriteEnabled: true,
		BackupDir:    filepath.Join(dir, "backups"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	const accountID = 900001
	const transID = 900002
	if _, err := a.db.Exec(`
INSERT INTO ACCOUNTLIST_V1
  (ACCOUNTID, ACCOUNTNAME, ACCOUNTTYPE, STATUS, INITIALBAL, FAVORITEACCT, CURRENCYID)
VALUES (?, 'MMXC Investment Test', 'Investment', 'Open', 0, 'FALSE', 35)`, accountID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`
INSERT INTO CHECKINGACCOUNT_V1
  (TRANSID, ACCOUNTID, PAYEEID, TRANSCODE, TRANSAMOUNT, STATUS,
   TRANSACTIONNUMBER, NOTES, CATEGID, TRANSDATE, FOLLOWUPID, LASTUPDATEDTIME, DELETEDTIME, COLOR)
VALUES (?, ?, 14, 'Withdrawal', 1, '', '', 'unsupported account transaction', 66, '2025-11-15T00:00:00', NULL, '2025-11-15T00:00:00', NULL, -1)`,
		transID, accountID); err != nil {
		t.Fatal(err)
	}

	accounts, err := a.listAccounts()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, account := range accounts {
		if account.ID == accountID {
			found = true
			if account.Editable {
				t.Fatal("unsupported investment account was marked editable")
			}
		}
	}
	if !found {
		t.Fatal("unsupported investment account not listed")
	}

	if err := a.updateAccountRecord(accountID, accountInput{
		Name:       "MMXC Investment Test Updated",
		Type:       "Checking",
		Status:     "Open",
		CurrencyID: 35,
	}); err == nil {
		t.Fatal("expected unsupported account update to fail")
	}

	if _, err := a.parseTransactionInput(formRequest(url.Values{
		"account_id":  {strconv.Itoa(accountID)},
		"date":        {"2025-11-15"},
		"type":        {"Withdrawal"},
		"amount":      {"1.00"},
		"payee_id":    {"14"},
		"category_id": {"66"},
	}), false); err == nil {
		t.Fatal("expected transaction input for unsupported account to fail")
	}

	tx, err := a.getTransaction(transID)
	if err != nil {
		t.Fatal(err)
	}
	if tx.Editable {
		t.Fatal("unsupported account transaction was marked editable")
	}
	if err := a.markTransactionVoid(transID); err == nil {
		t.Fatal("expected voiding unsupported account transaction to fail")
	}
}

func TestReadOnlyModeRejectsWriteHelpers(t *testing.T) {
	a, err := newApp(config{
		DBPath:       fixtureDB(t),
		WriteEnabled: false,
		BackupDir:    "backups",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	if err := a.probeWriteLock(); err == nil {
		t.Fatal("expected write-lock probe to fail in read-only mode")
	}
	if _, err := a.backupDatabase("test"); err == nil {
		t.Fatal("expected backup helper to fail in read-only mode")
	}
}

func TestCompatibilityReportOnFixture(t *testing.T) {
	a, err := newApp(config{
		DBPath:       fixtureDB(t),
		WriteEnabled: false,
		BackupDir:    "backups",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	if !a.compat.OK {
		t.Fatalf("fixture compatibility failed: %s", a.compat.Details)
	}
	info := a.info()
	if info.Status != "MMEX schema compatible" || info.CompatibilityDetail == "" {
		t.Fatalf("unexpected compatibility info: %+v", info)
	}
}

func TestIncompatibleSchemaBlocksWriteMode(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "bad.mmb")
	db, err := openSQLite(dbPath, "rwc", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE INFOTABLE_V1 (INFONAME TEXT, INFOVALUE TEXT)"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	readOnly, err := newApp(config{
		DBPath:       dbPath,
		WriteEnabled: false,
		BackupDir:    filepath.Join(dir, "backups"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if readOnly.compat.OK {
		t.Fatal("expected incomplete schema to be incompatible")
	}
	if !strings.Contains(readOnly.compat.Details, "ACCOUNTLIST_V1") {
		t.Fatalf("expected missing table detail, got %q", readOnly.compat.Details)
	}
	readOnly.db.Close()

	if _, err := newApp(config{
		DBPath:       dbPath,
		WriteEnabled: true,
		BackupDir:    filepath.Join(dir, "backups"),
	}); err == nil {
		t.Fatal("expected incompatible schema to block write mode")
	}
}

// The payee datalists carry each payee's default category so app.js can fill an
// empty Category when a payee is selected; a payee without one carries nothing.
func TestPayeeDatalistCarriesDefaultCategory(t *testing.T) {
	a, err := newApp(config{
		DBPath:       fixtureDB(t),
		WriteEnabled: false,
		BackupDir:    "backups",
	})
	if err != nil {
		t.Fatalf("newApp: %v", err)
	}
	defer a.db.Close()

	payees, err := a.listPayees()
	if err != nil {
		t.Fatalf("list payees: %v", err)
	}
	got := map[string]int{}
	for _, p := range payees {
		got[p.Name] = p.CategoryID
	}
	if got["Amazon"] != 66 {
		t.Fatalf("Amazon default category = %d, want 66", got["Amazon"])
	}
	if got["Acme Utilities"] != 0 {
		t.Fatalf("Acme Utilities default category = %d, want 0", got["Acme Utilities"])
	}

	cats, err := a.listCategories()
	if err != nil {
		t.Fatalf("list categories: %v", err)
	}
	var buf bytes.Buffer
	if err := a.tmpl.ExecuteTemplate(&buf, "tx_datalists", pageData{
		Info: a.info(), Categories: cats, Payees: payees,
	}); err != nil {
		t.Fatalf("render tx_datalists: %v", err)
	}
	html := buf.String()
	if !strings.Contains(html, `<option value="Amazon" data-id="14" data-category-id="66">`) {
		t.Fatalf("payee option missing default category: %s", html)
	}
	if !strings.Contains(html, `<option value="Acme Utilities" data-id="1">`) {
		t.Fatalf("payee without a default category should carry no data-category-id: %s", html)
	}
}

func TestResolvePayeeAndCategoryBySubstring(t *testing.T) {
	a, err := newApp(config{
		DBPath:       fixtureDB(t),
		WriteEnabled: false,
		BackupDir:    "backups",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	payeeID, err := a.resolvePayeeID("Amazon")
	if err != nil {
		t.Fatal(err)
	}
	if payeeID != 14 {
		t.Fatalf("Amazon payee id = %d, want 14", payeeID)
	}

	categoryID, err := a.resolveCategoryID("Hobb")
	if err != nil {
		t.Fatal(err)
	}
	if categoryID != 66 {
		t.Fatalf("Hobb category id = %d, want 66", categoryID)
	}

	if _, err := a.resolveCategoryID("Leisure"); err == nil {
		t.Fatal("expected top-level category to be rejected by default")
	}
}

func TestResolveTopLevelCategoryWhenAllowed(t *testing.T) {
	a, err := newApp(config{
		DBPath:                  fixtureDB(t),
		WriteEnabled:            false,
		BackupDir:               "backups",
		AllowTopLevelCategories: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	categoryID, err := a.resolveCategoryID("Leisure")
	if err != nil {
		t.Fatal(err)
	}
	if categoryID != 3 {
		t.Fatalf("Leisure category id = %d, want 3", categoryID)
	}
}

func TestCategoryNamesForPrompt(t *testing.T) {
	a, err := newApp(config{
		DBPath:       fixtureDB(t),
		WriteEnabled: false,
		BackupDir:    "backups",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	names := a.categoryNamesForPrompt()
	if !slices.Contains(names, "Leisure: Hobbies") {
		t.Fatalf("expected subcategory in Parent: Child form, got %v", names)
	}
	// Top-level names are excluded unless allowed for new transactions.
	if slices.Contains(names, "Leisure") {
		t.Fatal("top-level category should be excluded by default")
	}

	// The selectable list must be injected into the prompt so the model picks
	// from existing categories rather than inventing labels.
	prompt := statementTextPrompt(Prompts{}, "2026-06-19", "sample statement text", names)
	if !strings.Contains(prompt, "Existing categories") || !strings.Contains(prompt, "Leisure: Hobbies") {
		t.Fatal("statement prompt should include the existing-category guidance")
	}
	if statementTextPrompt(Prompts{}, "2026-06-19", "sample statement text", nil) != statementTextPrompt(Prompts{}, "2026-06-19", "sample statement text", []string{}) {
		t.Fatal("empty category list should add no guidance")
	}

	a.config.AllowTopLevelCategories = true
	if !slices.Contains(a.categoryNamesForPrompt(), "Leisure") {
		t.Fatal("top-level category should be listed when allowed")
	}
}

func TestCategoryDeleteEligibility(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.mmb")
	buildSyntheticDB(t, dbPath)

	a, err := newApp(config{
		DBPath:       dbPath,
		WriteEnabled: true,
		BackupDir:    filepath.Join(dir, "backups"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	categoryID, err := a.insertCategory("MMXC Delete Eligibility", 3, true)
	if err != nil {
		t.Fatal(err)
	}
	cats, err := a.listCategories()
	if err != nil {
		t.Fatal(err)
	}
	var topLevelBlocked bool
	var insertedDeletable bool
	for _, cat := range cats {
		if cat.ID == 3 && !cat.CanDelete {
			topLevelBlocked = true
		}
		if cat.ID == categoryID && cat.CanDelete {
			insertedDeletable = true
		}
	}
	if !topLevelBlocked {
		t.Fatal("expected top-level category to hide delete action")
	}
	if !insertedDeletable {
		t.Fatal("expected unused subcategory to allow delete action")
	}
}

func TestSplitTransactionsAreVisibleAndProtected(t *testing.T) {
	a, err := newApp(config{
		DBPath:       fixtureDB(t),
		WriteEnabled: false,
		BackupDir:    "backups",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	tx, err := a.getTransaction(186)
	if err != nil {
		t.Fatal(err)
	}
	if tx.SplitCount == 0 || tx.Category == "" {
		t.Fatalf("expected transaction 186 to expose split metadata: %+v", tx)
	}

	txs, err := a.listTransactions(transactionFilter{CategoryID: 30})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, tx := range txs {
		if tx.ID == 186 && tx.SplitCount > 0 {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected category-filtered transaction list to include split transaction 186")
	}

	cats, err := a.listCategories()
	if err != nil {
		t.Fatal(err)
	}
	foundSplitOnlyCategory := false
	for _, cat := range cats {
		if cat.ID == 102 && cat.Count > 0 {
			foundSplitOnlyCategory = true
			break
		}
	}
	if !foundSplitOnlyCategory {
		t.Fatal("expected split-only category 102 to have transaction usage")
	}
}

func TestListTransactionsBuildsSplitHint(t *testing.T) {
	a, err := newApp(config{
		DBPath:       fixtureDB(t),
		WriteEnabled: false,
		BackupDir:    "backups",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	txs, err := a.listTransactions(transactionFilter{AccountID: 12})
	if err != nil {
		t.Fatal(err)
	}
	hints := map[int]string{}
	for _, tx := range txs {
		hints[tx.ID] = tx.SplitHint
		if tx.SplitCount == 0 && tx.SplitHint != "" {
			t.Fatalf("non-split transaction %d has a split hint: %q", tx.ID, tx.SplitHint)
		}
	}
	// 186 is a withdrawal of 100 split into Groceries 40 and Subscriptions 60,
	// both under Food; the lines carry the parent's (negative) sign.
	want := "Food: Groceries  " + formatMoney(-40, a.currFmt) + " — groceries\n" +
		"Food: Subscriptions  " + formatMoney(-60, a.currFmt) + " — subs"
	if hints[186] != want {
		t.Fatalf("split hint for 186 =\n%q\nwant\n%q", hints[186], want)
	}
}

func TestListTransactionsIncludesTimestampedEndDate(t *testing.T) {
	a, err := newApp(config{
		DBPath:       fixtureDB(t),
		WriteEnabled: true,
		BackupDir:    filepath.Join(t.TempDir(), "backups"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	if err := a.insertManualTransaction(transactionInput{
		AccountID: 12, Date: "2025-10-31", Type: "Withdrawal", Amount: 12.34,
		PayeeID: 14, CategoryID: 66,
	}); err != nil {
		t.Fatal(err)
	}
	txs, err := a.listTransactions(transactionFilter{
		AccountID: 12,
		From:      "2025-10-01",
		To:        "2025-10-31",
	})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, tx := range txs {
		if tx.Date == "2025-10-31" && math.Abs(tx.Amount-12.34) < 0.005 {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("transaction stored at midnight on the inclusive end date was omitted")
	}
}

func TestSplitTransactionWritesAreProtected(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.mmb")
	buildSyntheticDB(t, dbPath)

	a, err := newApp(config{
		DBPath:       dbPath,
		WriteEnabled: true,
		BackupDir:    filepath.Join(dir, "backups"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	if err := a.updateTransactionRecord(186, transactionInput{
		AccountID:  12,
		Date:       "2025-11-15",
		Type:       "Withdrawal",
		Amount:     1,
		PayeeID:    14,
		CategoryID: 66,
	}); err == nil {
		t.Fatal("expected standard editor update of split transaction to fail")
	}

	var beforeSplitCount int
	var beforeSplitSum float64
	if err := a.db.QueryRow("SELECT COUNT(*), COALESCE(SUM(SPLITTRANSAMOUNT), 0) FROM SPLITTRANSACTIONS_V1 WHERE TRANSID = 186").Scan(&beforeSplitCount, &beforeSplitSum); err != nil {
		t.Fatal(err)
	}
	if err := a.updateSplitTransactionRecord(186, transactionInput{
		AccountID: 12,
		Date:      "2025-11-20",
		Type:      "Withdrawal",
		Amount:    175.55,
		PayeeID:   14,
		Status:    "Reconciled",
		Notes:     "updated split parent",
		Splits: []splitLine{
			{CategoryID: 30, Amount: 100.25, Notes: "groceries"},
			{CategoryID: 86, Amount: 75.30, Notes: "toiletries"},
		},
	}); err != nil {
		t.Fatalf("update split transaction: %v", err)
	}
	got, err := a.getTransaction(186)
	if err != nil {
		t.Fatal(err)
	}
	if got.Date != "2025-11-20" || got.PayeeID != 14 || got.Status != "Reconciled" || got.Notes != "updated split parent" {
		t.Fatalf("unexpected split parent after update: %+v", got)
	}
	if got.Amount != 175.55 || got.CategoryID != -1 || got.Type != "Withdrawal" {
		t.Fatalf("split parent fields not updated correctly: %+v", got)
	}
	var afterSplitCount int
	var afterSplitSum float64
	if err := a.db.QueryRow("SELECT COUNT(*), COALESCE(SUM(SPLITTRANSAMOUNT), 0) FROM SPLITTRANSACTIONS_V1 WHERE TRANSID = 186").Scan(&afterSplitCount, &afterSplitSum); err != nil {
		t.Fatal(err)
	}
	if beforeSplitCount == afterSplitCount && beforeSplitSum == afterSplitSum {
		t.Fatal("expected split rows to be replaced")
	}
	if afterSplitCount != 2 || afterSplitSum != 175.55 {
		t.Fatalf("split rows not replaced correctly: count=%d sum=%v", afterSplitCount, afterSplitSum)
	}

	if err := a.deleteUnusedCategory(102); err == nil {
		t.Fatal("expected category used by split transactions to be protected from deletion")
	}
}

func TestCreateSplitTransactionOnTempDatabase(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.mmb")
	buildSyntheticDB(t, dbPath)

	a, err := newApp(config{
		DBPath:       dbPath,
		WriteEnabled: true,
		BackupDir:    filepath.Join(dir, "backups"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	if err := a.insertManualTransaction(transactionInput{
		AccountID:  12,
		Date:       "2025-12-01",
		Type:       "Withdrawal",
		Amount:     42.75,
		PayeeID:    14,
		CategoryID: -1,
		Notes:      "created split transaction",
		Splits: []splitLine{
			{CategoryID: 30, Amount: 20.25, Notes: "food"},
			{CategoryID: 86, Amount: 22.50, Notes: "care"},
		},
	}); err != nil {
		t.Fatalf("insert split transaction: %v", err)
	}

	var transID int
	if err := a.db.QueryRow("SELECT TRANSID FROM CHECKINGACCOUNT_V1 WHERE NOTES = 'created split transaction'").Scan(&transID); err != nil {
		t.Fatal(err)
	}
	got, err := a.getTransaction(transID)
	if err != nil {
		t.Fatal(err)
	}
	if got.SplitCount != 2 || got.CategoryID != -1 || got.Amount != 42.75 {
		t.Fatalf("unexpected split transaction after insert: %+v", got)
	}
	if len(got.Splits) != 2 {
		t.Fatalf("split line count = %d, want 2", len(got.Splits))
	}
}

func TestTransactionInputValidation(t *testing.T) {
	a, err := newApp(config{
		DBPath:       fixtureDB(t),
		WriteEnabled: false,
		BackupDir:    "backups",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	if _, err := a.parseTransactionInput(formRequest(url.Values{
		"account_id":  {"12"},
		"date":        {"2025-11-15"},
		"type":        {"Withdrawal"},
		"amount":      {"1.00"},
		"payee_id":    {"14"},
		"category_id": {"3"},
	}), false); err == nil {
		t.Fatal("expected direct top-level category id to be rejected")
	}

	if _, err := a.parseTransactionInput(formRequest(url.Values{
		"account_id":  {"12"},
		"date":        {"2025-11-15"},
		"type":        {"Withdrawal"},
		"amount":      {"1.00"},
		"payee_id":    {"999999999"},
		"category_id": {"66"},
	}), false); err == nil {
		t.Fatal("expected missing payee id to be rejected")
	}

	if _, err := a.parseTransactionInput(formRequest(url.Values{
		"account_id":    {"12"},
		"date":          {"2025-11-15"},
		"type":          {"Transfer"},
		"amount":        {"1.00"},
		"to_account_id": {"12"},
	}), false); err == nil {
		t.Fatal("expected transfer to same account to be rejected")
	}

	input, err := a.parseTransactionInput(formRequest(url.Values{
		"account_id":          {"12"},
		"date":                {"2025-11-15"},
		"type":                {"Withdrawal"},
		"split_mode":          {"false", "true"},
		"payee_id":            {"14"},
		"split_category_id":   {"30", "86"},
		"split_category_text": {"", ""},
		"split_amount":        {"20.25", "22.50"},
		"split_notes":         {"food", "care"},
	}), false)
	if err != nil {
		t.Fatalf("parse split transaction input: %v", err)
	}
	if input.Amount != 42.75 || input.CategoryID != -1 || len(input.Splits) != 2 {
		t.Fatalf("unexpected split input: %+v", input)
	}

	if _, err := a.parseTransactionInput(formRequest(url.Values{
		"account_id":        {"12"},
		"date":              {"2025-11-15"},
		"type":              {"Transfer"},
		"split_mode":        {"false", "true"},
		"to_account_id":     {"2"},
		"split_category_id": {"30", "86"},
		"split_amount":      {"20.25", "22.50"},
	}), false); err == nil {
		t.Fatal("expected split transfer to be rejected")
	}
}

func TestTransactionAmountsAcceptCalculations(t *testing.T) {
	a, err := newApp(config{
		DBPath:       fixtureDB(t),
		WriteEnabled: false,
		BackupDir:    "backups",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	input, err := a.parseTransactionInput(formRequest(url.Values{
		"account_id":  {"12"},
		"date":        {"2025-11-15"},
		"type":        {"Withdrawal"},
		"amount":      {"123.25 + 20-0.01"},
		"payee_id":    {"14"},
		"category_id": {"66"},
	}), false)
	if err != nil {
		t.Fatalf("parse calculated amount: %v", err)
	}
	if input.Amount != 143.24 {
		t.Fatalf("calculated amount = %v, want 143.24", input.Amount)
	}

	split, err := a.parseTransactionInput(formRequest(url.Values{
		"account_id":          {"12"},
		"date":                {"2025-11-15"},
		"type":                {"Withdrawal"},
		"split_mode":          {"false", "true"},
		"payee_id":            {"14"},
		"split_category_id":   {"30", "86"},
		"split_category_text": {"", ""},
		"split_amount":        {"20.25 + 2", "22.50-0.50"},
		"split_notes":         {"food", "care"},
	}), false)
	if err != nil {
		t.Fatalf("parse calculated split amounts: %v", err)
	}
	if split.Amount != 44.25 || len(split.Splits) != 2 || split.Splits[0].Amount != 22.25 || split.Splits[1].Amount != 22 {
		t.Fatalf("unexpected calculated split input: %+v", split)
	}

	if _, err := a.parseTransactionInput(formRequest(url.Values{
		"account_id":  {"12"},
		"date":        {"2025-11-15"},
		"type":        {"Withdrawal"},
		"amount":      {"10-10"},
		"payee_id":    {"14"},
		"category_id": {"66"},
	}), false); err == nil {
		t.Fatal("expected a calculation resolving to zero to be rejected")
	}
}

func TestNewTransactionRejectsInactivePayeeAndCategory(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.mmb")
	buildSyntheticDB(t, dbPath)

	a, err := newApp(config{
		DBPath:       dbPath,
		WriteEnabled: true,
		BackupDir:    filepath.Join(dir, "backups"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	if _, err := a.db.Exec("UPDATE PAYEE_V1 SET ACTIVE = 0 WHERE PAYEEID = 14"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.parseTransactionInput(formRequest(url.Values{
		"account_id":  {"12"},
		"date":        {"2025-11-15"},
		"type":        {"Withdrawal"},
		"amount":      {"1.00"},
		"payee_id":    {"14"},
		"category_id": {"66"},
	}), true); err == nil {
		t.Fatal("expected inactive payee to be rejected for a new transaction")
	}
	if _, err := a.parseTransactionInput(formRequest(url.Values{
		"account_id":  {"12"},
		"date":        {"2025-11-15"},
		"type":        {"Withdrawal"},
		"amount":      {"1.00"},
		"payee_id":    {"14"},
		"category_id": {"66"},
	}), false); err != nil {
		t.Fatalf("existing transaction edit should accept an inactive payee reference: %v", err)
	}

	if _, err := a.db.Exec("UPDATE PAYEE_V1 SET ACTIVE = 1 WHERE PAYEEID = 14"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec("UPDATE CATEGORY_V1 SET ACTIVE = 0 WHERE CATEGID = 66"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.parseTransactionInput(formRequest(url.Values{
		"account_id":  {"12"},
		"date":        {"2025-11-15"},
		"type":        {"Withdrawal"},
		"amount":      {"1.00"},
		"payee_id":    {"14"},
		"category_id": {"66"},
	}), true); err == nil {
		t.Fatal("expected inactive category to be rejected for a new transaction")
	}
	if _, err := a.parseTransactionInput(formRequest(url.Values{
		"account_id":  {"12"},
		"date":        {"2025-11-15"},
		"type":        {"Withdrawal"},
		"amount":      {"1.00"},
		"payee_id":    {"14"},
		"category_id": {"66"},
	}), false); err != nil {
		t.Fatalf("existing transaction edit should accept an inactive category reference: %v", err)
	}
}

func TestSearchKeyFolding(t *testing.T) {
	cases := map[string]string{
		"AMAZON.com":         "amazon com",
		"Café Saint-Rémy":    "cafe saint remy",
		"  Électricité-Nord": "electricite nord",
		"":                   "",
		"!!!":                "",
	}
	for in, want := range cases {
		if got := searchKey(in); got != want {
			t.Errorf("searchKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTransactionFilterDefaultsToLast90Days(t *testing.T) {
	// No "from" key: default to a 90-day window so a freshly opened account shows
	// recent activity rather than everything.
	req := httptest.NewRequest(http.MethodGet, "/accounts?account_id=12", nil)
	want := time.Now().AddDate(0, 0, -90).Format("2006-01-02")
	if got := parseTransactionFilter(req).From; got != want {
		t.Fatalf("default From = %q, want %q", got, want)
	}
	// An explicitly empty "from" (the user cleared the field) means show all.
	req = httptest.NewRequest(http.MethodGet, "/accounts?account_id=12&from=", nil)
	if got := parseTransactionFilter(req).From; got != "" {
		t.Fatalf("cleared From = %q, want empty", got)
	}
}

func TestRecentTransactionFilterDefaultsToLatestCount(t *testing.T) {
	// Categories/Payees hold too few transactions for a date window to be a useful
	// default, so they cap the list at the latest N and leave the range open.
	req := httptest.NewRequest(http.MethodGet, "/categories?category_id=66", nil)
	filter := parseRecentTransactionFilter(req)
	if filter.From != "" || filter.To != "" {
		t.Fatalf("default range = %q..%q, want empty", filter.From, filter.To)
	}
	if filter.Limit != defaultRecentLimit {
		t.Fatalf("default Limit = %d, want %d", filter.Limit, defaultRecentLimit)
	}
	// An explicit count is honoured, including 0 ("All").
	req = httptest.NewRequest(http.MethodGet, "/payees?payee_id=14&limit=250", nil)
	if got := parseRecentTransactionFilter(req).Limit; got != 250 {
		t.Fatalf("Limit = %d, want 250", got)
	}
	req = httptest.NewRequest(http.MethodGet, "/payees?payee_id=14&limit=0", nil)
	if got := parseRecentTransactionFilter(req).Limit; got != 0 {
		t.Fatalf("Limit = %d, want 0 (all)", got)
	}
	// Garbage falls back to the default rather than showing nothing.
	req = httptest.NewRequest(http.MethodGet, "/payees?payee_id=14&limit=-3", nil)
	if got := parseRecentTransactionFilter(req).Limit; got != defaultRecentLimit {
		t.Fatalf("Limit = %d, want %d", got, defaultRecentLimit)
	}
	// A date range still narrows the window when the user sets one.
	req = httptest.NewRequest(http.MethodGet, "/categories?category_id=66&from=2025-01-01&to=2025-03-31", nil)
	filter = parseRecentTransactionFilter(req)
	if filter.From != "2025-01-01" || filter.To != "2025-03-31" {
		t.Fatalf("range = %q..%q, want 2025-01-01..2025-03-31", filter.From, filter.To)
	}
}

func TestTransactionSearchIsCaseAndAccentInsensitive(t *testing.T) {
	a, err := newApp(config{
		DBPath:       fixtureDB(t),
		WriteEnabled: false,
		BackupDir:    "backups",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	// Payee "Amazon" (id 14) is on transactions 186 and 187. A search in a
	// different case must still match via the folded searchkey() comparison.
	for _, needle := range []string{"AMAZON", "amazon", "amAZ"} {
		txs, err := a.listTransactions(transactionFilter{AccountID: 12, Search: needle})
		if err != nil {
			t.Fatal(err)
		}
		found := map[int]bool{}
		for _, tx := range txs {
			found[tx.ID] = true
		}
		if !found[186] || !found[187] {
			t.Fatalf("search %q missed Amazon transactions: got %v", needle, found)
		}
	}
}

func TestLocalReturnURLSanitization(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/payees?payee_id=14&edit_trans_id=99&scroll=120&type=Deposit", nil)
	if got := localReturnURL(req); got != "/payees?payee_id=14&type=Deposit" {
		t.Fatalf("localReturnURL = %q", got)
	}
	if got := sanitizeLocalPath("https://example.test/accounts"); got != "" {
		t.Fatalf("external return path accepted: %q", got)
	}
	if got := sanitizeLocalPath("//example.test/accounts"); got != "" {
		t.Fatalf("scheme-relative return path accepted: %q", got)
	}
	if got := sanitizeLocalPath("/accounts?account_id=12"); got != "/accounts?account_id=12" {
		t.Fatalf("local return path rejected: %q", got)
	}
}

func TestReviewRenderLocalizesDates(t *testing.T) {
	a, err := newApp(config{
		DBPath:       fixtureDB(t),
		WriteEnabled: false,
		BackupDir:    "backups",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	// German locale with no translation model: T() passes English through
	// (model is empty) but DateFmt must still reformat display dates to dd.mm.yyyy.
	a.config.TranslationLanguage = "de"

	req := httptest.NewRequest(http.MethodGet, "/review?month=2025-10&account_id=12", nil)
	rec := httptest.NewRecorder()
	a.monthlyReview(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("review status = %d", rec.Code)
	}
	html := rec.Body.String()
	// The detail head shows "MonthStart to MonthEnd"; in German that's dd.mm.yyyy.
	if !strings.Contains(html, "01.10.2025 to 31.10.2025") {
		t.Fatalf("review page did not localize the month range to German format")
	}
	// The ISO forms must not leak into the displayed range.
	if strings.Contains(html, "2025-10-01 to 2025-10-31") {
		t.Fatalf("review page still shows ISO dates for German locale")
	}
}

func TestMonthlyReviewBuildsAccountCoverage(t *testing.T) {
	a, err := newApp(config{
		DBPath:       fixtureDB(t),
		WriteEnabled: false,
		BackupDir:    "backups",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	req := httptest.NewRequest(http.MethodGet, "/review?month=2025-10&account_id=12", nil)
	review, err := a.buildMonthlyReview(req, nil, "", "")
	if err != nil {
		t.Fatalf("build monthly review: %v", err)
	}
	if review.MonthStart != "2025-10-01" || review.MonthEnd != "2025-10-31" {
		t.Fatalf("unexpected review range: %s to %s", review.MonthStart, review.MonthEnd)
	}
	if review.Selected == nil || review.Selected.Account.ID != 12 {
		t.Fatalf("selected account = %+v", review.Selected)
	}
	if review.Selected.TransactionN == 0 {
		t.Fatal("expected selected account to have month activity")
	}
	if len(review.Transactions) == 0 {
		t.Fatal("expected month transactions")
	}
	for _, row := range review.Rows {
		if row.Account.Status != "Open" {
			t.Fatalf("review included non-open account: %+v", row.Account)
		}
	}

	closedReq := httptest.NewRequest(http.MethodGet, "/review?month=2025-10&account_id=5", nil)
	closedReview, err := a.buildMonthlyReview(closedReq, nil, "", "")
	if err != nil {
		t.Fatalf("build review with closed account selected: %v", err)
	}
	if closedReview.Selected == nil || closedReview.Selected.Account.Status != "Open" {
		t.Fatalf("closed account selection did not fall back to an open account: %+v", closedReview.Selected)
	}

	accountID, _ := a.detectStatementAccount(statementHeader{AccountName: "Old Savings"})
	if accountID == 5 {
		t.Fatal("statement detection selected a closed account")
	}
}

func TestReviewImportCandidatesAndCommit(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.mmb")
	buildSyntheticDB(t, dbPath)

	a, err := newApp(config{
		DBPath:       dbPath,
		WriteEnabled: true,
		BackupDir:    filepath.Join(dir, "backups"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	raw := `[{"date":"2025-10-31","type":"Withdrawal","amount":9.87,"payee":"Amazon","category":"Leisure: Hobbies","notes":"import candidate"}]`
	req := formRequest(url.Values{
		"account_id":      {"12"},
		"candidates_json": {raw},
	})
	candidates, err := a.parseImportCandidates(req)
	if err != nil {
		t.Fatalf("parse import candidates: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("candidate count = %d, want 1", len(candidates))
	}
	if candidates[0].Error != "" || candidates[0].Duplicate {
		t.Fatalf("unexpected candidate flags: %+v", candidates[0])
	}
	if candidates[0].Status != "Reconciled" {
		t.Fatalf("candidate status = %q, want Reconciled", candidates[0].Status)
	}

	fuzzyReq := formRequest(url.Values{
		"account_id":      {"12"},
		"candidates_json": {`[{"date":"2025-10-24","type":"Withdrawal","amount":6.54,"payee":"Amazn","category":"Leisure: Hobies","notes":"fuzzy import"}]`},
	})
	fuzzy, err := a.parseImportCandidates(fuzzyReq)
	if err != nil {
		t.Fatalf("parse fuzzy candidates: %v", err)
	}
	if fuzzy[0].Payee != "Amazon" || fuzzy[0].PayeeID != 14 || fuzzy[0].CategoryID != 66 || fuzzy[0].Error != "" {
		t.Fatalf("fuzzy candidate did not resolve: %+v", fuzzy[0])
	}

	before := countRows(t, a, "CHECKINGACCOUNT_V1")
	commitReq := formRequest(url.Values{
		"account_id": {"12"},
		"month":      {"2025-10"},
		"approved":   {"0"},
		"date":       {"2025-10-31"},
		"type":       {"Withdrawal"},
		"amount":     {"9.87"},
		"payee":      {"Amazon"},
		"category":   {"Leisure: Hobbies"},
		"notes":      {"import candidate"},
		"status":     {"Reconciled"},
	})
	inputs, err := a.parseApprovedImportInputs(commitReq)
	if err != nil {
		t.Fatalf("parse approved inputs: %v", err)
	}
	if _, err := a.insertImportedTransactions(inputs, "", "", 0); err != nil {
		t.Fatalf("insert imported transactions: %v", err)
	}
	after := countRows(t, a, "CHECKINGACCOUNT_V1")
	if after != before+1 {
		t.Fatalf("transaction count after import = %d, want %d", after, before+1)
	}

	duplicateReq := formRequest(url.Values{
		"account_id":      {"12"},
		"candidates_json": {raw},
	})
	candidates, err = a.parseImportCandidates(duplicateReq)
	if err != nil {
		t.Fatalf("parse duplicate candidates: %v", err)
	}
	if !candidates[0].Duplicate || candidates[0].Selected {
		t.Fatalf("duplicate candidate not flagged: %+v", candidates[0])
	}
	messyDuplicateReq := formRequest(url.Values{
		"account_id":      {"12"},
		"candidates_json": {`[{"date":"2025-10-31","type":"Withdrawal","amount":9.87,"payee":"PAYPAL *AMZN","category":"Shopping","notes":"statement guess"}]`},
	})
	candidates, err = a.parseImportCandidates(messyDuplicateReq)
	if err != nil {
		t.Fatalf("parse messy duplicate candidate: %v", err)
	}
	if !candidates[0].Duplicate || candidates[0].Error != "" {
		t.Fatalf("messy duplicate candidate not cleaned up: %+v", candidates[0])
	}
	if candidates[0].Payee != "Amazon" || candidates[0].Category != "Leisure: Hobbies" || candidates[0].Notes != "import candidate" {
		t.Fatalf("duplicate candidate did not use existing transaction fields: %+v", candidates[0])
	}
	if len(candidates[0].Warnings) != 1 || candidates[0].Warnings[0] != "Possible duplicate" {
		t.Fatalf("duplicate candidate warnings = %#v, want only possible duplicate", candidates[0].Warnings)
	}
	if err := a.insertManualTransaction(transactionInput{
		AccountID:   12,
		ToAccountID: 2,
		Date:        "2025-10-28",
		Type:        "Transfer",
		Amount:      56.78,
		PayeeID:     -1,
		CategoryID:  4,
		Status:      "Reconciled",
		Notes:       "statement-side transfer duplicate",
	}); err != nil {
		t.Fatalf("insert transfer duplicate fixture: %v", err)
	}
	outgoingTransferReq := formRequest(url.Values{
		"account_id":      {"12"},
		"candidates_json": {`[{"date":"2025-10-28","type":"Withdrawal","amount":56.78,"payee":"Card Payment","category":"Leisure: Hobbies","notes":"statement payment"}]`},
	})
	candidates, err = a.parseImportCandidates(outgoingTransferReq)
	if err != nil {
		t.Fatalf("parse outgoing transfer duplicate: %v", err)
	}
	if !candidates[0].Duplicate || candidates[0].Type != "Transfer" || candidates[0].ToAccountID != 2 {
		t.Fatalf("outgoing transfer duplicate not recognized: %+v", candidates[0])
	}
	incomingTransferReq := formRequest(url.Values{
		"account_id":      {"2"},
		"candidates_json": {`[{"date":"2025-10-28","type":"Deposit","amount":56.78,"payee":"PAYMENT","category":"Leisure: Hobbies","notes":"statement payment"}]`},
	})
	candidates, err = a.parseImportCandidates(incomingTransferReq)
	if err != nil {
		t.Fatalf("parse incoming transfer duplicate: %v", err)
	}
	if !candidates[0].Duplicate || candidates[0].Type != "Transfer" || candidates[0].ToAccountID != 12 {
		t.Fatalf("incoming transfer duplicate not recognized: %+v", candidates[0])
	}
	partialReq := formRequest(url.Values{
		"account_id": {"12"},
		"month":      {"2025-10"},
		"approved":   {"0"},
		"date":       {"2025-10-29", "2025-10-30"},
		"type":       {"Withdrawal", "Withdrawal"},
		"amount":     {"12.34", "23.45"},
		"payee":      {"Amazon", "Amazon"},
		"category":   {"Leisure: Hobbies", "Leisure: Hobbies"},
		"notes":      {"partial imported", "partial remaining"},
		"status":     {"Reconciled", "Reconciled"},
	})
	rr := httptest.NewRecorder()
	a.commitReviewImport(rr, partialReq)
	if rr.Code != http.StatusOK {
		t.Fatalf("partial import status = %d, want %d; body %s", rr.Code, http.StatusOK, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "Remaining candidates are kept for another import.") {
		t.Fatal("partial import response should keep remaining candidates visible")
	}
	if !strings.Contains(rr.Body.String(), "partial remaining") {
		t.Fatal("partial import response should include the unapproved candidate")
	}

	newPayeeReq := formRequest(url.Values{
		"account_id": {"12"},
		"approved":   {"0"},
		"date":       {"2025-10-30"},
		"type":       {"Withdrawal"},
		"amount":     {"4.56"},
		"payee":      {"MMXC Imported New Payee"},
		"category":   {"Leisure: Hobbies"},
		"notes":      {"new payee import"},
		"status":     {"__blank"},
	})
	inputs, err = a.parseApprovedImportInputs(newPayeeReq)
	if err != nil {
		t.Fatalf("parse new payee import: %v", err)
	}
	if _, err := a.insertImportedTransactions(inputs, "", "", 0); err != nil {
		t.Fatalf("insert new payee import: %v", err)
	}
	var payeeID int
	if err := a.db.QueryRow("SELECT PAYEEID FROM PAYEE_V1 WHERE PAYEENAME = 'MMXC Imported New Payee'").Scan(&payeeID); err != nil {
		t.Fatalf("new imported payee not created: %v", err)
	}

	learnReq := formRequest(url.Values{
		"account_id":     {"12"},
		"approved":       {"0"},
		"date":           {"2025-10-29"},
		"type":           {"Withdrawal"},
		"amount":         {"7.89"},
		"payee":          {"Harbor Trust"},
		"original_payee": {"EFT Withdrawal to CITY MORTGAGE DEPT"},
		"category":       {"Leisure: Hobbies"},
		"notes":          {"learned payee import"},
		"status":         {"__blank"},
	})
	inputs, err = a.parseApprovedImportInputs(learnReq)
	if err != nil {
		t.Fatalf("parse learned payee import: %v", err)
	}
	if _, err := a.insertImportedTransactions(inputs, "", "", 0); err != nil {
		t.Fatalf("insert learned payee import: %v", err)
	}
	var pattern string
	if err := a.db.QueryRow("SELECT COALESCE(PATTERN, '') FROM PAYEE_V1 WHERE PAYEENAME = 'Harbor Trust'").Scan(&pattern); err != nil {
		t.Fatal(err)
	}
	// Learned patterns are generalized (lowercased significant tokens, volatile
	// parts dropped) so they match future statement variants.
	if !strings.Contains(strings.ToLower(pattern), "city mortgage dept") {
		t.Fatalf("learned payee pattern = %q", pattern)
	}

	learnedCandidateReq := formRequest(url.Values{
		"account_id":      {"12"},
		"candidates_json": {`[{"date":"2025-10-28","type":"Withdrawal","amount":8.90,"payee":"EFT Withdrawal to CITY MORTGAGE DEPT","category":"Leisure: Hobbies","notes":"future import"}]`},
	})
	learned, err := a.parseImportCandidates(learnedCandidateReq)
	if err != nil {
		t.Fatalf("parse learned candidate: %v", err)
	}
	if learned[0].Payee != "Harbor Trust" || learned[0].PayeeID <= 0 || learned[0].Error != "" {
		t.Fatalf("learned candidate did not map payee: %+v", learned[0])
	}

	transferLearnReq := formRequest(url.Values{
		"account_id":     {"12"},
		"approved":       {"0"},
		"date":           {"2025-10-27"},
		"type":           {"Transfer"},
		"amount":         {"12.34"},
		"payee":          {"Harbor Credit Card Payment"},
		"original_payee": {"Harbor Credit Card Payment"},
		"to_account_id":  {"2"},
		"category":       {""},
		"notes":          {"learned transfer import"},
		"status":         {"__blank"},
	})
	inputs, err = a.parseApprovedImportInputs(transferLearnReq)
	if err != nil {
		t.Fatalf("parse learned transfer import: %v", err)
	}
	if inputs[0].Type != "Transfer" || inputs[0].ToAccountID != 2 {
		t.Fatalf("unexpected learned transfer input: %+v", inputs[0])
	}
	if _, err := a.insertImportedTransactions(inputs, "", "", 0); err != nil {
		t.Fatalf("insert learned transfer import: %v", err)
	}
	transferCandidateReq := formRequest(url.Values{
		"account_id":      {"12"},
		"candidates_json": {`[{"date":"2025-10-26","type":"Withdrawal","amount":22.22,"payee":"Harbor Credit Card Payment","category":"Leisure: Hobbies","notes":"future transfer"}]`},
	})
	transferLearned, err := a.parseImportCandidates(transferCandidateReq)
	if err != nil {
		t.Fatalf("parse learned transfer candidate: %v", err)
	}
	if transferLearned[0].Type != "Transfer" || transferLearned[0].ToAccountID != 2 || transferLearned[0].Error != "" {
		t.Fatalf("learned transfer candidate did not map: %+v", transferLearned[0])
	}

	problemReq := formRequest(url.Values{
		"account_id":      {"12"},
		"candidates_json": {`[{"date":"2025-10-25","type":"Withdrawal","amount":1.23,"payee":"MMXC Missing Payee","category":"MMXC Missing Category","notes":"problem row"}]`},
	})
	problemCandidates, err := a.parseImportCandidates(problemReq)
	if err != nil {
		t.Fatalf("parse problem candidate: %v", err)
	}
	if len(problemCandidates[0].Errors) == 0 || len(problemCandidates[0].Warnings) == 0 {
		t.Fatalf("expected errors and warnings together: %+v", problemCandidates[0])
	}
}

func TestReviewIncomingTransferUsesCanonicalLedgerDirection(t *testing.T) {
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

	const (
		sourceID   = 12
		receiverID = 2
		amount     = 43.21
		date       = "2025-12-19"
	)
	request := formRequest(url.Values{
		"account_id":     {strconv.Itoa(receiverID)},
		"approved":       {"0"},
		"date":           {date},
		"type":           {"TransferIn"},
		"amount":         {"43.21"},
		"payee":          {"Internal move"},
		"original_payee": {"Internal move"},
		"to_account_id":  {strconv.Itoa(sourceID)},
		"category":       {""},
		"notes":          {"receiving statement"},
		"status":         {"Reconciled"},
	})
	inputs, err := a.parseApprovedImportInputs(request)
	if err != nil {
		t.Fatalf("parse incoming transfer: %v", err)
	}
	if len(inputs) != 1 {
		t.Fatalf("input count = %d, want 1", len(inputs))
	}
	input := inputs[0]
	if input.AccountID != sourceID || input.ToAccountID != receiverID {
		t.Fatalf("canonical accounts = %d -> %d, want %d -> %d", input.AccountID, input.ToAccountID, sourceID, receiverID)
	}
	if input.TransferDirection != "in" || input.CounterpartyAccountID != sourceID {
		t.Fatalf("review transfer metadata = %+v", input)
	}
	if got := candidateSignedDelta(importCandidate{Type: "Transfer", TransferDirection: "in", Amount: amount}); got != amount {
		t.Fatalf("incoming signed delta = %.2f, want %.2f", got, amount)
	}
	if got := candidateSignedDelta(importCandidate{Type: "Transfer", TransferDirection: "out", Amount: amount}); got != -amount {
		t.Fatalf("outgoing signed delta = %.2f, want %.2f", got, -amount)
	}

	if _, err := a.insertImportedTransactions(inputs, "", "", receiverID); err != nil {
		t.Fatalf("insert incoming transfer: %v", err)
	}
	var transID int64
	var accountID, toAccountID int
	var transCode, notes string
	if err := a.db.QueryRow(`
SELECT TRANSID, ACCOUNTID, TOACCOUNTID, TRANSCODE, NOTES
  FROM CHECKINGACCOUNT_V1
 WHERE TRANSDATE = ? AND ABS(TRANSAMOUNT - ?) < 0.005`, date+"T00:00:00", amount).
		Scan(&transID, &accountID, &toAccountID, &transCode, &notes); err != nil {
		t.Fatal(err)
	}
	if accountID != sourceID || toAccountID != receiverID || transCode != "Transfer" {
		t.Fatalf("stored transfer = %d -> %d (%s), want %d -> %d (Transfer)", accountID, toAccountID, transCode, sourceID, receiverID)
	}

	// The learned rule must retain that this statement description identifies an
	// incoming transfer, and a receiving-side duplicate upsert must not reverse
	// the canonical ledger row.
	candidates := []importCandidate{{
		Date: date, Type: "Deposit", Amount: amount,
		Description: "Internal move", Payee: "Internal move", Category: "Leisure: Hobbies",
	}}
	a.prepareImportCandidatesNoLLM(receiverID, candidates)
	candidate := candidates[0]
	if !candidate.Duplicate || candidate.DuplicateID != transID || candidate.Type != "Transfer" || candidate.TransferDirection != "in" || candidate.ToAccountID != sourceID {
		t.Fatalf("incoming duplicate candidate = %+v", candidate)
	}
	sourceCandidates := []importCandidate{{
		Date: date, Type: "Withdrawal", Amount: amount,
		Description: "Internal move", Payee: "Internal move", Category: "Leisure: Hobbies",
	}}
	a.prepareImportCandidatesNoLLM(sourceID, sourceCandidates)
	sourceCandidate := sourceCandidates[0]
	if !sourceCandidate.Duplicate || sourceCandidate.DuplicateID != transID || sourceCandidate.Type != "Transfer" || sourceCandidate.TransferDirection != "out" || sourceCandidate.ToAccountID != receiverID {
		t.Fatalf("sender-side duplicate candidate = %+v", sourceCandidate)
	}
	if _, err := a.db.Exec("UPDATE CHECKINGACCOUNT_V1 SET TRANSAMOUNT = 41.11 WHERE TRANSID = ?", transID); err != nil {
		t.Fatal(err)
	}

	update := formRequest(url.Values{
		"account_id":    {strconv.Itoa(receiverID)},
		"approved":      {"0"},
		"duplicate_id":  {strconv.FormatInt(transID, 10)},
		"date":          {date},
		"type":          {"TransferIn"},
		"amount":        {"43.21"},
		"payee":         {""},
		"to_account_id": {strconv.Itoa(sourceID)},
		"category":      {""},
		"notes":         {"confirmed while reviewing receiver"},
		"status":        {"Reconciled"},
	})
	updateInputs, err := a.parseApprovedImportInputs(update)
	if err != nil {
		t.Fatalf("parse incoming duplicate update: %v", err)
	}
	if _, err := a.insertImportedTransactions(updateInputs, "", "", receiverID); err != nil {
		t.Fatalf("update incoming duplicate: %v", err)
	}
	var sourceAmount, destinationAmount float64
	if err := a.db.QueryRow("SELECT ACCOUNTID, TOACCOUNTID, TRANSAMOUNT, TOTRANSAMOUNT, NOTES FROM CHECKINGACCOUNT_V1 WHERE TRANSID = ?", transID).
		Scan(&accountID, &toAccountID, &sourceAmount, &destinationAmount, &notes); err != nil {
		t.Fatal(err)
	}
	if accountID != sourceID || toAccountID != receiverID || sourceAmount != 41.11 || destinationAmount != amount || notes != "confirmed while reviewing receiver" {
		t.Fatalf("updated transfer = %d (%.2f) -> %d (%.2f), notes %q", accountID, sourceAmount, toAccountID, destinationAmount, notes)
	}
}

func TestParseImportMatchResponse(t *testing.T) {
	allowedPayees := map[string]string{
		"*taxi":                "*Taxi",
		"wikimedia foundation": "Wikimedia Foundation",
	}
	allowedCategories := map[string]string{"leisure: hobbies": "Leisure: Hobbies"}
	// Leading prose before the JSON object must be tolerated.
	raw := `Here you go: {"payees":[` +
		`{"input":"UBER","match":"*Taxi"},` +
		`{"input":"WIKIPEDIA","match":"Wikimedia Foundation"},` +
		`{"input":"MysteryCo","match":"Totally Invented Payee"},` +
		`{"input":"Nothing","match":""}],` +
		`"categories":[{"input":"video games","match":"Leisure: Hobbies"}]}`

	payees, categories := parseImportMatchResponse(raw, allowedPayees, allowedCategories)

	// Semantic matches the fuzzy matcher cannot make are accepted, keyed by
	// lowercased input.
	if payees["uber"] != "*Taxi" {
		t.Errorf("uber -> %q, want *Taxi", payees["uber"])
	}
	if payees["wikipedia"] != "Wikimedia Foundation" {
		t.Errorf("wikipedia -> %q, want Wikimedia Foundation", payees["wikipedia"])
	}
	// A name the model invented (not in the allowed list) must be dropped, so an
	// applied suggestion always resolves to a real payee.
	if _, ok := payees["mysteryco"]; ok {
		t.Error("invented payee match should be rejected")
	}
	if _, ok := payees["nothing"]; ok {
		t.Error("empty match should be dropped")
	}
	if categories["video games"] != "Leisure: Hobbies" {
		t.Errorf("category -> %q, want Leisure: Hobbies", categories["video games"])
	}
}

func TestStatementExtractionHelpers(t *testing.T) {
	if got := safeUploadName("../May Statement (final).pdf"); got != "May-Statement--final-.pdf" {
		t.Fatalf("safeUploadName = %q", got)
	}
	jsonText, err := extractJSONArray("```json\n[{\"date\":\"2025-10-31\"}]\n```")
	if err != nil {
		t.Fatalf("extract JSON array: %v", err)
	}
	if jsonText != "[{\"date\":\"2025-10-31\"}]" {
		t.Fatalf("json array = %q", jsonText)
	}
	if _, err := extractJSONArray("no transactions found"); err == nil {
		t.Fatal("expected missing JSON array to fail")
	}

	dir := t.TempDir()
	a := &app{config: config{StatementDir: dir}}
	if _, err := a.saveStatementFile(12, true, "../Statement.pdf", []byte("statement"), time.Now()); err != nil {
		t.Fatalf("save statement file: %v", err)
	}
	matches, err := filepath.Glob(filepath.Join(dir, "12", "*-Statement.pdf"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Fatalf("saved statement matches = %d, want 1", len(matches))
	}
}

func TestSamplePDFTextExtraction(t *testing.T) {
	// Sample statements and their corrected goldens are real-data-derived,
	// local-only dev artifacts (data/samples, gitignored), so what to expect
	// comes from each golden rather than from this file. Skip when there are no
	// samples so a clean checkout still passes.
	pdfs, _ := filepath.Glob(filepath.Join("..", "..", "data", "samples", "*.pdf"))
	checked := 0
	for _, pdf := range pdfs {
		golden, err := os.ReadFile(strings.TrimSuffix(pdf, ".pdf") + ".expected.json")
		if err != nil {
			continue
		}
		var expected ExtractedStatement
		if err := json.Unmarshal(golden, &expected); err != nil {
			t.Fatalf("%s: %v", pdf, err)
		}
		transactions := expected.Transactions
		for _, account := range expected.Accounts {
			transactions = append(transactions, account.Transactions...)
		}
		data, err := os.ReadFile(pdf)
		if err != nil {
			t.Fatal(err)
		}
		// Descriptions in a golden are the model's reading of the layout, so
		// only the amounts are compared with the extracted text, in either
		// 1,234.56 or 1 234,56 style.
		text := extractPDFText(data)
		english := strings.ReplaceAll(text, ",", "")
		french := strings.ReplaceAll(strings.ReplaceAll(text, " ", ""), "\u00a0", "")
		for _, tx := range transactions {
			want := strconv.FormatFloat(math.Abs(tx.Amount), 'f', 2, 64)
			if !strings.Contains(english, want) && !strings.Contains(french, strings.Replace(want, ".", ",", 1)) {
				t.Errorf("%s: extracted PDF text missing amount %s (%s)", filepath.Base(pdf), want, tx.Date)
			}
		}
		checked++
	}
	if checked == 0 {
		t.Skip("no sample statements with goldens in data/samples")
	}
}

func TestStatementArchivePeriodOverlapAndDedup(t *testing.T) {
	dir := t.TempDir()
	a := &app{config: config{StatementDir: dir}}

	// A credit-card statement straddling the Dec/Jan boundary.
	header := statementHeader{
		AccountName:    "Visa",
		AccountNumber:  "1234",
		PeriodStart:    "2025-12-15",
		PeriodEnd:      "2026-01-14",
		ClosingDate:    "2026-01-14",
		ClosingBalance: 250.00,
		HasClosing:     true,
	}
	cands := []importCandidate{{Date: "2025-12-20", Type: "Withdrawal", Amount: 9.99, Description: "Coffee"}}
	data := []byte("statement-bytes-v1")
	meta, err := a.archiveStatement(7, true, "Visa-Jan.pdf", data, header, cands, `[{"date":"2025-12-20"}]`)
	if err != nil {
		t.Fatalf("archive statement: %v", err)
	}
	if meta.PeriodStart != "2025-12-15" || meta.PeriodEnd != "2026-01-14" {
		t.Fatalf("resolved period = %s..%s", meta.PeriodStart, meta.PeriodEnd)
	}
	if meta.ContentHash != statementContentHash(data) {
		t.Fatalf("content hash mismatch")
	}

	// The statement must list under both months it overlaps, but not adjacent ones.
	for _, tc := range []struct {
		start, end string
		want       int
	}{
		{"2025-12-01", "2025-12-31", 1}, // December
		{"2026-01-01", "2026-01-31", 1}, // January
		{"2025-11-01", "2025-11-30", 0}, // November (before)
		{"2026-02-01", "2026-02-28", 0}, // February (after)
	} {
		got := a.statementsForMonth(7, tc.start, tc.end)
		if len(got) != tc.want {
			t.Fatalf("statementsForMonth(%s..%s) = %d, want %d", tc.start, tc.end, len(got), tc.want)
		}
	}

	// Sidecar round-trips the cached extraction.
	listed := a.listArchivedStatements(7)
	if len(listed) != 1 {
		t.Fatalf("listed = %d, want 1", len(listed))
	}
	if len(listed[0].Candidates) != 1 || listed[0].Candidates[0].Description != "Coffee" {
		t.Fatalf("cached candidates not round-tripped: %+v", listed[0].Candidates)
	}

	// Content-hash dedup finds the same file uploaded again (under any account).
	found, ok := a.findStatementByHash(statementContentHash(data))
	if !ok || found.File != meta.File {
		t.Fatalf("findStatementByHash did not return the archived statement (ok=%v)", ok)
	}
	if _, ok := a.findStatementByHash(statementContentHash([]byte("other"))); ok {
		t.Fatalf("findStatementByHash matched an unknown hash")
	}
}

func TestParseMultiAccountStatementSectionsIncludingEmptyAccount(t *testing.T) {
	raw := `{
		"accounts": [
			{
				"account": {"name": "Synthetic Savings", "number": "11111111"},
				"period": {"start": "2026-06-01", "end": "2026-06-30"},
				"closing_date": "2026-06-30", "closing_balance": 9617.73,
				"transactions": [
					{"date":"2026-06-06","type":"Deposit","amount":1000,"description":"INTERNET DEPOSIT","payee":"Synthetic Chequing","category":""}
				]
			},
			{
				"account": {"name": "Synthetic Savings", "number": "33333333"},
				"period": {"start": "2026-06-01", "end": "2026-06-30"},
				"closing_date": "2026-06-30", "closing_balance": 0.01,
				"transactions": []
			}
		]
	}`
	sections, _, err := parseStatementSectionsResponse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(sections) != 2 {
		t.Fatalf("sections = %d, want 2", len(sections))
	}
	if len(sections[0].Candidates) != 1 || sections[0].Candidates[0].Amount != 1000 {
		t.Fatalf("first account candidates = %+v", sections[0].Candidates)
	}
	if len(sections[1].Candidates) != 0 || !sections[1].Header.HasClosing || sections[1].Header.ClosingBalance != 0.01 {
		t.Fatalf("zero-transaction account was not preserved: %+v", sections[1])
	}
	if _, _, err := parseStatementSectionsResponse(`{"accounts":[]}`); err == nil {
		t.Fatal("an empty accounts array must not become an unidentified empty section")
	}
}

func TestParseStatementAcceptsQuotedAmounts(t *testing.T) {
	raw := `{"accounts":[{"account":{"name":"Synthetic Card"},"closing_balance":"$1,784.53",
		"transactions":[
			{"date":"2026-02-25","type":"Deposit","amount":"450.00-","description":"PAYMENT THANK YOU"},
			{"date":"2026-02-26","type":"Withdrawal","amount":"1.234,56 €","description":"SYNTHETIC STORE"},
			{"date":"2026-02-27","type":"Withdrawal","amount":12.5,"description":"SYNTHETIC CAFE"}]},
		{"account":{"name":"Synthetic Savings"},"closing_balance":"","transactions":[]}]}`
	sections, _, err := parseStatementSectionsResponse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if h := sections[0].Header; !h.HasClosing || h.ClosingBalance != 1784.53 {
		t.Fatalf("closing balance = %+v", h)
	}
	var amounts []float64
	for _, c := range sections[0].Candidates {
		amounts = append(amounts, c.Amount)
	}
	if !slices.Equal(amounts, []float64{-450, 1234.56, 12.5}) {
		t.Fatalf("amounts = %v", amounts)
	}
	if sections[1].Header.HasClosing {
		t.Fatal("a blank closing balance must count as not printed")
	}

	for text, want := range map[string]float64{
		"1 234,56": 1234.56, "1'234.50": 1234.5, "(35.00)": -35, "EUR 7.89": 7.89,
		"1.234.567": 1234567, "0,5": 0.5, "1,234": 1234, ",15": 0.15,
	} {
		if got, ok := parseStatementAmount(text); !ok || got != want {
			t.Errorf("parseStatementAmount(%q) = %v, %v; want %v", text, got, ok, want)
		}
	}
	if _, ok := parseStatementAmount("n/a"); ok {
		t.Error("parseStatementAmount accepted text without digits")
	}
}

func TestSettleStatementYearsMovesFutureStatementsBack(t *testing.T) {
	sections := []statementSection{
		{ // No year printed: the model used the current year, a month too late.
			Header:     statementHeader{PeriodStart: "2026-10-10", PeriodEnd: "2026-11-09", ClosingDate: "2026-11-09"},
			Candidates: []importCandidate{{Date: "2026-10-02"}, {Date: "2026-11-09"}},
			RawJSON:    `[{"date":"2026-10-02"}]`,
		},
		{ // Ends today: left alone.
			Header:     statementHeader{PeriodEnd: "2026-10-14"},
			Candidates: []importCandidate{{Date: "2026-10-01"}},
		},
		{ // No header dates: the latest row decides; 29 February stays in February.
			Candidates: []importCandidate{{Date: "2028-02-29"}, {Date: "2027-12-31"}},
		},
	}
	settleStatementYears(sections, "2026-10-14")

	if h := sections[0].Header; h.PeriodStart != "2025-10-10" || h.PeriodEnd != "2025-11-09" || h.ClosingDate != "2025-11-09" {
		t.Fatalf("header = %+v", h)
	}
	if sections[0].Candidates[0].Date != "2025-10-02" || !strings.Contains(sections[0].RawJSON, "2025-10-02") {
		t.Fatalf("rows or RawJSON not moved: %+v %s", sections[0].Candidates, sections[0].RawJSON)
	}
	if sections[1].Header.PeriodEnd != "2026-10-14" || sections[1].Candidates[0].Date != "2026-10-01" {
		t.Fatalf("a statement ending today was moved: %+v", sections[1])
	}
	if got := []string{sections[2].Candidates[0].Date, sections[2].Candidates[1].Date}; !slices.Equal(got, []string{"2026-02-28", "2025-12-31"}) {
		t.Fatalf("dates = %v", got)
	}
}

func TestSharedStatementDocumentScopesSectionsAndAssignments(t *testing.T) {
	a, err := newApp(config{DBPath: fixtureDB(t), BackupDir: "backups", StatementDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	data := []byte("synthetic multi-account PDF bytes")
	sections := []statementSection{
		{
			Header:     statementHeader{AccountName: "Synthetic Cash", AccountNumber: "11111111", PeriodStart: "2026-06-01", PeriodEnd: "2026-06-30", ClosingDate: "2026-06-30", ClosingBalance: 25, HasClosing: true},
			Candidates: []importCandidate{{Date: "2026-06-06", Type: "Deposit", Amount: 10, Description: "CASH SECTION ONLY"}},
		},
		{
			Header:     statementHeader{AccountName: "Synthetic Card", AccountNumber: "22222222", PeriodStart: "2026-06-01", PeriodEnd: "2026-06-30", ClosingDate: "2026-06-30", ClosingBalance: 0.01, HasClosing: true},
			Candidates: []importCandidate{},
		},
	}
	doc, attachments, err := a.archiveStatementDocument("combined.pdf", data, sections, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(attachments) != 2 || len(doc.Sections) != 2 {
		t.Fatalf("document sections/attachments = %d/%d", len(doc.Sections), len(attachments))
	}
	if attachments[0].Confirmed || attachments[1].Confirmed {
		t.Fatal("multi-account attachments must remain pending until the mapping form is confirmed")
	}
	if _, err := os.Stat(a.statementDocumentBlobPath(doc.ContentHash, doc.File)); err != nil {
		t.Fatalf("shared document blob: %v", err)
	}
	if matches, _ := filepath.Glob(filepath.Join(a.config.StatementDir, "*", "*.pdf")); len(matches) != 0 {
		t.Fatalf("account directories contain duplicated PDF blobs: %v", matches)
	}

	for i, accountID := range []int{12, 2} {
		updated, err := a.confirmStatement(attachments[i], accountID)
		if err != nil {
			t.Fatal(err)
		}
		attachments[i] = updated
	}
	checking := a.statementsForMonth(12, "2026-06-01", "2026-06-30")
	card := a.statementsForMonth(2, "2026-06-01", "2026-06-30")
	if len(checking) != 1 || len(checking[0].Candidates) != 1 || checking[0].Candidates[0].Description != "CASH SECTION ONLY" {
		t.Fatalf("checking section = %+v", checking)
	}
	if len(card) != 1 || len(card[0].Candidates) != 0 || !card[0].HasClosing {
		t.Fatalf("empty card section = %+v", card)
	}
	key := archivedCandidateKey(attachments[0].Candidates[0])
	if _, err := dismissArchivedCandidate(&attachments[0], 0, key); err != nil {
		t.Fatal(err)
	}
	if err := a.writeStatementSidecar(&attachments[0]); err != nil {
		t.Fatal(err)
	}
	reused := a.statementDocumentUploadResult(doc, attachments, "2026-06", "combined.pdf", true, 12)
	if len(reused.Candidates) != 0 {
		t.Fatalf("cached shared document restored a dismissed attachment candidate: %+v", reused.Candidates)
	}

	request := httptest.NewRequest(http.MethodGet, "/review/statement?account_id=2&file="+attachments[1].File, nil)
	recorder := httptest.NewRecorder()
	a.downloadStatement(recorder, request)
	if recorder.Code != http.StatusOK || !bytes.Equal(recorder.Body.Bytes(), data) {
		t.Fatalf("shared document download status/body = %d/%q", recorder.Code, recorder.Body.Bytes())
	}
	if err := a.deleteArchivedStatement(attachments[1]); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(a.statementDocumentBlobPath(doc.ContentHash, doc.File)); err != nil {
		t.Fatalf("detaching one section removed the shared document: %v", err)
	}
	if got := a.listArchivedStatements(12); len(got) != 1 {
		t.Fatalf("detaching card affected checking attachment: %d", len(got))
	}
	if err := a.deleteStatementDocument(doc); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(a.statementDocumentDir(doc.ContentHash)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("shared document directory remains after full delete: %v", err)
	}
	if got := a.listArchivedStatements(12); len(got) != 0 {
		t.Fatalf("full document delete left account attachments: %d", len(got))
	}
}

func TestConfirmSharedStatementSectionsWorkflow(t *testing.T) {
	a, err := newApp(config{DBPath: fixtureDB(t), BackupDir: "backups", StatementDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	doc, _, err := a.archiveStatementDocument("combined.pdf", []byte("mapping workflow"), []statementSection{
		{Header: statementHeader{AccountName: "Unknown Alpha", PeriodStart: "2025-10-01", PeriodEnd: "2025-10-31"}, Candidates: []importCandidate{{Date: "2025-10-02", Type: "Deposit", Amount: 1}}},
		{Header: statementHeader{AccountName: "Unknown Beta", PeriodStart: "2025-10-01", PeriodEnd: "2025-10-31"}},
	}, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{
		"document_hash":      {doc.ContentHash},
		"month":              {"2025-10"},
		"section_id":         {doc.Sections[0].ID, doc.Sections[1].ID},
		"section_account_id": {"12", "ignore"},
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/review/statements/attach", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	a.confirmStatementSections(recorder, request)
	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("confirm status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	if got := a.listArchivedStatements(12); len(got) != 1 || got[0].SectionID != doc.Sections[0].ID {
		t.Fatalf("first section assignment = %+v", got)
	}
	if got := a.listArchivedStatements(2); len(got) != 0 {
		t.Fatalf("ignored section was attached = %+v", got)
	}
	cached, err := a.loadStatementDocument(doc.ContentHash)
	if err != nil {
		t.Fatal(err)
	}
	if !cached.Sections[1].Ignored {
		t.Fatal("ignored section state was not persisted")
	}
	attachments, err := a.ensureStatementDocumentAttachments(cached)
	if err != nil {
		t.Fatal(err)
	}
	if len(attachments) != 1 || attachments[0].SectionID != doc.Sections[0].ID {
		t.Fatalf("cached re-upload recreated ignored attachment: %+v", attachments)
	}
	choices := a.statementDocumentChoices(cached, attachments)
	if len(choices) != 2 || !choices[1].Ignored {
		t.Fatalf("ignored mapping choice = %+v", choices)
	}

	// The ignored section remains in the extraction and can be attached later.
	form["section_account_id"] = []string{"12", "2"}
	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "/review/statements/attach", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	a.confirmStatementSections(recorder, request)
	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("reattach status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	if got := a.listArchivedStatements(2); len(got) != 1 || got[0].SectionID != doc.Sections[1].ID {
		t.Fatalf("reattached section assignment = %+v", got)
	}
	cached, err = a.loadStatementDocument(doc.ContentHash)
	if err != nil {
		t.Fatal(err)
	}
	if cached.Sections[1].Ignored {
		t.Fatal("reattached section remained ignored")
	}
}

func TestReviewListsAndServesArchivedStatement(t *testing.T) {
	a, err := newApp(config{
		DBPath:       fixtureDB(t),
		BackupDir:    "backups",
		StatementDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	body := []byte("%PDF-fake-statement")
	header := statementHeader{PeriodStart: "2025-10-01", PeriodEnd: "2025-10-31"}
	meta, err := a.archiveStatement(12, true, "Oct-Statement.pdf", body, header, nil, "")
	if err != nil {
		t.Fatalf("archive: %v", err)
	}

	// The review page for that account/month lists the statement with a link.
	req := httptest.NewRequest(http.MethodGet, "/review?month=2025-10&account_id=12", nil)
	rec := httptest.NewRecorder()
	a.monthlyReview(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("review status = %d", rec.Code)
	}
	html := rec.Body.String()
	if !strings.Contains(html, "Oct-Statement.pdf") {
		t.Fatalf("review page missing statement name")
	}
	if !strings.Contains(html, "/review/statement?account_id=12&file="+meta.File) {
		t.Fatalf("review page missing download link for %s", meta.File)
	}

	// The download handler serves the archived bytes and rejects unknown files.
	dl := httptest.NewRequest(http.MethodGet, "/review/statement?account_id=12&file="+meta.File, nil)
	drec := httptest.NewRecorder()
	a.downloadStatement(drec, dl)
	if drec.Code != http.StatusOK || drec.Body.String() != string(body) {
		t.Fatalf("download status=%d body=%q", drec.Code, drec.Body.String())
	}
	bad := httptest.NewRequest(http.MethodGet, "/review/statement?account_id=12&file=../escape", nil)
	brec := httptest.NewRecorder()
	a.downloadStatement(brec, bad)
	if brec.Code != http.StatusNotFound {
		t.Fatalf("traversal download status = %d, want 404", brec.Code)
	}
}

func TestDerivePendingCandidatesFromArchive(t *testing.T) {
	a, err := newApp(config{
		DBPath:       fixtureDB(t),
		WriteEnabled: true,
		BackupDir:    filepath.Join(t.TempDir(), "backups"),
		StatementDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	// One archived-statement charge is already committed as a transaction.
	if err := a.insertManualTransaction(transactionInput{
		AccountID: 12, Date: "2025-10-15", Type: "Withdrawal", Amount: 50,
		PayeeID: 14, CategoryID: 66,
	}); err != nil {
		t.Fatalf("seed committed tx: %v", err)
	}
	if err := a.insertManualTransaction(transactionInput{
		AccountID: 12, Date: "2025-10-18", Type: "Withdrawal", Amount: 25,
		PayeeID: 14,
		Splits: []splitLine{
			{CategoryID: 30, Amount: 10},
			{CategoryID: 66, Amount: 15},
		},
	}); err != nil {
		t.Fatalf("seed committed split tx: %v", err)
	}
	// Simulate a bad rule learned from a generic statement description. Pending
	// detection must match the archived extraction before this mapping rewrites
	// the payee, and must also recognize imported split parents.
	if _, err := a.db.Exec(`INSERT INTO INFOTABLE_V1 (INFONAME, INFOVALUE) VALUES (?, ?)`,
		importRulesInfoName,
		`[{"pattern":"already imported","type":"Withdrawal","payee":"Acme Utilities","category":"Leisure: Hobbies"}]`); err != nil {
		t.Fatal(err)
	}

	cands := []importCandidate{
		{Date: "2025-10-15", Type: "Withdrawal", Amount: 50, Description: "Already imported"},
		{Date: "2025-10-18", Type: "Withdrawal", Amount: 25, Payee: "Amazon", Description: "Imported split"},
		{Date: "2025-10-20", Type: "Withdrawal", Amount: 9.99, Description: "Coffee"},
		{Date: "2025-10-20", Type: "Withdrawal", Amount: 9.99, Description: "Coffee"}, // dup row across statements
		{Date: "2025-11-02", Type: "Withdrawal", Amount: 5, Description: "Next month"},
	}
	meta, err := a.archiveStatement(12, true, "Oct.pdf", []byte("oct"), statementHeader{PeriodStart: "2025-10-01", PeriodEnd: "2025-11-05"}, cands, "")
	if err != nil {
		t.Fatalf("archive: %v", err)
	}

	pending := a.derivePendingCandidates(12, "2025-10-01", "2025-10-31")
	if len(pending) != 1 {
		t.Fatalf("pending = %d (%+v), want 1", len(pending), pending)
	}
	if pending[0].Description != "Coffee" {
		t.Fatalf("pending[0] = %q, want Coffee", pending[0].Description)
	}
	if pending[0].Index != 1 {
		t.Fatalf("pending[0].Index = %d, want 1", pending[0].Index)
	}
	if pending[0].ArchiveFile != meta.File || pending[0].ArchiveCandidateIndex != 2 || pending[0].ArchiveCandidateKey == "" {
		t.Fatalf("pending archive reference = file %q index %d key %q", pending[0].ArchiveFile, pending[0].ArchiveCandidateIndex, pending[0].ArchiveCandidateKey)
	}

	dismiss := url.Values{
		"account_id":              {"12"},
		"month":                   {"2025-10"},
		"dismissed":               {"0"},
		"candidate_archive_file":  {pending[0].ArchiveFile},
		"candidate_archive_index": {strconv.Itoa(pending[0].ArchiveCandidateIndex)},
		"candidate_archive_key":   {pending[0].ArchiveCandidateKey},
	}
	req := httptest.NewRequest(http.MethodPost, "/review/candidate/dismiss", strings.NewReader(dismiss.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	a.dismissReviewCandidate(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("dismiss status = %d, body %q", rec.Code, rec.Body.String())
	}
	if got := a.derivePendingCandidates(12, "2025-10-01", "2025-10-31"); len(got) != 0 {
		t.Fatalf("dismissed candidate returned as pending: %+v", got)
	}
	reloaded, ok := a.findStatementForAccount(12, meta.File)
	if !ok || len(reloaded.Candidates) != len(cands)-2 {
		t.Fatalf("dismissed candidate was not removed from statement sidecar: ok=%v candidates=%d", ok, len(reloaded.Candidates))
	}
}

// A row imported under a different type than the statement printed (a refund
// the extraction read as a charge) must stop being pending even though the
// ledger no longer matches the extraction. A second statement overlapping the
// month lists the same row and must follow suit, and voiding the import makes
// the row pending again.
func TestImportedCandidateWithEditedTypeLeavesPending(t *testing.T) {
	a, err := newApp(config{
		DBPath:       fixtureDB(t),
		WriteEnabled: true,
		BackupDir:    filepath.Join(t.TempDir(), "backups"),
		StatementDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	cands := []importCandidate{
		{Date: "2025-10-15", Type: "Withdrawal", Amount: 42.5, Description: "Acme refund"},
		{Date: "2025-10-20", Type: "Withdrawal", Amount: 9.99, Description: "Coffee"},
	}
	header := statementHeader{PeriodStart: "2025-10-01", PeriodEnd: "2025-10-31"}
	if _, err := a.archiveStatement(12, true, "Oct.pdf", []byte("oct"), header, cands, ""); err != nil {
		t.Fatalf("archive: %v", err)
	}
	if _, err := a.archiveStatement(12, true, "Oct-copy.pdf", []byte("oct-copy"), header, cands, ""); err != nil {
		t.Fatalf("archive copy: %v", err)
	}

	pending := a.derivePendingCandidates(12, "2025-10-01", "2025-10-31")
	if len(pending) != 2 {
		t.Fatalf("pending = %d (%+v), want 2", len(pending), pending)
	}
	var refund importCandidate
	for _, c := range pending {
		if c.Description == "Acme refund" {
			refund = c
		}
	}
	if refund.ArchiveCandidateKey == "" {
		t.Fatalf("refund row missing from pending: %+v", pending)
	}

	commit := url.Values{
		"account_id":              {"12"},
		"month":                   {"2025-10"},
		"approved":                {"0"},
		"date":                    {refund.Date},
		"type":                    {"Deposit"},
		"amount":                  {"42.50"},
		"payee":                   {"Amazon"},
		"category":                {"Leisure: Hobbies"},
		"status":                  {"Reconciled"},
		"candidate_archive_file":  {refund.ArchiveFile},
		"candidate_archive_index": {strconv.Itoa(refund.ArchiveCandidateIndex)},
		"candidate_archive_key":   {refund.ArchiveCandidateKey},
	}
	rec := httptest.NewRecorder()
	a.commitReviewImport(rec, formRequest(commit))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("commit status = %d, body %q", rec.Code, rec.Body.String())
	}
	var transID int64
	if err := a.db.QueryRow(`SELECT TRANSID FROM CHECKINGACCOUNT_V1
	  WHERE ACCOUNTID = 12 AND TRANSCODE = 'Deposit' AND ABS(TRANSAMOUNT - 42.5) < 0.005`).Scan(&transID); err != nil {
		t.Fatalf("imported deposit not found: %v", err)
	}

	pending = a.derivePendingCandidates(12, "2025-10-01", "2025-10-31")
	if len(pending) != 1 || pending[0].Description != "Coffee" {
		t.Fatalf("after import pending = %+v, want only Coffee", pending)
	}

	if _, err := a.db.Exec(`UPDATE CHECKINGACCOUNT_V1 SET STATUS = 'Void' WHERE TRANSID = ?`, transID); err != nil {
		t.Fatal(err)
	}
	if got := a.derivePendingCandidates(12, "2025-10-01", "2025-10-31"); len(got) != 2 {
		t.Fatalf("after void pending = %+v, want the refund back", got)
	}
}

// The Review page pre-flights an import against validateReviewImport so a
// rejection can be shown in a dialog instead of replacing the page (and every
// unsaved edit on it) with a bare error response. The endpoint must agree with
// the commit about what is acceptable, and must never write.
func TestValidateReviewImportRejectsWithoutWriting(t *testing.T) {
	a, err := newApp(config{
		DBPath:       fixtureDB(t),
		WriteEnabled: true,
		BackupDir:    filepath.Join(t.TempDir(), "backups"),
		StatementDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	before := a.count("CHECKINGACCOUNT_V1")

	valid := url.Values{
		"account_id": {"12"},
		"month":      {"2025-10"},
		"approved":   {"0"},
		"date":       {"2025-10-31"},
		"type":       {"Withdrawal"},
		"amount":     {"9.87"},
		"payee":      {"Amazon"},
		"category":   {"Leisure: Hobbies"},
		"notes":      {"preflight candidate"},
		"status":     {"Reconciled"},
	}

	rec := httptest.NewRecorder()
	a.validateReviewImport(rec, formRequest(valid))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("valid preflight status = %d, want %d; body %q", rec.Code, http.StatusNoContent, rec.Body.String())
	}

	// Nothing ticked: the message the Review page shows in its dialog.
	empty := url.Values{}
	for k, v := range valid {
		empty[k] = v
	}
	delete(empty, "approved")
	rec = httptest.NewRecorder()
	a.validateReviewImport(rec, formRequest(empty))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty preflight status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	if !strings.Contains(rec.Body.String(), "select at least one import candidate") {
		t.Fatalf("empty preflight body = %q", rec.Body.String())
	}

	// An invalid row is rejected with the same per-candidate message the commit
	// would have produced.
	badCategory := url.Values{}
	for k, v := range valid {
		badCategory[k] = v
	}
	badCategory["category"] = []string{"No Such Category"}
	rec = httptest.NewRecorder()
	a.validateReviewImport(rec, formRequest(badCategory))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid preflight status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	if !strings.Contains(rec.Body.String(), "candidate 1") {
		t.Fatalf("invalid preflight body = %q, want a per-candidate message", rec.Body.String())
	}

	if after := a.count("CHECKINGACCOUNT_V1"); after != before {
		t.Fatalf("preflight wrote to the database: %d transactions before, %d after", before, after)
	}

	// Read-only mode must reject before any of that.
	a.config.WriteEnabled = false
	rec = httptest.NewRecorder()
	a.validateReviewImport(rec, formRequest(valid))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("read-only preflight status = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

func TestReviewImportPreflightAssets(t *testing.T) {
	for _, want := range []string{
		`data-preflight="/review/import/validate"`,
		`data-require-checked="approved"`,
		`data-require-checked-message=`,
		`data-import-submit`,
		`data-import-count`,
		// Ties an approved/dismissed value back to its row after an in-place
		// dismissal has shifted the submitted field positions.
		`<input type="hidden" name="row_index" value="{{$i}}">`,
	} {
		if !strings.Contains(templates, want) {
			t.Fatalf("import form missing %q", want)
		}
	}
	for _, want := range []string{
		`function preflightSubmission`,
		`function localSubmissionError`,
		`function updateImportButton`,
		`form.dataset.preflight`,
		// A transport failure must not be able to block an import outright; the
		// real handler still rejects anything invalid.
		`}).catch(function () {
    return true;
  });`,
	} {
		if !strings.Contains(js, want) {
			t.Fatalf("import preflight script missing %q", want)
		}
	}
}

// Dismissing a candidate in place removes its row from the DOM, so every row
// after it shifts down one position in the submitted field arrays while keeping
// the value it was rendered with. The submitted row_index is what ties an
// approved/dismissed value back to the row it came from; without it the server
// reads a neighbour's fields and imports (or dismisses) the wrong transaction.
func TestImportRowIdentitySurvivesInPlaceDismissal(t *testing.T) {
	a, err := newApp(config{
		DBPath:       fixtureDB(t),
		WriteEnabled: true,
		BackupDir:    filepath.Join(t.TempDir(), "backups"),
		StatementDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	// Three candidates were rendered (identities 0, 1, 2) and the first was then
	// dismissed in place: its fields are gone, the other two kept their values.
	form := url.Values{
		"account_id": {"12"},
		"month":      {"2025-10"},
		"row_index":  {"1", "2"},
		"approved":   {"2"},
		"date":       {"2025-10-15", "2025-10-16"},
		"type":       {"Withdrawal", "Withdrawal"},
		"amount":     {"11.11", "22.22"},
		"payee":      {"Amazon", "Amazon"},
		"category":   {"Leisure: Hobbies", "Leisure: Hobbies"},
		"notes":      {"row one", "row two"},
		"status":     {"Reconciled", "Reconciled"},
	}

	inputs, err := a.checkImportSubmission(formRequest(form))
	if err != nil {
		t.Fatalf("import after in-place dismissal: %v", err)
	}
	if len(inputs) != 1 {
		t.Fatalf("imported %d candidates, want 1", len(inputs))
	}
	if inputs[0].Notes != "row two" || inputs[0].Amount != 22.22 {
		t.Fatalf("imported the wrong candidate: notes=%q amount=%.2f, want %q/22.22", inputs[0].Notes, inputs[0].Amount, "row two")
	}

	// The row that was left unticked is the one that must survive for a
	// subsequent import, and it must be the row the user actually left alone.
	remaining := a.remainingImportCandidates(formRequest(form))
	if len(remaining) != 1 {
		t.Fatalf("remaining = %d candidates, want 1", len(remaining))
	}
	if remaining[0].Notes != "row one" {
		t.Fatalf("remaining candidate = %q, want %q", remaining[0].Notes, "row one")
	}
}

// The same shift breaks dismissal itself: the archive reference is read from the
// positional arrays, so a stale row value would retire a different candidate
// than the one whose button was pressed.
func TestDismissReviewCandidateResolvesRowAfterInPlaceDismissal(t *testing.T) {
	a, err := newApp(config{
		DBPath:       fixtureDB(t),
		WriteEnabled: true,
		BackupDir:    filepath.Join(t.TempDir(), "backups"),
		StatementDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	meta, err := a.archiveStatement(12, true, "Oct.pdf", []byte("oct"), statementHeader{
		PeriodStart: "2025-10-01",
		PeriodEnd:   "2025-10-31",
	}, []importCandidate{
		{Date: "2025-10-10", Type: "Withdrawal", Amount: 1.11, Description: "first"},
		{Date: "2025-10-11", Type: "Withdrawal", Amount: 2.22, Description: "second"},
		{Date: "2025-10-12", Type: "Withdrawal", Amount: 3.33, Description: "third"},
	}, "")
	if err != nil {
		t.Fatalf("archive: %v", err)
	}
	stored := archivedStatementCandidates(meta)

	// Row 0 was already dismissed in place, so only rows 1 and 2 are submitted -
	// but the button for row 2 still carries the value it was rendered with.
	form := url.Values{
		"account_id":              {"12"},
		"month":                   {"2025-10"},
		"dismissed":               {"2"},
		"row_index":               {"1", "2"},
		"candidate_archive_file":  {stored[1].ArchiveFile, stored[2].ArchiveFile},
		"candidate_archive_index": {strconv.Itoa(stored[1].ArchiveCandidateIndex), strconv.Itoa(stored[2].ArchiveCandidateIndex)},
		"candidate_archive_key":   {stored[1].ArchiveCandidateKey, stored[2].ArchiveCandidateKey},
	}
	req := httptest.NewRequest(http.MethodPost, "/review/candidate/dismiss", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-MMXC-Partial", "true")
	rec := httptest.NewRecorder()

	a.dismissReviewCandidate(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("dismiss status = %d, want %d; body %q", rec.Code, http.StatusNoContent, rec.Body.String())
	}
	reloaded, ok := a.findStatementForAccount(12, meta.File)
	if !ok {
		t.Fatal("statement disappeared")
	}
	left := archivedStatementCandidates(reloaded)
	if len(left) != 2 {
		t.Fatalf("candidates left = %d, want 2", len(left))
	}
	for _, c := range left {
		if c.Description == "third" {
			t.Fatal("dismissed the wrong candidate: \"third\" should be gone")
		}
	}
}

func TestDismissReviewCandidateSupportsPartialResponse(t *testing.T) {
	a, err := newApp(config{
		DBPath:       fixtureDB(t),
		WriteEnabled: true,
		BackupDir:    filepath.Join(t.TempDir(), "backups"),
		StatementDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	meta, err := a.archiveStatement(12, true, "Oct.pdf", []byte("oct"), statementHeader{
		PeriodStart: "2025-10-01",
		PeriodEnd:   "2025-10-31",
	}, []importCandidate{{
		Date: "2025-10-20", Type: "Withdrawal", Amount: 9.99, Description: "Coffee",
	}}, "")
	if err != nil {
		t.Fatalf("archive: %v", err)
	}
	candidate := archivedStatementCandidates(meta)[0]
	form := url.Values{
		"account_id":              {"12"},
		"month":                   {"2025-10"},
		"dismissed":               {"0"},
		"candidate_archive_file":  {candidate.ArchiveFile},
		"candidate_archive_index": {strconv.Itoa(candidate.ArchiveCandidateIndex)},
		"candidate_archive_key":   {candidate.ArchiveCandidateKey},
	}
	req := httptest.NewRequest(http.MethodPost, "/review/candidate/dismiss", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-MMXC-Partial", "true")
	rec := httptest.NewRecorder()

	a.dismissReviewCandidate(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("partial dismiss status = %d, want %d; body %q", rec.Code, http.StatusNoContent, rec.Body.String())
	}
	if location := rec.Header().Get("Location"); location != "" {
		t.Fatalf("partial dismiss redirected to %q", location)
	}
	reloaded, ok := a.findStatementForAccount(12, meta.File)
	if !ok || len(reloaded.Candidates) != 0 {
		t.Fatalf("partial dismiss did not update archive: ok=%v candidates=%d", ok, len(reloaded.Candidates))
	}
}

func TestConflictingImportRuleBecomesAmbiguous(t *testing.T) {
	a, err := newApp(config{
		DBPath:       fixtureDB(t),
		WriteEnabled: true,
		BackupDir:    filepath.Join(t.TempDir(), "backups"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	inputs := []transactionInput{
		{
			AccountID: 12, Date: "2025-12-01", Type: "Withdrawal", Amount: 11,
			PayeeID: 14, PayeeName: "Amazon", OriginalPayee: "Generic card purchase",
			CategoryID: 66, CategoryName: "Leisure: Hobbies",
		},
		{
			AccountID: 12, Date: "2025-12-02", Type: "Withdrawal", Amount: 12,
			PayeeID: 2, PayeeName: "Grocery Mart", OriginalPayee: "Generic card purchase",
			CategoryID: 30, CategoryName: "Food: Groceries",
		},
	}
	if _, err := a.insertImportedTransactions(inputs, "", "", 12); err != nil {
		t.Fatal(err)
	}
	rules := a.loadImportRules()
	found := false
	for _, rule := range rules {
		if rule.Pattern == "generic card purchase" {
			found = true
			if !importRuleEmpty(rule) {
				t.Fatalf("conflicting generic rule was retained: %+v", rule)
			}
		}
	}
	if !found {
		t.Fatal("conflicting rule should leave an ambiguity tombstone")
	}
}

func TestAmbiguousLearnedRuleDoesNotRewriteStatementBatch(t *testing.T) {
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
		`[{"pattern":"generic card purchase","type":"Withdrawal","payee":"Amazon","category":"Leisure: Hobbies"}]`); err != nil {
		t.Fatal(err)
	}
	candidates := []importCandidate{
		{Date: "2025-12-01", Type: "Withdrawal", Amount: 11, Description: "Generic card purchase", Payee: "Coffee Stop", Category: "Food: Misc"},
		{Date: "2025-12-02", Type: "Withdrawal", Amount: 12, Description: "Generic card purchase", Payee: "Grocery Mart", Category: "Food: Groceries"},
	}
	a.prepareImportCandidatesNoLLM(12, candidates)
	if candidates[0].Payee != "Coffee Stop" || candidates[1].Payee != "Grocery Mart" {
		t.Fatalf("ambiguous rule rewrote extracted payees: %+v", candidates)
	}
}

func TestMonthCoverage(t *testing.T) {
	st := func(s, e string) archivedStatement { return archivedStatement{PeriodStart: s, PeriodEnd: e} }
	for _, tc := range []struct {
		name        string
		statements  []archivedStatement
		wantFull    bool
		wantThrough string
	}{
		{"none", nil, false, ""},
		{"full single", []archivedStatement{st("2025-10-01", "2025-10-31")}, true, "2025-10-31"},
		{"credit-card gap to month end", []archivedStatement{st("2025-09-15", "2025-10-14")}, false, "2025-10-14"},
		{"two statements close the month", []archivedStatement{st("2025-09-15", "2025-10-14"), st("2025-10-15", "2025-11-14")}, true, "2025-10-31"},
		{"internal gap", []archivedStatement{st("2025-10-01", "2025-10-10"), st("2025-10-20", "2025-10-31")}, false, "2025-10-10"},
		{"gap at start", []archivedStatement{st("2025-10-05", "2025-10-31")}, false, ""},
	} {
		full, through := monthCoverage(tc.statements, "2025-10-01", "2025-10-31")
		if full != tc.wantFull || through != tc.wantThrough {
			t.Fatalf("%s: full=%v through=%q, want full=%v through=%q", tc.name, full, through, tc.wantFull, tc.wantThrough)
		}
	}
}

// MMEX carries a credit card balance as a negative (what is owed) while the
// statement prints it as a positive — the extraction prompt asks for the balance
// "signed as printed". The comparison has to convert between the two, or a card
// can never reconcile.
func TestCreditCardStatementReconciles(t *testing.T) {
	a, err := newApp(config{
		DBPath:       fixtureDB(t),
		WriteEnabled: true,
		BackupDir:    filepath.Join(t.TempDir(), "backups"),
		StatementDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	// Account 2 is the fixture's Credit Card and starts empty, so give it
	// something to owe: the bug is invisible at a zero balance.
	if err := a.insertManualTransaction(transactionInput{
		AccountID:  2,
		Date:       "2025-10-05",
		Type:       "Withdrawal",
		Amount:     120.50,
		PayeeID:    14,
		PayeeName:  "Amazon",
		CategoryID: 66,
		Notes:      "card purchase",
	}); err != nil {
		t.Fatalf("seed card activity: %v", err)
	}

	const closing = "2025-10-31"
	ledger, err := a.balanceAt(2, closing)
	if err != nil {
		t.Fatal(err)
	}
	if ledger >= 0 {
		t.Fatalf("card ledger balance = %.2f, want negative (amount owed)", ledger)
	}
	// What the statement would print for that same position.
	printed := -ledger

	if _, err := a.archiveStatement(2, true, "card.pdf", []byte("card"), statementHeader{
		PeriodStart:    "2025-10-01",
		PeriodEnd:      closing,
		ClosingDate:    closing,
		ClosingBalance: printed,
		HasClosing:     true,
	}, nil, ""); err != nil {
		t.Fatalf("archive: %v", err)
	}

	rows, err := a.listMonthlyReviewAccounts("2025-10-01", closing)
	if err != nil {
		t.Fatal(err)
	}
	var card *monthlyReviewAccount
	for i := range rows {
		if rows[i].Account.ID == 2 {
			card = &rows[i]
		}
	}
	if card == nil {
		t.Fatal("credit card row missing from the review")
	}
	if !card.HasStatementClosing {
		t.Fatal("card row did not pick up the statement closing balance")
	}
	if math.Abs(card.ClosingDelta) > 0.005 || !card.ClosingReconciled {
		t.Fatalf("card should reconcile: delta = %.2f, reconciled = %v (ledger %.2f vs printed %.2f)",
			card.ClosingDelta, card.ClosingReconciled, ledger, printed)
	}
	// The displayed figure sits next to the computed Closing Balance, so it must
	// read in the same (ledger) convention rather than as printed.
	if math.Abs(card.StatementClosingBalance-ledger) > 0.005 {
		t.Fatalf("StatementClosingBalance = %.2f, want ledger convention %.2f", card.StatementClosingBalance, ledger)
	}
}

// A card cycle closing mid-month puts two statements in view of the same month:
// the one that closed inside it and the one still running past its end. The
// month has to be reconciled against the cycle that actually closed in it —
// measuring July against an August closing reports the missing August activity
// as a July discrepancy.
func TestReviewReconcilesAgainstTheMonthsOwnClosing(t *testing.T) {
	a, err := newApp(config{
		DBPath:       fixtureDB(t),
		WriteEnabled: true,
		BackupDir:    filepath.Join(t.TempDir(), "backups"),
		StatementDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	// July is fully entered; August is not, which is what makes the two closings
	// disagree with the ledger by different amounts.
	if err := a.insertManualTransaction(transactionInput{
		AccountID:  2,
		Date:       "2025-07-10",
		Type:       "Withdrawal",
		Amount:     100,
		PayeeID:    14,
		PayeeName:  "Amazon",
		CategoryID: 66,
	}); err != nil {
		t.Fatalf("seed card activity: %v", err)
	}

	julyLedger, err := a.balanceAt(2, "2025-07-23")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.archiveStatement(2, true, "july.pdf", []byte("july"), statementHeader{
		PeriodStart:    "2025-06-24",
		PeriodEnd:      "2025-07-23",
		ClosingDate:    "2025-07-23",
		ClosingBalance: -julyLedger, // as the card prints it: the amount owed
		HasClosing:     true,
	}, nil, ""); err != nil {
		t.Fatalf("archive july: %v", err)
	}
	augLedger, err := a.balanceAt(2, "2025-08-23")
	if err != nil {
		t.Fatal(err)
	}
	const augGap = 250.0 // August charges the ledger has not been told about yet
	if _, err := a.archiveStatement(2, true, "august.pdf", []byte("august"), statementHeader{
		PeriodStart:    "2025-07-24",
		PeriodEnd:      "2025-08-23",
		ClosingDate:    "2025-08-23",
		ClosingBalance: -augLedger + augGap,
		HasClosing:     true,
	}, nil, ""); err != nil {
		t.Fatalf("archive august: %v", err)
	}

	cardRow := func(start, end string) monthlyReviewAccount {
		t.Helper()
		rows, err := a.listMonthlyReviewAccounts(start, end)
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range rows {
			if row.Account.ID == 2 {
				return row
			}
		}
		t.Fatalf("credit card row missing from the review for %s", start)
		return monthlyReviewAccount{}
	}

	july := cardRow("2025-07-01", "2025-07-31")
	if july.StatementClosingDate != "2025-07-23" {
		t.Fatalf("July reconciles against %q, want the 2025-07-23 closing", july.StatementClosingDate)
	}
	if !july.ClosingReconciled || math.Abs(july.ClosingDelta) > 0.005 {
		t.Fatalf("July should reconcile: delta = %.2f, reconciled = %v", july.ClosingDelta, july.ClosingReconciled)
	}

	august := cardRow("2025-08-01", "2025-08-31")
	if august.StatementClosingDate != "2025-08-23" {
		t.Fatalf("August reconciles against %q, want the 2025-08-23 closing", august.StatementClosingDate)
	}
	if math.Abs(august.ClosingDelta-augGap) > 0.005 {
		t.Fatalf("August delta = %.2f, want the %.2f of unentered activity", august.ClosingDelta, augGap)
	}
}

// A month the open cycle only runs through has no closing of its own. Borrowing
// the next one's would show a delta for activity that has not happened yet, so
// the month carries no statement closing figure at all.
func TestReviewHasNoClosingBeforeTheCycleCloses(t *testing.T) {
	a, err := newApp(config{
		DBPath:       fixtureDB(t),
		WriteEnabled: true,
		BackupDir:    filepath.Join(t.TempDir(), "backups"),
		StatementDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	if _, err := a.archiveStatement(2, true, "august.pdf", []byte("august"), statementHeader{
		PeriodStart:    "2025-07-24",
		PeriodEnd:      "2025-08-23",
		ClosingDate:    "2025-08-23",
		ClosingBalance: 42,
		HasClosing:     true,
	}, nil, ""); err != nil {
		t.Fatalf("archive: %v", err)
	}

	rows, err := a.listMonthlyReviewAccounts("2025-07-01", "2025-07-31")
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.Account.ID != 2 {
			continue
		}
		if row.HasStatementClosing {
			t.Fatalf("July borrowed the %s closing (delta %.2f)", row.StatementClosingDate, row.ClosingDelta)
		}
		// The statement still counts as coverage for the part of July it spans.
		if row.ReviewState == "none" {
			t.Fatalf("July should still show the statement's partial coverage, got %q", row.ReviewStatus)
		}
		return
	}
	t.Fatal("credit card row missing from the review")
}

// A chequing account's statement is printed in the same direction the ledger
// carries it, so the conversion must not touch it.
func TestCheckingStatementReconcilesUnconverted(t *testing.T) {
	a, err := newApp(config{
		DBPath:       fixtureDB(t),
		WriteEnabled: true,
		BackupDir:    filepath.Join(t.TempDir(), "backups"),
		StatementDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	const closing = "2025-10-31"
	ledger, err := a.balanceAt(12, closing)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.archiveStatement(12, true, "chq.pdf", []byte("chq"), statementHeader{
		PeriodStart:    "2025-10-01",
		PeriodEnd:      closing,
		ClosingDate:    closing,
		ClosingBalance: ledger,
		HasClosing:     true,
	}, nil, ""); err != nil {
		t.Fatalf("archive: %v", err)
	}

	rows, err := a.listMonthlyReviewAccounts("2025-10-01", closing)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.Account.ID != 12 {
			continue
		}
		if math.Abs(row.ClosingDelta) > 0.005 || !row.ClosingReconciled {
			t.Fatalf("chequing should reconcile: delta = %.2f, reconciled = %v", row.ClosingDelta, row.ClosingReconciled)
		}
		return
	}
	t.Fatal("chequing row missing from the review")
}

// The import banner's closing balance is round-tripped through a hidden form
// field, so the value the check publishes is fed straight back into it on the
// next render. Converting the sign there rather than at the boundary would flip
// it a second time and report a false mismatch on every render after the first.
func TestClosingBalanceCheckIsStableAcrossRoundTrip(t *testing.T) {
	a, err := newApp(config{
		DBPath:       fixtureDB(t),
		WriteEnabled: true,
		BackupDir:    filepath.Join(t.TempDir(), "backups"),
		StatementDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	if err := a.insertManualTransaction(transactionInput{
		AccountID:  2,
		Date:       "2025-10-05",
		Type:       "Withdrawal",
		Amount:     120.50,
		PayeeID:    14,
		PayeeName:  "Amazon",
		CategoryID: 66,
		Notes:      "card purchase",
	}); err != nil {
		t.Fatalf("seed card activity: %v", err)
	}

	const closing = "2025-10-31"
	ledger, err := a.balanceAt(2, closing)
	if err != nil {
		t.Fatal(err)
	}

	// First render: the printed balance arrives from extraction.
	first := monthlyReviewData{}
	a.applyClosingBalanceCheck(&first, 2, closing, a.statementBalanceToLedger(2, -ledger), nil)
	if !first.ClosingBalanceChecked {
		t.Fatal("closing balance check did not run")
	}
	if !first.ClosingBalanceMatches {
		t.Fatalf("first render should match: expected %.2f, projected %.2f", first.StatementClosingBalance, first.ProjectedClosingBalance)
	}

	// Second render: the template hands back what the first render published.
	second := monthlyReviewData{}
	a.applyClosingBalanceCheck(&second, 2, closing, first.StatementClosingBalance, nil)
	if !second.ClosingBalanceMatches {
		t.Fatalf("round trip flipped the sign: first %.2f, second %.2f", first.StatementClosingBalance, second.StatementClosingBalance)
	}
	if math.Abs(first.StatementClosingBalance-second.StatementClosingBalance) > 0.005 {
		t.Fatalf("closing balance not stable: %.2f then %.2f", first.StatementClosingBalance, second.StatementClosingBalance)
	}
}

// A balance at an arbitrary month boundary answers nothing about a credit card,
// so the review shows a card only its activity for the month. The reconciliation
// figures are still computed — the account list's badge uses them — but none of
// the balance cards are offered to the template.
func TestLiabilityRowsSuppressBalanceCards(t *testing.T) {
	a, err := newApp(config{
		DBPath:       fixtureDB(t),
		WriteEnabled: true,
		BackupDir:    filepath.Join(t.TempDir(), "backups"),
		StatementDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	// A statement with a closing balance on both a card and a chequing account,
	// so the difference between the rows is the account type and nothing else.
	for _, acct := range []int{2, 12} {
		if _, err := a.archiveStatement(acct, true, "s.pdf", []byte("s"), statementHeader{
			PeriodStart:    "2025-10-13",
			PeriodEnd:      "2025-10-31",
			ClosingDate:    "2025-10-31",
			ClosingBalance: 100,
			HasClosing:     true,
		}, nil, ""); err != nil {
			t.Fatalf("archive for %d: %v", acct, err)
		}
	}

	rows, err := a.listMonthlyReviewAccounts("2025-10-01", "2025-10-31")
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, row := range rows {
		switch row.Account.ID {
		case 2: // Rewards Credit Card
			seen++
			if !row.IsLiability {
				t.Fatal("credit card row should be flagged a liability")
			}
			if row.ShowStatementOpening || row.ShowStatementClosing || row.ClosingMatchesStatement {
				t.Fatalf("credit card should offer no balance cards: opening=%v closing=%v match=%v",
					row.ShowStatementOpening, row.ShowStatementClosing, row.ClosingMatchesStatement)
			}
			// Still computed, because the account list badge reads them.
			if !row.HasStatementClosing {
				t.Fatal("credit card reconciliation should still be computed for the list badge")
			}
		case 12: // Everyday Chequing
			seen++
			if row.IsLiability {
				t.Fatal("chequing row should not be flagged a liability")
			}
		}
	}
	if seen != 2 {
		t.Fatalf("expected both accounts in the review, saw %d", seen)
	}
}

// When the statement cycle is the calendar month the statement figure and the
// month figure are the same number, so the statement card is dropped and the
// reconciliation mark moves onto the month's closing card.
func TestRedundantStatementCardIsCollapsed(t *testing.T) {
	a, err := newApp(config{
		DBPath:       fixtureDB(t),
		WriteEnabled: true,
		BackupDir:    filepath.Join(t.TempDir(), "backups"),
		StatementDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	const start, end = "2025-10-01", "2025-10-31"
	ledger, err := a.balanceAt(12, end)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.archiveStatement(12, true, "aligned.pdf", []byte("a"), statementHeader{
		PeriodStart:    start,
		PeriodEnd:      end,
		ClosingDate:    end,
		ClosingBalance: ledger,
		HasClosing:     true,
	}, nil, ""); err != nil {
		t.Fatalf("archive: %v", err)
	}

	rows, err := a.listMonthlyReviewAccounts(start, end)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.Account.ID != 12 {
			continue
		}
		if row.ShowStatementClosing {
			t.Fatalf("statement closing repeats the month closing (%.2f vs %.2f) and should be collapsed",
				row.StatementClosingBalance, row.ClosingBalance)
		}
		if !row.ClosingMatchesStatement {
			t.Fatal("collapsing the card must move the reconciliation mark onto the closing balance")
		}
		if row.ShowStatementOpening {
			t.Fatal("a month-aligned cycle opens on the month boundary, so its opening card is redundant too")
		}
		return
	}
	t.Fatal("account 12 row missing from the review")
}

// The other half of the collapsing rule: a statement figure that is genuinely a
// different number from the month's keeps its own dated card, which is what makes
// the cross-check against the printed statement possible.
func TestStatementCardShownWhenTheFigureDiffers(t *testing.T) {
	a, err := newApp(config{
		DBPath:       fixtureDB(t),
		WriteEnabled: true,
		BackupDir:    filepath.Join(t.TempDir(), "backups"),
		StatementDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	// Activity inside the cycle's opening month, so the balance at the cycle start
	// differs from the one at the October boundary.
	if err := a.insertManualTransaction(transactionInput{
		AccountID:  12,
		Date:       "2025-09-25",
		Type:       "Withdrawal",
		Amount:     75,
		PayeeID:    14,
		PayeeName:  "Amazon",
		CategoryID: 66,
	}); err != nil {
		t.Fatalf("seed september activity: %v", err)
	}
	// A cycle running the 20th to the 19th: it closes inside October, so October
	// reconciles against it, and neither of its ends lands on a month boundary.
	if _, err := a.archiveStatement(12, true, "cycle.pdf", []byte("cycle"), statementHeader{
		PeriodStart:    "2025-09-20",
		PeriodEnd:      "2025-10-19",
		ClosingDate:    "2025-10-19",
		ClosingBalance: 12345,
		HasClosing:     true,
	}, nil, ""); err != nil {
		t.Fatalf("archive: %v", err)
	}

	rows, err := a.listMonthlyReviewAccounts("2025-10-01", "2025-10-31")
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.Account.ID != 12 {
			continue
		}
		if math.Abs(row.StatementOpeningBalance-row.OpeningBalance) < 0.005 {
			t.Fatalf("test premise broken: opening figures should differ (%.2f vs %.2f)",
				row.StatementOpeningBalance, row.OpeningBalance)
		}
		if !row.ShowStatementOpening {
			t.Fatal("an opening figure that differs from the month's should keep its own card")
		}
		if !row.ShowStatementClosing {
			t.Fatal("a closing figure that differs from the month's should keep its own card")
		}
		if row.ClosingMatchesStatement {
			t.Fatal("a mismatching statement must not badge the closing balance as reconciled")
		}
		return
	}
	t.Fatal("account 12 row missing from the review")
}

func TestReviewMetricsAssets(t *testing.T) {
	for _, want := range []string{
		`{{.T "Opening Balance" "account balance immediately before the reviewed month"}}`,
		`{{.T "Payments" "money paid onto a credit card or loan during the period, as opposed to income"}}`,
		`{{.T "Income" "money received during the period"}}`,
		`{{if not $selected.IsLiability}}`,
		`{{if $selected.ShowStatementOpening}}`,
		`{{if $selected.ShowStatementClosing}}`,
		`{{if $selected.ClosingMatchesStatement}}`,
	} {
		if !strings.Contains(templates, want) {
			t.Fatalf("review metrics missing %q", want)
		}
	}
	// The mark is reused on a metric card, so its styling must not stay scoped to
	// the account list.
	if strings.Contains(css, `.review-amount .recon`) {
		t.Fatal(".recon styling should not be scoped to the account list any more")
	}
	if !strings.Contains(css, ".recon.ok") {
		t.Fatal(".recon styling missing")
	}
	// Several statements can be in view for one month, so the mark says which
	// closing it is about on screen and not only in the tooltip.
	if !strings.Contains(templates, `<span class="recon-date">{{$.DateFmt .StatementClosingDate}}</span>`) {
		t.Fatal("the reconciliation mark should name the closing date it refers to")
	}
	if !strings.Contains(css, ".recon-date") {
		t.Fatal(".recon-date styling missing")
	}
}

// The opening figure is there to be checked against the balance printed at the
// top of the statement, so it has to be anchored to the statement's own period
// rather than the calendar month — the two only coincide when the cycle happens
// to be a calendar month, which is exactly what card cycles are not.
func TestStatementOpeningBalanceFollowsTheCycle(t *testing.T) {
	a, err := newApp(config{
		DBPath:       fixtureDB(t),
		WriteEnabled: true,
		BackupDir:    filepath.Join(t.TempDir(), "backups"),
		StatementDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	// A card cycle straddling two months, like the dev archive's 13th-to-12th.
	if _, err := a.archiveStatement(12, true, "cycle.pdf", []byte("cycle"), statementHeader{
		PeriodStart:    "2025-10-13",
		PeriodEnd:      "2025-11-12",
		ClosingDate:    "2025-11-12",
		ClosingBalance: 100,
		HasClosing:     true,
	}, nil, ""); err != nil {
		t.Fatalf("archive: %v", err)
	}

	rows, err := a.listMonthlyReviewAccounts("2025-10-01", "2025-10-31")
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.Account.ID != 12 {
			continue
		}
		if row.StatementOpeningDate != "2025-10-13" {
			t.Fatalf("StatementOpeningDate = %q, want the cycle start 2025-10-13", row.StatementOpeningDate)
		}
		want, err := a.balanceAt(12, "2025-10-12")
		if err != nil {
			t.Fatal(err)
		}
		if math.Abs(row.StatementOpeningBalance-want) > 0.005 {
			t.Fatalf("StatementOpeningBalance = %.2f, want the balance at 2025-10-12 (%.2f)", row.StatementOpeningBalance, want)
		}
		// Anchoring and display are separate questions: here the cycle starts on
		// the 13th but the fixture's only October activity is on the 15th, so the
		// figure is the same number as the month's opening and earns no card of
		// its own. TestStatementCardShownWhenTheFigureDiffers covers the other way.
		if row.ShowStatementOpening {
			t.Fatal("an opening figure identical to the month's should not get a second card")
		}
		// The month-anchored pair must be unaffected. Checked against the month
		// boundary directly: in this fixture the two happen to be equal (the only
		// October activity falls after the 12th), so comparing them to each other
		// would prove nothing.
		monthOpening, err := a.balanceAt(12, "2025-09-30")
		if err != nil {
			t.Fatal(err)
		}
		if math.Abs(row.OpeningBalance-monthOpening) > 0.005 {
			t.Fatalf("OpeningBalance = %.2f, want the month boundary %.2f", row.OpeningBalance, monthOpening)
		}
		return
	}
	t.Fatal("account 12 row missing from the review")
}

func TestMonthlyReviewShowsOpeningBalance(t *testing.T) {
	a, err := newApp(config{DBPath: fixtureDB(t), BackupDir: "backups", StatementDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	rows, err := a.listMonthlyReviewAccounts("2025-10-01", "2025-10-31")
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.Account.ID != 12 {
			continue
		}
		if math.Abs(row.OpeningBalance-885) > 0.005 {
			t.Fatalf("October opening balance = %.2f, want 885.00", row.OpeningBalance)
		}
		if math.Abs(row.OpeningBalance+row.Net-row.ClosingBalance) > 0.005 {
			t.Fatalf("opening %.2f + net %.2f != closing %.2f", row.OpeningBalance, row.Net, row.ClosingBalance)
		}
		if !strings.Contains(templates, `{{.T "Opening Balance" "account balance immediately before the reviewed month"}}`) {
			t.Fatal("Review metrics should render the opening balance")
		}
		return
	}
	t.Fatal("account 12 not found in monthly review")
}

func TestReviewStateFromCoverage(t *testing.T) {
	a, err := newApp(config{DBPath: fixtureDB(t), BackupDir: "backups", StatementDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	state := func() string {
		rows, err := a.listMonthlyReviewAccounts("2025-10-01", "2025-10-31")
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			if r.Account.ID == 12 {
				return r.ReviewState
			}
		}
		t.Fatal("account 12 not in review rows")
		return ""
	}

	if got := state(); got != "none" {
		t.Fatalf("state with no statements = %q, want none", got)
	}
	if _, err := a.archiveStatement(12, true, "partial.pdf", []byte("p"), statementHeader{PeriodStart: "2025-09-15", PeriodEnd: "2025-10-14"}, nil, ""); err != nil {
		t.Fatal(err)
	}
	if got := state(); got != "partial" {
		t.Fatalf("state with mid-month statement = %q, want partial", got)
	}
	if _, err := a.archiveStatement(12, true, "rest.pdf", []byte("r"), statementHeader{PeriodStart: "2025-10-15", PeriodEnd: "2025-11-14"}, nil, ""); err != nil {
		t.Fatal(err)
	}
	if got := state(); got != "reconciled" {
		t.Fatalf("state with full coverage = %q, want reconciled", got)
	}
}

func TestExtractStatementUploadAttachesWhenAIUnavailable(t *testing.T) {
	a, err := newApp(config{DBPath: fixtureDB(t), BackupDir: "backups", StatementDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if err := mw.WriteField("month", "2025-10"); err != nil {
		t.Fatal(err)
	}
	if err := mw.WriteField("account_id", "12"); err != nil {
		t.Fatal(err)
	}
	part, err := mw.CreateFormFile("statement", "October.pdf")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte("%PDF-1.4 synthetic statement")); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/review/extract", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if err := req.ParseMultipartForm(32 << 20); err != nil {
		t.Fatal(err)
	}

	res, err := a.extractStatementUpload(req)
	if err != nil {
		t.Fatal(err)
	}
	if res.AccountID != 12 || res.NeedsChoice {
		t.Fatalf("result account=%d needsChoice=%v, want account 12 confirmed", res.AccountID, res.NeedsChoice)
	}
	if len(res.Candidates) != 0 || res.RawJSON != "" {
		t.Fatalf("no-AI attachment candidates=%d raw=%q, want none", len(res.Candidates), res.RawJSON)
	}
	statements := a.statementsForMonth(12, "2025-10-01", "2025-10-31")
	if len(statements) != 1 {
		t.Fatalf("archived statements = %d, want 1", len(statements))
	}
	if statements[0].OriginalName != "October.pdf" || len(statements[0].Candidates) != 0 {
		t.Fatalf("archived statement = %+v, want attached PDF with no candidates", statements[0])
	}
}

func TestPendingStatementConfirmAndRelocate(t *testing.T) {
	a, err := newApp(config{DBPath: fixtureDB(t), BackupDir: "backups", StatementDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	data := []byte("pc-mastercard-statement-bytes")
	header := statementHeader{AccountName: "PC Mastercard", PeriodStart: "2025-10-01", PeriodEnd: "2025-10-31"}
	// Archived as an unconfirmed best guess under the wrong account (99).
	meta, err := a.archiveStatement(99, false, "PC.pdf", data, header, []importCandidate{{Date: "2025-10-05", Type: "Withdrawal", Amount: 10}}, "")
	if err != nil {
		t.Fatal(err)
	}
	if meta.Confirmed {
		t.Fatal("freshly guessed statement should be pending")
	}
	// A pending statement must not surface under (or count toward) any account.
	if got := a.listArchivedStatements(99); len(got) != 0 {
		t.Fatalf("pending statement leaked into account 99 list: %d", len(got))
	}
	// It is still findable by hash (dedup) and by file (confirm).
	if _, ok := a.findStatementByHash(statementContentHash(data)); !ok {
		t.Fatal("pending statement not found by hash")
	}

	// Confirming to account 12 relocates it there and out of _pending.
	updated, err := a.confirmStatement(meta, 12)
	if err != nil {
		t.Fatal(err)
	}
	if !updated.Confirmed || updated.AccountID != 12 {
		t.Fatalf("confirm: confirmed=%v account=%d", updated.Confirmed, updated.AccountID)
	}
	if got := a.statementsForMonth(12, "2025-10-01", "2025-10-31"); len(got) != 1 {
		t.Fatalf("statement not under account 12 after confirm: %d", len(got))
	}
	if left := statementsInDir(a.pendingStatementDir(), 0, false); len(left) != 0 {
		t.Fatalf("pending copy left behind after confirm: %d", len(left))
	}
	if found, ok := a.findStatementByHash(statementContentHash(data)); !ok || found.AccountID != 12 {
		t.Fatalf("after relocate, hash lookup account=%d ok=%v", found.AccountID, ok)
	}
}

func TestStatementReassignAndDeleteCorrectMapping(t *testing.T) {
	dir := t.TempDir()
	a, err := newApp(config{
		DBPath:       fixtureDB(t),
		WriteEnabled: true,
		BackupDir:    filepath.Join(dir, "backups"),
		StatementDir: filepath.Join(dir, "statements"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	header := statementHeader{
		AccountName:   "ZXQ Synthetic Statement",
		AccountNumber: "99112233",
		PeriodStart:   "2025-10-01",
		PeriodEnd:     "2025-10-31",
	}
	data := []byte("synthetic-reassignable-statement")
	meta, err := a.archiveStatement(12, true, "October.pdf", data, header, nil, "[]")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.learnStatementAccount(header.AccountName, header.AccountNumber, 12); err != nil {
		t.Fatal(err)
	}
	if accountID, confident := a.detectStatementAccount(header); accountID != 12 || !confident {
		t.Fatalf("initial detection = account %d confident=%v, want 12 true", accountID, confident)
	}

	reassign := url.Values{
		"account_id":        {"12"},
		"target_account_id": {"2"},
		"file":              {meta.File},
		"month":             {"2025-10"},
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/review/statement/reassign", strings.NewReader(reassign.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	a.reassignStatement(rr, req)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("reassign status = %d, body=%s", rr.Code, rr.Body.String())
	}
	if location := rr.Header().Get("Location"); location != "/review?month=2025-10&account_id=2" {
		t.Fatalf("reassign location = %q", location)
	}
	if got := a.listArchivedStatements(12); len(got) != 0 {
		t.Fatalf("source account still has %d statements", len(got))
	}
	moved := a.listArchivedStatements(2)
	if len(moved) != 1 || moved[0].File != meta.File {
		t.Fatalf("target statements = %+v", moved)
	}
	if accountID, confident := a.detectStatementAccount(header); accountID != 2 || !confident {
		t.Fatalf("corrected detection = account %d confident=%v, want 2 true", accountID, confident)
	}

	remove := url.Values{
		"account_id": {"2"},
		"file":       {meta.File},
		"month":      {"2025-10"},
	}
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/review/statement/delete", strings.NewReader(remove.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	a.deleteStatement(rr, req)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("delete status = %d, body=%s", rr.Code, rr.Body.String())
	}
	if _, ok := a.findStatementByHash(statementContentHash(data)); ok {
		t.Fatal("deleted statement is still present in the archive")
	}
	if _, confident := a.detectStatementAccount(header); confident {
		t.Fatal("deleted statement's learned account assignment was retained")
	}
	for _, suffix := range []string{"", ".json"} {
		if _, err := os.Stat(filepath.Join(a.accountStatementDir(2), meta.File+suffix)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("deleted archive file %q still exists or stat failed: %v", meta.File+suffix, err)
		}
	}
}

func TestReuseUnconfirmedStatementRedetectsAccount(t *testing.T) {
	a, err := newApp(config{DBPath: fixtureDB(t), BackupDir: "backups", StatementDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	header := statementHeader{AccountName: "Totally Unknown Bank", PeriodStart: "2025-10-01", PeriodEnd: "2025-10-31"}
	meta, err := a.archiveStatement(99, false, "x.pdf", []byte("x"), header, []importCandidate{{Date: "2025-10-05", Type: "Withdrawal", Amount: 5}}, "")
	if err != nil {
		t.Fatal(err)
	}
	// Re-uploading an unconfirmed statement must not silently bind to the stale
	// guess — it asks for the account again.
	res := a.reuseArchivedStatement(meta, "x.pdf", "2025-10")
	if !res.NeedsChoice {
		t.Fatal("unconfirmed reuse should require account confirmation")
	}
	if res.StatementFile != meta.File {
		t.Fatalf("reuse StatementFile = %q, want %q", res.StatementFile, meta.File)
	}

	// Once confirmed, a re-upload trusts the assigned account with no prompt.
	confirmed, err := a.confirmStatement(meta, 12)
	if err != nil {
		t.Fatal(err)
	}
	res2 := a.reuseArchivedStatement(confirmed, "x.pdf", "2025-10")
	if res2.NeedsChoice || res2.AccountID != 12 {
		t.Fatalf("confirmed reuse: needsChoice=%v account=%d", res2.NeedsChoice, res2.AccountID)
	}
}

func TestResolveStatementPeriodFallbacks(t *testing.T) {
	upload := time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name               string
		header             statementHeader
		wantStart, wantEnd string
	}{
		{"full period", statementHeader{PeriodStart: "2026-02-01", PeriodEnd: "2026-02-28"}, "2026-02-01", "2026-02-28"},
		{"closing only", statementHeader{ClosingDate: "2026-02-14"}, "2026-02-14", "2026-02-14"},
		{"end only", statementHeader{PeriodEnd: "2026-02-28"}, "2026-02-28", "2026-02-28"},
		{"empty falls back to upload", statementHeader{}, "2026-03-02", "2026-03-02"},
		{"reversed gets ordered", statementHeader{PeriodStart: "2026-02-28", PeriodEnd: "2026-02-01"}, "2026-02-01", "2026-02-28"},
	} {
		start, end := resolveStatementPeriod(tc.header, upload)
		if start != tc.wantStart || end != tc.wantEnd {
			t.Fatalf("%s: period = %s..%s, want %s..%s", tc.name, start, end, tc.wantStart, tc.wantEnd)
		}
	}
}

func TestClosingBalanceBannerIsLive(t *testing.T) {
	if !strings.Contains(templates, "data-closing-banner") {
		t.Fatal("closing-balance banner should expose data-closing-banner")
	}
	for _, attr := range []string{"data-closing-base", "data-closing-expected", "data-closing-date", "data-closing-text"} {
		if !strings.Contains(templates, attr) {
			t.Fatalf("closing-balance banner missing %s", attr)
		}
	}
	if !strings.Contains(js, "function updateClosingBanner") {
		t.Fatal("app.js should recompute the closing balance live")
	}
	if !strings.Contains(js, "function candidateDeltaSum") {
		t.Fatal("app.js should sum selected candidate deltas for the live projection")
	}
	if !strings.Contains(js, `type === "TransferIn" ? amount : -amount`) &&
		!strings.Contains(js, `type === "Deposit" || type === "TransferIn" ? amount : -amount`) {
		t.Fatal("live projection should add incoming transfers and subtract outgoing transfers")
	}
}

func TestImportCandidateManualUncheckIsRespected(t *testing.T) {
	if !strings.Contains(js, `checkbox.dataset.userToggled = "true"`) {
		t.Fatal("candidate checkbox clicks should be tracked as user choices")
	}
	if !strings.Contains(js, `checkbox.dataset.userToggled !== "true"`) {
		t.Fatal("candidate auto-selection should not override a manual checkbox choice")
	}
}

func TestImportValidationIgnoresUnapprovedCandidates(t *testing.T) {
	a, err := newApp(config{DBPath: fixtureDB(t), BackupDir: "backups"})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	request := formRequest(url.Values{
		"account_id": {"12"},
		"approved":   {"0"},
		"date":       {"2025-10-20", "not-a-date"},
		"type":       {"Withdrawal", "Withdrawal"},
		"amount":     {"12.34", "0"},
		"payee":      {"Amazon", ""},
		"category":   {"Leisure: Hobbies", ""},
		"notes":      {"approved", "leave for later"},
		"status":     {"Reconciled", "Reconciled"},
	})
	inputs, err := a.parseApprovedImportInputs(request)
	if err != nil {
		t.Fatalf("unapproved invalid candidate blocked import: %v", err)
	}
	if len(inputs) != 1 || inputs[0].Notes != "approved" {
		t.Fatalf("approved inputs = %+v, want only the first candidate", inputs)
	}
}

func TestCandidateDateRegexUsesDigits(t *testing.T) {
	if strings.Contains(templates, `type="date"`) || strings.Contains(templates, `type="month"`) {
		t.Fatal("native browser date/month inputs should not be used")
	}
	if strings.Contains(js, `^\\d{4}`) {
		t.Fatal("candidate date regex matches literal backslash-d instead of digits")
	}
	if !strings.Contains(js, `function candidateDateValid`) {
		t.Fatal("candidate date validator not found")
	}
	if !strings.Contains(js, `^\d{4}-\d{2}-\d{2}$`) {
		t.Fatal("candidate date validator should require YYYY-MM-DD")
	}
}

func TestReviewNewTransactionStaysOpenAfterSave(t *testing.T) {
	if strings.Contains(templates, `{{.T "Manual Gap Transaction"}}`) {
		t.Fatal("review should use the New Transaction label")
	}
	for _, want := range []string{
		`id="review-new-transaction"`,
		`{{.T "New Transaction"}}`,
		`account_id=%d#review-new-transaction`,
	} {
		if !strings.Contains(templates, want) {
			t.Fatalf("review new-transaction form missing %q", want)
		}
	}
	for _, want := range []string{
		`window.location.hash !== "#review-new-transaction"`,
		`panel.open = true`,
		`panel.querySelector('input[name="date"]')`,
		`date.focus()`,
	} {
		if !strings.Contains(js, want) {
			t.Fatalf("review new-transaction restore behavior missing %q", want)
		}
	}
}

func TestReviewCandidateDismissesInPlace(t *testing.T) {
	for _, want := range []string{
		`new FormData(form)`,
		`new URLSearchParams()`,
		`"X-MMXC-Partial": "true"`,
		`row.remove()`,
		`updateClosingBanner()`,
		`scrollIntoView({ block: "nearest" })`,
		`date.focus({ preventScroll: true })`,
	} {
		if !strings.Contains(js, want) {
			t.Fatalf("in-place candidate dismissal missing %q", want)
		}
	}
	if !strings.Contains(templates, `"Could not dismiss candidate. Please try again."`) ||
		!strings.Contains(templates, `"No statement candidates remain."`) {
		t.Fatal("candidate dismissal messages should be translatable")
	}
}

func TestImportReviewShowsOriginalStatementDescription(t *testing.T) {
	if !strings.Contains(templates, `class="candidate-source">{{$c.OriginalPayee}}`) {
		t.Fatal("import review should show original statement description")
	}
	if strings.Contains(templates, `candidate-source">Statement: `) {
		t.Fatal(`candidate-source should not carry a "Statement:" label`)
	}
}

func TestImportReviewDuplicateToggleAndTransferCategoryVisibility(t *testing.T) {
	if strings.Contains(templates, `name="import_json"`) {
		t.Fatal("review page should not expose manual candidate JSON textarea")
	}
	if !strings.Contains(templates, `data-statement-upload`) {
		t.Fatal("review navigation should expose statement upload")
	}
	if strings.Contains(templates, `<button type="submit">Import Statement</button>`) {
		t.Fatal("statement upload should auto-submit without an import button")
	}
	if !strings.Contains(js, `input.addEventListener("change"`) || !strings.Contains(js, `form.requestSubmit();`) {
		t.Fatal("choosing a statement file should start processing automatically")
	}
	if strings.Contains(js, `input.disabled = true`) {
		t.Fatal("statement file input must not be disabled before multipart submission")
	}
	if !strings.Contains(templates, `data-global-busy`) || !strings.Contains(templates, `Processing statement...`) {
		t.Fatal("review page should expose a global statement-processing overlay")
	}
	if !strings.Contains(css, `.global-busy`) || !strings.Contains(css, `.global-spinner`) {
		t.Fatal("global busy overlay should be styled")
	}
	if !strings.Contains(js, `function showGlobalBusy`) || !strings.Contains(js, `showGlobalBusy(i18n("Processing statement..."))`) {
		t.Fatal("statement upload should block page input while processing")
	}
	for _, want := range []string{
		`action="/review/statements/attach"`,
		`action="/review/statement/reassign"`,
		`action="/review/statement/delete"`,
		`action="/review/statement/document/delete"`,
		`name="section_account_id"`,
		`value="ignore"`,
		`name="target_account_id"`,
		`data-confirm=`,
	} {
		if !strings.Contains(templates, want) {
			t.Fatalf("statement archive controls missing %q", want)
		}
	}
	if !strings.Contains(js, `form.dataset.confirm`) {
		t.Fatal("statement deletion should require client confirmation")
	}
	if !strings.Contains(templates, `data-hide-duplicates checked`) {
		t.Fatal("import review should hide possible duplicates by default")
	}
	for _, want := range []string{
		`action="/review/import/commit" novalidate`,
		`formaction="/review/candidate/dismiss"`,
		`name="candidate_archive_file"`,
		`name="candidate_archive_index"`,
		`name="candidate_archive_key"`,
		`data-dismiss-candidate`,
	} {
		if !strings.Contains(templates, want) {
			t.Fatalf("candidate dismissal/import form missing %q", want)
		}
	}
	if !strings.Contains(templates, `class="candidate-category"`) {
		t.Fatal("candidate category cell should be targetable")
	}
	for _, want := range []string{`value="TransferOut"`, `value="TransferIn"`, `{{if eq $c.Type "Transfer"}}candidate-transfer `} {
		if !strings.Contains(templates, want) {
			t.Fatalf("account-relative transfer editor missing %q", want)
		}
	}
	if strings.Contains(templates, `data-candidate-account-label`) ||
		strings.Contains(js, `data-candidate-account-label`) {
		t.Fatal("transfer candidate account selector should not show a redundant from/to label")
	}
	if !strings.Contains(css, "tr.candidate-transfer .candidate-split-toggle {\n\tdisplay: none !important;") {
		t.Fatal("transfer candidate Split label should stay hidden despite checkline display styling")
	}
	if !strings.Contains(css, `tr.candidate-transfer .candidate-category input`) {
		t.Fatal("transfer rows should hide category field")
	}
	if !strings.Contains(js, `updateDuplicateVisibility`) {
		t.Fatal("duplicate visibility toggle should be wired")
	}
	if strings.Contains(templates, `candidate-update-hint`) || strings.Contains(css, `candidate-update-hint`) || strings.Contains(templates, `updates&nbsp;existing`) {
		t.Fatal("duplicate candidates should not show a separate updates-existing label")
	}
}

// Every overlay is a native <dialog> opened with showModal(): the browser then
// owns top-layer stacking, focus containment and restore, Escape, and background
// inertness, and the buttons are translated by the app rather than the browser.
// window.confirm/alert must not creep back — besides being untranslatable, they
// can be silently suppressed by the browser, which would make a destructive
// action appear to do nothing.
func TestAppRenderedDialogAssets(t *testing.T) {
	for _, want := range []string{
		`<dialog class="global-busy" data-global-busy>`,
		`<dialog class="app-dialog" data-app-dialog`,
		`<form method="dialog" class="app-dialog-form">`,
		`data-app-dialog-message`,
		`value="cancel" data-app-dialog-cancel`,
		`value="ok" data-app-dialog-ok autofocus`,
		`<dialog class="modal-panel" data-version-modal data-dismiss-on-backdrop`,
		`data-confirm-ok=`,
	} {
		if !strings.Contains(templates, want) {
			t.Fatalf("dialog markup missing %q", want)
		}
	}
	for _, kind := range []string{"recurring", "anomaly", "budget", "category", "payee", "trend"} {
		want := `<dialog class="modal-panel" data-` + kind + `-modal data-dismiss-on-backdrop`
		if !strings.Contains(templates, want) {
			t.Fatalf("report modal %q was not migrated to <dialog>: missing %q", kind, want)
		}
	}
	if strings.Contains(templates, `modal-backdrop`) || strings.Contains(css, `.modal-backdrop`) {
		t.Fatal("the hand-rolled modal backdrop should be replaced by dialog::backdrop")
	}
	for _, want := range []string{
		`function openDialog`,
		`dialog.showModal();`,
		`function appConfirm`,
		`function appAlert`,
		`function topDialog`,
		`appConfirm(form.dataset.confirm`,
		// The affirmative label is "Void transaction", not a bare "Void": in
		// several languages "Void" and "Cancel" collapse to the same word, which
		// would render two identically labelled buttons in the dialog.
		`appConfirm(i18n("Void this transaction?"), { ok: i18n("Void transaction") })`,
		`appAlert(i18n("Could not dismiss candidate. Please try again."))`,
		// Re-parenting keeps the row context menu usable (and non-inert) on rows
		// inside an open report modal.
		`var host = topDialog() || document.body;`,
		// A method="dialog" form closes its dialog instead of navigating, so it
		// must not raise the global busy overlay: nothing would ever hide it.
		`if ((form.getAttribute("method") || "").toLowerCase() === "dialog") return;`,
	} {
		if !strings.Contains(js, want) {
			t.Fatalf("dialog script missing %q", want)
		}
	}
	// The one permitted window.confirm is appConfirm's own fallback for browsers
	// without <dialog>, so a confirmation is never skipped outright.
	if strings.Count(js, "window.confirm(") != 1 || strings.Contains(js, "window.alert(") {
		t.Fatal("native browser dialogs should be routed through appConfirm/appAlert")
	}
	for _, want := range []string{`dialog::backdrop`, `.app-dialog-actions`, `.global-busy[open]`} {
		if !strings.Contains(css, want) {
			t.Fatalf("dialog styles missing %q", want)
		}
	}
}

func TestAccountNavigationFilterAssets(t *testing.T) {
	for _, want := range []string{
		`data-account-nav-filter`,
		`value="favorites"`,
		`value="open" selected`,
		`value="closed"`,
		`value="all"`,
		`data-account-nav-item`,
		`data-account-status="{{.Status}}"`,
		`data-account-favorite="{{if .Favorite}}true{{else}}false{{end}}"`,
	} {
		if !strings.Contains(templates, want) {
			t.Fatalf("account navigation template missing %q", want)
		}
	}
	for _, want := range []string{
		`mmxc.accounts.navigationFilter.v1`,
		`return "open";`,
		`filter === "favorites"`,
		`item.hidden = !itemVisible(item, filter);`,
	} {
		if !strings.Contains(js, want) {
			t.Fatalf("account navigation script missing %q", want)
		}
	}
}

func TestAccountBalanceColumnAssets(t *testing.T) {
	for _, want := range []string{
		`name="show_account_balance"`,
		`{{if .Info.ShowAccountBalance}}checked{{end}}`,
		`{{if .Info.ShowAccountBalance}}<th class="right">{{.T "Balance" "running account balance after this transaction"}}</th>{{end}}`,
		`{{if $.Info.ShowAccountBalance}}<td class="right amount`,
	} {
		if !strings.Contains(templates, want) {
			t.Fatalf("account balance column template missing %q", want)
		}
	}
}

func TestImportCandidateDescriptionIsStatementSource(t *testing.T) {
	c := importCandidate{
		Date:        "2025-11-03",
		Type:        "Withdrawal",
		Amount:      691.74,
		Description: "EFT Withdrawal to CITY MORTGAGE DEPT",
		Payee:       "City Mortgage",
		Category:    "Mortgage",
	}
	c.normalize()
	if c.OriginalPayee != "EFT Withdrawal to CITY MORTGAGE DEPT" {
		t.Fatalf("original statement description = %q", c.OriginalPayee)
	}
	if !strings.Contains(statementTextPrompt(Prompts{}, "2026-06-19", "sample statement text", nil), "description must be the original transaction description") {
		t.Fatal("statement prompt should require original description")
	}
	if strings.Contains(statementTextPrompt(Prompts{}, "2026-06-19", "sample statement text", nil), `"notes"`) {
		t.Fatal("statement prompt should not include notes in the model output schema")
	}
	if strings.Contains(statementTextPrompt(Prompts{}, "2026-06-19", "sample statement text", nil), "notes should include useful statement context") {
		t.Fatal("statement prompt should not ask the model to fill notes")
	}
}

func TestFormBoolDefault(t *testing.T) {
	if got := formBoolDefault(formRequest(url.Values{}), "active", true); !got {
		t.Fatal("missing form bool did not use true fallback")
	}
	if got := formBoolDefault(formRequest(url.Values{"active": {"false"}}), "active", true); got {
		t.Fatal("false form bool parsed as true")
	}
	if got := formBoolDefault(formRequest(url.Values{"active": {"false", "true"}}), "active", false); !got {
		t.Fatal("checked checkbox form bool parsed as false")
	}
}

func formRequest(values url.Values) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/transactions/create", strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if err := req.ParseForm(); err != nil {
		panic(err)
	}
	return req
}

func countRows(t *testing.T, a *app, table string) int {
	t.Helper()
	return a.count(table)
}

func TestBalanceAtMatchesListedBalance(t *testing.T) {
	a, err := newApp(config{DBPath: fixtureDB(t), WriteEnabled: false, BackupDir: "backups"})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	accounts, err := a.listAccounts()
	if err != nil {
		t.Fatalf("list accounts: %v", err)
	}
	var listed float64
	found := false
	for _, ac := range accounts {
		if ac.ID == 12 {
			listed, found = ac.Balance, true
		}
	}
	if !found {
		t.Fatal("account 12 not found")
	}
	// balanceAt with a far-future cutoff includes every transaction, so it must
	// equal the balance computed by listAccounts.
	got, err := a.balanceAt(12, "9999-12-31")
	if err != nil {
		t.Fatalf("balanceAt: %v", err)
	}
	if math.Abs(got-listed) > 0.005 {
		t.Fatalf("balanceAt(12, future) = %.2f, listAccounts balance = %.2f", got, listed)
	}
}

func TestPopulateAccountBalancesUsesCompleteLedger(t *testing.T) {
	a, err := newApp(config{DBPath: fixtureDB(t), WriteEnabled: true, BackupDir: filepath.Join(t.TempDir(), "backups")})
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
  (188, 12, 2,    -1, 'Transfer', 20.0, '',     '', '', 4,  '2025-11-01T00:00:00', NULL, 30.0, '2025-11-01T00:00:00', NULL, -1),
  (189, 12, NULL, -1, 'Deposit',  99.0, 'Void', '', '', 30, '2025-12-01T00:00:00', NULL, NULL, '2025-12-01T00:00:00', NULL, -1)`); err != nil {
		t.Fatal(err)
	}

	// Only show withdrawals from August onward. Their running balances must
	// still include the hidden July transaction and the account's initial value.
	txs, err := a.listTransactions(transactionFilter{
		AccountID: 12,
		From:      "2025-08-01",
		Type:      "Withdrawal",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.populateAccountBalances(12, 1000, txs); err != nil {
		t.Fatal(err)
	}
	got := make(map[int]float64, len(txs))
	for _, tx := range txs {
		got[tx.ID] = tx.Balance
	}
	if math.Abs(got[186]-885) > 0.005 {
		t.Fatalf("transaction 186 balance = %.2f, want 885.00", got[186])
	}
	if math.Abs(got[100]-835) > 0.005 {
		t.Fatalf("transaction 100 balance = %.2f, want 835.00", got[100])
	}

	sourceRows := []transaction{{ID: 188}, {ID: 189}}
	if err := a.populateAccountBalances(12, 1000, sourceRows); err != nil {
		t.Fatal(err)
	}
	if math.Abs(sourceRows[0].Balance-815) > 0.005 || math.Abs(sourceRows[1].Balance-815) > 0.005 {
		t.Fatalf("source transfer/void balances = %.2f, %.2f, want 815.00, 815.00", sourceRows[0].Balance, sourceRows[1].Balance)
	}
	destinationRows := []transaction{{ID: 188}}
	if err := a.populateAccountBalances(2, 0, destinationRows); err != nil {
		t.Fatal(err)
	}
	if math.Abs(destinationRows[0].Balance-30) > 0.005 {
		t.Fatalf("destination transfer balance = %.2f, want 30.00", destinationRows[0].Balance)
	}
}

func TestImportUpsertUpdatesDuplicate(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.mmb")
	buildSyntheticDB(t, dbPath)
	a, err := newApp(config{DBPath: dbPath, WriteEnabled: true, BackupDir: filepath.Join(dir, "backups")})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	// Seed a transaction with an unset status.
	seed := formRequest(url.Values{
		"account_id":      {"12"},
		"candidates_json": {`[{"date":"2025-09-12","type":"Withdrawal","amount":41.23,"payee":"Amazon","category":"Leisure: Hobbies","notes":"original"}]`},
	})
	seedCandidates, err := a.parseImportCandidates(seed)
	if err != nil {
		t.Fatalf("parse seed: %v", err)
	}
	seedCommit := formRequest(url.Values{
		"account_id": {"12"}, "approved": {"0"},
		"date": {"2025-09-12"}, "type": {"Withdrawal"}, "amount": {"41.23"},
		"payee": {"Amazon"}, "category": {"Leisure: Hobbies"},
		"notes": {"original"}, "status": {"__blank"},
	})
	inputs, err := a.parseApprovedImportInputs(seedCommit)
	if err != nil {
		t.Fatalf("parse seed commit: %v", err)
	}
	if _, err := a.insertImportedTransactions(inputs, "", "", 12); err != nil {
		t.Fatalf("insert seed: %v", err)
	}
	_ = seedCandidates
	before := countRows(t, a, "CHECKINGACCOUNT_V1")

	// The same transaction is now detected as a duplicate carrying its TRANSID.
	dupReq := formRequest(url.Values{
		"account_id":      {"12"},
		"candidates_json": {`[{"date":"2025-09-12","type":"Withdrawal","amount":41.23,"payee":"Amazon","category":"Leisure: Hobbies","notes":"original"}]`},
	})
	dupCandidates, err := a.parseImportCandidates(dupReq)
	if err != nil {
		t.Fatalf("parse duplicate: %v", err)
	}
	if !dupCandidates[0].Duplicate || dupCandidates[0].DuplicateID <= 0 {
		t.Fatalf("expected duplicate with id, got %+v", dupCandidates[0])
	}
	transID := dupCandidates[0].DuplicateID

	// Approve the duplicate as an upsert: change status and notes.
	upsert := formRequest(url.Values{
		"account_id": {"12"}, "approved": {"0"}, "duplicate_id": {strconv.FormatInt(transID, 10)},
		"date": {"2025-09-12"}, "type": {"Withdrawal"}, "amount": {"41.23"},
		"payee": {"Amazon"}, "category": {"Leisure: Hobbies"},
		"notes": {"reconciled note"}, "status": {"Reconciled"},
	})
	upsertInputs, err := a.parseApprovedImportInputs(upsert)
	if err != nil {
		t.Fatalf("parse upsert: %v", err)
	}
	if upsertInputs[0].UpdateID != transID {
		t.Fatalf("upsert UpdateID = %d, want %d", upsertInputs[0].UpdateID, transID)
	}
	if _, err := a.insertImportedTransactions(upsertInputs, "", "", 12); err != nil {
		t.Fatalf("commit upsert: %v", err)
	}

	if after := countRows(t, a, "CHECKINGACCOUNT_V1"); after != before {
		t.Fatalf("row count changed on upsert: before %d, after %d", before, after)
	}
	var status, notes string
	if err := a.db.QueryRow("SELECT COALESCE(STATUS,''), COALESCE(NOTES,'') FROM CHECKINGACCOUNT_V1 WHERE TRANSID = ?", transID).Scan(&status, &notes); err != nil {
		t.Fatalf("read upserted row: %v", err)
	}
	if status != "Reconciled" || notes != "reconciled note" {
		t.Fatalf("upsert did not update row: status=%q notes=%q", status, notes)
	}
}

func TestImportCandidateCategoryFromHistory(t *testing.T) {
	a, err := newApp(config{DBPath: fixtureDB(t), WriteEnabled: true, BackupDir: "backups"})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	// Payee "Acme Utilities" (1) has one categorized transaction (category 66,
	// "Leisure: Hobb"). A candidate for that payee whose category does not
	// resolve should inherit it (24a).
	candidates := []importCandidate{{
		Date:   "2025-11-15",
		Type:   "Withdrawal",
		Amount: 12.34,
		Payee:  "Acme Utilities",
	}}
	a.prepareImportCandidates(12, candidates)
	c := candidates[0]
	if c.CategoryID != 66 {
		t.Fatalf("category from history not applied: CategoryID=%d category=%q errors=%v", c.CategoryID, c.Category, c.Errors)
	}
	if c.Category != "Leisure: Hobbies" {
		t.Fatalf("unexpected category path: %q", c.Category)
	}
	if !containsString(c.Warnings, "Category from history — confirm") {
		t.Fatalf("expected history warning, got %v", c.Warnings)
	}
}

// insertPayeeHistory adds one categorized withdrawal on account 12 for the payee
// per given category, a day apart from 2025-01-01, as prior ledger history.
func insertPayeeHistory(t *testing.T, a *app, firstID int64, payeeID int, categoryIDs ...int) {
	t.Helper()
	for i, categoryID := range categoryIDs {
		d := time.Date(2025, 1, 1+i, 0, 0, 0, 0, time.UTC).Format("2006-01-02T15:04:05")
		if _, err := a.db.Exec(`INSERT INTO CHECKINGACCOUNT_V1
			(TRANSID, ACCOUNTID, TOACCOUNTID, PAYEEID, TRANSCODE, TRANSAMOUNT, STATUS,
			 TRANSACTIONNUMBER, NOTES, CATEGID, TRANSDATE, FOLLOWUPID, TOTRANSAMOUNT, LASTUPDATEDTIME, DELETEDTIME, COLOR)
			VALUES (?, 12, NULL, ?, 'Withdrawal', ?, 'Reconciled', '', '', ?, ?, NULL, NULL, ?, NULL, -1)`,
			firstID+int64(i), payeeID, 10+float64(i), categoryID, d, d); err != nil {
			t.Fatalf("insert payee history: %v", err)
		}
	}
}

// The payee's own history outranks a category the extraction model chose: a
// payee habitually booked to one category keeps it when the model reads the
// merchant as something else, while a category the payee has really used before
// is left as the model picked it.
func TestPayeeHistoryOutranksExtractedCategory(t *testing.T) {
	a, err := newApp(config{DBPath: fixtureDB(t), WriteEnabled: true, BackupDir: "backups"})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	insertPayeeHistory(t, a, 910001, 2, 30, 30, 30)  // Grocery Mart: always Groceries
	insertPayeeHistory(t, a, 910101, 3, 1, 1, 1, 30) // Coffee Stop: mostly Dining, once Groceries
	insertPayeeHistory(t, a, 910201, 1, 16, 30, 102) // Acme Utilities (+ seed Hobbies): no majority

	candidates := []importCandidate{
		{Date: "2025-12-01", Type: "Withdrawal", Amount: 21.01, Payee: "Grocery Mart", Category: "Leisure: Hobbies"},
		{Date: "2025-12-02", Type: "Withdrawal", Amount: 21.02, Payee: "Coffee Stop", Category: "Food: Groceries"},
		{Date: "2025-12-03", Type: "Withdrawal", Amount: 21.03, Payee: "Coffee Stop", Category: "Leisure: Hobbies"},
		{Date: "2025-12-04", Type: "Withdrawal", Amount: 21.04, Payee: "Acme Utilities", Category: "Food: Toiletries"},
	}
	a.prepareImportCandidates(12, candidates)
	for i, tc := range []struct {
		wantCategoryID int
		wantNote       bool
	}{
		{30, true},  // habitual category replaces the model's invention
		{30, false}, // model picked a category this payee has used: kept
		{1, true},   // model's category never used for this payee: majority wins
		{86, false}, // no majority in the history: the model's choice stands
	} {
		c := candidates[i]
		if c.CategoryID != tc.wantCategoryID {
			t.Fatalf("candidate %d (%s): CategoryID=%d category=%q, want %d; errors=%v", i, c.Payee, c.CategoryID, c.Category, tc.wantCategoryID, c.Errors)
		}
		if got := containsString(c.Warnings, "Category from history — confirm"); got != tc.wantNote {
			t.Fatalf("candidate %d (%s): history note=%v, want %v; warnings=%v", i, c.Payee, got, tc.wantNote, c.Warnings)
		}
	}
}

// A learned rule is a confirmation of this exact statement description, so its
// category stands even against a payee's habitual one.
func TestLearnedRuleCategoryOutranksPayeeHistory(t *testing.T) {
	a, err := newApp(config{DBPath: fixtureDB(t), WriteEnabled: true, BackupDir: "backups"})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	insertPayeeHistory(t, a, 910001, 3, 1, 1, 1) // Coffee Stop: always Dining
	if _, err := a.db.Exec(`INSERT INTO INFOTABLE_V1 (INFONAME, INFOVALUE) VALUES (?, ?)`,
		importRulesInfoName,
		`[{"pattern":"coffee stop catering","type":"Withdrawal","payee":"Coffee Stop","category":"Leisure: Hobbies"}]`); err != nil {
		t.Fatal(err)
	}
	candidates := []importCandidate{{
		Date: "2025-12-01", Type: "Withdrawal", Amount: 40,
		Description: "COFFEE STOP CATERING 4411", Payee: "Coffee Stop", Category: "Food: Dining",
	}}
	a.prepareImportCandidates(12, candidates)
	c := candidates[0]
	if c.CategoryID != 66 {
		t.Fatalf("learned rule category lost to history: CategoryID=%d category=%q", c.CategoryID, c.Category)
	}
	if containsString(c.Warnings, "Category from history — confirm") {
		t.Fatalf("rule-set category flagged as history: %v", c.Warnings)
	}
}

// A charge that appears twice on one statement (a mortgage paid twice a month)
// cannot be two different merchants. The extraction model labelling the two rows
// differently must not switch off the learned rule for both.
func TestRecurringIdenticalChargesKeepLearnedRule(t *testing.T) {
	a, err := newApp(config{DBPath: fixtureDB(t), WriteEnabled: true, BackupDir: "backups"})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	if _, err := a.db.Exec(`INSERT INTO INFOTABLE_V1 (INFONAME, INFOVALUE) VALUES (?, ?)`,
		importRulesInfoName,
		`[{"pattern":"preauth home loan dept","type":"Withdrawal","payee":"Acme Utilities","category":"Food: Misc"}]`); err != nil {
		t.Fatal(err)
	}
	candidates := []importCandidate{
		{Date: "2025-12-01", Type: "Withdrawal", Amount: 691.74, Description: "PREAUTH HOME LOAN DEPT", Payee: "Home Loan"},
		{Date: "2025-12-15", Type: "Withdrawal", Amount: 691.74, Description: "PREAUTH HOME LOAN DEPT", Payee: "Loan Dept", Category: "Leisure: Hobbies"},
	}
	a.prepareImportCandidatesNoLLM(12, candidates)
	for i, c := range candidates {
		if c.Payee != "Acme Utilities" || c.CategoryID != 16 {
			t.Fatalf("candidate %d did not get the learned rule: payee=%q CategoryID=%d category=%q", i, c.Payee, c.CategoryID, c.Category)
		}
	}
}

// Rows left over after a partial import carry what the user typed on the form;
// re-preparing them must not overwrite those edits with learned rules or
// history.
func TestRemainingReviewRowsKeepUserEdits(t *testing.T) {
	a, err := newApp(config{DBPath: fixtureDB(t), WriteEnabled: true, BackupDir: "backups"})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	insertPayeeHistory(t, a, 910001, 2, 30, 30, 30) // Grocery Mart: always Groceries
	if _, err := a.db.Exec(`INSERT INTO INFOTABLE_V1 (INFONAME, INFOVALUE) VALUES (?, ?)`,
		importRulesInfoName,
		`[{"pattern":"market purchase","type":"Withdrawal","payee":"Coffee Stop","category":"Food: Dining"}]`); err != nil {
		t.Fatal(err)
	}
	remaining := a.remainingImportCandidates(formRequest(url.Values{
		"account_id":     {"12"},
		"month":          {"2025-12"},
		"date":           {"2025-12-03"},
		"type":           {"Withdrawal"},
		"amount":         {"33.00"},
		"payee":          {"Grocery Mart"},
		"original_payee": {"MARKET PURCHASE"},
		"category":       {"Leisure: Hobbies"},
		"notes":          {""},
		"status":         {"Reconciled"},
	}))
	a.prepareImportCandidates(12, remaining)
	if len(remaining) != 1 {
		t.Fatalf("remaining = %d candidates, want 1", len(remaining))
	}
	c := remaining[0]
	if c.Payee != "Grocery Mart" || c.CategoryID != 66 {
		t.Fatalf("user edits overwritten: payee=%q CategoryID=%d category=%q warnings=%v", c.Payee, c.CategoryID, c.Category, c.Warnings)
	}
}

func TestImportCandidateSplitReplication(t *testing.T) {
	a, err := newApp(config{DBPath: fixtureDB(t), WriteEnabled: true, BackupDir: "backups"})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	// Payee "Amazon" (14) has a split transaction (186) of 100.00 split into
	// category 30 (40.00) and 102 (60.00). An identical recurring candidate
	// should reproduce that split (24b).
	candidates := []importCandidate{{
		Date:     "2025-11-20",
		Type:     "Withdrawal",
		Amount:   100.00,
		Payee:    "Amazon",
		Category: "Leisure: Hobb",
	}}
	a.prepareImportCandidates(12, candidates)
	c := candidates[0]
	if len(c.Errors) > 0 {
		t.Fatalf("unexpected errors: %v", c.Errors)
	}
	if c.CategoryID != -1 || len(c.Splits) != 2 {
		t.Fatalf("split not replicated: CategoryID=%d splits=%+v", c.CategoryID, c.Splits)
	}
	got := map[int]float64{}
	for _, s := range c.Splits {
		got[s.CategoryID] = s.Amount
	}
	if got[30] != 40.0 || got[102] != 60.0 {
		t.Fatalf("unexpected split lines: %+v", c.Splits)
	}
	if !containsString(c.Warnings, "Split copied from 2025-08-10 — confirm") {
		t.Fatalf("expected split-copied warning, got %v", c.Warnings)
	}

	// The per-row commit form (suffixed split fields) should round-trip the
	// edited split back into a transactionInput.
	inputs, err := a.parseApprovedImportInputs(formRequest(url.Values{
		"account_id":            {"12"},
		"approved":              {"0"},
		"date":                  {"2025-11-20"},
		"type":                  {"Withdrawal"},
		"amount":                {"100.00"},
		"payee":                 {"Amazon"},
		"split_category_id_0":   {"30", "102"},
		"split_category_text_0": {"", ""},
		"split_amount_0":        {"40.00", "60.00"},
		"split_notes_0":         {"groceries", "subs"},
	}))
	if err != nil {
		t.Fatalf("parse approved split candidate: %v", err)
	}
	if len(inputs) != 1 || inputs[0].CategoryID != -1 || len(inputs[0].Splits) != 2 {
		t.Fatalf("unexpected split input: %+v", inputs)
	}

	// A split whose lines do not sum to the row amount must be rejected.
	if _, err := a.parseApprovedImportInputs(formRequest(url.Values{
		"account_id":          {"12"},
		"approved":            {"0"},
		"date":                {"2025-11-20"},
		"type":                {"Withdrawal"},
		"amount":              {"100.00"},
		"payee":               {"Amazon"},
		"split_category_id_0": {"30", "102"},
		"split_amount_0":      {"40.00", "55.00"},
	})); err == nil {
		t.Fatal("expected mismatched split sum to be rejected")
	}
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func TestReviewTemplateRendersSplitEditor(t *testing.T) {
	a, err := newApp(config{DBPath: fixtureDB(t), WriteEnabled: true, BackupDir: "backups"})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	req := formRequest(url.Values{"month": {"2025-11"}, "account_id": {"12"}})
	candidates := []importCandidate{{
		Date:     "2025-11-20",
		Type:     "Withdrawal",
		Amount:   100.00,
		Payee:    "Amazon",
		Category: "Leisure: Hobbies",
	}}
	a.prepareImportCandidates(12, candidates)
	review, err := a.buildMonthlyReview(req, candidates, "", "")
	if err != nil {
		t.Fatalf("build review: %v", err)
	}
	accounts, _ := a.listAccounts()
	cats, _ := a.listCategories()
	payees, _ := a.listPayees()

	var buf bytes.Buffer
	if err := a.tmpl.ExecuteTemplate(&buf, "review", pageData{
		Title: "Monthly Review", Active: "review", Info: a.info(),
		Accounts: accounts, Categories: cats, Payees: payees, MonthlyReview: review,
	}); err != nil {
		t.Fatalf("render review: %v", err)
	}
	html := buf.String()
	for _, want := range []string{
		`data-split-editor data-split-suffix="_0"`,
		`name="split_amount_0"`,
		`name="split_category_id_0"`,
		`readonly`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("rendered review missing %q", want)
		}
	}
}

func TestImportCandidateSplitReplicationByAccountFallback(t *testing.T) {
	a, err := newApp(config{DBPath: fixtureDB(t), WriteEnabled: true, BackupDir: "backups"})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	// Two recurring, identically-split 222.22 withdrawals on account 12 (payee
	// 14), mirroring a fixed monthly charge. A new statement line for the same
	// account and amount whose payee does NOT resolve (a renamed merchant) should
	// still reproduce the split via the account fallback (24b).
	for i, d := range []string{"2025-09-25", "2025-10-25"} {
		id := 900001 + i
		if _, err := a.db.Exec(`INSERT INTO CHECKINGACCOUNT_V1
			(TRANSID, ACCOUNTID, TOACCOUNTID, PAYEEID, TRANSCODE, TRANSAMOUNT, STATUS,
			 TRANSACTIONNUMBER, NOTES, CATEGID, TRANSDATE, FOLLOWUPID, TOTRANSAMOUNT, LASTUPDATEDTIME, DELETEDTIME, COLOR)
			VALUES (?, 12, NULL, 14, 'Withdrawal', 222.22, 'Reconciled', '', '', -1, ?, NULL, NULL, ?, NULL, -1)`,
			id, d+"T00:00:00", d+"T00:00:00"); err != nil {
			t.Fatalf("insert split parent: %v", err)
		}
		if _, err := a.db.Exec(`INSERT INTO SPLITTRANSACTIONS_V1 (SPLITTRANSID, TRANSID, CATEGID, SPLITTRANSAMOUNT, NOTES) VALUES (?, ?, 30, 122.22, ''), (?, ?, 102, 100.00, '')`,
			90001+i*2, id, 90002+i*2, id); err != nil {
			t.Fatalf("insert split lines: %v", err)
		}
	}

	candidates := []importCandidate{{
		Date:     "2025-12-01",
		Type:     "Withdrawal",
		Amount:   222.22,
		Payee:    "Zzz Brand New Merchant",
		Category: "Leisure: Hobbies",
	}}
	a.prepareImportCandidates(12, candidates)
	c := candidates[0]
	if c.CategoryID != -1 || len(c.Splits) != 2 {
		t.Fatalf("account-fallback split not replicated: CategoryID=%d splits=%+v errors=%v", c.CategoryID, c.Splits, c.Errors)
	}
	got := map[int]float64{}
	for _, s := range c.Splits {
		got[s.CategoryID] = s.Amount
	}
	if got[30] != 122.22 || got[102] != 100.00 {
		t.Fatalf("unexpected fallback split lines: %+v", c.Splits)
	}
	// The matched history (payee 14, "Amazon") should also repair the unresolved
	// candidate payee.
	if c.Payee != "Amazon" || c.PayeeID != 14 {
		t.Fatalf("payee not repaired from recurring split: Payee=%q PayeeID=%d", c.Payee, c.PayeeID)
	}
	if !containsString(c.Warnings, "Payee \"Amazon\" from recurring charge — confirm") {
		t.Fatalf("expected payee-repair warning, got %v", c.Warnings)
	}

	// A single (non-recurring) split on the account must NOT trigger the fallback:
	// payee 14's lone 100.00 split (seed tx 186) for an unresolved payee stays a
	// single category.
	single := []importCandidate{{
		Date:     "2025-12-02",
		Type:     "Withdrawal",
		Amount:   100.00,
		Payee:    "Zzz Brand New Merchant",
		Category: "Leisure: Hobbies",
	}}
	a.prepareImportCandidates(12, single)
	if len(single[0].Splits) != 0 {
		t.Fatalf("one-off split should not replicate via fallback: %+v", single[0].Splits)
	}
}

func TestCheckCompatibilityRejectsOldDataVersion(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "old.mmb")
	buildSyntheticDB(t, dbPath)

	// Downgrade the database to a DATAVERSION below the supported floor.
	func() {
		db, err := openSQLite(dbPath, "rw", "")
		if err != nil {
			t.Fatalf("open for downgrade: %v", err)
		}
		defer db.Close()
		if _, err := db.Exec(
			"UPDATE INFOTABLE_V1 SET INFOVALUE = ? WHERE INFONAME = 'DATAVERSION'",
			strconv.Itoa(minDataVersion-1),
		); err != nil {
			t.Fatalf("downgrade DATAVERSION: %v", err)
		}
	}()

	// Read-only open succeeds; the compatibility report must flag the old version.
	a, err := newApp(config{DBPath: dbPath})
	if err != nil {
		t.Fatalf("newApp read-only: %v", err)
	}
	defer a.db.Close()

	report := a.checkCompatibility()
	if report.OK {
		t.Fatal("expected old DATAVERSION to be incompatible")
	}
	if !strings.Contains(report.Details, "MMEX desktop") {
		t.Fatalf("expected upgrade-in-MMEX message, got %q", report.Details)
	}

	// Write mode must refuse to open an incompatible database at all.
	if _, err := newApp(config{DBPath: dbPath, WriteEnabled: true, BackupDir: filepath.Join(dir, "backups")}); err == nil {
		t.Fatal("expected write-mode open of old database to fail")
	}
}

func TestApplyEnvOverrides(t *testing.T) {
	t.Setenv("MMXC_DB_PATH", "/data")
	t.Setenv("MMXC_AUTH_ENABLED", "true")
	t.Setenv("MMXC_OIDC_ISSUER", "https://id.example.com")
	t.Setenv("MMXC_SESSION_TTL_HOURS", "24")

	cfg := applyEnvOverrides(config{DBPath: "data/from-yaml.mmb", LLMBaseURL: "http://yaml/v1"})

	if cfg.DBPath != "/data" {
		t.Errorf("DBPath = %q, want env value", cfg.DBPath)
	}
	if cfg.WriteEnabled {
		t.Error("WriteEnabled should not be set from env")
	}
	if !cfg.AuthEnabled || cfg.OIDCIssuer != "https://id.example.com" {
		t.Errorf("auth env not applied: %+v", cfg)
	}
	if cfg.SessionTTLHours != 24 {
		t.Errorf("SessionTTLHours = %d, want 24", cfg.SessionTTLHours)
	}
	// Runtime-editable settings have no env override and must be left untouched.
	if cfg.LLMBaseURL != "http://yaml/v1" {
		t.Errorf("LLMBaseURL should not change via env, got %q", cfg.LLMBaseURL)
	}
}

func TestDatabasePasswordFromSecretFile(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(dir, "db-password")
	// Secret managers and `echo` add a trailing newline, but a password may
	// contain spaces, so only the newline is stripped.
	if err := os.WriteFile(secret, []byte("correct horse battery staple\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MMXC_DB_PASSWORD_FILE", secret)

	if cfg := applyEnvOverrides(config{}); cfg.DBKey != "correct horse battery staple" {
		t.Fatalf("DBKey = %q, want the file contents without the trailing newline", cfg.DBKey)
	}

	// Pointing at a secret file is the more deliberate choice, so it wins.
	t.Setenv("MMXC_DB_PASSWORD", "inline")
	if cfg := applyEnvOverrides(config{}); cfg.DBKey != "correct horse battery staple" {
		t.Fatalf("DBKey = %q, want the secret file to win over MMXC_DB_PASSWORD", cfg.DBKey)
	}

	// An unreadable secret file must not silently discard a password that was
	// supplied inline.
	t.Setenv("MMXC_DB_PASSWORD_FILE", filepath.Join(dir, "missing"))
	if cfg := applyEnvOverrides(config{}); cfg.DBKey != "inline" {
		t.Fatalf("DBKey = %q, want the inline password to survive an unreadable secret file", cfg.DBKey)
	}
}

func TestResolveStartupDatabaseScansDirectoryAndDerivesWriteMode(t *testing.T) {
	dir := t.TempDir()
	readOnlyDB := filepath.Join(dir, "archive.mmb")
	writeDB := filepath.Join(dir, "current.mmb")
	buildSyntheticDB(t, readOnlyDB)
	buildSyntheticDB(t, writeDB)
	if err := os.Chmod(readOnlyDB, 0o400); err != nil {
		t.Fatal(err)
	}

	cfg, files, err := resolveStartupDatabase(config{DBPath: dir})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DBScanPath != dir {
		t.Fatalf("DBScanPath = %q, want %q", cfg.DBScanPath, dir)
	}
	if cfg.DBPath != readOnlyDB {
		t.Fatalf("selected DB = %q, want first sorted %q", cfg.DBPath, readOnlyDB)
	}
	if cfg.WriteEnabled {
		t.Fatal("selected read-only file should disable writes")
	}
	if len(files) != 2 {
		t.Fatalf("files = %d, want 2", len(files))
	}
	if files[0].Name != "archive.mmb" || files[0].WriteEnabled {
		t.Fatalf("first file = %+v, want read-only archive", files[0])
	}
	if files[1].Name != "current.mmb" || !files[1].WriteEnabled {
		t.Fatalf("second file = %+v, want writable current", files[1])
	}
}

func TestResolveStartupDatabasePrefersConfiguredFile(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "a.mmb")
	second := filepath.Join(dir, "b.mmb")
	buildSyntheticDB(t, first)
	buildSyntheticDB(t, second)

	cfg, files, err := resolveStartupDatabase(config{DBPath: second})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DBScanPath != dir {
		t.Fatalf("DBScanPath = %q, want %q", cfg.DBScanPath, dir)
	}
	if cfg.DBPath != second {
		t.Fatalf("selected DB = %q, want configured file %q", cfg.DBPath, second)
	}
	if !cfg.WriteEnabled {
		t.Fatal("configured writable file should enable writes")
	}
	if len(files) != 2 {
		t.Fatalf("files = %d, want 2", len(files))
	}
}

func TestResolveStartupDatabaseReopensRememberedSelection(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "a.mmb")
	second := filepath.Join(dir, "b.mmb")
	buildSyntheticDB(t, first)
	buildSyntheticDB(t, second)

	// The folder is scanned, but the file Settings remembered is what opens.
	cfg, _, err := resolveStartupDatabase(config{DBPath: dir, SelectedDB: "b.mmb"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DBPath != second {
		t.Fatalf("selected DB = %q, want remembered %q", cfg.DBPath, second)
	}

	// A db_path naming one specific file is an explicit pin and outranks it.
	cfg, _, err = resolveStartupDatabase(config{DBPath: first, SelectedDB: "b.mmb"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DBPath != first {
		t.Fatalf("selected DB = %q, want pinned %q", cfg.DBPath, first)
	}

	// A remembered file that is gone falls back to the first in the folder.
	cfg, _, err = resolveStartupDatabase(config{DBPath: dir, SelectedDB: "deleted.mmb"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DBPath != first {
		t.Fatalf("selected DB = %q, want fallback %q", cfg.DBPath, first)
	}
	if cfg.SelectedDB != "a.mmb" {
		t.Fatalf("SelectedDB = %q, want the file actually opened", cfg.SelectedDB)
	}
}

func TestResolveStartupDatabaseSurvivesMissingConfiguredFile(t *testing.T) {
	dir := t.TempDir()
	present := filepath.Join(dir, "present.mmb")
	buildSyntheticDB(t, present)

	// db_path points at a file that no longer exists; the folder still has one,
	// so this is not a first run and setup must not take over.
	cfg, files, err := resolveStartupDatabase(config{DBPath: filepath.Join(dir, "gone.mmb")})
	if err != nil {
		t.Fatalf("resolveStartupDatabase: %v", err)
	}
	if cfg.DBPath != present {
		t.Fatalf("selected DB = %q, want %q", cfg.DBPath, present)
	}
	if len(files) != 1 {
		t.Fatalf("files = %d, want 1", len(files))
	}

	// With nothing left in the folder it is a first run again.
	if err := os.Remove(present); err != nil {
		t.Fatal(err)
	}
	if _, _, err := resolveStartupDatabase(config{DBPath: filepath.Join(dir, "gone.mmb")}); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("err = %v, want fs.ErrNotExist", err)
	}
}

// The Review month-transaction list uses the same in-place editor as the
// Accounts ledger: right-clicking a row navigates back to /review with
// edit_trans_id set, and the shared transaction_entry form posts to
// /transactions/update with return_to pointing at the review month.
func TestReviewMonthTransactionsEditInPlace(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.mmb")
	buildSyntheticDB(t, dbPath)

	a, err := newApp(config{
		DBPath:       dbPath,
		WriteEnabled: true,
		BackupDir:    filepath.Join(dir, "backups"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	render := func(query string) string {
		rec := httptest.NewRecorder()
		a.monthlyReview(rec, httptest.NewRequest(http.MethodGet, query, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("review %s status = %d", query, rec.Code)
		}
		return rec.Body.String()
	}

	// Plain load: the row is actionable (cursor affordance + row-menu target)
	// and carries the identifiers app.js needs to build the edit URL.
	html := render("/review?month=2025-10&account_id=12")
	for _, want := range []string{
		`class="actionable" data-trans-id="100"`,
		`data-return-to="/review?account_id=12&amp;month=2025-10"`,
		`id="row-menu"`,
		`data-action="void"`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("review month transactions missing %q", want)
		}
	}

	// An incoming transfer's canonical ledger row belongs to the *sending*
	// account, but the row must still point back at the account under review.
	transferCat, err := a.transferCategoryID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`
INSERT INTO CHECKINGACCOUNT_V1
  (TRANSID, ACCOUNTID, TOACCOUNTID, PAYEEID, TRANSCODE, TRANSAMOUNT, STATUS,
   TRANSACTIONNUMBER, NOTES, CATEGID, TRANSDATE, FOLLOWUPID, TOTRANSAMOUNT,
   LASTUPDATEDTIME, DELETEDTIME, COLOR)
VALUES (400, 2, 12, -1, 'Transfer', 25.0, '', '', 'card payment', ?,
        '2025-10-20T00:00:00', NULL, 25.0, '2025-10-20T00:00:00', NULL, -1)`,
		transferCat); err != nil {
		t.Fatal(err)
	}
	html = render("/review?month=2025-10&account_id=12")
	if !strings.Contains(html, `data-trans-id="400" data-account-id="12"`) {
		t.Fatal("incoming transfer row does not point back at the reviewed account")
	}

	// With edit_trans_id the row is replaced by the update form. Cancel returns
	// to this month at the clicked offset and re-highlights the row.
	html = render("/review?month=2025-10&account_id=12&edit_trans_id=100&scroll=120")
	for _, want := range []string{
		`action="/transactions/update"`,
		`name="trans_id" value="100"`,
		`name="scroll" value="120"`,
		`name="return_to" value="/review?account_id=12&amp;month=2025-10"`,
		`href="/review?account_id=12&amp;month=2025-10&amp;scroll=120&amp;highlight=100"`,
		`value="seed utilities"`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("review edit form missing %q", want)
		}
	}
	if strings.Contains(html, `data-trans-id="100"`) {
		t.Fatal("edited row should be replaced by the editor, not rendered twice")
	}

	// A split parent gets the split-parent variant, same as the ledger.
	html = render("/review?month=2025-08&account_id=12&edit_trans_id=186")
	if !strings.Contains(html, "Update Split Parent") {
		t.Fatal("review edit form did not use the split-parent variant for a split transaction")
	}

	// Void from the row menu posts return_to and lands back on the review month.
	rec := httptest.NewRecorder()
	a.voidTransaction(rec, formRequest(url.Values{
		"trans_id":   {"100"},
		"account_id": {"12"},
		"scroll":     {"120"},
		"return_to":  {"/review?account_id=12&month=2025-10"},
	}))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("void status = %d", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != "/review?account_id=12&month=2025-10&highlight=100" {
		t.Fatalf("void redirect = %q", got)
	}
	var status string
	if err := a.db.QueryRow(`SELECT STATUS FROM CHECKINGACCOUNT_V1 WHERE TRANSID = 100`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "Void" {
		t.Fatalf("transaction status = %q, want Void", status)
	}

	// A voided row is no longer actionable, so it cannot be re-voided.
	html = render("/review?month=2025-10&account_id=12")
	if strings.Contains(html, `data-trans-id="100"`) {
		t.Fatal("voided review row is still actionable")
	}
}

// Read-only mode must not offer the review row menu or honour edit_trans_id.
func TestReviewMonthTransactionsReadOnlyHasNoEditor(t *testing.T) {
	a, err := newApp(config{
		DBPath:       fixtureDB(t),
		WriteEnabled: false,
		BackupDir:    "backups",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	rec := httptest.NewRecorder()
	a.monthlyReview(rec, httptest.NewRequest(http.MethodGet,
		"/review?month=2025-10&account_id=12&edit_trans_id=100", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("review status = %d", rec.Code)
	}
	html := rec.Body.String()
	for _, unwanted := range []string{`id="row-menu"`, `data-trans-id="100"`, `action="/transactions/update"`} {
		if strings.Contains(html, unwanted) {
			t.Fatalf("read-only review page exposed %q", unwanted)
		}
	}
}

// A pending candidate that resolves completely (existing payee and category, no
// warnings, not a duplicate) arrives ticked; anything that still needs a look,
// such as a payee the import would create, arrives unticked.
func TestPendingCandidatesTickOnlyFullyResolvedRows(t *testing.T) {
	a, err := newApp(config{
		DBPath:       fixtureDB(t),
		WriteEnabled: true,
		BackupDir:    filepath.Join(t.TempDir(), "backups"),
		StatementDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	cands := []importCandidate{
		{Date: "2025-10-20", Type: "Withdrawal", Amount: 12.5, Payee: "Amazon", Category: "Leisure: Hobbies", Description: "AMAZON ORDER"},
		{Date: "2025-10-21", Type: "Withdrawal", Amount: 7, Payee: "Corner Bakery", Category: "Leisure: Hobbies", Description: "CORNER BAKERY"},
	}
	if _, err := a.archiveStatement(12, true, "Oct.pdf", []byte("oct"), statementHeader{PeriodStart: "2025-10-01", PeriodEnd: "2025-10-31"}, cands, ""); err != nil {
		t.Fatalf("archive: %v", err)
	}

	pending := a.derivePendingCandidates(12, "2025-10-01", "2025-10-31")
	got := map[string]importCandidate{}
	for _, c := range pending {
		got[c.Description] = c
	}
	if c := got["AMAZON ORDER"]; !c.Selected {
		t.Fatalf("fully resolved candidate not ticked: payee=%d category=%d warnings=%v errors=%v", c.PayeeID, c.CategoryID, c.Warnings, c.Errors)
	}
	if c := got["CORNER BAKERY"]; c.Selected {
		t.Fatalf("candidate with an unknown payee was ticked: warnings=%v", c.Warnings)
	}
}

func TestCandidateFullyResolved(t *testing.T) {
	cases := []struct {
		name string
		c    importCandidate
		want bool
	}{
		{"payee and category", importCandidate{Type: "Withdrawal", PayeeID: 14, CategoryID: 66}, true},
		{"split", importCandidate{Type: "Withdrawal", PayeeID: 14, CategoryID: -1, Splits: []splitLine{{CategoryID: 66, Amount: 5}}}, true},
		{"transfer", importCandidate{Type: "Transfer", ToAccountID: 13}, true},
		{"transfer without account", importCandidate{Type: "Transfer"}, false},
		{"no category", importCandidate{Type: "Withdrawal", PayeeID: 14}, false},
		{"AI suggestion to confirm", importCandidate{Type: "Withdrawal", PayeeID: 14, CategoryID: 66, Warnings: []string{"Payee suggested by AI — confirm"}}, false},
		{"duplicate", importCandidate{Type: "Withdrawal", PayeeID: 14, CategoryID: 66, Duplicate: true}, false},
		{"error", importCandidate{Type: "Withdrawal", PayeeID: 14, CategoryID: 66, Errors: []string{"amount must be greater than zero"}}, false},
	}
	for _, tc := range cases {
		if got := candidateFullyResolved(tc.c); got != tc.want {
			t.Errorf("%s: fully resolved = %v, want %v", tc.name, got, tc.want)
		}
	}
}
