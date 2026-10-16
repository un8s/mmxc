package mmxc

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

type account struct {
	ID             int
	Name           string
	Type           string
	Status         string
	AccountNum     string
	Notes          string
	HeldAt         string
	Website        string
	ContactInfo    string
	AccessInfo     string
	Initial        float64
	Balance        float64
	CurrencyID     int
	CurrencyCode   string
	Favorite       bool
	StatementDate  string
	MinimumBalance float64
	CreditLimit    float64
	InterestRate   float64
	PaymentDueDate string
	MinimumPayment float64
	InitialDate    string
	TransactionN   int
	Editable       bool
}

type currency struct {
	ID     int
	Name   string
	Symbol string
}

type accountInput struct {
	Name           string
	Type           string
	Status         string
	AccountNum     string
	Notes          string
	HeldAt         string
	Website        string
	ContactInfo    string
	AccessInfo     string
	Initial        float64
	CurrencyID     int
	Favorite       bool
	StatementDate  string
	MinimumBalance float64
	CreditLimit    float64
	InterestRate   float64
	PaymentDueDate string
	MinimumPayment float64
	InitialDate    string
}

func (a *app) accounts(w http.ResponseWriter, r *http.Request) {
	accounts, err := a.listAccounts()
	if err != nil {
		a.renderError(w, "Accounts", "accounts", err)
		return
	}
	var selected *account
	if idText := r.URL.Query().Get("account_id"); idText != "" {
		id, _ := strconv.Atoi(idText)
		for i := range accounts {
			if accounts[i].ID == id {
				selected = &accounts[i]
				break
			}
		}
	}
	if selected == nil && len(accounts) > 0 {
		selected = &accounts[0]
	}
	filter := parseTransactionFilter(r)
	var txs []transaction
	if selected != nil {
		filter.AccountID = selected.ID
		txs, err = a.listTransactions(filter)
		if err != nil {
			a.renderError(w, "Accounts", "accounts", err)
			return
		}
		if a.config.ShowAccountBalance {
			if err := a.populateAccountBalances(selected.ID, selected.Initial, txs); err != nil {
				a.renderError(w, "Accounts", "accounts", err)
				return
			}
		}
	}
	cats, err := a.listCategories()
	if err != nil {
		a.renderError(w, "Accounts", "accounts", err)
		return
	}
	payees, err := a.listPayees()
	if err != nil {
		a.renderError(w, "Accounts", "accounts", err)
		return
	}
	currencies, err := a.listCurrencies()
	if err != nil {
		a.renderError(w, "Accounts", "accounts", err)
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
	// An explicit return_to (an "Edit transaction" launched from a Reports
	// contributing-row) sends the user back where they came from — on Cancel just
	// as much as on Save, otherwise cancelling strands them here. Without one,
	// Cancel falls back to this ledger at the offset the row was clicked at.
	scroll := scrollParam(r.URL.Query().Get("scroll"))
	returnTo := localReturnURL(r)
	cancelTo := ""
	if explicit := sanitizeLocalPath(r.URL.Query().Get("return_to")); explicit != "" {
		returnTo = explicit
		cancelTo = explicit
	} else if editTx != nil {
		cancelTo = accountsURL(editTx.AccountID, scroll)
	}
	a.render(w, "accounts", pageData{
		Title:           "Accounts",
		Active:          "accounts",
		Info:            a.info(),
		Accounts:        accounts,
		AccountTypes:    accountTypes(accounts),
		Currencies:      currencies,
		Selected:        selected,
		EditTransaction: editTx,
		Scroll:          scroll,
		ReturnTo:        returnTo,
		CancelTo:        cancelTo,
		Transactions:    txs,
		Filter:          filter,
		Categories:      cats,
		Payees:          payees,
	})
}

// populateAccountBalances adds the account balance immediately after each
// displayed transaction. It walks the complete ledger so balances remain
// accurate when the visible transaction list is filtered.
func (a *app) populateAccountBalances(accountID int, initial float64, txs []transaction) error {
	if accountID <= 0 || len(txs) == 0 {
		return nil
	}
	rows, err := a.db.Query(`
SELECT TRANSID, ACCOUNTID, COALESCE(TOACCOUNTID, 0), TRANSCODE,
       TRANSAMOUNT, TOTRANSAMOUNT, COALESCE(STATUS, '')
  FROM CHECKINGACCOUNT_V1
 WHERE ACCOUNTID = ? OR TOACCOUNTID = ?
 ORDER BY TRANSDATE ASC, TRANSID ASC`, accountID, accountID)
	if err != nil {
		return err
	}
	defer rows.Close()

	balance := initial
	visible := make(map[int]bool, len(txs))
	for _, tx := range txs {
		visible[tx.ID] = true
	}
	balances := make(map[int]float64, len(txs))
	for rows.Next() {
		var id, sourceID, destinationID int
		var transactionType, status string
		var amount float64
		var toAmount sql.NullFloat64
		if err := rows.Scan(&id, &sourceID, &destinationID, &transactionType, &amount, &toAmount, &status); err != nil {
			return err
		}
		if status != "Void" {
			switch {
			case sourceID == accountID && transactionType == "Deposit":
				balance += amount
			case sourceID == accountID && (transactionType == "Withdrawal" || transactionType == "Transfer"):
				balance -= amount
			case destinationID == accountID && transactionType == "Transfer":
				if toAmount.Valid {
					balance += toAmount.Float64
				} else {
					balance += amount
				}
			}
		}
		if visible[id] {
			balances[id] = balance
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range txs {
		txs[i].Balance = balances[txs[i].ID]
	}
	return nil
}

func (a *app) createAccount(w http.ResponseWriter, r *http.Request) {
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
	input, err := a.parseAccountInput(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	id, err := a.insertAccount(input)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	a.audit(r, auditEntry{
		Action: fmt.Sprintf("account create id=%d type=%q status=%q", id, input.Type, input.Status),
		Detail: fmt.Sprintf("name=%q", input.Name),
	})
	http.Redirect(w, r, "/accounts?account_id="+strconv.Itoa(id), http.StatusSeeOther)
}

func (a *app) updateAccount(w http.ResponseWriter, r *http.Request) {
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
	id, err := strconv.Atoi(r.FormValue("account_id"))
	if err != nil || id <= 0 {
		a.httpError(w, "account id is required", http.StatusBadRequest)
		return
	}
	input, err := a.parseAccountInput(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	oldName := a.accountName(id)
	if err := a.updateAccountRecord(id, input); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	a.audit(r, auditEntry{
		Action: fmt.Sprintf("account edit id=%d type=%q status=%q", id, input.Type, input.Status),
		Detail: fmt.Sprintf("old_name=%q name=%q", oldName, input.Name),
	})
	http.Redirect(w, r, "/accounts?account_id="+strconv.Itoa(id), http.StatusSeeOther)
}

func (a *app) listAccounts() ([]account, error) {
	rows, err := a.db.Query(`
WITH tx AS (
  SELECT ACCOUNTID AS ACCOUNTID,
         SUM(CASE
           WHEN TRANSCODE = 'Deposit' THEN TRANSAMOUNT
           WHEN TRANSCODE IN ('Withdrawal', 'Transfer') THEN -TRANSAMOUNT
           ELSE 0
         END) AS DELTA,
         COUNT(*) AS N
    FROM CHECKINGACCOUNT_V1
   WHERE COALESCE(STATUS, '') <> 'Void'
   GROUP BY ACCOUNTID
  UNION ALL
  SELECT TOACCOUNTID AS ACCOUNTID,
         SUM(CASE
           WHEN TRANSCODE = 'Transfer' THEN COALESCE(TOTRANSAMOUNT, TRANSAMOUNT)
           ELSE 0
         END) AS DELTA,
         COUNT(*) AS N
    FROM CHECKINGACCOUNT_V1
   WHERE TRANSCODE = 'Transfer'
     AND TOACCOUNTID IS NOT NULL
     AND COALESCE(STATUS, '') <> 'Void'
   GROUP BY TOACCOUNTID
),
rollup AS (
  SELECT ACCOUNTID, SUM(DELTA) AS DELTA, SUM(N) AS N
    FROM tx
   GROUP BY ACCOUNTID
)
SELECT a.ACCOUNTID, a.ACCOUNTNAME, a.ACCOUNTTYPE, a.STATUS,
       COALESCE(a.ACCOUNTNUM, ''), COALESCE(a.NOTES, ''), COALESCE(a.HELDAT, ''),
       COALESCE(a.WEBSITE, ''), COALESCE(a.CONTACTINFO, ''), COALESCE(a.ACCESSINFO, ''),
       a.INITIALBAL,
       a.INITIALBAL + COALESCE(r.DELTA, 0) AS BALANCE,
       a.CURRENCYID, c.CURRENCY_SYMBOL,
       a.FAVORITEACCT,
       COALESCE(a.STATEMENTDATE, ''), COALESCE(a.MINIMUMBALANCE, 0),
       COALESCE(a.CREDITLIMIT, 0), COALESCE(a.INTERESTRATE, 0),
       COALESCE(a.PAYMENTDUEDATE, ''), COALESCE(a.MINIMUMPAYMENT, 0),
       COALESCE(a.INITIALDATE, ''),
       COALESCE(r.N, 0) AS TRANSACTION_COUNT
  FROM ACCOUNTLIST_V1 a
  LEFT JOIN rollup r ON r.ACCOUNTID = a.ACCOUNTID
  LEFT JOIN CURRENCYFORMATS_V1 c ON c.CURRENCYID = a.CURRENCYID
 ORDER BY CASE a.STATUS WHEN 'Open' THEN 0 ELSE 1 END, a.ACCOUNTTYPE, a.ACCOUNTNAME`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var accounts []account
	for rows.Next() {
		var a account
		var favorite string
		if err := rows.Scan(&a.ID, &a.Name, &a.Type, &a.Status, &a.AccountNum, &a.Notes,
			&a.HeldAt, &a.Website, &a.ContactInfo, &a.AccessInfo, &a.Initial,
			&a.Balance, &a.CurrencyID, &a.CurrencyCode, &favorite, &a.StatementDate,
			&a.MinimumBalance, &a.CreditLimit, &a.InterestRate, &a.PaymentDueDate,
			&a.MinimumPayment, &a.InitialDate, &a.TransactionN); err != nil {
			return nil, err
		}
		a.Favorite = strings.EqualFold(favorite, "TRUE")
		a.Editable = validAccountType(a.Type)
		accounts = append(accounts, a)
	}
	return accounts, rows.Err()
}

func (a *app) parseAccountInput(r *http.Request) (accountInput, error) {
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		return accountInput{}, errors.New("account name is required")
	}
	accountType := strings.TrimSpace(r.FormValue("type"))
	if !validAccountType(accountType) {
		return accountInput{}, errors.New("account type must be checking, term, cash, credit card, or loan")
	}
	status := strings.TrimSpace(r.FormValue("status"))
	if status != "Open" && status != "Closed" {
		return accountInput{}, errors.New("account status must be open or closed")
	}
	initial, err := parseFormFloat(r, "initial", false)
	if err != nil {
		return accountInput{}, fmt.Errorf("initial balance: %w", err)
	}
	currencyID, err := strconv.Atoi(r.FormValue("currency_id"))
	if err != nil || currencyID <= 0 {
		return accountInput{}, errors.New("currency is required")
	}
	if !a.currencyExists(currencyID) {
		return accountInput{}, fmt.Errorf("currency %d was not found", currencyID)
	}
	statementDate, err := parseOptionalDate(r.FormValue("statement_date"), "statement date")
	if err != nil {
		return accountInput{}, err
	}
	paymentDueDate, err := parseOptionalDate(r.FormValue("payment_due_date"), "payment due date")
	if err != nil {
		return accountInput{}, err
	}
	initialDate, err := parseOptionalDate(r.FormValue("initial_date"), "initial date")
	if err != nil {
		return accountInput{}, err
	}
	minimumBalance, err := parseFormFloat(r, "minimum_balance", true)
	if err != nil {
		return accountInput{}, fmt.Errorf("minimum balance: %w", err)
	}
	creditLimit, err := parseFormFloat(r, "credit_limit", true)
	if err != nil {
		return accountInput{}, fmt.Errorf("credit limit: %w", err)
	}
	interestRate, err := parseFormFloat(r, "interest_rate", true)
	if err != nil {
		return accountInput{}, fmt.Errorf("interest rate: %w", err)
	}
	minimumPayment, err := parseFormFloat(r, "minimum_payment", true)
	if err != nil {
		return accountInput{}, fmt.Errorf("minimum payment: %w", err)
	}
	return accountInput{
		Name:           name,
		Type:           accountType,
		Status:         status,
		AccountNum:     strings.TrimSpace(r.FormValue("account_num")),
		Notes:          strings.TrimSpace(r.FormValue("notes")),
		HeldAt:         strings.TrimSpace(r.FormValue("held_at")),
		Website:        strings.TrimSpace(r.FormValue("website")),
		ContactInfo:    strings.TrimSpace(r.FormValue("contact_info")),
		AccessInfo:     strings.TrimSpace(r.FormValue("access_info")),
		Initial:        initial,
		CurrencyID:     currencyID,
		Favorite:       r.FormValue("favorite") == "true",
		StatementDate:  statementDate,
		MinimumBalance: minimumBalance,
		CreditLimit:    creditLimit,
		InterestRate:   interestRate,
		PaymentDueDate: paymentDueDate,
		MinimumPayment: minimumPayment,
		InitialDate:    initialDate,
	}, nil
}

func (a *app) accountName(id int) string {
	var name string
	if err := a.db.QueryRow("SELECT ACCOUNTNAME FROM ACCOUNTLIST_V1 WHERE ACCOUNTID = ?", id).Scan(&name); err != nil {
		return ""
	}
	return name
}

func (a *app) accountNumber(id int) string {
	var num string
	if err := a.db.QueryRow("SELECT COALESCE(ACCOUNTNUM, '') FROM ACCOUNTLIST_V1 WHERE ACCOUNTID = ?", id).Scan(&num); err != nil {
		return ""
	}
	return num
}

func (a *app) accountType(id int) string {
	var t string
	if err := a.db.QueryRow("SELECT COALESCE(ACCOUNTTYPE, '') FROM ACCOUNTLIST_V1 WHERE ACCOUNTID = ?", id).Scan(&t); err != nil {
		return ""
	}
	return t
}

// accountIsLiability reports whether the account's balance is carried as a
// negative (amount owed) in MMEX — credit cards and loans. Their statements
// print the balance owed as a positive number, so it must be negated before
// comparing to the computed account balance.
func (a *app) accountIsLiability(id int) bool {
	switch a.accountType(id) {
	case "Credit Card", "Loan":
		return true
	}
	return false
}

// statementBalanceToLedger converts a balance as printed on a statement into the
// sign convention MMEX stores it in. A liability statement prints what is owed as
// a positive number while the ledger carries it negative; every other account
// type prints it the way the ledger already holds it.
//
// Apply this exactly once, where a printed value first enters — at extraction, or
// when reading an archived statement's sidecar. Never on a value that has already
// been through it: the Review page round-trips the converted figure through a
// hidden form field, and converting again would silently flip it back.
func (a *app) statementBalanceToLedger(accountID int, printed float64) float64 {
	if a.accountIsLiability(accountID) {
		return -printed
	}
	return printed
}

// balanceAt returns the account balance as of the end of the given date:
// INITIALBAL plus all non-void activity with TRANSDATE on or before date
// (deposits and incoming transfers add; withdrawals and outgoing transfers
// subtract). Mirrors the balance computation in listAccounts.
func (a *app) balanceAt(accountID int, date string) (float64, error) {
	var bal float64
	err := a.db.QueryRow(`
SELECT a.INITIALBAL
     + COALESCE((SELECT SUM(CASE
           WHEN TRANSCODE = 'Deposit' THEN TRANSAMOUNT
           WHEN TRANSCODE IN ('Withdrawal', 'Transfer') THEN -TRANSAMOUNT
           ELSE 0 END)
         FROM CHECKINGACCOUNT_V1
        WHERE ACCOUNTID = a.ACCOUNTID
          AND COALESCE(STATUS, '') <> 'Void'
          AND substr(COALESCE(TRANSDATE, ''), 1, 10) <= ?), 0)
     + COALESCE((SELECT SUM(COALESCE(TOTRANSAMOUNT, TRANSAMOUNT))
         FROM CHECKINGACCOUNT_V1
        WHERE TOACCOUNTID = a.ACCOUNTID
          AND TRANSCODE = 'Transfer'
          AND COALESCE(STATUS, '') <> 'Void'
          AND substr(COALESCE(TRANSDATE, ''), 1, 10) <= ?), 0)
  FROM ACCOUNTLIST_V1 a
 WHERE a.ACCOUNTID = ?`, date, date, accountID).Scan(&bal)
	if err != nil {
		return 0, err
	}
	return bal, nil
}

// hasActivityBetween reports whether the account has any non-void transaction
// dated in (after, end] — either directly (ACCOUNTID) or as the destination of
// an incoming transfer (TOACCOUNTID). Used to decide whether a trailing gap
// after the last statement period actually needs reconciling.
func (a *app) hasActivityBetween(accountID int, after, end string) bool {
	var n int
	err := a.db.QueryRow(`
SELECT COUNT(*)
  FROM CHECKINGACCOUNT_V1
 WHERE COALESCE(STATUS, '') <> 'Void'
   AND substr(COALESCE(TRANSDATE, ''), 1, 10) > ?
   AND substr(COALESCE(TRANSDATE, ''), 1, 10) <= ?
   AND (ACCOUNTID = ? OR (TRANSCODE = 'Transfer' AND TOACCOUNTID = ?))`,
		after, end, accountID, accountID).Scan(&n)
	if err != nil {
		return false
	}
	return n > 0
}

func (a *app) insertAccount(input accountInput) (int, error) {
	if !a.config.WriteEnabled {
		return 0, errors.New("write mode disabled")
	}
	if !a.accountNameAvailable(0, input.Name) {
		return 0, fmt.Errorf("account name %q already exists", input.Name)
	}
	if err := a.probeWriteLock(); err != nil {
		return 0, fmt.Errorf("database write lock unavailable: %w", err)
	}
	if _, err := a.backupDatabase("account"); err != nil {
		return 0, fmt.Errorf("backup database: %w", err)
	}

	tx, err := a.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	id, err := nextIntID(tx, "ACCOUNTLIST_V1", "ACCOUNTID")
	if err != nil {
		return 0, err
	}
	_, err = tx.Exec(`
INSERT INTO ACCOUNTLIST_V1
  (ACCOUNTID, ACCOUNTNAME, ACCOUNTTYPE, ACCOUNTNUM, STATUS, NOTES, HELDAT,
   WEBSITE, CONTACTINFO, ACCESSINFO, INITIALBAL, FAVORITEACCT, CURRENCYID,
   STATEMENTLOCKED, STATEMENTDATE, MINIMUMBALANCE, CREDITLIMIT, INTERESTRATE,
   PAYMENTDUEDATE, MINIMUMPAYMENT, INITIALDATE)
VALUES
  (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?, ?, ?, ?, ?, ?)`,
		id, input.Name, input.Type, input.AccountNum, input.Status, input.Notes,
		input.HeldAt, input.Website, input.ContactInfo, input.AccessInfo,
		input.Initial, boolText(input.Favorite), input.CurrencyID,
		nullableString(input.StatementDate), input.MinimumBalance, input.CreditLimit,
		input.InterestRate, nullableString(input.PaymentDueDate), input.MinimumPayment,
		nullableString(input.InitialDate))
	if err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

func (a *app) updateAccountRecord(id int, input accountInput) error {
	if !a.config.WriteEnabled {
		return errors.New("write mode disabled")
	}
	if !a.accountEditable(id) {
		return fmt.Errorf("account %d is not editable in MMXC", id)
	}
	if !a.accountNameAvailable(id, input.Name) {
		return fmt.Errorf("account name %q already exists", input.Name)
	}
	if err := a.probeWriteLock(); err != nil {
		return fmt.Errorf("database write lock unavailable: %w", err)
	}
	if _, err := a.backupDatabase("account-update"); err != nil {
		return fmt.Errorf("backup database: %w", err)
	}
	result, err := a.db.Exec(`
UPDATE ACCOUNTLIST_V1
   SET ACCOUNTNAME = ?, ACCOUNTTYPE = ?, ACCOUNTNUM = ?, STATUS = ?, NOTES = ?,
       HELDAT = ?, WEBSITE = ?, CONTACTINFO = ?, ACCESSINFO = ?, INITIALBAL = ?,
       FAVORITEACCT = ?, CURRENCYID = ?, STATEMENTDATE = ?, MINIMUMBALANCE = ?,
       CREDITLIMIT = ?, INTERESTRATE = ?, PAYMENTDUEDATE = ?, MINIMUMPAYMENT = ?,
       INITIALDATE = ?
 WHERE ACCOUNTID = ?`,
		input.Name, input.Type, input.AccountNum, input.Status, input.Notes,
		input.HeldAt, input.Website, input.ContactInfo, input.AccessInfo,
		input.Initial, boolText(input.Favorite), input.CurrencyID,
		nullableString(input.StatementDate), input.MinimumBalance, input.CreditLimit,
		input.InterestRate, nullableString(input.PaymentDueDate), input.MinimumPayment,
		nullableString(input.InitialDate), id)
	if err != nil {
		return err
	}
	return requireRowsAffected(result, "account", id)
}

func (a *app) listCurrencies() ([]currency, error) {
	baseID, _ := strconv.Atoi(a.infoValue("BASECURRENCYID"))
	rows, err := a.db.Query(`
SELECT CURRENCYID, CURRENCYNAME, CURRENCY_SYMBOL
  FROM CURRENCYFORMATS_V1
 ORDER BY CASE WHEN CURRENCYID = ? THEN 0 ELSE 1 END, CURRENCY_SYMBOL`, baseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var currencies []currency
	for rows.Next() {
		var c currency
		if err := rows.Scan(&c.ID, &c.Name, &c.Symbol); err != nil {
			return nil, err
		}
		currencies = append(currencies, c)
	}
	return currencies, rows.Err()
}

func accountTypes(accounts []account) []string {
	seen := map[string]bool{}
	var types []string
	for _, a := range accounts {
		if !seen[a.Type] {
			seen[a.Type] = true
			types = append(types, a.Type)
		}
	}
	return types
}

func validAccountType(value string) bool {
	switch value {
	case "Checking", "Term", "Cash", "Credit Card", "Loan":
		return true
	default:
		return false
	}
}

// transactionEditable reports whether a transaction sitting on these account
// types can be edited here. A transfer needs both sides editable; pass an empty
// toAccountType for everything else.
func transactionEditable(accountType, toAccountType string) bool {
	return validAccountType(accountType) && (toAccountType == "" || validAccountType(toAccountType))
}

func accountsURL(accountID int, scroll string) string {
	u := "/accounts"
	if accountID > 0 {
		u += "?account_id=" + strconv.Itoa(accountID)
	}
	return withScroll(u, scroll)
}
