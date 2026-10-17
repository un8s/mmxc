package mmxc

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type transaction struct {
	ID           int
	AccountID    int
	ToAccountID  int
	Date         string
	Type         string
	Status       string
	Account      string
	ToAccount    string
	Payee        string
	Category     string
	SplitCount   int
	SplitSummary string
	// SplitHint is the multi-line per-split-line breakdown shown as a hover hint
	// on the "Split: …" category cell of a transaction list. Only listTransactions
	// fills it; the edit form renders the lines themselves.
	SplitHint   string
	Editable    bool
	PayeeID     int
	CategoryID  int
	Amount      float64
	ToAmount    sql.NullFloat64
	Number      string
	Notes       string
	DisplayAmt  float64
	DisplayText string
	Balance     float64
	Splits      []splitLine
}

type splitLine struct {
	ID         int
	CategoryID int
	Category   string
	Amount     float64
	Notes      string
}

type transactionFilter struct {
	AccountID               int
	CategoryID              int
	IncludeCategoryChildren bool
	Uncategorized           bool
	PayeeID                 int
	From                    string
	To                      string
	Type                    string
	Status                  string
	Payee                   string
	Category                string
	Search                  string
	AmountMin               float64
	AmountMax               float64
	Sort                    string
	Limit                   int
}

type transactionSummary struct {
	Count       int
	Withdrawals float64
	Deposits    float64
	Net         float64
}

type transactionInput struct {
	AccountID     int
	ToAccountID   int
	Date          string
	Type          string
	Amount        float64
	PayeeID       int
	PayeeName     string
	OriginalPayee string
	CategoryID    int
	CategoryName  string
	// NewCategoryName, when set, is a subcategory of NewCategoryParentID that an
	// import creates in the same transaction; CategoryID is then filled in.
	NewCategoryParentID int
	NewCategoryName     string
	Status              string
	Notes               string
	Splits              []splitLine
	// TransferDirection and CounterpartyAccountID retain the account-relative
	// Review semantics after an imported transfer has been canonicalized to
	// MMEX's ACCOUNTID (source) / TOACCOUNTID (destination) representation. They
	// support side-aware duplicate updates and learning future import rules.
	TransferDirection     string
	CounterpartyAccountID int
	ReviewAccountID       int
	// UpdateID, when > 0, makes an imported candidate update that existing
	// transaction (upsert of a possible duplicate) instead of inserting.
	UpdateID int64
	// ArchiveFile/ArchiveCandidateKey name the archived statement row an imported
	// candidate came from, so the import can be linked back to it.
	ArchiveFile         string
	ArchiveCandidateKey string
}

func (a *app) createTransaction(w http.ResponseWriter, r *http.Request) {
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
	input, err := a.parseTransactionInput(r, true)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := a.insertManualTransaction(input); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, redirectAfterTransaction(r, input.AccountID), http.StatusSeeOther)
}

// checkTransaction is the transaction form's pre-flight: it runs the same
// parsing the real submission does, without writing. 204 means the form can be
// submitted; 409 names the payee or category it would create, for the page to
// confirm (the JSON "set" fields carry the confirmed names back); anything else
// is the error the submission would hit, as plain text.
func (a *app) checkTransaction(w http.ResponseWriter, r *http.Request) {
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
	// New transactions must use active payees and categories; edits may keep
	// inactive ones, exactly as createTransaction and updateTransaction parse.
	_, err := a.parseTransactionInput(r, strings.TrimSpace(r.FormValue("trans_id")) == "")
	var unconfirmed *unconfirmedCreation
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.As(err, &unconfirmed):
		const guidance = "confirmation before saving a transaction creates a new payee or category"
		var message string
		set := map[string]string{}
		switch {
		case unconfirmed.Payee != "" && unconfirmed.Category != "":
			message = fmt.Sprintf(a.translateUI("This creates the new category “%s” and the new payee “%s”, with that category as its default.", guidance), unconfirmed.Category, unconfirmed.Payee)
		case unconfirmed.Payee != "" && unconfirmed.PayeeDefault != "":
			message = fmt.Sprintf(a.translateUI("This creates the new payee “%s”, with “%s” as its default category.", guidance), unconfirmed.Payee, unconfirmed.PayeeDefault)
		case unconfirmed.Payee != "":
			message = fmt.Sprintf(a.translateUI("This creates the new payee “%s”.", guidance), unconfirmed.Payee)
		default:
			message = fmt.Sprintf(a.translateUI("This creates the new category “%s”.", guidance), unconfirmed.Category)
		}
		if unconfirmed.Payee != "" {
			set["create_payee"] = unconfirmed.Payee
		}
		if unconfirmed.Category != "" {
			set["create_category"] = unconfirmed.Category
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"confirm": message,
			"ok":      a.translateUI("Create and save", "button that confirms creating a new payee or category and saves the transaction"),
			"set":     set,
		})
	default:
		http.Error(w, err.Error(), http.StatusBadRequest)
	}
}

func (a *app) updateTransaction(w http.ResponseWriter, r *http.Request) {
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
	transID, err := strconv.ParseInt(r.FormValue("trans_id"), 10, 64)
	if err != nil || transID <= 0 {
		a.httpError(w, "transaction id is required", http.StatusBadRequest)
		return
	}
	var accountID int
	if a.transactionHasSplits(transID) {
		input, err := a.parseTransactionInput(r, false)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := a.updateSplitTransactionRecord(transID, input); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		accountID = input.AccountID
	} else {
		input, err := a.parseTransactionInput(r, false)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := a.updateTransactionRecord(transID, input); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		accountID = input.AccountID
	}
	http.Redirect(w, r, redirectAfterTransaction(r, accountID), http.StatusSeeOther)
}

func (a *app) voidTransaction(w http.ResponseWriter, r *http.Request) {
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
	transID, err := strconv.ParseInt(r.FormValue("trans_id"), 10, 64)
	if err != nil || transID <= 0 {
		a.httpError(w, "transaction id is required", http.StatusBadRequest)
		return
	}
	accountID, _ := strconv.Atoi(r.FormValue("account_id"))
	if err := a.markTransactionVoid(transID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, redirectAfterTransaction(r, accountID), http.StatusSeeOther)
}

func (a *app) deleteTransaction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		a.httpError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !a.config.WriteEnabled {
		a.httpError(w, "write mode is disabled", http.StatusForbidden)
		return
	}
	if !a.config.AllowTransactionDelete {
		a.httpError(w, "transaction deletion is disabled in Settings", http.StatusForbidden)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	transID, err := strconv.ParseInt(r.FormValue("trans_id"), 10, 64)
	if err != nil || transID <= 0 {
		a.httpError(w, "transaction id is required", http.StatusBadRequest)
		return
	}
	accountID, _ := strconv.Atoi(r.FormValue("account_id"))
	// Read the row first: once it is deleted the audit log is the only record
	// of what it was.
	deleted, err := a.getTransaction(int(transID))
	if err != nil {
		http.Error(w, fmt.Sprintf("transaction %d was not found", transID), http.StatusNotFound)
		return
	}
	if err := a.deleteTransactionRecord(transID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	a.audit(r, auditEntry{
		Action: fmt.Sprintf("transaction delete id=%d account_id=%d type=%q status=%q",
			transID, deleted.AccountID, deleted.Type, deleted.Status),
		Detail: fmt.Sprintf("account=%q date=%q amount=%.2f payee=%q",
			deleted.Account, deleted.Date, deleted.Amount, deleted.Payee),
	})
	// No highlight: the row it would re-select is gone.
	http.Redirect(w, r, transactionReturnURL(r, accountID), http.StatusSeeOther)
}

func (a *app) parseTransactionInput(r *http.Request, requireActive bool) (transactionInput, error) {
	accountID, err := strconv.Atoi(r.FormValue("account_id"))
	if err != nil || accountID <= 0 {
		return transactionInput{}, errors.New("account is required")
	}
	if !a.accountExists(accountID) {
		return transactionInput{}, fmt.Errorf("account %d was not found", accountID)
	}
	if !a.accountEditable(accountID) {
		return transactionInput{}, fmt.Errorf("account %d is not editable in MMXC", accountID)
	}
	date := strings.TrimSpace(r.FormValue("date"))
	if _, err := time.Parse("2006-01-02", date); err != nil {
		return transactionInput{}, errors.New("date must be YYYY-MM-DD")
	}
	transType := strings.TrimSpace(r.FormValue("type"))
	if transType != "Withdrawal" && transType != "Deposit" && transType != "Transfer" {
		return transactionInput{}, errors.New("transaction type must be withdrawal, deposit, or transfer")
	}
	amount := 0.0
	var amountErr error
	if !formBoolDefault(r, "split_mode", false) {
		amount, amountErr = parseAmountExpression(r.FormValue("amount"))
		if amountErr != nil || amount <= 0 {
			return transactionInput{}, errors.New("amount must be greater than zero")
		}
	}
	status := strings.TrimSpace(r.FormValue("status"))
	if status == "__blank" {
		status = ""
	}
	if !validTransactionStatus(status) {
		return transactionInput{}, errors.New("transaction status is not MMEX-compatible")
	}
	payeeID := -1
	categoryID := 0
	toAccountID := 0
	var splits []splitLine
	var payeeName, newCategoryName string
	var newCategoryParentID int
	var unconfirmed unconfirmedCreation
	if transType == "Transfer" {
		if formBoolDefault(r, "split_mode", false) {
			return transactionInput{}, errors.New("transfers cannot be split")
		}
		toAccountID, err = strconv.Atoi(r.FormValue("to_account_id"))
		if err != nil || toAccountID <= 0 {
			return transactionInput{}, errors.New("to account is required for transfers")
		}
		if toAccountID == accountID {
			return transactionInput{}, errors.New("transfer accounts must be different")
		}
		if !a.accountExists(toAccountID) {
			return transactionInput{}, fmt.Errorf("to account %d was not found", toAccountID)
		}
		if !a.accountEditable(toAccountID) {
			return transactionInput{}, fmt.Errorf("to account %d is not editable in MMXC", toAccountID)
		}
		categoryID, err = a.transferCategoryID()
		if err != nil {
			return transactionInput{}, err
		}
	} else {
		payeeID, err = strconv.Atoi(r.FormValue("payee_id"))
		if err != nil || payeeID <= 0 {
			payeeText := strings.TrimSpace(r.FormValue("payee_text"))
			payeeID, err = a.resolvePayeeID(payeeText)
			var notFound *payeeNotFoundError
			if errors.As(err, &notFound) {
				// A name that matches nothing may become a new payee, but only
				// with the user's confirmation of this exact name.
				payeeID, err = 0, nil
				if confirmedName(r, "create_payee", payeeText) {
					payeeName = payeeText
				} else {
					unconfirmed.Payee = payeeText
				}
			}
			if err != nil {
				return transactionInput{}, err
			}
		}
		if payeeID > 0 {
			if !a.payeeExists(payeeID) {
				return transactionInput{}, fmt.Errorf("payee %d was not found", payeeID)
			}
			if requireActive && !a.payeeActive(payeeID) {
				return transactionInput{}, fmt.Errorf("payee %d is inactive; choose an active payee", payeeID)
			}
		}
		if formBoolDefault(r, "split_mode", false) {
			splits, amount, err = a.parseSplitLines(r, requireActive)
			if err != nil {
				return transactionInput{}, err
			}
			categoryID = -1
		} else {
			categoryID, err = strconv.Atoi(r.FormValue("category_id"))
			if err != nil || categoryID <= 0 {
				categoryText := strings.TrimSpace(r.FormValue("category_text"))
				categoryID, err = a.resolveCategoryID(categoryText)
				var notFound *categoryNotFoundError
				if errors.As(err, &notFound) {
					// Only a new subcategory of an existing parent can be created,
					// the same rule the statement import follows.
					if parentID, name, canonical, ok := a.newCategoryFor(categoryText); ok {
						categoryID, err = 0, nil
						if confirmedName(r, "create_category", canonical) {
							newCategoryParentID, newCategoryName = parentID, name
						} else {
							unconfirmed.Category = canonical
						}
					}
				}
				if err != nil {
					return transactionInput{}, err
				}
			}
			if categoryID > 0 {
				if err := a.validateTransactionCategory(categoryID); err != nil {
					return transactionInput{}, err
				}
				if requireActive && !a.categoryActive(categoryID) {
					return transactionInput{}, fmt.Errorf("category %d is inactive; choose an active category", categoryID)
				}
			}
		}
	}
	if unconfirmed.Payee != "" || unconfirmed.Category != "" {
		// A new payee takes the transaction's category as its default, so
		// the confirmation names it (a split has no single category).
		if unconfirmed.Payee != "" {
			switch {
			case unconfirmed.Category != "":
				unconfirmed.PayeeDefault = unconfirmed.Category
			case newCategoryName == "" && categoryID > 0:
				unconfirmed.PayeeDefault = a.categoryPath(categoryID)
			}
		}
		return transactionInput{}, &unconfirmed
	}
	return transactionInput{
		AccountID:           accountID,
		ToAccountID:         toAccountID,
		Date:                date,
		Type:                transType,
		Amount:              amount,
		PayeeID:             payeeID,
		PayeeName:           payeeName,
		CategoryID:          categoryID,
		NewCategoryParentID: newCategoryParentID,
		NewCategoryName:     newCategoryName,
		Status:              status,
		Notes:               strings.TrimSpace(r.FormValue("notes")),
		Splits:              splits,
	}, nil
}

// unconfirmedCreation is parseTransactionInput's answer when the form names a
// payee or a "Parent: Sub" category that does not exist yet and the user has
// not confirmed creating it. The check endpoint turns it into a question; a
// submission that skipped the check gets it as an ordinary error.
type unconfirmedCreation struct {
	Payee    string
	Category string
	// PayeeDefault is the category a new payee will default to, if any.
	PayeeDefault string
}

func (e *unconfirmedCreation) Error() string {
	switch {
	case e.Payee != "" && e.Category != "":
		return fmt.Sprintf("payee %q and category %q do not exist yet; confirm to create them", e.Payee, e.Category)
	case e.Payee != "":
		return fmt.Sprintf("payee %q does not exist yet; confirm to create it", e.Payee)
	default:
		return fmt.Sprintf("category %q does not exist yet; confirm to create it", e.Category)
	}
}

// confirmedName reports whether the form confirms creating exactly name. The
// confirmation carries the name rather than a yes/no flag, so editing the
// field after confirming cannot create something the user never saw.
func confirmedName(r *http.Request, field, name string) bool {
	confirmed := strings.TrimSpace(r.FormValue(field))
	return confirmed != "" && strings.EqualFold(confirmed, name)
}

// createConfirmedNamesInTx creates the subcategory and payee a confirmed form
// asked for, so the transaction can refer to them by id.
func createConfirmedNamesInTx(tx *sql.Tx, input *transactionInput) error {
	if input.NewCategoryName != "" {
		id, err := ensureCategoryInTx(tx, input.NewCategoryParentID, input.NewCategoryName)
		if err != nil {
			return err
		}
		input.CategoryID = id
	}
	if input.PayeeID <= 0 && input.PayeeName != "" && input.Type != "Transfer" {
		id, err := ensurePayeeInTx(tx, input.PayeeName, input.CategoryID)
		if err != nil {
			return err
		}
		input.PayeeID = id
	}
	return nil
}

func (a *app) parseSplitLines(r *http.Request, requireActive bool) ([]splitLine, float64, error) {
	return a.parseSplitLinesFrom(
		r.Form["split_category_id"],
		r.Form["split_category_text"],
		r.Form["split_amount"],
		r.Form["split_notes"],
		requireActive,
	)
}

// parseSplitLinesFrom builds and validates split lines from already-extracted
// parallel form slices, so the same logic serves both the manual editor (whose
// fields are unsuffixed) and the per-candidate import rows (whose fields are
// suffixed with the row index). It returns the lines and their rounded total.
func (a *app) parseSplitLinesFrom(categoryIDs, categoryTexts, amounts, notes []string, requireActive bool) ([]splitLine, float64, error) {
	maxLen := max(len(categoryIDs), len(categoryTexts), len(amounts), len(notes))
	var splits []splitLine
	var total float64
	for i := 0; i < maxLen; i++ {
		rawAmount := strings.TrimSpace(formValueAt(amounts, i))
		rawCategoryID := strings.TrimSpace(formValueAt(categoryIDs, i))
		rawCategoryText := strings.TrimSpace(formValueAt(categoryTexts, i))
		rawNotes := strings.TrimSpace(formValueAt(notes, i))
		if rawAmount == "" && rawCategoryID == "" && rawCategoryText == "" && rawNotes == "" {
			continue
		}
		amount, err := parseAmountExpression(rawAmount)
		if err != nil || amount <= 0 {
			return nil, 0, fmt.Errorf("split line %d amount must be greater than zero", len(splits)+1)
		}
		categoryID, err := strconv.Atoi(rawCategoryID)
		if err != nil || categoryID <= 0 {
			categoryID, err = a.resolveCategoryID(rawCategoryText)
			if err != nil {
				return nil, 0, fmt.Errorf("split line %d: %w", len(splits)+1, err)
			}
		}
		if err := a.validateTransactionCategory(categoryID); err != nil {
			return nil, 0, fmt.Errorf("split line %d: %w", len(splits)+1, err)
		}
		if requireActive && !a.categoryActive(categoryID) {
			return nil, 0, fmt.Errorf("split line %d: category %d is inactive; choose an active category", len(splits)+1, categoryID)
		}
		splits = append(splits, splitLine{CategoryID: categoryID, Amount: amount, Notes: rawNotes})
		total += amount
	}
	if len(splits) < 2 {
		return nil, 0, errors.New("split transactions require at least two split lines")
	}
	return splits, roundMoney(total), nil
}

func (a *app) parseSplitParentInput(transID int64, r *http.Request) (transactionInput, error) {
	existing, err := a.getTransaction(int(transID))
	if err != nil {
		return transactionInput{}, err
	}
	if existing.SplitCount == 0 {
		return transactionInput{}, errors.New("transaction is not split")
	}
	accountID, err := strconv.Atoi(r.FormValue("account_id"))
	if err != nil || accountID <= 0 {
		accountID = existing.AccountID
	}
	if !a.accountExists(accountID) {
		return transactionInput{}, fmt.Errorf("account %d was not found", accountID)
	}
	if !a.accountEditable(accountID) {
		return transactionInput{}, fmt.Errorf("account %d is not editable in MMXC", accountID)
	}
	date := strings.TrimSpace(r.FormValue("date"))
	if _, err := time.Parse("2006-01-02", date); err != nil {
		return transactionInput{}, errors.New("date must be YYYY-MM-DD")
	}
	status := strings.TrimSpace(r.FormValue("status"))
	if status == "__blank" {
		status = ""
	}
	if !validTransactionStatus(status) {
		return transactionInput{}, errors.New("transaction status is not MMEX-compatible")
	}
	payeeID, err := strconv.Atoi(r.FormValue("payee_id"))
	if err != nil || payeeID <= 0 {
		payeeID, err = a.resolvePayeeID(r.FormValue("payee_text"))
		if err != nil {
			return transactionInput{}, err
		}
	}
	if !a.payeeExists(payeeID) {
		return transactionInput{}, fmt.Errorf("payee %d was not found", payeeID)
	}
	return transactionInput{
		AccountID: accountID,
		Date:      date,
		PayeeID:   payeeID,
		Status:    status,
		Notes:     strings.TrimSpace(r.FormValue("notes")),
	}, nil
}

func (a *app) validateTransactionCategory(id int) error {
	var parentID int
	err := a.db.QueryRow("SELECT COALESCE(PARENTID, -1) FROM CATEGORY_V1 WHERE CATEGID = ?", id).Scan(&parentID)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("category %d was not found", id)
	}
	if err != nil {
		return err
	}
	if parentID <= 0 && !a.config.AllowTopLevelCategories {
		return fmt.Errorf("category %d is top-level; choose a subcategory", id)
	}
	return nil
}

func (a *app) insertManualTransaction(input transactionInput) error {
	if !a.config.WriteEnabled {
		return errors.New("write mode disabled")
	}
	if err := a.probeWriteLock(); err != nil {
		return fmt.Errorf("database write lock unavailable: %w", err)
	}
	if _, err := a.backupDatabase("manual-transaction"); err != nil {
		return fmt.Errorf("backup database: %w", err)
	}

	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if err := createConfirmedNamesInTx(tx, &input); err != nil {
		return err
	}
	if _, err := insertTransactionInTx(tx, input); err != nil {
		return err
	}
	return tx.Commit()
}

// insertTransactionInTx inserts input as a new transaction and returns its TRANSID.
func insertTransactionInTx(tx *sql.Tx, input transactionInput) (int64, error) {
	transID, err := nextTransactionID(tx)
	if err != nil {
		return 0, err
	}
	payeeID := input.PayeeID
	if payeeID <= 0 && input.Type != "Transfer" {
		payeeID, err = ensurePayeeInTx(tx, input.PayeeName, input.CategoryID)
		if err != nil {
			return 0, err
		}
	}
	now := time.Now().Format("2006-01-02T15:04:05")
	var toAccount any
	var toAmount any
	if input.Type == "Transfer" {
		toAccount = input.ToAccountID
		toAmount = input.Amount
	}
	categoryID := input.CategoryID
	if len(input.Splits) > 0 {
		categoryID = -1
	}
	_, err = tx.Exec(`
INSERT INTO CHECKINGACCOUNT_V1
  (TRANSID, ACCOUNTID, TOACCOUNTID, PAYEEID, TRANSCODE, TRANSAMOUNT, STATUS,
   TRANSACTIONNUMBER, NOTES, CATEGID, TRANSDATE, FOLLOWUPID, TOTRANSAMOUNT,
   LASTUPDATEDTIME, DELETEDTIME, COLOR)
VALUES
  (?, ?, ?, ?, ?, ?, ?, '', ?, ?, ?, NULL, ?, ?, NULL, -1)`,
		transID, input.AccountID, toAccount, payeeID, input.Type, input.Amount, input.Status,
		input.Notes, categoryID, input.Date+"T00:00:00", toAmount, now)
	if err != nil {
		return 0, err
	}
	if len(input.Splits) > 0 {
		if err := replaceSplitLines(tx, transID, input.Splits); err != nil {
			return 0, err
		}
	}
	if payeeID > 0 && input.OriginalPayee != "" && !strings.EqualFold(strings.TrimSpace(input.OriginalPayee), strings.TrimSpace(input.PayeeName)) {
		if err := appendPayeePatternInTx(tx, payeeID, input.OriginalPayee); err != nil {
			return 0, err
		}
	}
	return transID, nil
}

func (a *app) updateTransactionRecord(transID int64, input transactionInput) error {
	if !a.config.WriteEnabled {
		return errors.New("write mode disabled")
	}
	if a.transactionHasSplits(transID) {
		return errors.New("split transactions require the split parent editor")
	}
	if !a.transactionAccountsEditable(transID) {
		return fmt.Errorf("transaction %d belongs to an account type that is not editable in MMXC", transID)
	}
	if err := a.probeWriteLock(); err != nil {
		return fmt.Errorf("database write lock unavailable: %w", err)
	}
	if _, err := a.backupDatabase("transaction-update"); err != nil {
		return fmt.Errorf("backup database: %w", err)
	}
	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := createConfirmedNamesInTx(tx, &input); err != nil {
		return err
	}

	var toAccount any
	var toAmount any
	if input.Type == "Transfer" {
		toAccount = input.ToAccountID
		toAmount = input.Amount
	}
	categoryID := input.CategoryID
	if len(input.Splits) > 0 {
		categoryID = -1
	}
	result, err := tx.Exec(`
UPDATE CHECKINGACCOUNT_V1
   SET ACCOUNTID = ?, TOACCOUNTID = ?, PAYEEID = ?, TRANSCODE = ?, TRANSAMOUNT = ?,
       STATUS = ?, NOTES = ?, CATEGID = ?, TRANSDATE = ?, TOTRANSAMOUNT = ?, LASTUPDATEDTIME = ?
 WHERE TRANSID = ?`,
		input.AccountID, toAccount, input.PayeeID, input.Type, input.Amount,
		input.Status, input.Notes, categoryID, input.Date+"T00:00:00", toAmount,
		time.Now().Format("2006-01-02T15:04:05"), transID)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		return fmt.Errorf("transaction %d was not found", transID)
	}
	if len(input.Splits) > 0 {
		if err := replaceSplitLines(tx, transID, input.Splits); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (a *app) updateSplitTransactionRecord(transID int64, input transactionInput) error {
	if !a.config.WriteEnabled {
		return errors.New("write mode disabled")
	}
	if !a.transactionHasSplits(transID) {
		return errors.New("transaction is not split")
	}
	if len(input.Splits) == 0 {
		return errors.New("split transaction update requires split lines")
	}
	if !a.transactionAccountsEditable(transID) {
		return fmt.Errorf("transaction %d belongs to an account type that is not editable in MMXC", transID)
	}
	if err := a.probeWriteLock(); err != nil {
		return fmt.Errorf("database write lock unavailable: %w", err)
	}
	if _, err := a.backupDatabase("split-transaction-update"); err != nil {
		return fmt.Errorf("backup database: %w", err)
	}
	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := createConfirmedNamesInTx(tx, &input); err != nil {
		return err
	}
	result, err := tx.Exec(`
UPDATE CHECKINGACCOUNT_V1
   SET ACCOUNTID = ?, TOACCOUNTID = NULL, PAYEEID = ?, TRANSCODE = ?, TRANSAMOUNT = ?,
       STATUS = ?, NOTES = ?, CATEGID = -1, TRANSDATE = ?, TOTRANSAMOUNT = NULL, LASTUPDATEDTIME = ?
 WHERE TRANSID = ?`,
		input.AccountID, input.PayeeID, input.Type, input.Amount, input.Status, input.Notes,
		input.Date+"T00:00:00", time.Now().Format("2006-01-02T15:04:05"), transID)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		return fmt.Errorf("transaction %d was not found", transID)
	}
	if err := replaceSplitLines(tx, transID, input.Splits); err != nil {
		return err
	}
	return tx.Commit()
}

func (a *app) markTransactionVoid(transID int64) error {
	if !a.config.WriteEnabled {
		return errors.New("write mode disabled")
	}
	if !a.transactionAccountsEditable(transID) {
		return fmt.Errorf("transaction %d belongs to an account type that is not editable in MMXC", transID)
	}
	if err := a.probeWriteLock(); err != nil {
		return fmt.Errorf("database write lock unavailable: %w", err)
	}
	if _, err := a.backupDatabase("void-transaction"); err != nil {
		return fmt.Errorf("backup database: %w", err)
	}
	result, err := a.db.Exec("UPDATE CHECKINGACCOUNT_V1 SET STATUS = 'Void', LASTUPDATEDTIME = ? WHERE TRANSID = ?", time.Now().Format("2006-01-02T15:04:05"), transID)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		return fmt.Errorf("transaction %d was not found", transID)
	}
	return nil
}

// deleteTransactionRecord physically removes a transaction: the opt-in
// (allow_transaction_delete) alternative to markTransactionVoid. The rows MMEX
// keys to the transaction go with it (split lines and their tags, tags, custom
// field values) so nothing is left orphaned. Attachments and asset/share links
// are refused instead: MMEX pairs those with files and holdings MMXC does not
// manage, so such a transaction has to be deleted in MMEX.
func (a *app) deleteTransactionRecord(transID int64) error {
	if !a.config.WriteEnabled {
		return errors.New("write mode disabled")
	}
	if !a.config.AllowTransactionDelete {
		return errors.New("transaction deletion is disabled")
	}
	if !a.transactionAccountsEditable(transID) {
		return fmt.Errorf("transaction %d belongs to an account type that is not editable in MMXC", transID)
	}
	// Older MMEX files predate the tag and custom-field tables, so each one is
	// only touched when present. Resolved up front: the checks query a.db, which
	// must not happen while the write transaction below holds the connection.
	if a.tableExists("ATTACHMENT_V1") && a.countWhere("ATTACHMENT_V1", "REFTYPE = 'Transaction' AND REFID = ?", transID) > 0 {
		return fmt.Errorf("transaction %d has attachments; delete it in MMEX so the attached files are removed too", transID)
	}
	if (a.tableExists("TRANSLINK_V1") && a.countWhere("TRANSLINK_V1", "CHECKINGACCOUNTID = ?", transID) > 0) ||
		(a.tableExists("SHAREINFO_V1") && a.countWhere("SHAREINFO_V1", "CHECKINGACCOUNTID = ?", transID) > 0) {
		return fmt.Errorf("transaction %d is linked to an asset or share; delete it in MMEX so the holding is updated too", transID)
	}
	hasTags := a.tableExists("TAGLINK_V1")
	hasCustomFields := a.tableExists("CUSTOMFIELD_V1") && a.tableExists("CUSTOMFIELDDATA_V1")
	if err := a.probeWriteLock(); err != nil {
		return fmt.Errorf("database write lock unavailable: %w", err)
	}
	if _, err := a.backupDatabase("delete-transaction"); err != nil {
		return fmt.Errorf("backup database: %w", err)
	}
	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var cleanup []string
	if hasTags {
		cleanup = append(cleanup,
			"DELETE FROM TAGLINK_V1 WHERE REFTYPE = 'TransactionSplit' AND REFID IN (SELECT SPLITTRANSID FROM SPLITTRANSACTIONS_V1 WHERE TRANSID = ?)",
			"DELETE FROM TAGLINK_V1 WHERE REFTYPE = 'Transaction' AND REFID = ?")
	}
	cleanup = append(cleanup, "DELETE FROM SPLITTRANSACTIONS_V1 WHERE TRANSID = ?")
	if hasCustomFields {
		cleanup = append(cleanup, "DELETE FROM CUSTOMFIELDDATA_V1 WHERE REFID = ? AND FIELDID IN (SELECT FIELDID FROM CUSTOMFIELD_V1 WHERE REFTYPE = 'Transaction')")
	}
	for _, stmt := range cleanup {
		if _, err := tx.Exec(stmt, transID); err != nil {
			return err
		}
	}
	result, err := tx.Exec("DELETE FROM CHECKINGACCOUNT_V1 WHERE TRANSID = ?", transID)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		return fmt.Errorf("transaction %d was not found", transID)
	}
	return tx.Commit()
}

func replaceSplitLines(tx *sql.Tx, transID int64, splits []splitLine) error {
	if _, err := tx.Exec("DELETE FROM SPLITTRANSACTIONS_V1 WHERE TRANSID = ?", transID); err != nil {
		return err
	}
	nextID, err := nextIntID(tx, "SPLITTRANSACTIONS_V1", "SPLITTRANSID")
	if err != nil {
		return err
	}
	for i, split := range splits {
		id := nextID + i
		if _, err := tx.Exec("INSERT INTO SPLITTRANSACTIONS_V1 (SPLITTRANSID, TRANSID, CATEGID, SPLITTRANSAMOUNT, NOTES) VALUES (?, ?, ?, ?, ?)", id, transID, split.CategoryID, split.Amount, split.Notes); err != nil {
			return err
		}
	}
	return nil
}

func formValueAt(values []string, index int) string {
	if index < 0 || index >= len(values) {
		return ""
	}
	return values[index]
}

func roundMoney(value float64) float64 {
	if value < 0 {
		return float64(int64(value*100-0.5)) / 100
	}
	return float64(int64(value*100+0.5)) / 100
}

// defaultRecentLimit is how many transactions the Categories and Payees pages
// list when no explicit count was chosen.
const defaultRecentLimit = 50

func parseTransactionFilter(r *http.Request) transactionFilter {
	q := r.URL.Query()
	filter := parseFilterFields(q)
	// Default to the last 90 days when no date range was requested — i.e. the
	// account was just opened (no "from" key in the query). An explicitly empty
	// "from" (the user cleared the field) is honoured and shows everything.
	if !q.Has("from") {
		filter.From = time.Now().AddDate(0, 0, -90).Format("2006-01-02")
	}
	return filter
}

// parseRecentTransactionFilter parses the same toolbar for the Categories and
// Payees pages. A single category or payee can go months between transactions,
// so a default date window there often shows an empty list; these pages default
// to the latest N transactions instead and leave the range open. A count of 0
// ("All") lifts the cap, and an explicit From/To still narrows the window.
func parseRecentTransactionFilter(r *http.Request) transactionFilter {
	q := r.URL.Query()
	filter := parseFilterFields(q)
	filter.Limit = defaultRecentLimit
	if text := strings.TrimSpace(q.Get("limit")); text != "" {
		if n, err := strconv.Atoi(text); err == nil && n >= 0 {
			filter.Limit = n
		}
	}
	return filter
}

func parseFilterFields(q url.Values) transactionFilter {
	return transactionFilter{
		From:     strings.TrimSpace(q.Get("from")),
		To:       strings.TrimSpace(q.Get("to")),
		Type:     strings.TrimSpace(q.Get("type")),
		Status:   strings.TrimSpace(q.Get("status")),
		Payee:    strings.TrimSpace(q.Get("payee")),
		Category: strings.TrimSpace(q.Get("category")),
		Search:   strings.TrimSpace(q.Get("search")),
	}
}

func (a *app) listTransactions(filter transactionFilter) ([]transaction, error) {
	var where []string
	var args []any

	if filter.AccountID != 0 {
		where = append(where, "(t.ACCOUNTID = ? OR t.TOACCOUNTID = ?)")
		args = append(args, filter.AccountID, filter.AccountID)
	}
	if filter.CategoryID != 0 {
		if filter.IncludeCategoryChildren {
			where = append(where, `(t.CATEGID IN (
				WITH RECURSIVE descendants(id) AS (
					SELECT ?
					UNION ALL
					SELECT c.CATEGID FROM CATEGORY_V1 c JOIN descendants d ON c.PARENTID = d.id
				)
				SELECT id FROM descendants
			) OR EXISTS (
				SELECT 1
				  FROM SPLITTRANSACTIONS_V1 s
				 WHERE s.TRANSID = t.TRANSID
				   AND s.CATEGID IN (
					WITH RECURSIVE descendants(id) AS (
						SELECT ?
						UNION ALL
						SELECT c.CATEGID FROM CATEGORY_V1 c JOIN descendants d ON c.PARENTID = d.id
					)
					SELECT id FROM descendants
				   )
			))`)
			args = append(args, filter.CategoryID)
		} else {
			where = append(where, `(t.CATEGID = ? OR EXISTS (
				SELECT 1
				  FROM SPLITTRANSACTIONS_V1 s
				 WHERE s.TRANSID = t.TRANSID
				   AND s.CATEGID = ?
			))`)
			args = append(args, filter.CategoryID)
		}
		args = append(args, filter.CategoryID)
	}
	if filter.Uncategorized {
		where = append(where, `((COALESCE(t.CATEGID, 0) <= 0 AND NOT EXISTS (
			SELECT 1 FROM SPLITTRANSACTIONS_V1 parent_split WHERE parent_split.TRANSID = t.TRANSID
		)) OR EXISTS (
			SELECT 1
			  FROM SPLITTRANSACTIONS_V1 uncategorized_split
			 WHERE uncategorized_split.TRANSID = t.TRANSID
			   AND COALESCE(uncategorized_split.CATEGID, 0) <= 0
		))`)
	}
	if filter.PayeeID != 0 {
		where = append(where, "t.PAYEEID = ?")
		args = append(args, filter.PayeeID)
	}
	if len(where) == 0 {
		where = append(where, "1 = 1")
	}

	if filter.From != "" {
		where = append(where, "substr(COALESCE(t.TRANSDATE, ''), 1, 10) >= ?")
		args = append(args, filter.From)
	}
	if filter.To != "" {
		where = append(where, "substr(COALESCE(t.TRANSDATE, ''), 1, 10) <= ?")
		args = append(args, filter.To)
	}
	if filter.Type != "" {
		where = append(where, "t.TRANSCODE = ?")
		args = append(args, filter.Type)
	}
	if filter.Status != "" {
		if filter.Status == "__blank" {
			where = append(where, "COALESCE(t.STATUS, '') = ''")
		} else {
			where = append(where, "COALESCE(t.STATUS, '') = ?")
			args = append(args, filter.Status)
		}
	}
	if filter.Payee != "" {
		where = append(where, "searchkey(p.PAYEENAME) LIKE ?")
		args = append(args, "%"+searchKey(filter.Payee)+"%")
	}
	if filter.Category != "" {
		where = append(where, "searchkey(c.CATEGNAME) LIKE ?")
		args = append(args, "%"+searchKey(filter.Category)+"%")
	}
	if filter.Search != "" {
		needle := "%" + searchKey(filter.Search) + "%"
		where = append(where, `(searchkey(p.PAYEENAME) LIKE ?
			OR searchkey(c.CATEGNAME) LIKE ?
			OR searchkey(split.SPLIT_CATEGORIES) LIKE ?
			OR searchkey(t.NOTES) LIKE ?
			OR searchkey(t.TRANSACTIONNUMBER) LIKE ?
			OR searchkey(a.ACCOUNTNAME) LIKE ?
			OR searchkey(ta.ACCOUNTNAME) LIKE ?)`)
		args = append(args, needle, needle, needle, needle, needle, needle, needle)
	}
	if filter.AmountMin > 0 {
		where = append(where, "t.TRANSAMOUNT >= ?")
		args = append(args, filter.AmountMin)
	}
	if filter.AmountMax > 0 {
		where = append(where, "t.TRANSAMOUNT <= ?")
		args = append(args, filter.AmountMax)
	}

	orderBy := "t.TRANSDATE DESC, t.TRANSID DESC"
	switch filter.Sort {
	case "", "date_desc":
	case "date_asc":
		orderBy = "t.TRANSDATE ASC, t.TRANSID ASC"
	case "amount_desc":
		orderBy = "t.TRANSAMOUNT DESC, t.TRANSDATE DESC, t.TRANSID DESC"
	case "amount_asc":
		orderBy = "t.TRANSAMOUNT ASC, t.TRANSDATE DESC, t.TRANSID DESC"
	}
	limitSQL := ""
	if filter.Limit > 0 {
		limitSQL = " LIMIT ?"
		args = append(args, filter.Limit)
	}

	rows, err := a.db.Query(`
SELECT t.TRANSID, t.ACCOUNTID, COALESCE(t.TOACCOUNTID, 0),
       COALESCE(t.TRANSDATE, ''), t.TRANSCODE, COALESCE(t.STATUS, ''),
       a.ACCOUNTNAME, COALESCE(ta.ACCOUNTNAME, ''), COALESCE(p.PAYEENAME, ''),
       COALESCE(c.CATEGNAME, ''), t.TRANSAMOUNT, t.TOTRANSAMOUNT,
       COALESCE(t.TRANSACTIONNUMBER, ''), COALESCE(t.NOTES, ''),
       COALESCE(split.SPLIT_COUNT, 0), COALESCE(split.SPLIT_CATEGORIES, ''),
       a.ACCOUNTTYPE, COALESCE(ta.ACCOUNTTYPE, '')
  FROM CHECKINGACCOUNT_V1 t
  JOIN ACCOUNTLIST_V1 a ON a.ACCOUNTID = t.ACCOUNTID
  LEFT JOIN ACCOUNTLIST_V1 ta ON ta.ACCOUNTID = t.TOACCOUNTID
  LEFT JOIN PAYEE_V1 p ON p.PAYEEID = t.PAYEEID
  LEFT JOIN CATEGORY_V1 c ON c.CATEGID = t.CATEGID
  LEFT JOIN (
    SELECT s.TRANSID,
           COUNT(*) AS SPLIT_COUNT,
           GROUP_CONCAT(COALESCE(sc.CATEGNAME, 'Uncategorized'), ', ') AS SPLIT_CATEGORIES
      FROM SPLITTRANSACTIONS_V1 s
      LEFT JOIN CATEGORY_V1 sc ON sc.CATEGID = s.CATEGID
     GROUP BY s.TRANSID
  ) split ON split.TRANSID = t.TRANSID
 WHERE `+strings.Join(where, " AND ")+`
 ORDER BY `+orderBy+limitSQL, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var txs []transaction
	for rows.Next() {
		var t transaction
		var accountType string
		var toAccountType string
		if err := rows.Scan(&t.ID, &t.AccountID, &t.ToAccountID, &t.Date, &t.Type, &t.Status, &t.Account, &t.ToAccount, &t.Payee, &t.Category, &t.Amount, &t.ToAmount, &t.Number, &t.Notes, &t.SplitCount, &t.SplitSummary, &accountType, &toAccountType); err != nil {
			return nil, err
		}
		t.Editable = transactionEditable(accountType, toAccountType)
		if len(t.Date) >= len("2006-01-02") {
			t.Date = t.Date[:len("2006-01-02")]
		}
		t.DisplayAmt = t.Amount
		if t.Type == "Withdrawal" || (t.Type == "Transfer" && t.AccountID == filter.AccountID) {
			t.DisplayAmt = -t.Amount
		}
		if t.Type == "Transfer" && t.ToAccountID == filter.AccountID && t.ToAmount.Valid {
			t.DisplayAmt = t.ToAmount.Float64
		}
		if t.Type == "Transfer" {
			t.DisplayText = strings.TrimSpace(t.Account + " -> " + t.ToAccount)
		} else {
			t.DisplayText = t.Payee
		}
		if t.SplitCount > 0 {
			t.Category = "Split: " + t.SplitSummary
		}
		txs = append(txs, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	if err := a.attachSplitHints(txs); err != nil {
		return nil, err
	}
	return txs, nil
}

func summarizeTransactions(txs []transaction) transactionSummary {
	var summary transactionSummary
	summary.Count = len(txs)
	for _, tx := range txs {
		switch tx.Type {
		case "Withdrawal":
			summary.Withdrawals += tx.Amount
		case "Deposit":
			summary.Deposits += tx.Amount
		}
	}
	summary.Net = summary.Deposits - summary.Withdrawals
	return summary
}

func (a *app) getTransaction(id int) (transaction, error) {
	var t transaction
	var accountType string
	var toAccountType string
	err := a.db.QueryRow(`
SELECT t.TRANSID, t.ACCOUNTID, COALESCE(t.TOACCOUNTID, 0),
       COALESCE(t.TRANSDATE, ''), t.TRANSCODE, COALESCE(t.STATUS, ''),
       a.ACCOUNTNAME, COALESCE(ta.ACCOUNTNAME, ''), COALESCE(p.PAYEENAME, ''),
       COALESCE(c.CATEGNAME, ''), COALESCE(t.PAYEEID, 0), COALESCE(t.CATEGID, 0),
       t.TRANSAMOUNT, t.TOTRANSAMOUNT, COALESCE(t.TRANSACTIONNUMBER, ''), COALESCE(t.NOTES, ''),
       COALESCE(split.SPLIT_COUNT, 0), COALESCE(split.SPLIT_CATEGORIES, ''),
       a.ACCOUNTTYPE, COALESCE(ta.ACCOUNTTYPE, '')
  FROM CHECKINGACCOUNT_V1 t
  JOIN ACCOUNTLIST_V1 a ON a.ACCOUNTID = t.ACCOUNTID
  LEFT JOIN ACCOUNTLIST_V1 ta ON ta.ACCOUNTID = t.TOACCOUNTID
  LEFT JOIN PAYEE_V1 p ON p.PAYEEID = t.PAYEEID
  LEFT JOIN CATEGORY_V1 c ON c.CATEGID = t.CATEGID
  LEFT JOIN (
    SELECT s.TRANSID,
           COUNT(*) AS SPLIT_COUNT,
           GROUP_CONCAT(COALESCE(sc.CATEGNAME, 'Uncategorized'), ', ') AS SPLIT_CATEGORIES
      FROM SPLITTRANSACTIONS_V1 s
      LEFT JOIN CATEGORY_V1 sc ON sc.CATEGID = s.CATEGID
     GROUP BY s.TRANSID
  ) split ON split.TRANSID = t.TRANSID
 WHERE t.TRANSID = ?`, id).Scan(&t.ID, &t.AccountID, &t.ToAccountID, &t.Date, &t.Type, &t.Status,
		&t.Account, &t.ToAccount, &t.Payee, &t.Category, &t.PayeeID, &t.CategoryID,
		&t.Amount, &t.ToAmount, &t.Number, &t.Notes, &t.SplitCount, &t.SplitSummary, &accountType, &toAccountType)
	if err != nil {
		return transaction{}, err
	}
	if len(t.Date) >= len("2006-01-02") {
		t.Date = t.Date[:len("2006-01-02")]
	}
	t.Editable = transactionEditable(accountType, toAccountType)
	if t.SplitCount > 0 {
		t.Category = "Split: " + t.SplitSummary
		splits, err := a.listSplitLines(t.ID)
		if err != nil {
			return transaction{}, err
		}
		t.Splits = splits
	}
	return t, nil
}

func (a *app) listSplitLines(transID int) ([]splitLine, error) {
	rows, err := a.db.Query(`
SELECT s.SPLITTRANSID, s.CATEGID, COALESCE(c.CATEGNAME, ''), s.SPLITTRANSAMOUNT, COALESCE(s.NOTES, '')
  FROM SPLITTRANSACTIONS_V1 s
  LEFT JOIN CATEGORY_V1 c ON c.CATEGID = s.CATEGID
 WHERE s.TRANSID = ?
 ORDER BY s.SPLITTRANSID`, transID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var splits []splitLine
	for rows.Next() {
		var split splitLine
		if err := rows.Scan(&split.ID, &split.CategoryID, &split.Category, &split.Amount, &split.Notes); err != nil {
			return nil, err
		}
		splits = append(splits, split)
	}
	return splits, rows.Err()
}

// splitHintBatch caps how many transaction IDs go into one IN clause, so an
// unbounded list (the account and category views have no LIMIT) can never run
// into SQLite's bound-parameter ceiling.
const splitHintBatch = 500

// attachSplitHints fills SplitHint on every split transaction in txs. The list
// query only carries the concatenated category names, so the lines are fetched
// here — batched over the whole list rather than one query per split row — and
// formatted with the parent transaction's sign.
func (a *app) attachSplitHints(txs []transaction) error {
	positions := make(map[int][]int)
	var ids []int
	for i := range txs {
		if txs[i].SplitCount == 0 {
			continue
		}
		if _, seen := positions[txs[i].ID]; !seen {
			ids = append(ids, txs[i].ID)
		}
		positions[txs[i].ID] = append(positions[txs[i].ID], i)
	}
	if len(ids) == 0 {
		return nil
	}

	lines := make(map[int][]splitLine, len(ids))
	for start := 0; start < len(ids); start += splitHintBatch {
		end := min(start+splitHintBatch, len(ids))
		batch := make([]any, 0, end-start)
		for _, id := range ids[start:end] {
			batch = append(batch, id)
		}
		if err := a.collectSplitHintLines(batch, lines); err != nil {
			return err
		}
	}

	for transID, ls := range lines {
		for _, i := range positions[transID] {
			txs[i].SplitHint = a.formatSplitHint(ls, txs[i].DisplayAmt < 0)
		}
	}
	return nil
}

// collectSplitHintLines reads the split lines of the given transaction IDs into
// lines, keyed by TRANSID and qualified with the parent category name.
func (a *app) collectSplitHintLines(ids []any, lines map[int][]splitLine) error {
	rows, err := a.db.Query(`
SELECT s.TRANSID, COALESCE(parent.CATEGNAME, ''), COALESCE(c.CATEGNAME, ''),
       s.SPLITTRANSAMOUNT, COALESCE(s.NOTES, '')
  FROM SPLITTRANSACTIONS_V1 s
  LEFT JOIN CATEGORY_V1 c ON c.CATEGID = s.CATEGID
  LEFT JOIN CATEGORY_V1 parent ON parent.CATEGID = c.PARENTID
 WHERE s.TRANSID IN (`+sqlPlaceholders(len(ids))+`)
 ORDER BY s.TRANSID, s.SPLITTRANSID`, ids...)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var transID int
		var parent string
		var line splitLine
		if err := rows.Scan(&transID, &parent, &line.Category, &line.Amount, &line.Notes); err != nil {
			return err
		}
		if parent != "" && line.Category != "" {
			line.Category = parent + ": " + line.Category
		}
		lines[transID] = append(lines[transID], line)
	}
	return rows.Err()
}

// formatSplitHint renders split lines as one "Category  amount — notes" row per
// line. negate flips the stored line amounts so they carry the same sign as the
// amount shown for the parent transaction.
func (a *app) formatSplitHint(lines []splitLine, negate bool) string {
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		name := line.Category
		if name == "" {
			name = a.translateUI("Uncategorized", "placeholder for a split transaction line without a category")
		}
		amount := line.Amount
		if negate {
			amount = -amount
		}
		text := name + "  " + formatMoney(amount, a.currFmt)
		if notes := strings.TrimSpace(line.Notes); notes != "" {
			text += " — " + notes
		}
		out = append(out, text)
	}
	return strings.Join(out, "\n")
}

func validTransactionStatus(value string) bool {
	switch value {
	case "", "Reconciled", "Void", "Follow up", "Duplicate":
		return true
	default:
		return false
	}
}

func redirectAfterTransaction(r *http.Request, accountID int) string {
	// Carry the edited transaction's ID so the landing page can re-highlight
	// the row that was just saved (createTransaction has no trans_id yet).
	return appendHighlight(transactionReturnURL(r, accountID), r.FormValue("trans_id"))
}

// transactionReturnURL is where a transaction action lands: the page that sent
// it (return_to), or else the account ledger at the saved scroll offset.
func transactionReturnURL(r *http.Request, accountID int) string {
	if returnTo := sanitizeLocalPath(r.FormValue("return_to")); returnTo != "" {
		return returnTo
	}
	return accountsURL(accountID, r.FormValue("scroll"))
}

// appendHighlight adds a highlight=<id> query parameter to a local URL so the
// client can re-select the just-edited transaction row after the editor closes.
func appendHighlight(dest, transID string) string {
	if n, err := strconv.Atoi(strings.TrimSpace(transID)); err != nil || n <= 0 {
		return dest
	}
	sep := "?"
	if strings.Contains(dest, "?") {
		sep = "&"
	}
	return dest + sep + "highlight=" + strings.TrimSpace(transID)
}
