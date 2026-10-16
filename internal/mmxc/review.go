package mmxc

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const importRulesInfoName = "MMXC_IMPORT_RULES_V1"

const statementAccountsInfoName = "MMXC_STATEMENT_ACCOUNTS_V1"

// reviewedMonthsInfoName stores the set of account-months a user has manually
// marked reviewed (for accounts that never receive statements, like cash, or any
// month the user has verified by hand). Persisted in INFOTABLE_V1 as a JSON list
// of "<accountID>:<YYYY-MM>" keys.
const reviewedMonthsInfoName = "MMXC_REVIEWED_MONTHS_V1"

type monthlyReviewAccount struct {
	Account      account
	TransactionN int
	FirstDate    string
	LastDate     string
	Income       float64
	Expenses     float64
	Net          float64
	// OpeningBalance is the account balance immediately before MonthStart.
	OpeningBalance float64
	// ClosingBalance is the account balance as of the month end (INITIALBAL plus
	// all non-void activity through MonthEnd) — what the statement closing balance
	// should be compared against.
	ClosingBalance float64
	// Statement reconciliation for the sidebar/detail: the reconciliation anchor
	// is the last cycle to close on or before month end (closingStatementForMonth),
	// so a month is measured against its own closing rather than one that lands in
	// a later month. When that statement carries a closing balance,
	// HasStatementClosing is set and ClosingDelta is the computed balance at its
	// closing date minus that stated balance (≈0 ⇒ reconciled); a month in which
	// no cycle has closed yet gets no figure at all. StatementClosingBalance is
	// held in ledger convention (see statementBalanceToLedger), not as the statement
	// printed it, so it reads the same way as ClosingBalance beside it.
	HasStatementClosing     bool
	StatementClosingDate    string
	StatementClosingBalance float64
	ClosingDelta            float64
	ClosingReconciled       bool
	// The same statement's opening end: the period start and the computed balance
	// the day before it. Anchored to the statement cycle rather than the month, so
	// it can be checked against the opening balance printed on the statement even
	// when the cycle straddles months (as card cycles do).
	StatementOpeningDate    string
	StatementOpeningBalance float64
	// Which of the balance figures above are worth putting on screen.
	//
	// IsLiability suppresses all of them: on a credit card or loan a balance at an
	// arbitrary month boundary answers no question the user has, and the reviewed
	// month's activity (payments, expenses, net) is the whole story.
	//
	// The two Show flags drop a statement figure that merely repeats the month
	// figure beside it, which is what happens whenever the statement cycle is a
	// calendar month. ClosingMatchesStatement then carries the reconciliation mark
	// onto the month's closing card, so collapsing the pair loses nothing: the two
	// can only be equal when the statement reconciles *and* nothing happened
	// between its closing date and month end.
	IsLiability             bool
	ShowStatementOpening    bool
	ShowStatementClosing    bool
	ClosingMatchesStatement bool
	// ManuallyReviewed is set when the user has explicitly marked this account's
	// month reviewed (for cash and other statement-less accounts). It overrides
	// the statement-coverage indicator.
	ManuallyReviewed bool
	ReviewStatus     string
	// ReviewState is the statement-coverage reconciliation indicator:
	// "reconciled" (statements cover the whole month), "partial" (statements
	// cover only part of it — the classic credit-card gap), or "none" (no
	// statement imported for the month).
	ReviewState string
}

type monthlyReviewData struct {
	Month         string
	MonthStart    string
	MonthEnd      string
	PreviousMonth string
	NextMonth     string
	// Year/month picker: Years runs from the earliest transaction year through
	// next year; Months is the fixed Jan–Dec list. SelectedYear/SelectedMonth
	// mark the currently open month for the dropdowns.
	Years             []int
	Months            []monthOption
	SelectedYear      int
	SelectedMonth     string
	SelectedAccountID int
	Rows              []monthlyReviewAccount
	Selected          *monthlyReviewAccount
	Transactions      []transaction
	ImportJSON        string
	ImportMessage     string
	ImportCandidates  []importCandidate
	// Statements archived for the selected account whose period overlaps the
	// review month (clickable download links).
	Statements []archivedStatement
	// Statement-import detection: when the account could not be identified
	// confidently, NeedsAccountChoice prompts the user to confirm/pick one.
	NeedsAccountChoice     bool
	StatementAccountName   string
	StatementAccountNumber string
	// StatementFile is the archived statement basename carried through the import
	// forms so the chosen account can be persisted to the archive (relocated out
	// of _pending) the moment it is selected.
	StatementFile string
	// A multi-account upload is represented by one shared document and one row
	// per extracted account section. The mapping form confirms all section → MMEX
	// account attachments together before normal account-by-account review.
	StatementDocumentHash string
	StatementSections     []statementSectionChoice
	// Closing-balance verification: compares the statement's closing balance to
	// the account balance projected after importing the (non-duplicate)
	// candidates dated on or before the statement's closing date.
	ClosingBalanceChecked   bool
	StatementClosingDate    string
	StatementClosingBalance float64
	ProjectedClosingBalance float64
	ClosingBalanceDelta     float64
	ClosingBalanceMatches   bool
	// ClosingBaseBalance is the account balance at the closing date before any
	// candidates are imported; the client adds the selected candidates' deltas to
	// it to recompute the projection live.
	ClosingBaseBalance float64
}

type importCandidate struct {
	Index         int
	Selected      bool
	Warning       string
	Date          string  `json:"date"`
	Type          string  `json:"type"`
	Amount        float64 `json:"amount"`
	Description   string  `json:"description"`
	Payee         string  `json:"payee"`
	OriginalPayee string  `json:"-"`
	Category      string  `json:"category"`
	Notes         string  `json:"notes"`
	Status        string  `json:"status"`
	Duplicate     bool
	Error         string
	Errors        []string
	Warnings      []string
	// Field-grouped messages for display: each issue is shown under the field
	// it belongs to (PayeeMessages under the payee/to-account cell,
	// CategoryMessages under the category cell, GeneralMessages — including
	// "Possible duplicate" — under the date cell). Derived from Errors/Warnings
	// by finalizeImportCandidate; the flat slices remain authoritative for the
	// skip logic and row styling.
	PayeeMessages    []candidateMessage
	CategoryMessages []candidateMessage
	GeneralMessages  []candidateMessage
	PayeeID          int
	ToAccountID      int
	ToAccount        string
	// TransferDirection is relative to the account currently being reviewed:
	// "out" means reviewed account -> counterparty, "in" means counterparty ->
	// reviewed account. ToAccountID remains the counterparty account for display
	// and form compatibility, despite its historical name.
	TransferDirection string `json:"transfer_direction,omitempty"`
	CategoryID        int
	// DuplicateID is the TRANSID of the existing transaction this candidate
	// matches, when Duplicate is set. Approving such a row updates that row
	// (upsert) rather than inserting a new transaction.
	DuplicateID int64
	// Splits, when non-empty, makes this candidate a split transaction: the
	// single Category/CategoryID is replaced by these lines (CategoryID = -1).
	// Populated by deriveCandidateFromHistory (24b) when a recurring identical
	// charge is already split, or from the per-row form fields on commit.
	Splits []splitLine `json:"-"`
	// ArchiveFile/ArchiveCandidateIndex/ArchiveCandidateKey identify the raw
	// candidate in its account-scoped statement attachment. They let the user
	// dismiss an extraction false-positive permanently without modifying the
	// shared, immutable document manifest.
	ArchiveFile           string `json:"-"`
	ArchiveCandidateIndex int    `json:"-"`
	ArchiveCandidateKey   string `json:"-"`
	// Reviewed marks a row re-submitted from the Review form (left over after a
	// partial import): its payee and category are the user's, so preparation
	// neither applies learned rules nor lets history override them.
	Reviewed bool `json:"-"`
	// NewCategoryParentID/NewCategoryName are set by validation when a Reviewed
	// row names a "Parent: Sub" category that does not exist yet; the import
	// creates it. Never filled from model output.
	NewCategoryParentID int    `json:"-"`
	NewCategoryName     string `json:"-"`
	// LearnedCategory records that a learned import rule set the category — a
	// confirmation of this statement description that history must not override.
	LearnedCategory bool `json:"-"`
}

// candidateMessage is one validation message shown under a candidate field.
type candidateMessage struct {
	Text    string
	IsError bool
}

// candidateIssue is a validation result tagged with the form field it concerns,
// so finalizeImportCandidate can route it to the right column. Field is
// "payee", "category", or "" for general issues (shown under the date cell).
type candidateIssue struct {
	Field   string
	Text    string
	IsError bool
}

type importRule struct {
	Pattern               string `json:"pattern"`
	AccountID             int    `json:"account_id,omitempty"`
	Type                  string `json:"type"`
	Payee                 string `json:"payee,omitempty"`
	Category              string `json:"category,omitempty"`
	CounterpartyAccountID int    `json:"counterparty_account_id,omitempty"`
	TransferDirection     string `json:"transfer_direction,omitempty"`
}

// statementHeader holds the statement-level metadata extracted alongside the
// transaction candidates: which account it belongs to, the period it covers,
// and the closing balance (used later for balance verification).
type statementHeader struct {
	AccountName    string  `json:"account_name,omitempty"`
	AccountNumber  string  `json:"account_number,omitempty"`
	PeriodStart    string  `json:"period_start,omitempty"`
	PeriodEnd      string  `json:"period_end,omitempty"`
	ClosingDate    string  `json:"closing_date,omitempty"`
	ClosingBalance float64 `json:"closing_balance,omitempty"`
	HasClosing     bool    `json:"has_closing,omitempty"`
}

// statementAccountMapping remembers that a statement identity (account name +
// number, normalized) maps to a particular MMEX account, so future statements
// from the same account are detected automatically. Persisted in INFOTABLE_V1.
type statementAccountMapping struct {
	Key       string `json:"key"`
	AccountID int    `json:"account_id"`
}

// statementUploadResult bundles everything the extract handler needs to render
// the review page for an uploaded statement.
type statementUploadResult struct {
	AccountID   int
	Month       string
	Candidates  []importCandidate
	RawJSON     string
	Header      statementHeader
	NeedsChoice bool
	Message     string
	// StatementFile is the archived statement's stored basename, threaded through
	// the import forms so confirming/importing can relocate it to the chosen
	// account.
	StatementFile string
	DocumentHash  string
	Sections      []statementSectionChoice
}

type statementSectionChoice struct {
	ID                 string
	AccountName        string
	AccountNumber      string
	PeriodStart        string
	PeriodEnd          string
	ClosingDate        string
	ClosingBalance     float64
	HasClosing         bool
	CandidateN         int
	SuggestedAccountID int
	SuggestedAccount   string
	Confirmed          bool
	AttachmentFile     string
	Ignored            bool
}

func (a *app) statementDocumentChoices(doc statementDocument, attachments []archivedStatement) []statementSectionChoice {
	bySection := map[string]archivedStatement{}
	for _, meta := range attachments {
		bySection[meta.SectionID] = meta
	}
	choices := make([]statementSectionChoice, 0, len(doc.Sections))
	for _, section := range doc.Sections {
		meta := bySection[section.ID]
		suggestedID := meta.AccountID
		if suggestedID <= 0 {
			suggestedID, _ = a.detectStatementAccount(section.Header)
			if suggestedID <= 0 {
				suggestedID = a.firstReviewAccountID()
			}
		}
		choices = append(choices, statementSectionChoice{
			ID:                 section.ID,
			AccountName:        section.Header.AccountName,
			AccountNumber:      section.Header.AccountNumber,
			PeriodStart:        section.Header.PeriodStart,
			PeriodEnd:          section.Header.PeriodEnd,
			ClosingDate:        section.Header.ClosingDate,
			ClosingBalance:     section.Header.ClosingBalance,
			HasClosing:         section.Header.HasClosing,
			CandidateN:         len(section.Candidates),
			SuggestedAccountID: suggestedID,
			SuggestedAccount:   a.accountName(suggestedID),
			Confirmed:          meta.Confirmed,
			AttachmentFile:     meta.File,
			Ignored:            section.Ignored,
		})
	}
	return choices
}

func (a *app) statementDocumentUploadResult(doc statementDocument, attachments []archivedStatement, fallbackMonth, name string, cached bool, preferredAccountID int) statementUploadResult {
	choices := a.statementDocumentChoices(doc, attachments)
	bySection := map[string]archivedStatement{}
	for _, meta := range attachments {
		bySection[meta.SectionID] = meta
	}
	selected := 0
	for i, section := range doc.Sections {
		if meta, ok := bySection[section.ID]; ok && meta.AccountID == preferredAccountID && preferredAccountID > 0 {
			selected = i
			break
		}
	}
	if preferredAccountID <= 0 {
		for i := range doc.Sections {
			if !doc.Sections[i].Ignored && len(doc.Sections[i].Candidates) > 0 {
				selected = i
				break
			}
		}
	}
	if selected >= len(doc.Sections) {
		selected = 0
	}
	section := doc.Sections[selected]
	attachment := bySection[section.ID]
	accountID := attachment.AccountID
	if accountID <= 0 && selected < len(choices) {
		accountID = choices[selected].SuggestedAccountID
	}
	candidates := append([]importCandidate(nil), section.Candidates...)
	if attachment.File != "" {
		candidates = archivedStatementCandidates(attachment)
	}
	a.prepareImportCandidates(accountID, candidates)
	message := fmt.Sprintf("Extracted %s into %d account sections. Confirm the account assignments below.", name, len(doc.Sections))
	if len(doc.Sections) == 1 {
		message = fmt.Sprintf("Extracted %s. Confirm the account below.", name)
	}
	if cached {
		message = fmt.Sprintf("%s was already extracted; reusing %d saved account sections.", name, len(doc.Sections))
	}
	if len(doc.Sections) == 1 && attachment.Confirmed {
		message = fmt.Sprintf("Extracted %s into %s (%s).", name, a.accountName(accountID), monthFromHeader(section.Header, fallbackMonth))
		if cached {
			message = fmt.Sprintf("%s was already extracted; reusing the saved extraction (%s).", name, monthFromHeader(section.Header, fallbackMonth))
		}
	}
	return statementUploadResult{
		AccountID:     accountID,
		Month:         monthFromHeader(section.Header, fallbackMonth),
		Candidates:    candidates,
		RawJSON:       section.RawJSON,
		Header:        section.Header,
		NeedsChoice:   !attachment.Confirmed,
		Message:       message,
		StatementFile: attachment.File,
		DocumentHash:  doc.ContentHash,
		Sections:      choices,
	}
}

func (a *app) monthlyReview(w http.ResponseWriter, r *http.Request) {
	review, err := a.buildMonthlyReview(r, nil, "", "")
	if err != nil {
		a.renderError(w, "Monthly Review", "review", err)
		return
	}
	cats, err := a.listCategories()
	if err != nil {
		a.renderError(w, "Monthly Review", "review", err)
		return
	}
	payees, err := a.listPayees()
	if err != nil {
		a.renderError(w, "Monthly Review", "review", err)
		return
	}
	accounts, err := a.listReviewAccounts()
	if err != nil {
		a.renderError(w, "Monthly Review", "review", err)
		return
	}
	// Editing a month transaction in place: same contract as the Accounts
	// ledger (edit_trans_id + scroll on the URL, row menu in app.js), so the
	// shared transaction_entry form and /transactions/update handle the rest.
	// Cancel returns to this review month at the offset the row was clicked at;
	// localReturnURL already strips edit_trans_id/scroll from ReturnTo.
	var editTx *transaction
	if idText := r.URL.Query().Get("edit_trans_id"); idText != "" && a.config.WriteEnabled {
		if id, err := strconv.Atoi(idText); err == nil {
			if tx, err := a.getTransaction(id); err == nil {
				editTx = &tx
			}
		}
	}
	scroll := scrollParam(r.URL.Query().Get("scroll"))
	returnTo := localReturnURL(r)
	cancelTo := ""
	if editTx != nil {
		cancelTo = withScroll(returnTo, scroll)
	}
	a.render(w, "review", pageData{
		Title:           "Monthly Review",
		Active:          "review",
		Info:            a.info(),
		Accounts:        accounts,
		Categories:      cats,
		Payees:          payees,
		ReturnTo:        returnTo,
		CancelTo:        cancelTo,
		EditTransaction: editTx,
		Scroll:          scroll,
		MonthlyReview:   review,
		ImportRules:     a.listImportRuleRows(),
	})
}

func (a *app) extractStatement(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		a.httpError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	res, err := a.extractStatementUpload(r)
	message := res.Message
	if err != nil && message == "" {
		message = err.Error()
	}
	r.Form.Set("month", res.Month)
	if res.AccountID > 0 {
		r.Form.Set("account_id", strconv.Itoa(res.AccountID))
	}
	displayCandidates := res.Candidates
	displayJSON := res.RawJSON
	if len(res.Sections) > 1 {
		// Multi-account imports stop at the mapping summary. An empty, non-nil
		// slice suppresses archived-candidate derivation until all sections have
		// been explicitly attached and the handler redirects to one account.
		displayCandidates = []importCandidate{}
		displayJSON = ""
	}
	review, buildErr := a.buildMonthlyReview(r, displayCandidates, displayJSON, message)
	if buildErr != nil {
		a.renderError(w, "Monthly Review", "review", buildErr)
		return
	}
	review.NeedsAccountChoice = res.NeedsChoice && (len(res.Candidates) > 0 || strings.TrimSpace(res.RawJSON) != "")
	if len(res.Sections) > 1 {
		review.NeedsAccountChoice = false
	}
	review.StatementAccountName = res.Header.AccountName
	review.StatementAccountNumber = res.Header.AccountNumber
	review.StatementFile = res.StatementFile
	review.StatementDocumentHash = res.DocumentHash
	review.StatementSections = res.Sections
	if len(res.Sections) <= 1 && res.Header.HasClosing {
		// Straight from extraction, so still signed as the statement printed it.
		closing := a.statementBalanceToLedger(res.AccountID, res.Header.ClosingBalance)
		a.applyClosingBalanceCheck(&review, res.AccountID, res.Header.ClosingDate, closing, res.Candidates)
	}
	cats, err := a.listCategories()
	if err != nil {
		a.renderError(w, "Monthly Review", "review", err)
		return
	}
	payees, err := a.listPayees()
	if err != nil {
		a.renderError(w, "Monthly Review", "review", err)
		return
	}
	accounts, err := a.listReviewAccounts()
	if err != nil {
		a.renderError(w, "Monthly Review", "review", err)
		return
	}
	a.render(w, "review", pageData{
		Title:         "Monthly Review",
		Active:        "review",
		Info:          a.info(),
		Accounts:      accounts,
		Categories:    cats,
		Payees:        payees,
		ReturnTo:      localReturnURL(r),
		MonthlyReview: review,
	})
}

// confirmStatementSections attaches every extracted section in a shared
// document to the MMEX account chosen in the multi-account mapping form.
func (a *app) confirmStatementSections(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		a.httpError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	hash := strings.TrimSpace(r.FormValue("document_hash"))
	doc, err := a.loadStatementDocument(hash)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	sectionIDs := r.Form["section_id"]
	accountIDs := r.Form["section_account_id"]
	if len(sectionIDs) != len(doc.Sections) || len(accountIDs) != len(doc.Sections) {
		a.httpError(w, "every statement account requires an assignment", http.StatusBadRequest)
		return
	}
	sectionIndexes := map[string]int{}
	for i, section := range doc.Sections {
		sectionIndexes[section.ID] = i
	}
	type sectionAssignment struct {
		sectionID string
		meta      archivedStatement
		hasMeta   bool
		accountID int
		ignore    bool
	}
	assignments := make([]sectionAssignment, 0, len(sectionIDs))
	submitted := map[string]bool{}
	for i, sectionID := range sectionIDs {
		_, ok := sectionIndexes[sectionID]
		if !ok || submitted[sectionID] {
			a.httpError(w, "unknown or duplicate statement account section", http.StatusBadRequest)
			return
		}
		submitted[sectionID] = true
		meta, hasMeta := a.findStatementSectionAttachment(hash, sectionID)
		if accountIDs[i] == "ignore" {
			assignments = append(assignments, sectionAssignment{sectionID: sectionID, meta: meta, hasMeta: hasMeta, ignore: true})
			continue
		}
		accountID, _ := strconv.Atoi(accountIDs[i])
		if !a.isReviewAccount(accountID) {
			a.httpError(w, "statement account assignment is not an editable open account", http.StatusBadRequest)
			return
		}
		assignments = append(assignments, sectionAssignment{sectionID: sectionID, meta: meta, hasMeta: hasMeta, accountID: accountID})
	}
	redirectID := 0
	redirectHasCandidates := false
	for _, assignment := range assignments {
		sectionIndex := sectionIndexes[assignment.sectionID]
		section := doc.Sections[sectionIndex]
		if assignment.ignore {
			if assignment.hasMeta {
				if err := a.deleteArchivedStatement(assignment.meta); err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
			}
			doc.Sections[sectionIndex].Ignored = true
			a.audit(r, auditEntry{
				Action: fmt.Sprintf("statement section ignore document=%q section=%q", hash, section.ID),
				Detail: fmt.Sprintf("account_name=%q account_number=%q", section.Header.AccountName, section.Header.AccountNumber),
			})
			continue
		}
		doc.Sections[sectionIndex].Ignored = false
		meta := assignment.meta
		if !assignment.hasMeta {
			var err error
			meta, err = a.createStatementSectionAttachment(doc, section)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}
		updated, err := a.confirmStatement(meta, assignment.accountID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := a.learnStatementAccount(section.Header.AccountName, section.Header.AccountNumber, assignment.accountID); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		a.audit(r, auditEntry{
			Action: fmt.Sprintf("statement section confirm document=%q section=%q account_id=%d", hash, section.ID, assignment.accountID),
			Detail: fmt.Sprintf("file=%q account=%q", updated.File, a.accountName(assignment.accountID)),
		})
		if redirectID == 0 || (!redirectHasCandidates && len(section.Candidates) > 0) {
			redirectID = assignment.accountID
			redirectHasCandidates = len(section.Candidates) > 0
		}
	}
	if err := a.writeStatementDocument(doc); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if redirectID == 0 {
		redirectID = a.firstReviewAccountID()
	}
	month := sanitizeMonth(r.FormValue("month"))
	http.Redirect(w, r, reviewURL(month, redirectID), http.StatusSeeOther)
}

func (a *app) reviewImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		a.httpError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	raw := strings.TrimSpace(r.FormValue("candidates_json"))
	candidates, err := a.parseImportCandidates(r)
	if err != nil {
		candidates = []importCandidate{{Index: 1, Error: err.Error()}}
	}

	// This handler backs the account-confirmation form: persist the chosen
	// account to the archive (relocating the statement out of _pending) and learn
	// the mapping immediately, so the association survives even if the user never
	// imports any candidate. Done before building the review so the rebuilt
	// statement list reflects the confirmation.
	stmtName := strings.TrimSpace(r.FormValue("stmt_account_name"))
	stmtNumber := strings.TrimSpace(r.FormValue("stmt_account_number"))
	stmtFile := strings.TrimSpace(r.FormValue("stmt_file"))
	chosenID, _ := strconv.Atoi(r.FormValue("account_id"))
	if stmtFile != "" && chosenID > 0 && a.accountExists(chosenID) {
		if meta, ok := a.findStatementByFile(stmtFile); ok {
			if updated, cerr := a.confirmStatement(meta, chosenID); cerr != nil {
				log.Printf("confirm statement %s: %v", stmtFile, cerr)
			} else {
				stmtFile = updated.File
				a.audit(r, auditEntry{
					Action: fmt.Sprintf("statement confirm account_id=%d", chosenID),
					Detail: fmt.Sprintf("file=%q account=%q", stmtFile, a.accountName(chosenID)),
				})
			}
		}
		if lerr := a.learnStatementAccount(stmtName, stmtNumber, chosenID); lerr != nil {
			log.Printf("learn statement account: %v", lerr)
		}
	}

	review, buildErr := a.buildMonthlyReview(r, candidates, raw, "")
	if buildErr != nil {
		a.renderError(w, "Monthly Review", "review", buildErr)
		return
	}
	review.StatementAccountName = stmtName
	review.StatementAccountNumber = stmtNumber
	review.StatementFile = stmtFile
	if cd := strings.TrimSpace(r.FormValue("stmt_closing_date")); cd != "" {
		// Already in ledger convention: this is the value a previous render
		// published into the hidden field, so it must not be converted again.
		cb, _ := strconv.ParseFloat(r.FormValue("stmt_closing_balance"), 64)
		a.applyClosingBalanceCheck(&review, review.SelectedAccountID, cd, cb, candidates)
	}
	cats, err := a.listCategories()
	if err != nil {
		a.renderError(w, "Monthly Review", "review", err)
		return
	}
	payees, err := a.listPayees()
	if err != nil {
		a.renderError(w, "Monthly Review", "review", err)
		return
	}
	accounts, err := a.listReviewAccounts()
	if err != nil {
		a.renderError(w, "Monthly Review", "review", err)
		return
	}
	a.render(w, "review", pageData{
		Title:         "Monthly Review",
		Active:        "review",
		Info:          a.info(),
		Accounts:      accounts,
		Categories:    cats,
		Payees:        payees,
		ReturnTo:      localReturnURL(r),
		MonthlyReview: review,
	})
}

// errNoImportCandidates is the one import rejection that is plain UI copy rather
// than a row-specific complaint, so it is the one that gets translated.
var errNoImportCandidates = errors.New("select at least one import candidate")

// checkImportSubmission runs every check that decides whether an import
// submission is acceptable, and writes nothing. commitReviewImport and
// validateReviewImport both go through it so the pre-flight can never disagree
// with the commit about what will be accepted.
func (a *app) checkImportSubmission(r *http.Request) ([]transactionInput, error) {
	inputs, err := a.parseApprovedImportInputs(r)
	if err != nil {
		return nil, err
	}
	if len(inputs) == 0 {
		return nil, errNoImportCandidates
	}
	return inputs, nil
}

// writeImportSubmissionError reports a checkImportSubmission rejection. The
// empty-selection message is fixed UI copy and is translated; the per-candidate
// messages embed a row number ("candidate 3 appears to duplicate ..."), so
// translating them would mint a separate cache entry and LLM round trip for
// every row index. They stay in English until they carry the number as a
// placeholder instead.
func (a *app) writeImportSubmissionError(w http.ResponseWriter, err error) {
	if errors.Is(err, errNoImportCandidates) {
		a.httpError(w, err.Error(), http.StatusBadRequest)
		return
	}
	http.Error(w, err.Error(), http.StatusBadRequest)
}

// validateReviewImport pre-flights an import submission. It performs exactly the
// checks commitReviewImport performs and touches nothing, so the Review page can
// learn that an import would be rejected without navigating: a failed commit
// replaces the page (and every unsaved edit in it) with a bare error response.
func (a *app) validateReviewImport(w http.ResponseWriter, r *http.Request) {
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
	if _, err := a.checkImportSubmission(r); err != nil {
		a.writeImportSubmissionError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *app) commitReviewImport(w http.ResponseWriter, r *http.Request) {
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
	inputs, err := a.checkImportSubmission(r)
	if err != nil {
		a.writeImportSubmissionError(w, err)
		return
	}
	accountID, _ := strconv.Atoi(r.FormValue("account_id"))
	stmtName := strings.TrimSpace(r.FormValue("stmt_account_name"))
	stmtNumber := strings.TrimSpace(r.FormValue("stmt_account_number"))
	stmtFile := strings.TrimSpace(r.FormValue("stmt_file"))
	remaining := a.remainingImportCandidates(r)
	ids, err := a.insertImportedTransactions(inputs, stmtName, stmtNumber, accountID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	a.audit(r, auditEntry{
		Action: fmt.Sprintf("statement import commit account_id=%d transactions=%d", accountID, len(inputs)),
		Detail: fmt.Sprintf("account=%q statement=%q", a.accountName(accountID), stmtFile),
	})
	// Make sure the archived statement is filed under the imported-to account
	// (covers the case where the user imported without going through the
	// account-confirmation form). insertImportedTransactions already learned the
	// mapping.
	if stmtFile != "" && accountID > 0 {
		if meta, ok := a.findStatementByFile(stmtFile); ok {
			if updated, cerr := a.confirmStatement(meta, accountID); cerr != nil {
				log.Printf("confirm statement %s: %v", stmtFile, cerr)
			} else {
				stmtFile = updated.File
				a.audit(r, auditEntry{
					Action: fmt.Sprintf("statement confirm account_id=%d", accountID),
					Detail: fmt.Sprintf("file=%q account=%q", stmtFile, a.accountName(accountID)),
				})
			}
		}
	}
	a.linkImportedCandidates(accountID, inputs, ids)
	month := sanitizeMonth(r.FormValue("month"))
	if len(remaining) > 0 {
		a.prepareImportCandidates(accountID, remaining)
		for i := range remaining {
			remaining[i].Selected = false
		}
		message := fmt.Sprintf("Imported %d candidate", len(inputs))
		if len(inputs) != 1 {
			message += "s"
		}
		message += ". Remaining candidates are kept for another import."
		review, buildErr := a.buildMonthlyReview(r, remaining, "", message)
		if buildErr != nil {
			a.renderError(w, "Monthly Review", "review", buildErr)
			return
		}
		review.StatementAccountName = stmtName
		review.StatementAccountNumber = stmtNumber
		review.StatementFile = stmtFile
		cats, err := a.listCategories()
		if err != nil {
			a.renderError(w, "Monthly Review", "review", err)
			return
		}
		payees, err := a.listPayees()
		if err != nil {
			a.renderError(w, "Monthly Review", "review", err)
			return
		}
		accounts, err := a.listReviewAccounts()
		if err != nil {
			a.renderError(w, "Monthly Review", "review", err)
			return
		}
		a.render(w, "review", pageData{
			Title:         "Monthly Review",
			Active:        "review",
			Info:          a.info(),
			Accounts:      accounts,
			Categories:    cats,
			Payees:        payees,
			ReturnTo:      localReturnURL(r),
			MonthlyReview: review,
		})
		return
	}
	http.Redirect(w, r, reviewURL(month, accountID), http.StatusSeeOther)
}

func (a *app) dismissReviewCandidate(w http.ResponseWriter, r *http.Request) {
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
	accountID, _ := strconv.Atoi(r.FormValue("account_id"))
	rowIndex, err := strconv.Atoi(r.FormValue("dismissed"))
	if accountID <= 0 || err != nil || rowIndex < 0 {
		a.httpError(w, "account and candidate are required", http.StatusBadRequest)
		return
	}
	// The button carries the row's identity; its archive fields have to be read
	// from the position that row currently occupies, which is no longer the same
	// number once an earlier row has been dismissed in place.
	pos := candidateRowPosition(r, rowIndex)
	if pos < 0 {
		a.httpError(w, "candidate archive reference is required", http.StatusBadRequest)
		return
	}
	file := strings.TrimSpace(formValueAt(r.Form["candidate_archive_file"], pos))
	archiveIndex, err := strconv.Atoi(formValueAt(r.Form["candidate_archive_index"], pos))
	key := strings.TrimSpace(formValueAt(r.Form["candidate_archive_key"], pos))
	if file == "" || err != nil || archiveIndex < 0 || key == "" {
		a.httpError(w, "candidate archive reference is required", http.StatusBadRequest)
		return
	}
	meta, ok := a.findStatementByFile(file)
	if !ok || !meta.Confirmed || meta.AccountID != accountID {
		http.NotFound(w, r)
		return
	}
	dismissed, err := dismissArchivedCandidate(&meta, archiveIndex, key)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	if err := a.writeStatementSidecar(&meta); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	month := sanitizeMonth(r.FormValue("month"))
	if len(month) == len("2006-01") {
		start := month + "-01"
		monthStart, _ := time.Parse("2006-01", month)
		end := monthStart.AddDate(0, 1, -1).Format("2006-01-02")
		for _, other := range a.statementsForMonth(accountID, start, end) {
			if other.File == meta.File {
				continue
			}
			if _, err := dismissArchivedCandidate(&other, -1, key); err != nil {
				continue
			}
			if err := a.writeStatementSidecar(&other); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}
	}
	a.audit(r, auditEntry{
		Action: fmt.Sprintf("statement candidate dismiss account_id=%d type=%q", accountID, dismissed.Type),
		Detail: fmt.Sprintf("account=%q statement=%q date=%q amount=%.2f description=%q",
			a.accountName(accountID), meta.File, dismissed.Date, dismissed.Amount, dismissed.Description),
	})
	if strings.EqualFold(r.Header.Get("X-MMXC-Partial"), "true") {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, reviewURL(month, accountID), http.StatusSeeOther)
}

func dismissArchivedCandidate(meta *archivedStatement, archiveIndex int, key string) (importCandidate, error) {
	var dismissed importCandidate
	found := false
	if archiveIndex >= 0 && archiveIndex < len(meta.Candidates) &&
		archivedCandidateKey(meta.Candidates[archiveIndex]) == key {
		dismissed = meta.Candidates[archiveIndex]
		found = true
	}
	kept := make([]importCandidate, 0, len(meta.Candidates))
	for i := range meta.Candidates {
		if archivedCandidateKey(meta.Candidates[i]) == key {
			if !found {
				dismissed = meta.Candidates[i]
				found = true
			}
			continue
		}
		kept = append(kept, meta.Candidates[i])
	}
	if !found {
		return importCandidate{}, errors.New("candidate archive changed; reload Review and try again")
	}
	meta.Candidates = kept
	return dismissed, nil
}

// linkImportedCandidates records each committed input's TRANSID on the archived
// statement row it came from, and on the same row in the account's other
// statements overlapping that statement's period (they list the charge too, as
// dismissal also accounts for). The import is already committed, so a failure
// here is only logged: it loses the link, and pending detection falls back to
// matching the ledger against the row as extracted.
func (a *app) linkImportedCandidates(accountID int, inputs []transactionInput, ids []int64) {
	live := func(id int64) bool { return a.importedTransactionLive(accountID, id) }
	loaded := map[string]*archivedStatement{}
	dirty := map[string]bool{}
	link := func(meta archivedStatement, key string, transID int64) {
		m, ok := loaded[meta.File]
		if !ok {
			m = &meta
			loaded[meta.File] = m
		}
		if linkImportedCandidate(m, key, transID, live) {
			dirty[m.File] = true
		}
	}
	for i, input := range inputs {
		if i >= len(ids) || input.ArchiveFile == "" || input.ArchiveCandidateKey == "" {
			continue
		}
		meta, ok := a.findStatementByFile(input.ArchiveFile)
		if !ok {
			continue
		}
		link(meta, input.ArchiveCandidateKey, ids[i])
		for _, other := range a.statementsForMonth(accountID, meta.PeriodStart, meta.PeriodEnd) {
			if other.File != meta.File {
				link(other, input.ArchiveCandidateKey, ids[i])
			}
		}
	}
	for file := range dirty {
		if err := a.writeStatementSidecar(loaded[file]); err != nil {
			log.Printf("link imported candidates in %s: %v", file, err)
		}
	}
}

// liveImportedLinks counts, per candidate key, meta's imported links whose
// transaction still stands in the account's ledger.
func (a *app) liveImportedLinks(accountID int, meta archivedStatement) map[string]int {
	live := map[string]int{}
	for _, l := range meta.Imported {
		if a.importedTransactionLive(accountID, l.TransID) {
			live[l.Key]++
		}
	}
	return live
}

// importedTransactionLive reports whether transID is still a non-void
// transaction on accountID (on either side, for a transfer).
func (a *app) importedTransactionLive(accountID int, transID int64) bool {
	return a.firstTransID(
		"TRANSID = ? AND (ACCOUNTID = ? OR TOACCOUNTID = ?) AND COALESCE(STATUS, '') <> 'Void'",
		transID, accountID, accountID,
	) > 0
}

func (a *app) buildMonthlyReview(r *http.Request, candidates []importCandidate, rawJSON string, message string) (monthlyReviewData, error) {
	month, start, end, err := reviewMonth(r)
	if err != nil {
		return monthlyReviewData{}, err
	}
	rows, err := a.listMonthlyReviewAccounts(start, end)
	if err != nil {
		return monthlyReviewData{}, err
	}
	selectedID, _ := strconv.Atoi(r.FormValue("account_id"))
	if selectedID <= 0 {
		selectedID, _ = strconv.Atoi(r.URL.Query().Get("account_id"))
	}
	if selectedID <= 0 {
		for i := range rows {
			if rows[i].Account.Editable && rows[i].Account.Status == "Open" {
				selectedID = rows[i].Account.ID
				break
			}
		}
	}
	var selected *monthlyReviewAccount
	for i := range rows {
		if rows[i].Account.ID == selectedID {
			selected = &rows[i]
			break
		}
	}
	if selected == nil && len(rows) > 0 {
		selectedID = rows[0].Account.ID
		selected = &rows[0]
	}
	var txs []transaction
	var statements []archivedStatement
	if selected != nil {
		txs, err = a.listTransactions(transactionFilter{
			AccountID: selected.Account.ID,
			From:      start,
			To:        end,
		})
		if err != nil {
			return monthlyReviewData{}, err
		}
		statements = a.statementsForMonth(selected.Account.ID, start, end)
	}

	// On a plain page load (no fresh upload / no posted candidates), surface the
	// still-unimported rows from this month's archived statements so a month can
	// be finished without re-uploading the file.
	if candidates == nil && selected != nil {
		if pending := a.derivePendingCandidates(selected.Account.ID, start, end); len(pending) > 0 {
			candidates = pending
			if rawJSON == "" {
				if encoded, jerr := json.MarshalIndent(pending, "", "  "); jerr == nil {
					rawJSON = string(encoded)
				}
			}
			if message == "" {
				source := "%d candidate from archived statements has not been imported yet."
				if len(pending) != 1 {
					source = "%d candidates from archived statements have not been imported yet."
				}
				message = fmt.Sprintf(a.translateUI(source, "monthly review notice for unimported bank statement transaction candidates"), len(pending))
			}
		}
	}

	prev, next := adjacentReviewMonths(month)
	selYear, _ := strconv.Atoi(month[:4])
	return monthlyReviewData{
		Month:             month,
		MonthStart:        start,
		MonthEnd:          end,
		PreviousMonth:     prev,
		NextMonth:         next,
		Years:             a.reviewYearRange(),
		Months:            monthOptions(),
		SelectedYear:      selYear,
		SelectedMonth:     month[5:7],
		SelectedAccountID: selectedID,
		Rows:              rows,
		Selected:          selected,
		Transactions:      txs,
		ImportJSON:        rawJSON,
		ImportMessage:     message,
		ImportCandidates:  candidates,
		Statements:        statements,
	}, nil
}

// derivePendingCandidates reconstructs the import candidates from this month's
// archived statements (their cached extractions), keeps only those dated within
// the month that are not already committed as transactions, and de-dups rows a
// boundary-straddling pair of statements both cover. This is what lets a month
// be finished incrementally: as each row is imported it stops being pending, and
// the month reconciles without re-uploading anything. A row linked to the
// transaction it was imported as stops being pending while that transaction
// stands, however the import edited it.
func (a *app) derivePendingCandidates(accountID int, start, end string) []importCandidate {
	var raw []importCandidate
	for _, s := range a.statementsForMonth(accountID, start, end) {
		linked := a.liveImportedLinks(accountID, s)
		for _, c := range archivedStatementCandidates(s) {
			if linked[c.ArchiveCandidateKey] > 0 {
				linked[c.ArchiveCandidateKey]--
				continue
			}
			raw = append(raw, c)
		}
	}
	if len(raw) == 0 {
		return nil
	}
	// Match the extraction as archived before learned rules and history guesses
	// can rewrite its payee/category. A generic statement description may have
	// been mapped to more than one real merchant during the original import; a
	// later learned mapping must not make those already-committed rows reappear.
	var unmatched []importCandidate
	for _, c := range raw {
		c.normalize()
		d := normalizeISODate(c.Date)
		if d == "" || d < start || d > end {
			continue
		}
		if a.importCandidateCommitted(accountID, c) {
			continue
		}
		unmatched = append(unmatched, c)
	}
	if len(unmatched) == 0 {
		return nil
	}
	cands := append([]importCandidate(nil), unmatched...)
	a.prepareImportCandidatesNoLLM(accountID, cands)

	var pending []importCandidate
	seen := map[string]bool{}
	for _, c := range cands {
		d := normalizeISODate(c.Date)
		if d == "" || d < start || d > end {
			continue // only rows that belong to this calendar month
		}
		if c.Duplicate {
			continue // already imported (matched a committed transaction)
		}
		key := fmt.Sprintf("%s|%s|%.2f|%s", d, c.Type, c.Amount, strings.ToLower(strings.TrimSpace(c.Description)))
		if seen[key] {
			continue // both overlapping statements list the same charge
		}
		seen[key] = true
		c.Selected = false
		pending = append(pending, c)
	}
	for i := range pending {
		pending[i].Index = i + 1
	}
	return pending
}

func (a *app) listMonthlyReviewAccounts(start, end string) ([]monthlyReviewAccount, error) {
	accounts, err := a.listReviewAccounts()
	if err != nil {
		return nil, err
	}
	stats, err := a.monthlyAccountStats(start, end)
	if err != nil {
		return nil, err
	}
	month := start[:7]
	monthStart, _ := time.Parse("2006-01-02", start)
	openingDate := monthStart.AddDate(0, 0, -1).Format("2006-01-02")
	reviewed := a.reviewedMonths()
	rows := make([]monthlyReviewAccount, 0, len(accounts))
	for _, acct := range accounts {
		row := monthlyReviewAccount{Account: acct}
		row.IsLiability = a.accountIsLiability(acct.ID)
		row.ManuallyReviewed = reviewed[reviewedMonthKey(acct.ID, month)]
		if stat, ok := stats[acct.ID]; ok {
			row.TransactionN = stat.TransactionN
			row.FirstDate = stat.FirstDate
			row.LastDate = stat.LastDate
			row.Income = stat.Income
			row.Expenses = stat.Expenses
			row.Net = stat.Net
		}
		if bal, berr := a.balanceAt(acct.ID, openingDate); berr == nil {
			row.OpeningBalance = bal
		}
		if bal, berr := a.balanceAt(acct.ID, end); berr == nil {
			row.ClosingBalance = bal
		}
		statements := a.statementsForMonth(acct.ID, start, end)
		closing, hasClosing := closingStatementForMonth(statements, end)
		if hasClosing {
			row.HasStatementClosing = true
			row.StatementClosingDate = closing.ClosingDate
			// The sidecar holds the balance as the statement printed it, which for a
			// card is the positive amount owed against a negative ledger balance.
			stated := a.statementBalanceToLedger(acct.ID, closing.ClosingBalance)
			row.StatementClosingBalance = roundMoney(stated)
			if bal, berr := a.balanceAt(acct.ID, closing.ClosingDate); berr == nil {
				row.ClosingDelta = roundMoney(bal - stated)
				row.ClosingReconciled = math.Abs(row.ClosingDelta) < 0.005
			}
		}
		// The opening figure is anchored to a cycle too, never to the month
		// boundary: the reconciled cycle's own period start when one closed here,
		// otherwise the cycle that opened inside this month — still running, but
		// its printed opening balance is checkable already.
		openingDate := ""
		if hasClosing {
			openingDate = closing.PeriodStart
		}
		if openingDate == "" {
			if st, ok := openingStatementForMonth(statements, start, end); ok {
				openingDate = st.PeriodStart
			}
		}
		if openingDate != "" {
			row.StatementOpeningDate = openingDate
			if bal, berr := a.balanceAt(acct.ID, prevISODate(openingDate)); berr == nil {
				row.StatementOpeningBalance = roundMoney(bal)
			}
		}
		// A statement figure earns a card of its own only when it says something
		// the month figure beside it does not; a cycle that is a calendar month
		// makes the two identical. Suppressed wholesale for liabilities, where no
		// balance card is shown at all.
		if !row.IsLiability {
			row.ShowStatementOpening = row.StatementOpeningDate != "" &&
				math.Abs(row.StatementOpeningBalance-row.OpeningBalance) >= 0.005
			row.ShowStatementClosing = row.HasStatementClosing &&
				math.Abs(row.StatementClosingBalance-row.ClosingBalance) >= 0.005
			row.ClosingMatchesStatement = row.HasStatementClosing && !row.ShowStatementClosing && row.ClosingReconciled
		}
		full, through := monthCoverage(statements, start, end)
		// A statement that closes a few days before month end (e.g. a bank cycle
		// closing on the 29th) leaves a trailing gap no later statement will fill.
		// Treat the month as reconciled only when that statement's closing balance
		// actually matches *and* nothing happened after its closing date — i.e.
		// everything is accounted for and there is genuinely nothing left to cover.
		// A mismatched closing balance (or activity in the gap) stays partial.
		reconciledThrough := full
		if !full && row.HasStatementClosing && row.ClosingReconciled &&
			!a.hasActivityBetween(acct.ID, row.StatementClosingDate, end) {
			reconciledThrough = true
		}
		switch {
		case row.ManuallyReviewed:
			row.ReviewState = "reconciled"
			row.ReviewStatus = a.monthlyReviewStatus("Marked reviewed")
		case len(statements) == 0:
			row.ReviewState = "none"
			if row.TransactionN > 0 {
				row.ReviewStatus = a.monthlyReviewStatus("No statement imported")
			} else {
				row.ReviewStatus = a.monthlyReviewStatus("No statement or activity")
			}
		case full:
			row.ReviewState = "reconciled"
			row.ReviewStatus = a.monthlyReviewStatus("Statements cover the month")
		case reconciledThrough:
			row.ReviewState = "reconciled"
			row.ReviewStatus = fmt.Sprintf(a.monthlyReviewStatus("Reconciled through %s; no later activity"), row.StatementClosingDate)
		default:
			row.ReviewState = "partial"
			if through != "" {
				row.ReviewStatus = fmt.Sprintf(a.monthlyReviewStatus("Statements through %s"), through)
			} else {
				row.ReviewStatus = a.monthlyReviewStatus("Partial statement coverage")
			}
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// listReviewAccounts keeps the review workflow focused on accounts that can
// still receive edits. Closed and MMEX account types MMXC cannot edit remain
// available in the Accounts module, but are not review/import targets.
func (a *app) listReviewAccounts() ([]account, error) {
	accounts, err := a.listAccounts()
	if err != nil {
		return nil, err
	}
	open := make([]account, 0, len(accounts))
	for _, acct := range accounts {
		if acct.Editable && acct.Status == "Open" {
			open = append(open, acct)
		}
	}
	return open, nil
}

func (a *app) monthlyReviewStatus(source string) string {
	return a.translateUI(source, "monthly account review and bank statement reconciliation status")
}

func (a *app) monthlyAccountStats(start, end string) (map[int]monthlyReviewAccount, error) {
	rows, err := a.db.Query(`
WITH tx AS (
  SELECT ACCOUNTID, substr(COALESCE(TRANSDATE, ''), 1, 10) AS D,
         CASE WHEN TRANSCODE = 'Deposit' THEN TRANSAMOUNT ELSE 0 END AS INCOME,
         CASE WHEN TRANSCODE IN ('Withdrawal', 'Transfer') THEN TRANSAMOUNT ELSE 0 END AS EXPENSES,
         CASE
           WHEN TRANSCODE = 'Deposit' THEN TRANSAMOUNT
           WHEN TRANSCODE IN ('Withdrawal', 'Transfer') THEN -TRANSAMOUNT
           ELSE 0
         END AS NET
    FROM CHECKINGACCOUNT_V1
   WHERE substr(COALESCE(TRANSDATE, ''), 1, 10) BETWEEN ? AND ?
     AND COALESCE(STATUS, '') <> 'Void'
  UNION ALL
  SELECT TOACCOUNTID, substr(COALESCE(TRANSDATE, ''), 1, 10) AS D,
         CASE WHEN TRANSCODE = 'Transfer' THEN COALESCE(TOTRANSAMOUNT, TRANSAMOUNT) ELSE 0 END AS INCOME,
         0 AS EXPENSES,
         CASE WHEN TRANSCODE = 'Transfer' THEN COALESCE(TOTRANSAMOUNT, TRANSAMOUNT) ELSE 0 END AS NET
    FROM CHECKINGACCOUNT_V1
   WHERE substr(COALESCE(TRANSDATE, ''), 1, 10) BETWEEN ? AND ?
     AND TRANSCODE = 'Transfer'
     AND TOACCOUNTID IS NOT NULL
     AND COALESCE(STATUS, '') <> 'Void'
)
SELECT ACCOUNTID, COUNT(*), MIN(D), MAX(D), SUM(INCOME), SUM(EXPENSES), SUM(NET)
  FROM tx
 GROUP BY ACCOUNTID`, start, end, start, end)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	stats := map[int]monthlyReviewAccount{}
	for rows.Next() {
		var accountID int
		var stat monthlyReviewAccount
		if err := rows.Scan(&accountID, &stat.TransactionN, &stat.FirstDate, &stat.LastDate, &stat.Income, &stat.Expenses, &stat.Net); err != nil {
			return nil, err
		}
		stats[accountID] = stat
	}
	return stats, rows.Err()
}

func (a *app) parseImportCandidates(r *http.Request) ([]importCandidate, error) {
	accountID, err := strconv.Atoi(r.FormValue("account_id"))
	if err != nil || accountID <= 0 {
		return nil, errors.New("account is required")
	}
	if !a.accountExists(accountID) {
		return nil, fmt.Errorf("account %d was not found", accountID)
	}
	raw := strings.TrimSpace(r.FormValue("candidates_json"))
	if raw == "" {
		return nil, errors.New("candidate JSON is required")
	}
	var candidates []importCandidate
	if err := json.Unmarshal([]byte(raw), &candidates); err != nil {
		return nil, fmt.Errorf("parse candidate JSON: %w", err)
	}
	a.prepareImportCandidates(accountID, candidates)
	return candidates, nil
}

func (a *app) extractStatementUpload(r *http.Request) (statementUploadResult, error) {
	file, fileHeader, err := r.FormFile("statement")
	if err != nil {
		return statementUploadResult{}, errors.New("statement file is required")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 24<<20))
	if err != nil {
		return statementUploadResult{}, err
	}
	if len(data) == 0 {
		return statementUploadResult{}, errors.New("statement file is empty")
	}
	name := filepath.Base(fileHeader.Filename)

	fallbackMonth := sanitizeMonth(r.FormValue("month"))
	if fallbackMonth == "" {
		fallbackMonth = time.Now().Format("2006-01")
	}

	preferredAccountID, _ := strconv.Atoi(r.FormValue("account_id"))
	// Content-hash dedup reuses the whole multi-account extraction. A sidecar
	// archived without a document (the no-AI path below) deliberately does not
	// satisfy this lookup: it is re-extracted once, so additional account sections
	// cannot stay hidden.
	hash := statementContentHash(data)
	if cached, err := a.loadStatementDocument(hash); err == nil {
		attachments, err := a.ensureStatementDocumentAttachments(cached)
		if err != nil {
			return statementUploadResult{}, fmt.Errorf("restore statement attachments: %w", err)
		}
		a.audit(r, auditEntry{
			Action: fmt.Sprintf("statement document reuse document=%q sections=%d", hash, len(cached.Sections)),
			Detail: fmt.Sprintf("upload=%q", name),
		})
		return a.statementDocumentUploadResult(cached, attachments, fallbackMonth, name, true, preferredAccountID), nil
	}

	// OFX and QIF downloads are read by code, so they need no AI endpoint.
	if sections, ok, err := parseBankFile(name, data); ok {
		if err != nil {
			return statementUploadResult{Month: fallbackMonth, AccountID: preferredAccountID, Message: "Could not read " + name + ": " + err.Error()}, err
		}
		doc, attachments, err := a.archiveStatementDocument(fileHeader.Filename, data, sections, "")
		if err != nil {
			return statementUploadResult{}, fmt.Errorf("save statement: %w", err)
		}
		a.audit(r, auditEntry{
			Action: fmt.Sprintf("statement document parse document=%q sections=%d", doc.ContentHash, len(doc.Sections)),
			Detail: fmt.Sprintf("upload=%q", name),
		})
		return a.statementDocumentUploadResult(doc, attachments, fallbackMonth, name, false, preferredAccountID), nil
	}

	st := a.aiRuntimeStatus(false)
	if !st.Available {
		// Still no AI: reuse a document-less archive of the same bytes instead of
		// storing the file a second time.
		if archived, ok := a.findStatementByHash(hash); ok && archived.DocumentHash == "" {
			return a.reuseArchivedStatement(archived, name, fallbackMonth), nil
		}
		res, err := a.attachStatementWithoutAI(r, fileHeader.Filename, data, fallbackMonth, st)
		if err != nil {
			return statementUploadResult{}, err
		}
		return res, nil
	}

	extraction, extractErr := a.extractCandidatesFromStatement(fileHeader.Filename, data)
	res := statementUploadResult{Month: fallbackMonth, AccountID: preferredAccountID}
	if extractErr != nil {
		// Keep the file (downloadable) but write no sidecar, so a later
		// re-upload retries extraction rather than serving an empty cache.
		accountID := preferredAccountID
		if accountID <= 0 {
			accountID = a.firstReviewAccountID()
		}
		confirmed := accountID > 0 && a.accountExists(accountID)
		if _, err := a.saveStatementFile(accountID, confirmed, fileHeader.Filename, data, time.Now()); err != nil {
			return statementUploadResult{}, fmt.Errorf("save statement: %w", err)
		}
		a.audit(r, auditEntry{
			Action: fmt.Sprintf("statement import save_failed_extraction account_id=%d confirmed=%t", accountID, confirmed),
			Detail: fmt.Sprintf("upload=%q account=%q error=%q", name, a.accountName(accountID), extractErr.Error()),
		})
		res.Message = "Saved " + name + ", but extraction failed: " + extractErr.Error()
		return res, extractErr
	}

	doc, attachments, err := a.archiveStatementDocument(fileHeader.Filename, data, extraction.Sections, extraction.RawJSON)
	if err != nil {
		return statementUploadResult{}, fmt.Errorf("save statement: %w", err)
	}
	a.audit(r, auditEntry{
		Action: fmt.Sprintf("statement document extract document=%q sections=%d", doc.ContentHash, len(doc.Sections)),
		Detail: fmt.Sprintf("upload=%q", name),
	})
	return a.statementDocumentUploadResult(doc, attachments, fallbackMonth, name, false, preferredAccountID), nil
}

func (a *app) attachStatementWithoutAI(r *http.Request, filename string, data []byte, fallbackMonth string, st aiStatus) (statementUploadResult, error) {
	accountID, _ := strconv.Atoi(r.FormValue("account_id"))
	confident := accountID > 0 && a.accountExists(accountID)
	if !confident {
		accountID = a.firstReviewAccountID()
	}
	header := statementHeaderForMonth(fallbackMonth)
	archived, err := a.archiveStatement(accountID, confident, filename, data, header, nil, "[]")
	if err != nil {
		return statementUploadResult{}, fmt.Errorf("save statement: %w", err)
	}
	detail := st.Detail
	if detail == "" {
		detail = st.Status
	}
	msg := fmt.Sprintf("Attached %s without AI-assisted import.", filepath.Base(filename))
	if detail != "" {
		msg += " " + detail
	}
	if !confident {
		msg += " Confirm the account before relying on the archive location."
	}
	a.audit(r, auditEntry{
		Action: fmt.Sprintf("statement attach_no_ai account_id=%d confirmed=%t status=%q", accountID, confident, st.Status),
		Detail: fmt.Sprintf("upload=%q stored=%q account=%q", filepath.Base(filename), archived.File, a.accountName(accountID)),
	})
	return statementUploadResult{
		AccountID:     accountID,
		Month:         fallbackMonth,
		Header:        header,
		NeedsChoice:   !confident,
		StatementFile: archived.File,
		Message:       msg,
	}, nil
}

func statementHeaderForMonth(month string) statementHeader {
	t, err := time.Parse("2006-01", month)
	if err != nil {
		return statementHeader{}
	}
	return statementHeader{
		PeriodStart: t.Format("2006-01-02"),
		PeriodEnd:   t.AddDate(0, 1, -1).Format("2006-01-02"),
	}
}

// reuseArchivedStatement builds an upload result from a content-hash cache hit,
// reusing the stored extraction so no LLM call is made and no second copy is
// archived. A confirmed statement keeps its account; an unconfirmed (pending)
// one re-runs account detection so a re-upload is never silently bound to the
// stale best guess — the user is asked to confirm again unless detection (e.g. a
// learned mapping) is now confident. The cached candidates are raw (archived
// before display mutation), so they are re-prepared against current history.
func (a *app) reuseArchivedStatement(cached archivedStatement, name, fallbackMonth string) statementUploadResult {
	header := cached.header()
	month := monthFromHeader(header, fallbackMonth)
	accountID := cached.AccountID
	needsChoice := false
	if !cached.Confirmed {
		var confident bool
		accountID, confident = a.detectStatementAccount(header)
		if accountID <= 0 {
			accountID = a.firstReviewAccountID()
			confident = false
		}
		needsChoice = !confident
	}
	candidates := archivedStatementCandidates(cached)
	a.prepareImportCandidates(accountID, candidates)
	rawJSON := strings.TrimSpace(cached.RawJSON)
	if rawJSON == "" {
		if encoded, err := json.MarshalIndent(candidates, "", "  "); err == nil {
			rawJSON = string(encoded)
		}
	}
	msg := fmt.Sprintf("%s was already extracted on %s; reusing the saved extraction (%s).", name, cached.UploadedDate(), month)
	if needsChoice {
		msg += " Confirm the account below."
	}
	return statementUploadResult{
		AccountID:     accountID,
		Month:         month,
		Header:        header,
		Candidates:    candidates,
		RawJSON:       rawJSON,
		NeedsChoice:   needsChoice,
		StatementFile: cached.File,
		Message:       msg,
	}
}

func (a *app) prepareImportCandidates(accountID int, candidates []importCandidate) {
	a.prepareImportCandidatesNoLLM(accountID, candidates)
	a.applyLLMSuggestions(accountID, candidates)
}

// prepareImportCandidatesNoLLM runs only the deterministic preparation (rules,
// history, name resolution, duplicate detection) without the batched LLM
// follow-up. Used where a model round-trip would be wrong — notably the pending
// derivation that runs on every plain review page load.
func (a *app) prepareImportCandidatesNoLLM(accountID int, candidates []importCandidate) {
	rules := a.loadImportRules()
	for i := range candidates {
		candidates[i].Index = i + 1
		candidates[i].normalize()
		if candidates[i].Status == "" {
			candidates[i].Status = "Reconciled"
		}
	}
	ambiguous := ambiguousCandidatePatterns(candidates)
	for i := range candidates {
		pattern := generalizePayeePattern(candidates[i].OriginalPayee)
		if !ambiguous[pattern] && !candidates[i].Reviewed {
			a.applyImportRules(accountID, &candidates[i], rules)
		}
		a.finalizeImportCandidate(accountID, &candidates[i])
	}
}

// ambiguousCandidatePatterns finds one raw statement description that the
// extractor has already distinguished as multiple targets within this batch.
// Applying one learned rule to all such rows would erase useful merchant or
// category information before the user can review it. Rows that also share the
// amount are one recurring charge (a mortgage debited twice a month), so the
// model labelling them differently is noise, not two merchants, and does not
// count.
func ambiguousCandidatePatterns(candidates []importCandidate) map[string]bool {
	type row struct {
		target string
		amount float64
	}
	seen := map[string][]row{}
	ambiguous := map[string]bool{}
	for _, c := range candidates {
		pattern := generalizePayeePattern(c.OriginalPayee)
		if pattern == "" {
			continue
		}
		target := strings.ToLower(strings.Join([]string{
			c.Type,
			c.TransferDirection,
			c.Payee,
			c.Category,
			strconv.Itoa(c.ToAccountID),
		}, "|"))
		for _, prev := range seen[pattern] {
			if prev.target != target && math.Abs(prev.amount-c.Amount) >= 0.005 {
				ambiguous[pattern] = true
			}
		}
		seen[pattern] = append(seen[pattern], row{target: target, amount: c.Amount})
	}
	return ambiguous
}

type llmMatch struct {
	Input string `json:"input"`
	Match string `json:"match"`
}

// applyLLMSuggestions is the second resolution pass: for candidates whose payee
// or category did not resolve deterministically, it asks the model — in a single
// batched call per statement — to map each unresolved string to one of the
// user's existing payees/categories. Matches it returns are real names (the
// model picks from the supplied lists), so applying one resolves cleanly and
// also seeds a learnable pattern on the next confirmed import. No-ops when
// disabled or when the LLM endpoint is not configured, so it never runs in tests.
func (a *app) applyLLMSuggestions(accountID int, candidates []importCandidate) {
	if !a.config.LLMResolveEnabled || !a.aiRuntimeStatus(false).Available {
		return
	}
	payeeSet := map[string]bool{}
	categorySet := map[string]bool{}
	for i := range candidates {
		c := &candidates[i]
		if c.Type == "Transfer" {
			continue
		}
		if c.PayeeID <= 0 && strings.TrimSpace(c.Payee) != "" {
			payeeSet[c.Payee] = true
		}
		if c.CategoryID <= 0 && strings.TrimSpace(c.Category) != "" {
			categorySet[c.Category] = true
		}
	}
	if len(payeeSet) == 0 && len(categorySet) == 0 {
		return
	}
	payeeMatches, categoryMatches := a.suggestImportMatches(sortedKeys(payeeSet), sortedKeys(categorySet))
	if len(payeeMatches) == 0 && len(categoryMatches) == 0 {
		return
	}
	for i := range candidates {
		c := &candidates[i]
		if c.Type == "Transfer" {
			continue
		}
		var notes []string
		if c.PayeeID <= 0 {
			if m := payeeMatches[strings.ToLower(c.Payee)]; m != "" {
				c.Payee = m
				notes = append(notes, "Payee suggested by AI — confirm")
			}
		}
		aiCategory := ""
		if c.CategoryID <= 0 {
			if m := categoryMatches[strings.ToLower(c.Category)]; m != "" {
				c.Category = m
				aiCategory = m
			}
		}
		if len(notes) == 0 && aiCategory == "" {
			continue
		}
		a.finalizeImportCandidate(accountID, c)
		// Re-finalizing lets the (possibly just resolved) payee's history replace
		// the model's category; only credit the model when its pick stood.
		if aiCategory != "" && c.Category == aiCategory {
			notes = append(notes, "Category suggested by AI — confirm")
		}
		if len(notes) == 0 {
			continue
		}
		c.Warnings = append(c.Warnings, notes...)
		c.Warning = strings.Join(c.Warnings, "; ")
	}
}

// suggestImportMatches sends the unresolved strings together with the full lists
// of existing payees and categories and returns lowercased-input -> canonical
// name maps for the matches the model is confident about. Sending the full payee
// list is affordable here because this is a separate, focused call with no
// statement text competing for the context window.
func (a *app) suggestImportMatches(payeeInputs, categoryInputs []string) (map[string]string, map[string]string) {
	payees, err := a.listPayees()
	if err != nil {
		log.Printf("llm resolve: list payees: %v", err)
		return nil, nil
	}
	allowedPayees := map[string]string{}
	payeeNames := make([]string, 0, len(payees))
	for _, p := range payees {
		allowedPayees[strings.ToLower(p.Name)] = p.Name
		payeeNames = append(payeeNames, p.Name)
	}
	categoryNames := a.categoryNamesForPrompt()
	allowedCategories := map[string]string{}
	for _, name := range categoryNames {
		allowedCategories[strings.ToLower(name)] = name
	}
	// Only ask about a list when we both have inputs for it and names to match
	// against.
	if len(payeeNames) == 0 {
		payeeInputs = nil
	}
	if len(categoryNames) == 0 {
		categoryInputs = nil
	}
	if len(payeeInputs) == 0 && len(categoryInputs) == 0 {
		return nil, nil
	}
	prompt := buildImportMatchPrompt(payeeInputs, payeeNames, categoryInputs, categoryNames)
	raw, err := a.llm.generate(a.config.LLMModel, prompt, nil, llmOptions{JSONMode: true})
	if err != nil {
		log.Printf("llm resolve: %v", err)
		return nil, nil
	}
	return parseImportMatchResponse(raw, allowedPayees, allowedCategories)
}

func buildImportMatchPrompt(payeeInputs, payeeNames, categoryInputs, categoryNames []string) string {
	var b strings.Builder
	b.WriteString("You match merchant and category labels read from a bank statement to the user's own existing names.\n")
	b.WriteString("For each input, choose the single best match from the corresponding list, copied exactly, or \"\" when none is a reasonable match. ")
	b.WriteString("Use real-world knowledge of brands (e.g. a rideshare company maps to the user's taxi payee; a donation to an organisation maps to that organisation).\n")
	b.WriteString("Return only JSON: {\"payees\":[{\"input\":\"...\",\"match\":\"...\"}],\"categories\":[{\"input\":\"...\",\"match\":\"...\"}]}\n")
	if len(payeeInputs) > 0 {
		b.WriteString("\nExisting payees:\n- ")
		b.WriteString(strings.Join(payeeNames, "\n- "))
		b.WriteString("\n\nPayee inputs:\n- ")
		b.WriteString(strings.Join(payeeInputs, "\n- "))
		b.WriteString("\n")
	}
	if len(categoryInputs) > 0 {
		b.WriteString("\nExisting categories:\n- ")
		b.WriteString(strings.Join(categoryNames, "\n- "))
		b.WriteString("\n\nCategory inputs:\n- ")
		b.WriteString(strings.Join(categoryInputs, "\n- "))
		b.WriteString("\n")
	}
	return b.String()
}

func parseImportMatchResponse(raw string, allowedPayees, allowedCategories map[string]string) (map[string]string, map[string]string) {
	objText, err := extractJSONObject(raw)
	if err != nil {
		return nil, nil
	}
	var resp struct {
		Payees     []llmMatch `json:"payees"`
		Categories []llmMatch `json:"categories"`
	}
	if err := json.Unmarshal([]byte(objText), &resp); err != nil {
		log.Printf("llm resolve: parse response: %v", err)
		return nil, nil
	}
	return pickValidMatches(resp.Payees, allowedPayees), pickValidMatches(resp.Categories, allowedCategories)
}

// pickValidMatches keeps only matches whose value is a real existing name,
// keyed by lowercased input so lookup is robust to casing the model echoes back.
func pickValidMatches(matches []llmMatch, allowed map[string]string) map[string]string {
	out := map[string]string{}
	for _, m := range matches {
		input := strings.TrimSpace(m.Input)
		match := strings.TrimSpace(m.Match)
		if input == "" || match == "" {
			continue
		}
		if canonical, ok := allowed[strings.ToLower(match)]; ok {
			out[strings.ToLower(input)] = canonical
		}
	}
	return out
}

func sortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// finalizeImportCandidate runs duplicate detection and validation for a single
// candidate and (re)derives its Selected/Error/Warning presentation fields. It
// is idempotent so the LLM follow-up pass can re-run it after changing a
// payee/category.
func (a *app) finalizeImportCandidate(accountID int, c *importCandidate) {
	c.Selected = true
	historyNotes := a.deriveCandidateFromHistory(accountID, c)
	c.DuplicateID = a.importCandidateDuplicateID(accountID, *c)
	c.Duplicate = c.DuplicateID > 0
	if c.Duplicate {
		a.applyExistingTransactionToCandidate(accountID, c, c.DuplicateID)
	}
	issues := a.validateImportCandidateIssues(accountID, c)
	if !c.Duplicate {
		for _, n := range historyNotes {
			issues = append(issues, candidateIssue{Text: n})
		}
	}
	c.Errors = nil
	c.Warnings = nil
	for _, is := range issues {
		if is.IsError {
			c.Errors = append(c.Errors, is.Text)
		} else {
			c.Warnings = append(c.Warnings, is.Text)
		}
	}
	hasError := len(c.Errors) > 0
	if !hasError && c.Duplicate {
		issues = append(issues, candidateIssue{Text: "Possible duplicate"})
		c.Warnings = append(c.Warnings, "Possible duplicate")
	}
	c.Error = strings.Join(c.Errors, "; ")
	c.Warning = ""
	if !hasError {
		c.Warning = strings.Join(c.Warnings, "; ")
	}
	if hasError || c.Duplicate {
		c.Selected = false
	}
	c.PayeeMessages = nil
	c.CategoryMessages = nil
	c.GeneralMessages = nil
	for _, is := range issues {
		m := candidateMessage{Text: a.importCandidateMessageText(is.Text), IsError: is.IsError}
		switch is.Field {
		case "payee":
			c.PayeeMessages = append(c.PayeeMessages, m)
		case "category":
			c.CategoryMessages = append(c.CategoryMessages, m)
		default:
			c.GeneralMessages = append(c.GeneralMessages, m)
		}
	}
}

func (a *app) importCandidateMessageText(source string) string {
	const guidance = "bank statement transaction import validation message"
	switch source {
	case "date must be YYYY-MM-DD",
		"type must be Withdrawal, Deposit, or Transfer",
		"amount must be greater than zero",
		"transaction status is not MMEX-compatible",
		"to account is required for transfers",
		"from account is required for transfers",
		"transfer accounts must be different",
		"Unknown payee; importing will create it",
		"Unknown category; importing will create it",
		"Possible duplicate",
		"Payee suggested by AI — confirm",
		"Category suggested by AI — confirm",
		"Category from history — confirm":
		return a.translateUI(source, guidance)
	}
	if strings.HasPrefix(source, "to account ") && strings.HasSuffix(source, " was not found") {
		id := strings.TrimSuffix(strings.TrimPrefix(source, "to account "), " was not found")
		return fmt.Sprintf(a.translateUI("to account %s was not found", guidance), id)
	}
	if strings.HasPrefix(source, "to account ") && strings.HasSuffix(source, " is not editable in MMXC") {
		id := strings.TrimSuffix(strings.TrimPrefix(source, "to account "), " is not editable in MMXC")
		return fmt.Sprintf(a.translateUI("to account %s is not editable in MMXC", guidance), id)
	}
	if strings.HasPrefix(source, "payee ") && strings.HasSuffix(source, " is inactive; choose an active payee") {
		name := strings.TrimSuffix(strings.TrimPrefix(source, "payee "), " is inactive; choose an active payee")
		return fmt.Sprintf(a.translateUI("payee %s is inactive; choose an active payee", guidance), name)
	}
	if strings.HasPrefix(source, "category ") && strings.HasSuffix(source, " is inactive; choose an active category") {
		name := strings.TrimSuffix(strings.TrimPrefix(source, "category "), " is inactive; choose an active category")
		return fmt.Sprintf(a.translateUI("category %s is inactive; choose an active category", guidance), name)
	}
	if strings.HasPrefix(source, "Split copied from ") && strings.HasSuffix(source, " — confirm") {
		date := strings.TrimSuffix(strings.TrimPrefix(source, "Split copied from "), " — confirm")
		return fmt.Sprintf(a.translateUI("Split copied from %s — confirm", guidance), date)
	}
	if strings.HasPrefix(source, "Payee \"") && strings.HasSuffix(source, "\" from recurring charge — confirm") {
		name := strings.TrimSuffix(strings.TrimPrefix(source, "Payee \""), "\" from recurring charge — confirm")
		return fmt.Sprintf(a.translateUI("Payee \"%s\" from recurring charge — confirm", guidance), name)
	}
	return a.translateUI(source, guidance)
}

// deriveCandidateFromHistory fills best-guesses from existing transactions: it
// reproduces the split of an identical recurring charge (24b), or takes the
// single category from the payee's history, which outranks the extraction
// model's pick (24a). It returns
// confirm-me notes for the caller to surface as warnings. No-op for transfers;
// it is idempotent so finalize can re-run it after the LLM follow-up pass.
func (a *app) deriveCandidateFromHistory(accountID int, c *importCandidate) []string {
	if c.Type == "Transfer" {
		return nil
	}
	payeeID, _ := a.resolvePayeeID(c.Payee)
	if payeeID <= 0 {
		payeeID, _ = a.resolveFuzzyPayeeID(c.Payee)
	}
	// 24b: reproduce a recurring identical charge's split structure. Keyed to the
	// payee when it resolves, else (e.g. a renamed merchant that fell back to a
	// "*Unknown" placeholder) to the same account, where a recurring fixed-amount
	// split still identifies the charge.
	if len(c.Splits) == 0 {
		if splits, src, splitPayee, ok := a.historicalSplit(accountID, payeeID, c.Type, c.Amount); ok {
			c.Splits = splits
			notes := []string{"Split copied from " + src + " — confirm"}
			// The matched history also names the real payee, so repair an
			// unresolved or "*"-placeholder payee from it.
			if name := a.repairPlaceholderPayee(c, payeeID, splitPayee); name != "" {
				notes = append(notes, "Payee \""+name+"\" from recurring charge — confirm")
			}
			return notes
		}
	}
	if len(c.Splits) > 0 {
		return nil
	}
	// 24a: the payee's own history outranks the extraction model's category. An
	// unresolved category is filled from it; a resolved one is replaced only when
	// the payee has never been booked to it and one category holds a clear
	// majority of its recent history — so the model still tells apart the
	// categories a mixed payee really uses. A learned rule's category and the
	// user's own edit are confirmations, not guesses: history never overrides them.
	if payeeID <= 0 {
		return nil
	}
	current, err := a.resolveCategoryID(c.Category)
	if err != nil {
		current, err = a.resolveFuzzyCategoryID(c.Category)
	}
	if err != nil {
		current = 0
	}
	if current > 0 && (c.Reviewed || c.LearnedCategory) {
		return nil
	}
	history := a.recentPayeeCategories(payeeID)
	if history.dominant <= 0 || history.dominant == current {
		return nil
	}
	if current > 0 && (history.counts[current] > 0 || !history.habitual()) {
		return nil
	}
	if err := a.validateTransactionCategory(history.dominant); err != nil {
		return nil
	}
	if name := a.categoryPath(history.dominant); name != "" {
		c.Category = name
		return []string{"Category from history — confirm"}
	}
	return nil
}

// repairPlaceholderPayee replaces a candidate's payee with the real payee named
// by a matched recurring split, but only when the candidate's own payee is
// unresolved or a "*"-prefixed placeholder — so a genuine payee is never
// overwritten. It sets c.Payee (validation re-resolves it to an ID) and returns
// the adopted name, or "" when nothing changed.
func (a *app) repairPlaceholderPayee(c *importCandidate, currentPayeeID, splitPayeeID int) string {
	if splitPayeeID <= 0 || splitPayeeID == currentPayeeID {
		return ""
	}
	name := a.payeeName(splitPayeeID)
	if name == "" || strings.HasPrefix(name, "*") {
		return ""
	}
	placeholder := currentPayeeID <= 0 || strings.HasPrefix(a.payeeName(currentPayeeID), "*")
	if !placeholder {
		return ""
	}
	c.Payee = name
	return name
}

// payeeHistoryWindow bounds the payee history consulted, so a payee the user has
// since re-categorized follows the new category rather than years of old
// bookings.
const payeeHistoryWindow = 20

// payeeCategoryHistory summarizes the single categories a payee was booked to in
// its most recent transactions: how often each was used, and the most-used one
// (dominant, ties going to the most recently used; 0 without history).
type payeeCategoryHistory struct {
	counts   map[int]int
	total    int
	dominant int
}

// habitual reports whether the dominant category is a real habit — used at
// least twice and on a strict majority of the recent transactions — rather than
// merely the most frequent of several.
func (h payeeCategoryHistory) habitual() bool {
	n := h.counts[h.dominant]
	return n >= 2 && n*2 > h.total
}

// recentPayeeCategories reads the payee's last payeeHistoryWindow non-void,
// categorized transactions; splits (CATEGID = -1) and uncategorized rows carry
// no single category and are excluded.
func (a *app) recentPayeeCategories(payeeID int) payeeCategoryHistory {
	history := payeeCategoryHistory{counts: map[int]int{}}
	rows, err := a.db.Query(`
SELECT CATEGID
  FROM CHECKINGACCOUNT_V1
 WHERE COALESCE(PAYEEID, 0) = ?
   AND COALESCE(CATEGID, 0) > 0
   AND COALESCE(STATUS, '') <> 'Void'
 ORDER BY TRANSDATE DESC, TRANSID DESC
 LIMIT ?`, payeeID, payeeHistoryWindow)
	if err != nil {
		return history
	}
	defer rows.Close()
	var newestFirst []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			return payeeCategoryHistory{counts: map[int]int{}}
		}
		if history.counts[id] == 0 {
			newestFirst = append(newestFirst, id)
		}
		history.counts[id]++
		history.total++
	}
	for _, id := range newestFirst {
		if history.counts[id] > history.counts[history.dominant] {
			history.dominant = id
		}
	}
	return history
}

// historicalSplit finds the split lines of an identical recurring charge to
// reproduce on a candidate. It prefers a payee-anchored match (account
// independent, a single occurrence is enough since the payee identifies it);
// failing that — when the payee did not resolve or has no such split — it falls
// back to the same account, where it requires the split to recur (≥2 occurrences)
// so a one-off coincidental amount is not replicated. The source date and the
// matched rows' common payee (0 if they disagree) are returned: the payee lets a
// fallback match also repair an unresolved/placeholder payee.
func (a *app) historicalSplit(accountID, payeeID int, transType string, amount float64) ([]splitLine, string, int, bool) {
	if amount <= 0 {
		return nil, "", 0, false
	}
	if payeeID > 0 {
		if splits, date, p, ok := a.consistentSplit(1,
			"COALESCE(PAYEEID, 0) = ? AND TRANSCODE = ? AND ABS(TRANSAMOUNT - ?) < 0.005",
			payeeID, transType, amount); ok {
			return splits, date, p, true
		}
	}
	if accountID > 0 {
		if splits, date, p, ok := a.consistentSplit(2,
			"ACCOUNTID = ? AND TRANSCODE = ? AND ABS(TRANSAMOUNT - ?) < 0.005",
			accountID, transType, amount); ok {
			return splits, date, p, true
		}
	}
	return nil, "", 0, false
}

// consistentSplit returns fresh copies of the most recent non-void split
// matching condition, but only when at least minCount such splits exist and they
// all share the same category/amount breakdown — so an ambiguous or varying
// split is never guessed. It also returns the source date and the payee shared
// by all matched rows (0 when they disagree, so the caller never adopts an
// ambiguous payee).
func (a *app) consistentSplit(minCount int, condition string, args ...any) ([]splitLine, string, int, bool) {
	rows, err := a.db.Query(`
SELECT TRANSID, substr(COALESCE(TRANSDATE, ''), 1, 10), COALESCE(PAYEEID, 0)
  FROM CHECKINGACCOUNT_V1
 WHERE `+condition+`
   AND COALESCE(CATEGID, 0) = -1
   AND COALESCE(STATUS, '') <> 'Void'
 ORDER BY TRANSDATE DESC
 LIMIT 24`, args...)
	if err != nil {
		return nil, "", 0, false
	}
	type ref struct {
		id    int64
		date  string
		payee int
	}
	var refs []ref
	for rows.Next() {
		var r ref
		if err := rows.Scan(&r.id, &r.date, &r.payee); err != nil {
			rows.Close()
			return nil, "", 0, false
		}
		refs = append(refs, r)
	}
	rows.Close()
	if len(refs) < minCount {
		return nil, "", 0, false
	}
	var best []splitLine
	var bestDate, sig string
	commonPayee := refs[0].payee
	for i, r := range refs {
		if r.payee != commonPayee {
			commonPayee = 0
		}
		lines, err := a.listSplitLines(int(r.id))
		if err != nil || len(lines) < 2 {
			return nil, "", 0, false
		}
		if i == 0 {
			best, bestDate, sig = lines, r.date, splitSignature(lines)
		} else if splitSignature(lines) != sig {
			return nil, "", 0, false
		}
	}
	out := make([]splitLine, len(best))
	for i, l := range best {
		// Drop the source SPLITTRANSID so these insert as new split lines.
		out[i] = splitLine{CategoryID: l.CategoryID, Category: l.Category, Amount: l.Amount, Notes: l.Notes}
	}
	return out, bestDate, commonPayee, true
}

// splitSignature is an order-independent fingerprint of a split's category and
// amount breakdown, used to tell whether recurring splits are consistent.
func splitSignature(lines []splitLine) string {
	parts := make([]string, len(lines))
	for i, l := range lines {
		parts[i] = fmt.Sprintf("%d:%.2f", l.CategoryID, l.Amount)
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

// validateCandidateSplits checks a split candidate's lines (each a valid
// transaction category, at least two lines, summing to the candidate amount),
// resolving any category given by name. It sets the candidate's CATEGID to the
// split sentinel and a "Split: …" display summary. Returns the validation
// errors, if any.
func (a *app) validateCandidateSplits(c *importCandidate) []string {
	var errs []string
	if len(c.Splits) < 2 {
		errs = append(errs, "split transactions require at least two split lines")
	}
	var total float64
	summary := make([]string, 0, len(c.Splits))
	for i := range c.Splits {
		s := &c.Splits[i]
		if s.Amount <= 0 {
			errs = append(errs, fmt.Sprintf("split line %d amount must be greater than zero", i+1))
		}
		if s.CategoryID <= 0 && strings.TrimSpace(s.Category) != "" {
			if id, err := a.resolveCategoryID(s.Category); err == nil {
				s.CategoryID = id
			}
		}
		switch {
		case s.CategoryID <= 0:
			errs = append(errs, fmt.Sprintf("split line %d category is required", i+1))
		default:
			if err := a.validateTransactionCategory(s.CategoryID); err != nil {
				errs = append(errs, fmt.Sprintf("split line %d: %v", i+1, err))
			} else if name := a.categoryPath(s.CategoryID); name != "" {
				s.Category = name
			}
		}
		total += s.Amount
		summary = append(summary, s.Category)
	}
	if c.Amount > 0 && math.Abs(total-c.Amount) >= 0.005 {
		errs = append(errs, fmt.Sprintf("split lines sum to %.2f but amount is %.2f", total, c.Amount))
	}
	c.CategoryID = -1
	c.Category = "Split: " + strings.Join(summary, ", ")
	return errs
}

func safeUploadName(filename string) string {
	name := filepath.Base(strings.TrimSpace(filename))
	if name == "." || name == string(filepath.Separator) || name == "" {
		return "statement"
	}
	name = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '-' || r == '_' {
			return r
		}
		return '-'
	}, name)
	name = strings.Trim(name, ".-")
	if name == "" {
		return "statement"
	}
	return name
}

// imageMIME maps an image file extension to the MIME type the data: URI needs.
func imageMIME(ext string) string {
	switch ext {
	case ".png":
		return "image/png"
	case ".webp":
		return "image/webp"
	default:
		return "image/jpeg"
	}
}

// extractCandidatesFromStatement turns an uploaded statement into transaction
// candidates via the LLM. The reconstructed text is sent for PDFs and plain
// text; images go to the same model, which must then be multimodal.
func (a *app) extractCandidatesFromStatement(filename string, data []byte) (statementExtraction, error) {
	ext := strings.ToLower(filepath.Ext(filename))
	var prompt string
	var images []llmImage
	categories := a.categoryNamesForPrompt()
	today := time.Now().Format("2006-01-02")
	switch {
	case ext == ".png" || ext == ".jpg" || ext == ".jpeg" || ext == ".webp":
		images = []llmImage{{MIME: imageMIME(ext), Base64: base64.StdEncoding.EncodeToString(data)}}
		prompt = statementImagePrompt(a.prompts(), today, categories)
	case ext == ".pdf":
		text, err := extractPDFTextResult(data)
		if err != nil {
			return statementExtraction{}, err
		}
		if len(strings.Fields(text)) < 20 {
			return statementExtraction{}, errors.New("could not extract enough text from PDF; upload an image-capable statement screenshot instead")
		}
		prompt = statementTextPrompt(a.prompts(), today, text, categories)
	default:
		text := strings.TrimSpace(string(data))
		if len(strings.Fields(text)) < 10 {
			return statementExtraction{}, errors.New("statement text is too short for extraction")
		}
		prompt = statementTextPrompt(a.prompts(), today, text, categories)
	}
	raw, err := a.llm.generate(a.config.LLMModel, prompt, images, llmOptions{JSONMode: true})
	if err != nil {
		return statementExtraction{}, err
	}
	sections, jsonText, err := parseStatementSectionsResponse(raw)
	if err != nil {
		// Surface the raw model output so a parse failure (e.g. the model
		// emitted only reasoning, or its JSON was cut off) is diagnosable from
		// the server log without needing MMXC_LOG_LLM.
		log.Printf("statement extraction parse failed for %s: %v; raw response (%dB):\n%s",
			filename, err, len(raw), logExcerpt(raw, 4000))
		return statementExtraction{}, err
	}
	settleStatementYears(sections, today)
	return statementExtraction{Sections: sections, RawJSON: jsonText}, nil
}

type extractedAccountResponse struct {
	Account struct {
		Name   string `json:"name"`
		Number string `json:"number"`
	} `json:"account"`
	Period struct {
		Start string `json:"start"`
		End   string `json:"end"`
	} `json:"period"`
	ClosingDate    string            `json:"closing_date"`
	ClosingBalance *float64          `json:"closing_balance"`
	Transactions   []importCandidate `json:"transactions"`
}

func sectionFromExtractedResponse(resp extractedAccountResponse) statementSection {
	header := statementHeader{
		AccountName:   strings.TrimSpace(resp.Account.Name),
		AccountNumber: strings.TrimSpace(resp.Account.Number),
		PeriodStart:   strings.TrimSpace(resp.Period.Start),
		PeriodEnd:     strings.TrimSpace(resp.Period.End),
		ClosingDate:   strings.TrimSpace(resp.ClosingDate),
	}
	if resp.ClosingBalance != nil {
		header.ClosingBalance = *resp.ClosingBalance
		header.HasClosing = true
	}
	jsonText := "[]"
	if encoded, err := json.MarshalIndent(resp.Transactions, "", "  "); err == nil {
		jsonText = string(encoded)
	}
	return statementSection{Header: header, Candidates: resp.Transactions, RawJSON: jsonText}
}

func extractedAccountHasContent(resp extractedAccountResponse) bool {
	return strings.TrimSpace(resp.Account.Name) != "" ||
		strings.TrimSpace(resp.Account.Number) != "" ||
		strings.TrimSpace(resp.Period.Start) != "" ||
		strings.TrimSpace(resp.Period.End) != "" ||
		strings.TrimSpace(resp.ClosingDate) != "" ||
		resp.ClosingBalance != nil || len(resp.Transactions) > 0
}

// parseStatementSectionsResponse parses the v2 multi-account response. The
// legacy single-account object and bare transaction array remain accepted and
// are normalized into a one-section document.
func parseStatementSectionsResponse(raw string) ([]statementSection, string, error) {
	if objText, err := extractJSONObject(raw); err == nil {
		objText = normalizeStatementNumbers(objText)
		var multi struct {
			Accounts json.RawMessage `json:"accounts"`
		}
		if err := json.Unmarshal([]byte(objText), &multi); err == nil && len(multi.Accounts) > 0 {
			var accounts []extractedAccountResponse
			if err := json.Unmarshal(multi.Accounts, &accounts); err != nil {
				return nil, raw, fmt.Errorf("parse LLM accounts JSON: %w", err)
			}
			if len(accounts) == 0 {
				return nil, raw, errors.New("statement extraction returned no account sections")
			}
			sections := make([]statementSection, 0, len(accounts))
			for _, account := range accounts {
				if extractedAccountHasContent(account) {
					sections = append(sections, sectionFromExtractedResponse(account))
				}
			}
			if len(sections) > 0 {
				return sections, objText, nil
			}
		}

		var single extractedAccountResponse
		if err := json.Unmarshal([]byte(objText), &single); err == nil && extractedAccountHasContent(single) {
			return []statementSection{sectionFromExtractedResponse(single)}, objText, nil
		}
	}
	jsonText, err := extractJSONArray(raw)
	if err != nil {
		return nil, raw, err
	}
	jsonText = normalizeStatementNumbers(jsonText)
	var candidates []importCandidate
	if err := json.Unmarshal([]byte(jsonText), &candidates); err != nil {
		return nil, raw, fmt.Errorf("parse LLM JSON: %w", err)
	}
	return []statementSection{{Candidates: candidates, RawJSON: jsonText}}, jsonText, nil
}

// settleStatementYears moves a section that ends after today back by whole
// years. The prompt has the model use the current year for a statement that
// prints none, which dates one from last autumn into the future, and no
// uploaded statement can end after the day it is read. A section ends at its
// period end, else its closing date, else its latest transaction. today is
// YYYY-MM-DD, so the dates compare as strings.
func settleStatementYears(sections []statementSection, today string) {
	for i := range sections {
		section := &sections[i]
		end := section.Header.PeriodEnd
		if end == "" {
			end = section.Header.ClosingDate
		}
		if end == "" {
			for _, c := range section.Candidates {
				end = max(end, c.Date)
			}
		}
		endDate, err := time.Parse("2006-01-02", end)
		if err != nil || end <= today {
			continue
		}
		years := 0
		for shiftYears(endDate, years).Format("2006-01-02") > today {
			years++
		}
		shift := func(date string) string {
			t, err := time.Parse("2006-01-02", date)
			if err != nil {
				return date
			}
			return shiftYears(t, years).Format("2006-01-02")
		}
		header := &section.Header
		header.PeriodStart, header.PeriodEnd, header.ClosingDate = shift(header.PeriodStart), shift(header.PeriodEnd), shift(header.ClosingDate)
		for j := range section.Candidates {
			section.Candidates[j].Date = shift(section.Candidates[j].Date)
		}
		// RawJSON re-validates the rows later, so it must carry the new dates.
		if encoded, err := json.MarshalIndent(section.Candidates, "", "  "); err == nil {
			section.RawJSON = string(encoded)
		}
	}
}

// shiftYears moves t back by years, keeping 29 February on the last day of
// February rather than spilling into March.
func shiftYears(t time.Time, years int) time.Time {
	shifted := t.AddDate(-years, 0, 0)
	if shifted.Month() != t.Month() {
		shifted = shifted.AddDate(0, 0, -shifted.Day())
	}
	return shifted
}

// statementNumberKeys are the response fields the parser reads as numbers.
var statementNumberKeys = map[string]bool{"amount": true, "closing_balance": true}

// normalizeStatementNumbers rewrites the amounts a model returned as strings
// ("1,234.56", "$450.00-", "1.234,56") as JSON numbers, and drops blank ones,
// so one quoted value does not fail the whole extraction. A string it cannot
// read is left alone and fails in the parser as before.
func normalizeStatementNumbers(jsonText string) string {
	var doc any
	decoder := json.NewDecoder(strings.NewReader(jsonText))
	decoder.UseNumber()
	if err := decoder.Decode(&doc); err != nil || !normalizeNumberFields(doc) {
		return jsonText
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return jsonText
	}
	return string(out)
}

func normalizeNumberFields(v any) bool {
	changed := false
	switch node := v.(type) {
	case map[string]any:
		for key, child := range node {
			if text, ok := child.(string); ok && statementNumberKeys[key] {
				if strings.TrimSpace(text) == "" {
					delete(node, key)
					changed = true
				} else if n, ok := parseStatementAmount(text); ok {
					node[key] = n
					changed = true
				}
				continue
			}
			if normalizeNumberFields(child) {
				changed = true
			}
		}
	case []any:
		for _, child := range node {
			if normalizeNumberFields(child) {
				changed = true
			}
		}
	}
	return changed
}

// parseStatementAmount reads an amount as statements print it: currency signs
// and codes, digit grouping by comma, dot, space or apostrophe, a decimal comma,
// and a leading, trailing or parenthesized minus.
func parseStatementAmount(text string) (float64, bool) {
	text = strings.TrimSpace(text)
	negative := strings.Contains(text, "-") || strings.HasPrefix(text, "(") && strings.HasSuffix(text, ")")
	digits := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' || r == ',' || r == '.' {
			return r
		}
		return -1
	}, text)
	lastComma, lastDot := strings.LastIndex(digits, ","), strings.LastIndex(digits, ".")
	switch {
	case lastComma >= 0 && lastDot >= 0:
		// Whichever separator comes last is the decimal one.
		if lastComma > lastDot {
			digits = strings.ReplaceAll(digits, ".", "")
			digits = strings.Replace(digits, ",", ".", 1)
		} else {
			digits = strings.ReplaceAll(digits, ",", "")
		}
	case lastComma >= 0:
		if strings.Count(digits, ",") == 1 && len(digits)-lastComma-1 <= 2 {
			digits = strings.Replace(digits, ",", ".", 1)
		} else {
			digits = strings.ReplaceAll(digits, ",", "")
		}
	case strings.Count(digits, ".") > 1:
		digits = strings.ReplaceAll(digits, ".", "")
	}
	value, err := strconv.ParseFloat(digits, 64)
	if err != nil {
		return 0, false
	}
	if negative {
		value = -value
	}
	return value, true
}

// parseStatementResponse reads the model output, preferring the structured
// object {account, period, closing_*, transactions}. It falls back to a bare
// transaction array for models that ignore the object shape. The returned string
// is the transactions JSON, suitable for the hidden re-validation field.
func parseStatementResponse(raw string) (statementHeader, []importCandidate, string, error) {
	sections, _, err := parseStatementSectionsResponse(raw)
	if err != nil {
		return statementHeader{}, nil, raw, err
	}
	if len(sections) == 0 {
		return statementHeader{}, nil, raw, errors.New("statement extraction returned no account sections")
	}
	return sections[0].Header, sections[0].Candidates, sections[0].RawJSON, nil
}

// The extraction contract is the response shape (statementJSONShape) and the
// field meanings (statementFieldRules) that parseStatementSectionsResponse
// depends on. How to tell real transactions from summary rows, which date to
// pick and the like is guidance, which comes from the overridable statement
// prompt file (see prompts.go). The guidance sits between shape and field
// rules, where the bank rules were before the split, and the category rule
// stays right above the category list it refers to. Payee and category scores
// in the extraction eval move by whole rows when a single word of the prompt
// changes, so compare totals over all statements, not one statement's flip.
const statementJSONShape = `Return only a JSON object (no markdown) shaped like:
{
  "accounts": [
    {
      "account": {"name": "<account name as printed>", "number": "<account or card number, may be masked>"},
      "period": {"start": "YYYY-MM-DD", "end": "YYYY-MM-DD"},
      "closing_date": "YYYY-MM-DD",
      "closing_balance": <number>,
      "transactions": [ {"date": "YYYY-MM-DD", "type": "Withdrawal|Deposit", "amount": <positive number>, "description": "...", "payee": "...", "category": "..."} ]
    }
  ]
}`

const statementFieldRules = `account holds that section's own account name and number; leave a field empty if it is not printed. period is that account section's covered date range; closing_date and closing_balance are its closing/ending date and balance (use the ending/new balance, signed as printed). Omit closing_balance if it is not printed.
For each transaction: date must be YYYY-MM-DD. When a date is printed without its year, take the year from the statement period or header; if no year is printed anywhere, use the current year (current date: %s). type must be "Withdrawal" for debits/purchases/fees/payments out, or "Deposit" for credits/refunds/payments in; amount must be a positive number.
description must be the original transaction description exactly as it appears on the statement.
payee should be the cleaned merchant or counterparty.
category should be one of the user's existing MMEX categories listed below when one reasonably fits; otherwise leave category empty.`

func statementTextPrompt(prompts Prompts, currentDate, text string, categories []string) string {
	return "Extract the account details and real transactions from the statement text below.\n" +
		statementInstructions(prompts, currentDate) + categoryGuidance(categories) +
		"\n\nStatement text:\n" + truncateForPrompt(text, 24000)
}

func statementImagePrompt(prompts Prompts, currentDate string, categories []string) string {
	return "Extract the account details and real transactions from this statement image.\n" +
		statementInstructions(prompts, currentDate) + categoryGuidance(categories)
}

// statementInstructions assembles the contract around the guidance. The current
// date only gives a statement that prints no year one; settleStatementYears
// moves it back if that put it in the future.
func statementInstructions(prompts Prompts, currentDate string) string {
	return statementJSONShape + "\n" + prompts.guidance(promptStatement) + "\n" + fmt.Sprintf(statementFieldRules, currentDate)
}

// categoryGuidance appends the closed list of selectable category names to the
// prompt so the model picks an existing MMEX category instead of inventing a
// label that only fuzzy-matching can (poorly) reconcile afterward. Names use the
// "Parent: Child" form that resolveCategoryID accepts.
func categoryGuidance(categories []string) string {
	if len(categories) == 0 {
		return ""
	}
	return "\n\nExisting categories (copy one exactly, keeping the \"Parent: Child\" form):\n- " +
		strings.Join(categories, "\n- ")
}

// categoryNamesForPrompt returns the active, selectable category names for the
// LLM: subcategories as "Parent: Child" (the MMEX convention that transactions
// attach to subcategories), plus bare top-level names only when those are
// allowed for new transactions. Returns nil on error so extraction still runs
// with the free-form fallback.
func (a *app) categoryNamesForPrompt() []string {
	cats, err := a.listCategories()
	if err != nil {
		return nil
	}
	var names []string
	for _, c := range cats {
		if !c.Active {
			continue
		}
		switch {
		case c.ParentID > 0:
			names = append(names, c.Parent+": "+c.Name)
		case a.config.AllowTopLevelCategories:
			names = append(names, c.Name)
		}
	}
	return names
}

// truncateForPrompt caps the statement text length while preserving its line and
// column layout, which helps the model read tabular statements (trailing
// whitespace per line is trimmed only to save tokens).
func truncateForPrompt(text string, limit int) string {
	var b strings.Builder
	for _, line := range strings.Split(text, "\n") {
		b.WriteString(strings.TrimRight(line, " \t"))
		b.WriteByte('\n')
	}
	text = strings.TrimRight(b.String(), "\n")
	if len(text) <= limit {
		return text
	}
	return text[:limit]
}

// logExcerpt caps a string for logging, keeping enough to diagnose model output
// without writing multi-megabyte log lines.
func logExcerpt(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit] + fmt.Sprintf("... (%d more bytes)", len(s)-limit)
}

func extractJSONArray(text string) (string, error) {
	text = strings.TrimSpace(text)
	start := strings.Index(text, "[")
	end := strings.LastIndex(text, "]")
	if start < 0 || end < start {
		return "", errors.New("LLM response did not contain a JSON array")
	}
	return text[start : end+1], nil
}

func extractJSONObject(text string) (string, error) {
	text = strings.TrimSpace(text)
	start := strings.Index(text, "{")
	end := strings.LastIndex(text, "}")
	if start < 0 || end < start {
		return "", errors.New("LLM response did not contain a JSON object")
	}
	return text[start : end+1], nil
}

func (a *app) parseApprovedImportInputs(r *http.Request) ([]transactionInput, error) {
	accountID, err := strconv.Atoi(r.FormValue("account_id"))
	if err != nil || accountID <= 0 {
		return nil, errors.New("account is required")
	}
	approved := approvedImportIndexes(r)
	dates := r.Form["date"]
	types := r.Form["type"]
	amounts := r.Form["amount"]
	payees := r.Form["payee"]
	originalPayees := r.Form["original_payee"]
	categories := r.Form["category"]
	notes := r.Form["notes"]
	statuses := r.Form["status"]
	toAccounts := r.Form["to_account_id"]
	duplicateIDs := r.Form["duplicate_id"]
	archiveFiles := r.Form["candidate_archive_file"]
	archiveKeys := r.Form["candidate_archive_key"]
	maxLen := max(len(dates), len(types), len(amounts), len(payees), len(categories), len(notes), len(statuses), len(toAccounts))
	rowIndexes := candidateRowIndexes(r, maxLen)
	var inputs []transactionInput
	// pos walks the submitted field arrays; rowIndex is the identity the page gave
	// the row, which is what "approved" and the split suffixes are keyed by. The
	// numbers in messages are the visible row number, so pos is the right one.
	for pos := 0; pos < maxLen; pos++ {
		rowIndex := rowIndexes[pos]
		if !approved[rowIndex] {
			continue
		}
		amount, err := parseAmountExpression(formValueAt(amounts, pos))
		if err != nil || amount <= 0 {
			return nil, fmt.Errorf("candidate %d amount must be greater than zero", pos+1)
		}
		candidate := importCandidate{
			Index:       pos + 1,
			Date:        formValueAt(dates, pos),
			Type:        formValueAt(types, pos),
			Amount:      amount,
			Payee:       formValueAt(payees, pos),
			ToAccountID: parsePositiveInt(formValueAt(toAccounts, pos)),
			Notes:       formValueAt(notes, pos),
			Status:      formValueAt(statuses, pos),
		}
		originalPayee := strings.TrimSpace(formValueAt(originalPayees, pos))
		if originalPayee == "" {
			originalPayee = candidate.Payee
		}
		candidate.Category = formValueAt(categories, pos)
		// Every submitted row is the user's own text, so it may name a new category.
		candidate.Reviewed = true
		candidate.normalize()
		// Per-row split lines (suffixed with the row identity) make this candidate
		// a split; validation then checks them instead of the single category.
		if splits, ok, err := a.parseCandidateSplitForm(r, rowIndex, pos); err != nil {
			return nil, fmt.Errorf("candidate %d: %w", pos+1, err)
		} else if ok {
			candidate.Splits = splits
		}
		if err := a.validateImportCandidate(accountID, &candidate); err != nil {
			return nil, fmt.Errorf("candidate %d: %w", pos+1, err)
		}
		// A row carrying a duplicate_id is an intentional upsert: update that
		// existing transaction. Otherwise reject if it now matches one, to avoid
		// inserting an accidental duplicate.
		updateID := int64(parsePositiveInt(formValueAt(duplicateIDs, pos)))
		if updateID <= 0 && a.importCandidateDuplicate(accountID, candidate) {
			return nil, fmt.Errorf("candidate %d appears to duplicate an existing transaction", pos+1)
		}
		ledgerAccountID := accountID
		ledgerToAccountID := candidate.ToAccountID
		if candidate.Type == "Transfer" && candidate.TransferDirection == "in" {
			ledgerAccountID = candidate.ToAccountID
			ledgerToAccountID = accountID
		}
		inputs = append(inputs, transactionInput{
			AccountID:             ledgerAccountID,
			Date:                  candidate.Date,
			Type:                  candidate.Type,
			Amount:                candidate.Amount,
			PayeeID:               candidate.PayeeID,
			PayeeName:             candidate.Payee,
			OriginalPayee:         originalPayee,
			ToAccountID:           ledgerToAccountID,
			CategoryID:            candidate.CategoryID,
			CategoryName:          candidate.Category,
			NewCategoryParentID:   candidate.NewCategoryParentID,
			NewCategoryName:       candidate.NewCategoryName,
			Status:                candidate.Status,
			Notes:                 candidate.Notes,
			Splits:                candidate.Splits,
			TransferDirection:     candidate.TransferDirection,
			CounterpartyAccountID: candidate.ToAccountID,
			ReviewAccountID:       accountID,
			UpdateID:              updateID,
			ArchiveFile:           strings.TrimSpace(formValueAt(archiveFiles, pos)),
			ArchiveCandidateKey:   strings.TrimSpace(formValueAt(archiveKeys, pos)),
		})
	}
	return inputs, nil
}

// parseCandidateSplitForm reads the split lines for an import candidate row,
// returning ok=false when the row carries no split. It validates the lines and
// that they sum to the row's amount, so a hand-edited split can't be committed
// inconsistently.
//
// The two arguments are the row's two numbers and are not interchangeable (see
// candidateRowIndexes): the split fields are named after the row's identity
// (split_amount_<rowIndex>, …), while the amount to reconcile against is read
// from the field arrays by position.
func (a *app) parseCandidateSplitForm(r *http.Request, rowIndex, pos int) ([]splitLine, bool, error) {
	suffix := "_" + strconv.Itoa(rowIndex)
	amounts := r.Form["split_amount"+suffix]
	if len(amounts) == 0 {
		return nil, false, nil
	}
	splits, total, err := a.parseSplitLinesFrom(
		r.Form["split_category_id"+suffix],
		r.Form["split_category_text"+suffix],
		amounts,
		r.Form["split_notes"+suffix],
		true,
	)
	if err != nil {
		return nil, false, err
	}
	rowAmount, _ := parseAmountExpression(formValueAt(r.Form["amount"], pos))
	if math.Abs(total-rowAmount) >= 0.005 {
		return nil, false, fmt.Errorf("split lines sum to %.2f but amount is %.2f", total, rowAmount)
	}
	return splits, true, nil
}

// A submitted candidate row has two different numbers, and mixing them up
// imports or dismisses the wrong transaction:
//
//   - its *position* in the repeated field arrays (r.Form["date"][n] and friends)
//   - its *identity*, the index the page rendered it with, which is what the
//     approved/dismissed values and the split-input suffixes refer to
//
// The two coincide on a freshly rendered page. They stop coinciding the moment
// the page drops a row from the DOM — an in-place dismissal does exactly that —
// because the surviving rows shift down a position while keeping the values they
// were rendered with. Every row therefore submits a row_index carrying its
// identity, and the two lookups below keep the mapping explicit.

// candidateRowIndexes returns the identity of each submitted row position.
func candidateRowIndexes(r *http.Request, positions int) []int {
	declared := r.Form["row_index"]
	out := make([]int, positions)
	for pos := range out {
		// Without a declared identity (a hand-built request, or a page rendered
		// before row_index existed) position and identity are the same thing.
		out[pos] = pos
		if pos < len(declared) {
			if n, err := strconv.Atoi(declared[pos]); err == nil && n >= 0 {
				out[pos] = n
			}
		}
	}
	return out
}

// candidateRowPosition finds where the row with the given identity currently
// sits in the submitted field arrays, or -1 when it was not submitted at all.
func candidateRowPosition(r *http.Request, rowIndex int) int {
	declared := r.Form["row_index"]
	if len(declared) == 0 {
		return rowIndex
	}
	for pos, raw := range declared {
		if n, err := strconv.Atoi(raw); err == nil && n == rowIndex {
			return pos
		}
	}
	return -1
}

func approvedImportIndexes(r *http.Request) map[int]bool {
	approved := map[int]bool{}
	for _, raw := range r.Form["approved"] {
		if n, err := strconv.Atoi(raw); err == nil && n >= 0 {
			approved[n] = true
		}
	}
	return approved
}

func (a *app) remainingImportCandidates(r *http.Request) []importCandidate {
	approved := approvedImportIndexes(r)
	dates := r.Form["date"]
	types := r.Form["type"]
	amounts := r.Form["amount"]
	payees := r.Form["payee"]
	originalPayees := r.Form["original_payee"]
	categories := r.Form["category"]
	notes := r.Form["notes"]
	statuses := r.Form["status"]
	toAccounts := r.Form["to_account_id"]
	archiveFiles := r.Form["candidate_archive_file"]
	archiveIndexes := r.Form["candidate_archive_index"]
	archiveKeys := r.Form["candidate_archive_key"]
	maxLen := max(len(dates), len(types), len(amounts), len(payees), len(categories), len(notes), len(statuses), len(toAccounts))
	rowIndexes := candidateRowIndexes(r, maxLen)
	candidates := make([]importCandidate, 0, maxLen)
	// As in parseApprovedImportInputs: pos indexes the field arrays, rowIndex is
	// what "approved" and the split suffixes are keyed by.
	for pos := 0; pos < maxLen; pos++ {
		rowIndex := rowIndexes[pos]
		if approved[rowIndex] {
			continue
		}
		amount, _ := parseAmountExpression(formValueAt(amounts, pos))
		candidate := importCandidate{
			Index:               pos + 1,
			Selected:            false,
			Date:                formValueAt(dates, pos),
			Type:                formValueAt(types, pos),
			Amount:              amount,
			Payee:               formValueAt(payees, pos),
			OriginalPayee:       formValueAt(originalPayees, pos),
			Category:            formValueAt(categories, pos),
			Notes:               formValueAt(notes, pos),
			Status:              formValueAt(statuses, pos),
			ToAccountID:         parsePositiveInt(formValueAt(toAccounts, pos)),
			ArchiveFile:         formValueAt(archiveFiles, pos),
			ArchiveCandidateKey: formValueAt(archiveKeys, pos),
			Reviewed:            true,
		}
		candidate.ArchiveCandidateIndex, _ = strconv.Atoi(formValueAt(archiveIndexes, pos))
		candidate.normalize()
		// Preserve a hand-edited split on a not-yet-imported row; prepare's
		// deriveCandidateFromHistory leaves an already-set split untouched.
		if splits, ok, err := a.parseCandidateSplitForm(r, rowIndex, pos); err == nil && ok {
			candidate.Splits = splits
		}
		candidates = append(candidates, candidate)
	}
	return candidates
}

func (c *importCandidate) normalize() {
	c.Date = strings.TrimSpace(c.Date)
	c.Type = strings.TrimSpace(c.Type)
	switch c.Type {
	case "TransferIn":
		c.Type = "Transfer"
		c.TransferDirection = "in"
	case "TransferOut":
		c.Type = "Transfer"
		c.TransferDirection = "out"
	}
	if c.Type == "" {
		c.Type = "Withdrawal"
	}
	if c.Type == "Transfer" && c.TransferDirection != "in" {
		// Only an incoming transfer says so explicitly; everything else (an
		// archived candidate, a form post that omitted the field) means outgoing.
		c.TransferDirection = "out"
	}
	c.Payee = strings.TrimSpace(c.Payee)
	c.Description = strings.TrimSpace(c.Description)
	if c.OriginalPayee == "" {
		c.OriginalPayee = c.Description
	}
	if c.OriginalPayee == "" {
		c.OriginalPayee = c.Payee
	}
	c.Category = strings.TrimSpace(c.Category)
	c.Notes = strings.TrimSpace(c.Notes)
	c.Status = strings.TrimSpace(c.Status)
	if c.Status == "__blank" {
		c.Status = ""
	}
}

func (a *app) validateImportCandidate(accountID int, c *importCandidate) error {
	var errs []string
	for _, is := range a.validateImportCandidateIssues(accountID, c) {
		if is.IsError {
			errs = append(errs, is.Text)
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// validateImportCandidateIssues returns the validation messages for a candidate,
// each tagged with the field it concerns (payee, category, or general) so the
// caller can route it to the right column. It also normalizes resolved fields
// (PayeeID, CategoryID, Category, ToAccount) and sets Duplicate as a side effect.
func (a *app) validateImportCandidateIssues(accountID int, c *importCandidate) []candidateIssue {
	var issues []candidateIssue
	hasError := func() bool {
		for _, is := range issues {
			if is.IsError {
				return true
			}
		}
		return false
	}
	if _, err := time.Parse("2006-01-02", c.Date); err != nil {
		issues = append(issues, candidateIssue{Text: "date must be YYYY-MM-DD", IsError: true})
	}
	if c.Type != "Withdrawal" && c.Type != "Deposit" && c.Type != "Transfer" {
		issues = append(issues, candidateIssue{Text: "type must be Withdrawal, Deposit, or Transfer", IsError: true})
	}
	if c.Amount <= 0 {
		issues = append(issues, candidateIssue{Text: "amount must be greater than zero", IsError: true})
	}
	if !validTransactionStatus(c.Status) {
		issues = append(issues, candidateIssue{Text: "transaction status is not MMEX-compatible", IsError: true})
	}
	if c.Type == "Transfer" {
		if c.ToAccountID <= 0 {
			message := "to account is required for transfers"
			if c.TransferDirection == "in" {
				message = "from account is required for transfers"
			}
			issues = append(issues, candidateIssue{Field: "payee", Text: message, IsError: true})
		} else {
			if c.ToAccountID == accountID {
				issues = append(issues, candidateIssue{Field: "payee", Text: "transfer accounts must be different", IsError: true})
			}
			if !a.accountExists(c.ToAccountID) {
				issues = append(issues, candidateIssue{Field: "payee", Text: fmt.Sprintf("to account %d was not found", c.ToAccountID), IsError: true})
			} else if !a.accountEditable(c.ToAccountID) {
				issues = append(issues, candidateIssue{Field: "payee", Text: fmt.Sprintf("to account %d is not editable in MMXC", c.ToAccountID), IsError: true})
			}
		}
		if categoryID, err := a.transferCategoryID(); err != nil {
			issues = append(issues, candidateIssue{Field: "category", Text: err.Error(), IsError: true})
		} else {
			c.CategoryID = categoryID
			c.Category = "Transfer"
		}
		c.PayeeID = -1
		c.Payee = ""
		c.ToAccount = a.accountName(c.ToAccountID)
		if !hasError() && a.importCandidateDuplicate(accountID, *c) {
			c.Duplicate = true
		}
		return issues
	}
	payeeID, _ := a.resolvePayeeID(c.Payee)
	if payeeID <= 0 {
		payeeID, _ = a.resolveFuzzyPayeeID(c.Payee)
	}
	if payeeID > 0 {
		if !a.payeeActive(payeeID) {
			issues = append(issues, candidateIssue{Field: "payee", Text: fmt.Sprintf("payee %q is inactive; choose an active payee", c.Payee), IsError: true})
		}
		if name := a.payeeName(payeeID); name != "" {
			c.Payee = name
		}
	} else {
		issues = append(issues, candidateIssue{Field: "payee", Text: "Unknown payee; importing will create it"})
	}
	c.PayeeID = payeeID
	if len(c.Splits) > 0 {
		for _, e := range a.validateCandidateSplits(c) {
			issues = append(issues, candidateIssue{Field: "category", Text: e, IsError: true})
		}
		if !hasError() && a.importCandidateDuplicate(accountID, *c) {
			c.Duplicate = true
		}
		return issues
	}
	c.NewCategoryParentID, c.NewCategoryName = 0, ""
	categoryID, err := a.resolveCategoryID(c.Category)
	var notFound *categoryNotFoundError
	if c.Reviewed && errors.As(err, &notFound) {
		// A category the user typed that matches nothing may be a new subcategory
		// of an existing parent. Model output never reaches this branch: only
		// rows re-submitted from the form are Reviewed.
		if parentID, name, canonical, ok := a.newCategoryFor(c.Category); ok {
			c.NewCategoryParentID, c.NewCategoryName = parentID, name
			c.Category = canonical
			c.CategoryID = 0
			issues = append(issues, candidateIssue{Field: "category", Text: "Unknown category; importing will create it"})
			if !hasError() && a.importCandidateDuplicate(accountID, *c) {
				c.Duplicate = true
			}
			return issues
		}
	}
	if err != nil {
		if id, fuzzyErr := a.resolveFuzzyCategoryID(c.Category); fuzzyErr == nil {
			categoryID = id
			err = nil
		}
	}
	if err != nil {
		issues = append(issues, candidateIssue{Field: "category", Text: err.Error(), IsError: true})
	} else if !a.categoryActive(categoryID) {
		issues = append(issues, candidateIssue{Field: "category", Text: fmt.Sprintf("category %q is inactive; choose an active category", c.Category), IsError: true})
	} else {
		c.CategoryID = categoryID
	}
	if !hasError() && a.importCandidateDuplicate(accountID, *c) {
		c.Duplicate = true
	}
	return issues
}

func (a *app) importCandidateDuplicate(accountID int, c importCandidate) bool {
	return a.importCandidateDuplicateID(accountID, c) > 0
}

// importCandidateDuplicateID returns the TRANSID of the existing transaction a
// candidate matches (same account, date, type, amount, and payee when known),
// or 0 if none. Split parents (CATEGID = -1) are excluded so an upsert never
// overwrites a split.
func (a *app) importCandidateDuplicateID(accountID int, c importCandidate) int64 {
	return a.importCandidateMatchID(accountID, c, false)
}

// importCandidateCommitted reports whether the archived statement row already
// exists in the ledger. Unlike an editable duplicate, this includes split
// parents: they must disappear from pending candidates after import, but they
// remain ineligible for the single-category upsert path.
func (a *app) importCandidateCommitted(accountID int, c importCandidate) bool {
	return a.importCandidateMatchID(accountID, c, true) > 0
}

func (a *app) importCandidateMatchID(accountID int, c importCandidate, includeSplits bool) int64 {
	nonSplit := " AND COALESCE(CATEGID, 0) <> -1"
	if includeSplits {
		nonSplit = ""
	}
	if c.Type == "Transfer" {
		if c.TransferDirection == "in" {
			return a.firstTransID(
				"ACCOUNTID = ? AND TOACCOUNTID = ? AND substr(COALESCE(TRANSDATE, ''), 1, 10) = ? AND TRANSCODE = 'Transfer' AND (ABS(COALESCE(TOTRANSAMOUNT, TRANSAMOUNT) - ?) < 0.005 OR ABS(TRANSAMOUNT - ?) < 0.005)"+nonSplit+" AND COALESCE(STATUS, '') <> 'Void'",
				c.ToAccountID, accountID, c.Date, c.Amount, c.Amount,
			)
		}
		return a.firstTransID(
			"ACCOUNTID = ? AND TOACCOUNTID = ? AND substr(COALESCE(TRANSDATE, ''), 1, 10) = ? AND TRANSCODE = 'Transfer' AND ABS(TRANSAMOUNT - ?) < 0.005"+nonSplit+" AND COALESCE(STATUS, '') <> 'Void'",
			accountID, c.ToAccountID, c.Date, c.Amount,
		)
	}
	if c.PayeeID <= 0 {
		if id, err := a.resolvePayeeID(c.Payee); err == nil {
			c.PayeeID = id
		}
	}
	if c.PayeeID <= 0 {
		if id := a.firstTransID(
			"ACCOUNTID = ? AND substr(COALESCE(TRANSDATE, ''), 1, 10) = ? AND TRANSCODE = ? AND ABS(TRANSAMOUNT - ?) < 0.005"+nonSplit+" AND COALESCE(STATUS, '') <> 'Void'",
			accountID, c.Date, c.Type, c.Amount,
		); id > 0 {
			return id
		}
		return a.importCandidateTransferMatchID(accountID, c, includeSplits)
	}
	if id := a.firstTransID(
		"ACCOUNTID = ? AND substr(COALESCE(TRANSDATE, ''), 1, 10) = ? AND TRANSCODE = ? AND ABS(TRANSAMOUNT - ?) < 0.005 AND COALESCE(PAYEEID, 0) = ?"+nonSplit+" AND COALESCE(STATUS, '') <> 'Void'",
		accountID, c.Date, c.Type, c.Amount, c.PayeeID,
	); id > 0 {
		return id
	}
	return a.importCandidateTransferMatchID(accountID, c, includeSplits)
}

func (a *app) importCandidateTransferDuplicateID(accountID int, c importCandidate) int64 {
	return a.importCandidateTransferMatchID(accountID, c, false)
}

func (a *app) importCandidateTransferMatchID(accountID int, c importCandidate, includeSplits bool) int64 {
	nonSplit := " AND COALESCE(CATEGID, 0) <> -1"
	if includeSplits {
		nonSplit = ""
	}
	switch c.Type {
	case "Deposit":
		return a.firstTransID(
			"TOACCOUNTID = ? AND substr(COALESCE(TRANSDATE, ''), 1, 10) = ? AND TRANSCODE = 'Transfer' AND (ABS(COALESCE(TOTRANSAMOUNT, TRANSAMOUNT) - ?) < 0.005 OR ABS(TRANSAMOUNT - ?) < 0.005)"+nonSplit+" AND COALESCE(STATUS, '') <> 'Void'",
			accountID, c.Date, c.Amount, c.Amount,
		)
	case "Withdrawal":
		return a.firstTransID(
			"ACCOUNTID = ? AND TOACCOUNTID IS NOT NULL AND TOACCOUNTID > 0 AND substr(COALESCE(TRANSDATE, ''), 1, 10) = ? AND TRANSCODE = 'Transfer' AND ABS(TRANSAMOUNT - ?) < 0.005"+nonSplit+" AND COALESCE(STATUS, '') <> 'Void'",
			accountID, c.Date, c.Amount,
		)
	default:
		return 0
	}
}

func (a *app) applyExistingTransactionToCandidate(accountID int, c *importCandidate, duplicateID int64) {
	tx, err := a.getTransaction(int(duplicateID))
	if err != nil {
		log.Printf("load duplicate transaction %d: %v", duplicateID, err)
		return
	}
	// The matched transaction is never a split (the lookup excludes CATEGID = -1),
	// so a candidate that duplicates it adopts its single category wholesale; drop
	// any replicated split so the row doesn't end up half split, half single.
	c.Splits = nil
	c.Date = tx.Date
	c.Type = tx.Type
	c.Amount = tx.Amount
	c.Payee = tx.Payee
	c.PayeeID = tx.PayeeID
	c.ToAccountID = tx.ToAccountID
	c.ToAccount = tx.ToAccount
	if tx.Type == "Transfer" {
		c.TransferDirection = "out"
		if tx.ToAccountID == accountID && tx.AccountID != accountID {
			c.TransferDirection = "in"
			c.ToAccountID = tx.AccountID
			c.ToAccount = tx.Account
			if tx.ToAmount.Valid {
				c.Amount = tx.ToAmount.Float64
			}
		}
	}
	c.Category = tx.Category
	c.CategoryID = tx.CategoryID
	if tx.CategoryID > 0 {
		if name := a.categoryPath(tx.CategoryID); name != "" {
			c.Category = name
		}
	}
	c.Notes = tx.Notes
	c.Status = tx.Status
}

func (a *app) categoryPath(id int) string {
	if id <= 0 {
		return ""
	}
	var name string
	var parent string
	err := a.db.QueryRow(`
SELECT c.CATEGNAME, COALESCE(parent.CATEGNAME, '')
  FROM CATEGORY_V1 c
  LEFT JOIN CATEGORY_V1 parent ON parent.CATEGID = c.PARENTID
 WHERE c.CATEGID = ?`, id).Scan(&name, &parent)
	if err != nil {
		return ""
	}
	if parent != "" {
		return parent + ": " + name
	}
	return name
}

func normalizeISODate(s string) string {
	s = strings.TrimSpace(s)
	if len(s) < 10 {
		return ""
	}
	s = s[:10]
	if _, err := time.Parse("2006-01-02", s); err != nil {
		return ""
	}
	return s
}

func candidateSignedDelta(c importCandidate) float64 {
	if c.Type == "Deposit" || (c.Type == "Transfer" && c.TransferDirection == "in") {
		return c.Amount
	}
	return -c.Amount
}

// applyClosingBalanceCheck fills in the review's closing-balance comparison: the
// account balance at the statement's closing date, projected forward by the
// non-duplicate, valid candidates dated on or before that date, versus the
// statement's own closing balance.
func (a *app) applyClosingBalanceCheck(review *monthlyReviewData, accountID int, closingDate string, closingBalance float64, candidates []importCandidate) {
	base, projected, expected, ok := a.projectClosing(accountID, closingDate, closingBalance, candidates)
	if !ok {
		return
	}
	review.ClosingBalanceChecked = true
	review.StatementClosingDate = normalizeISODate(closingDate)
	review.ClosingBaseBalance = roundMoney(base)
	review.StatementClosingBalance = roundMoney(expected)
	review.ProjectedClosingBalance = roundMoney(projected)
	review.ClosingBalanceDelta = roundMoney(projected - expected)
	review.ClosingBalanceMatches = math.Abs(projected-expected) < 0.005
}

// firstTransID runs a CHECKINGACCOUNT_V1 lookup and returns the lowest matching
// TRANSID, or 0 when there is no match.
func (a *app) firstTransID(condition string, args ...any) int64 {
	var id int64
	err := a.db.QueryRow("SELECT TRANSID FROM CHECKINGACCOUNT_V1 WHERE "+condition+" ORDER BY TRANSID LIMIT 1", args...).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0
	}
	if err != nil {
		log.Printf("duplicate lookup: %v", err)
		return 0
	}
	return id
}

func (a *app) loadImportRules() []importRule {
	raw := strings.TrimSpace(a.infoValue(importRulesInfoName))
	if raw == "" {
		return nil
	}
	var rules []importRule
	if err := json.Unmarshal([]byte(raw), &rules); err != nil {
		log.Printf("import rules: %v", err)
		return nil
	}
	return rules
}

func (a *app) applyImportRules(accountID int, c *importCandidate, rules []importRule) {
	source := normalizePayeePattern(c.OriginalPayee)
	if source == "" {
		return
	}
	var matched *importRule
	// Transfer rules are scoped to the account they were confirmed on, because one
	// description names a different counterparty seen from each side. Every other
	// rule is account-independent and is stored with AccountID 0, so the first
	// pass prefers a rule scoped to this account and the second takes those.
	for pass := 0; pass < 2 && matched == nil; pass++ {
		for i := range rules {
			rule := &rules[i]
			if (pass == 0 && rule.AccountID != accountID) || (pass == 1 && rule.AccountID != 0) {
				continue
			}
			pattern := normalizePayeePattern(rule.Pattern)
			if pattern != "" && importRuleMatches(source, pattern) {
				matched = rule
				break
			}
		}
	}
	if matched == nil {
		return
	}
	rule := *matched
	if rule.Type != "" {
		c.Type = rule.Type
	}
	if rule.Payee != "" {
		c.Payee = rule.Payee
	}
	if rule.Category != "" {
		c.Category = rule.Category
		c.LearnedCategory = true
	}
	if rule.CounterpartyAccountID > 0 {
		c.ToAccountID = rule.CounterpartyAccountID
		c.ToAccount = a.accountName(rule.CounterpartyAccountID)
	}
	if rule.Type == "Transfer" {
		c.TransferDirection = importRuleTransferDirection(rule)
	}
}

func importRuleMatches(source, pattern string) bool {
	s := strings.ToLower(source)
	p := strings.ToLower(pattern)
	if s == p || strings.Contains(s, p) || strings.Contains(p, s) {
		return true
	}
	// Token-subset: a generalized pattern matches when all of its significant
	// tokens appear in the source description, so "home depot" still maps a
	// later "the home depot #3344 riverton ny".
	patternTokens := significantTokens(normalizeSearchText(pattern))
	if len(patternTokens) == 0 {
		return false
	}
	inSource := map[string]bool{}
	for _, t := range strings.Fields(normalizeSearchText(source)) {
		inSource[t] = true
	}
	for _, t := range patternTokens {
		if !inSource[t] {
			return false
		}
	}
	return true
}

func (a *app) saveImportRulesInTx(tx *sql.Tx, inputs []transactionInput) error {
	rules, err := loadImportRulesInTx(tx)
	if err != nil {
		return err
	}
	for _, input := range inputs {
		pattern := generalizePayeePattern(input.OriginalPayee)
		if pattern == "" {
			continue
		}
		rule := importRule{
			Pattern:               pattern,
			Type:                  input.Type,
			Payee:                 strings.TrimSpace(input.PayeeName),
			Category:              strings.TrimSpace(input.CategoryName),
			CounterpartyAccountID: input.CounterpartyAccountID,
			TransferDirection:     input.TransferDirection,
		}
		if input.Type == "Transfer" {
			rule.AccountID = input.ReviewAccountID
			rule.Payee = ""
			rule.Category = ""
		}
		// A split has no single category to learn (CategoryName is a "Split: …"
		// summary), so keep the payee mapping but drop the category.
		if len(input.Splits) > 0 {
			rule.Category = ""
		}
		replaced := false
		for i := range rules {
			if !strings.EqualFold(generalizePayeePattern(rules[i].Pattern), pattern) {
				continue
			}
			if rules[i].AccountID != rule.AccountID {
				continue
			}
			// A pattern-only entry is an ambiguity tombstone. Once the same raw
			// description has meant different transactions, do not let a later
			// import arbitrarily make it learnable again.
			if importRuleEmpty(rules[i]) {
				replaced = true
				break
			}
			if !sameImportRuleTarget(rules[i], rule) {
				rules[i] = importRule{Pattern: pattern, AccountID: rule.AccountID}
			} else {
				rules[i] = rule
			}
			replaced = true
			break
		}
		if !replaced {
			rules = append(rules, rule)
		}
	}
	return storeImportRulesInTx(tx, rules)
}

func storeImportRulesInTx(tx *sql.Tx, rules []importRule) error {
	raw, err := json.Marshal(rules)
	if err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM INFOTABLE_V1 WHERE INFONAME = ?", importRulesInfoName); err != nil {
		return err
	}
	_, err = tx.Exec("INSERT INTO INFOTABLE_V1 (INFONAME, INFOVALUE) VALUES (?, ?)", importRulesInfoName, string(raw))
	return err
}

func importRuleEmpty(rule importRule) bool {
	return rule.Type == "" && rule.Payee == "" && rule.Category == "" &&
		rule.CounterpartyAccountID == 0 && rule.TransferDirection == ""
}

func sameImportRuleTarget(a, b importRule) bool {
	return a.Type == b.Type && strings.EqualFold(a.Payee, b.Payee) &&
		strings.EqualFold(a.Category, b.Category) &&
		a.AccountID == b.AccountID &&
		a.CounterpartyAccountID == b.CounterpartyAccountID &&
		importRuleTransferDirection(a) == importRuleTransferDirection(b)
}

func importRuleTransferDirection(rule importRule) string {
	if rule.Type != "Transfer" {
		return ""
	}
	if rule.TransferDirection == "in" {
		return "in"
	}
	return "out"
}

func loadImportRulesInTx(tx *sql.Tx) ([]importRule, error) {
	var raw string
	err := tx.QueryRow("SELECT INFOVALUE FROM INFOTABLE_V1 WHERE INFONAME = ?", importRulesInfoName).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var rules []importRule
	if err := json.Unmarshal([]byte(raw), &rules); err != nil {
		return nil, err
	}
	return rules, nil
}

// monthFromHeader picks the review month (YYYY-MM) from the statement's
// period/closing dates, falling back to the supplied default.
func monthFromHeader(header statementHeader, fallback string) string {
	for _, d := range []string{header.PeriodEnd, header.ClosingDate, header.PeriodStart} {
		d = strings.TrimSpace(d)
		if len(d) >= 7 {
			if _, err := time.Parse("2006-01", d[:7]); err == nil {
				return d[:7]
			}
		}
	}
	return fallback
}

func (a *app) firstReviewAccountID() int {
	accounts, err := a.listReviewAccounts()
	if err != nil {
		return 0
	}
	if len(accounts) > 0 {
		return accounts[0].ID
	}
	return 0
}

func accountDigits(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// statementAccountKey normalizes a statement's account identity (name + the
// trailing digits of its number) into a stable key for learned mappings.
func statementAccountKey(name, number string) string {
	name = normalizeSearchText(name)
	digits := accountDigits(number)
	if len(digits) > 4 {
		digits = digits[len(digits)-4:]
	}
	if name == "" && digits == "" {
		return ""
	}
	return name + "#" + digits
}

func (a *app) loadStatementAccounts() []statementAccountMapping {
	raw := strings.TrimSpace(a.infoValue(statementAccountsInfoName))
	if raw == "" {
		return nil
	}
	var mappings []statementAccountMapping
	if err := json.Unmarshal([]byte(raw), &mappings); err != nil {
		log.Printf("statement accounts: %v", err)
		return nil
	}
	return mappings
}

// detectStatementAccount resolves the MMEX account a statement belongs to,
// returning the account ID and whether the match is confident enough to skip
// the confirmation prompt. It checks learned mappings first, then a unique
// account-number match, then a unique high-scoring account-name match.
func (a *app) detectStatementAccount(header statementHeader) (int, bool) {
	accounts, err := a.listReviewAccounts()
	if err != nil {
		return 0, false
	}
	key := statementAccountKey(header.AccountName, header.AccountNumber)
	if key != "" {
		for _, m := range a.loadStatementAccounts() {
			if m.Key != key {
				continue
			}
			for _, ac := range accounts {
				if ac.ID == m.AccountID {
					return m.AccountID, true
				}
			}
		}
	}
	if sd := accountDigits(header.AccountNumber); len(sd) >= 4 {
		tail := sd[len(sd)-4:]
		matchID, matches := 0, 0
		for _, ac := range accounts {
			ad := accountDigits(ac.AccountNum)
			if len(ad) >= 4 && ad[len(ad)-4:] == tail {
				matchID, matches = ac.ID, matches+1
			}
		}
		if matches == 1 {
			return matchID, true
		}
	}
	name := normalizeSearchText(header.AccountName)
	if name != "" {
		bestID, bestScore, tie := 0, 0, false
		for _, ac := range accounts {
			score := fuzzyTextScore(name, normalizeSearchText(ac.Name))
			if score > bestScore {
				bestScore, bestID, tie = score, ac.ID, false
			} else if score == bestScore && score > 0 {
				tie = true
			}
		}
		if bestID > 0 && !tie && bestScore >= 82 {
			return bestID, true
		}
		if bestID > 0 {
			return bestID, false
		}
	}
	return 0, false
}

// saveStatementAccountInTx remembers (or refreshes) the mapping from a
// statement's account identity to the MMEX account it was imported into.
// learnStatementAccount persists the statement-identity → account mapping
// outside of an import, so confirming the account (without committing any
// candidate) is enough to auto-detect future statements from the same account.
// No-op when write mode is off, when there is nothing to key on, or when the
// mapping already points to accountID — so a re-confirmation does not trigger a
// needless backup/write.
func (a *app) learnStatementAccount(name, number string, accountID int) error {
	if !a.config.WriteEnabled {
		return nil
	}
	key := statementAccountKey(name, number)
	if key == "" || accountID <= 0 {
		return nil
	}
	for _, m := range a.loadStatementAccounts() {
		if m.Key == key {
			if m.AccountID == accountID {
				return nil // already learned
			}
			break
		}
	}
	if err := a.probeWriteLock(); err != nil {
		return err
	}
	if _, err := a.backupDatabase("statement-account-map"); err != nil {
		return err
	}
	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := a.saveStatementAccountInTx(tx, name, number, accountID); err != nil {
		return err
	}
	return tx.Commit()
}

// forgetStatementAccount removes a learned statement identity only when it
// still points to accountID. This lets deleting a wrongly assigned statement
// stop influencing future uploads without erasing a mapping that has already
// been corrected by a later reassignment.
func (a *app) forgetStatementAccount(name, number string, accountID int) error {
	if !a.config.WriteEnabled {
		return nil
	}
	key := statementAccountKey(name, number)
	if key == "" || accountID <= 0 {
		return nil
	}
	mappings := a.loadStatementAccounts()
	found := false
	for _, mapping := range mappings {
		if mapping.Key == key && mapping.AccountID == accountID {
			found = true
			break
		}
	}
	if !found {
		return nil
	}
	if err := a.probeWriteLock(); err != nil {
		return err
	}
	if _, err := a.backupDatabase("statement-account-forget"); err != nil {
		return err
	}
	kept := make([]statementAccountMapping, 0, len(mappings)-1)
	for _, mapping := range mappings {
		if mapping.Key == key && mapping.AccountID == accountID {
			continue
		}
		kept = append(kept, mapping)
	}
	encoded, err := json.Marshal(kept)
	if err != nil {
		return err
	}
	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("DELETE FROM INFOTABLE_V1 WHERE INFONAME = ?", statementAccountsInfoName); err != nil {
		return err
	}
	if len(kept) > 0 {
		if _, err := tx.Exec("INSERT INTO INFOTABLE_V1 (INFONAME, INFOVALUE) VALUES (?, ?)", statementAccountsInfoName, string(encoded)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (a *app) saveStatementAccountInTx(tx *sql.Tx, name, number string, accountID int) error {
	key := statementAccountKey(name, number)
	if key == "" || accountID <= 0 {
		return nil
	}
	var raw string
	err := tx.QueryRow("SELECT INFOVALUE FROM INFOTABLE_V1 WHERE INFONAME = ?", statementAccountsInfoName).Scan(&raw)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var mappings []statementAccountMapping
	if strings.TrimSpace(raw) != "" {
		if err := json.Unmarshal([]byte(raw), &mappings); err != nil {
			mappings = nil
		}
	}
	replaced := false
	for i := range mappings {
		if mappings[i].Key == key {
			mappings[i].AccountID = accountID
			replaced = true
			break
		}
	}
	if !replaced {
		mappings = append(mappings, statementAccountMapping{Key: key, AccountID: accountID})
	}
	encoded, err := json.Marshal(mappings)
	if err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM INFOTABLE_V1 WHERE INFONAME = ?", statementAccountsInfoName); err != nil {
		return err
	}
	_, err = tx.Exec("INSERT INTO INFOTABLE_V1 (INFONAME, INFOVALUE) VALUES (?, ?)", statementAccountsInfoName, string(encoded))
	return err
}

// statementExtraction bundles the result of extracting one statement from the
// LLM: the parsed header, the transaction candidates, and the transactions JSON
// used for the hidden re-validation field.
type statementExtraction struct {
	Sections []statementSection
	RawJSON  string
}

// projectClosing computes, for the closing date: base (the account balance there
// before importing anything), projected (base advanced by the valid,
// non-duplicate candidates dated on/before it), and expected (the statement's
// closing balance). base is exposed so the client can recompute projected live as
// candidates are toggled/edited. ok is false when it cannot be computed (unknown
// account or unreadable date).
//
// closingBalance must already be in ledger convention — see
// statementBalanceToLedger. This function deliberately does not convert: its
// result is published into the page and handed back to it on the next render, so
// converting here would flip the sign again on every round trip.
func (a *app) projectClosing(accountID int, closingDate string, closingBalance float64, candidates []importCandidate) (base, projected, expected float64, ok bool) {
	closingDate = normalizeISODate(closingDate)
	if accountID <= 0 || closingDate == "" {
		return 0, 0, 0, false
	}
	bal, err := a.balanceAt(accountID, closingDate)
	if err != nil {
		return 0, 0, 0, false
	}
	projected = bal
	for _, c := range candidates {
		if c.Duplicate || len(c.Errors) > 0 || c.Error != "" {
			continue
		}
		if d := normalizeISODate(c.Date); d != "" && d <= closingDate {
			projected += candidateSignedDelta(c)
		}
	}
	return bal, projected, closingBalance, true
}

func (a *app) insertImportedTransactions(inputs []transactionInput, stmtName, stmtNumber string, accountID int) ([]int64, error) {
	if !a.config.WriteEnabled {
		return nil, errors.New("write mode disabled")
	}
	if err := a.probeWriteLock(); err != nil {
		return nil, fmt.Errorf("database write lock unavailable: %w", err)
	}
	if _, err := a.backupDatabase("statement-import"); err != nil {
		return nil, fmt.Errorf("backup database: %w", err)
	}
	tx, err := a.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	// ids[i] is the TRANSID inputs[i] was committed as: the upserted row or the
	// newly inserted one.
	ids := make([]int64, 0, len(inputs))
	for _, input := range inputs {
		if input.NewCategoryName != "" {
			id, err := ensureCategoryInTx(tx, input.NewCategoryParentID, input.NewCategoryName)
			if err != nil {
				return nil, err
			}
			input.CategoryID = id
		}
		if input.UpdateID > 0 {
			if err := updateImportedTransactionInTx(tx, input); err != nil {
				return nil, err
			}
			ids = append(ids, input.UpdateID)
			continue
		}
		id, err := insertTransactionInTx(tx, input)
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := a.saveImportRulesInTx(tx, inputs); err != nil {
		return nil, err
	}
	if err := a.saveStatementAccountInTx(tx, stmtName, stmtNumber, accountID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return ids, nil
}

// updateImportedTransactionInTx upserts a possible-duplicate candidate onto its
// matched existing transaction (input.UpdateID), refreshing the editable fields
// without creating a new row. Splits are never targeted (see
// importCandidateDuplicateID), so this only touches single-category rows.
func updateImportedTransactionInTx(tx *sql.Tx, input transactionInput) error {
	payeeID := input.PayeeID
	if payeeID <= 0 && input.Type != "Transfer" {
		var err error
		payeeID, err = ensurePayeeInTx(tx, input.PayeeName, input.CategoryID)
		if err != nil {
			return err
		}
	}
	var toAccount any
	var toAmount any
	sourceAmount := input.Amount
	if input.Type == "Transfer" {
		toAccount = input.ToAccountID
		toAmount = input.Amount
		// A transfer can carry different source and destination amounts. A Review
		// upsert confirms only the statement side currently being reviewed, so
		// preserve the existing amount on the other side.
		var existingSourceAmount float64
		var existingToAmount sql.NullFloat64
		if err := tx.QueryRow("SELECT TRANSAMOUNT, TOTRANSAMOUNT FROM CHECKINGACCOUNT_V1 WHERE TRANSID = ?", input.UpdateID).
			Scan(&existingSourceAmount, &existingToAmount); err != nil {
			return err
		}
		if input.TransferDirection == "in" {
			sourceAmount = existingSourceAmount
		} else if existingToAmount.Valid {
			toAmount = existingToAmount.Float64
		}
	}
	now := time.Now().Format("2006-01-02T15:04:05")
	result, err := tx.Exec(`
UPDATE CHECKINGACCOUNT_V1
   SET ACCOUNTID = ?, TOACCOUNTID = ?, PAYEEID = ?, TRANSCODE = ?, TRANSAMOUNT = ?,
       STATUS = ?, NOTES = ?, CATEGID = ?, TRANSDATE = ?, TOTRANSAMOUNT = ?, LASTUPDATEDTIME = ?
 WHERE TRANSID = ? AND COALESCE(CATEGID, 0) <> -1`,
		input.AccountID, toAccount, payeeID, input.Type, sourceAmount,
		input.Status, input.Notes, input.CategoryID, input.Date+"T00:00:00", toAmount, now,
		input.UpdateID)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		return fmt.Errorf("transaction %d could not be updated (not found or is a split)", input.UpdateID)
	}
	if payeeID > 0 && input.OriginalPayee != "" && !strings.EqualFold(strings.TrimSpace(input.OriginalPayee), strings.TrimSpace(input.PayeeName)) {
		if err := appendPayeePatternInTx(tx, payeeID, input.OriginalPayee); err != nil {
			return err
		}
	}
	return nil
}

func reviewedMonthKey(accountID int, month string) string {
	return strconv.Itoa(accountID) + ":" + month
}

// reviewedMonths returns the set of manually-reviewed "<accountID>:<month>" keys.
func (a *app) reviewedMonths() map[string]bool {
	set := map[string]bool{}
	raw := strings.TrimSpace(a.infoValue(reviewedMonthsInfoName))
	if raw == "" {
		return set
	}
	var keys []string
	if err := json.Unmarshal([]byte(raw), &keys); err != nil {
		log.Printf("reviewed months: %v", err)
		return set
	}
	for _, k := range keys {
		set[k] = true
	}
	return set
}

// setMonthReviewed adds or removes an account-month from the manually-reviewed
// set, following the standard write-mode safety dance.
func (a *app) setMonthReviewed(accountID int, month string, reviewed bool) error {
	if !a.config.WriteEnabled {
		return errors.New("write mode disabled")
	}
	if accountID <= 0 || month == "" {
		return errors.New("account and month are required")
	}
	if err := a.probeWriteLock(); err != nil {
		return fmt.Errorf("database write lock unavailable: %w", err)
	}
	// No backup here: this only flips a lightweight INFOTABLE_V1 review flag (no
	// MMEX financial data is touched), and toggling it repeatedly would otherwise
	// litter BackupDir. The write-lock probe still guards against a running MMEX.
	set := a.reviewedMonths()
	key := reviewedMonthKey(accountID, month)
	if reviewed {
		set[key] = true
	} else {
		delete(set, key)
	}
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	encoded, err := json.Marshal(keys)
	if err != nil {
		return err
	}
	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("DELETE FROM INFOTABLE_V1 WHERE INFONAME = ?", reviewedMonthsInfoName); err != nil {
		return err
	}
	if _, err := tx.Exec("INSERT INTO INFOTABLE_V1 (INFONAME, INFOVALUE) VALUES (?, ?)", reviewedMonthsInfoName, string(encoded)); err != nil {
		return err
	}
	return tx.Commit()
}

func (a *app) markReviewed(w http.ResponseWriter, r *http.Request) {
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
	accountID, _ := strconv.Atoi(r.FormValue("account_id"))
	month := sanitizeMonth(r.FormValue("month"))
	if accountID <= 0 || month == "" {
		a.httpError(w, "account and month are required", http.StatusBadRequest)
		return
	}
	if err := a.setMonthReviewed(accountID, month, r.FormValue("reviewed") == "true"); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, reviewURL(month, accountID), http.StatusSeeOther)
}

func reviewURL(month string, accountID int) string {
	u := "/review?month=" + month
	if accountID > 0 {
		u += "&account_id=" + strconv.Itoa(accountID)
	}
	return u
}

func reviewMonth(r *http.Request) (string, string, string, error) {
	month := sanitizeMonth(r.FormValue("month"))
	if month == "" {
		month = sanitizeMonth(r.URL.Query().Get("month"))
	}
	if month == "" {
		// Fall back to the year/month dropdown selections (the picker form posts
		// these as separate fields rather than a combined YYYY-MM string).
		if y, m := r.FormValue("year"), r.FormValue("mon"); y != "" && m != "" {
			month = sanitizeMonth(y + "-" + m)
		}
	}
	if month == "" {
		month = time.Now().AddDate(0, -1, 0).Format("2006-01")
	}
	t, err := time.Parse("2006-01", month)
	if err != nil {
		return "", "", "", errors.New("month must be YYYY-MM")
	}
	start := t.Format("2006-01-02")
	end := t.AddDate(0, 1, -1).Format("2006-01-02")
	return month, start, end, nil
}

func sanitizeMonth(value string) string {
	value = strings.TrimSpace(value)
	if _, err := time.Parse("2006-01", value); err != nil {
		return ""
	}
	return value
}

func adjacentReviewMonths(month string) (string, string) {
	t, err := time.Parse("2006-01", month)
	if err != nil {
		return "", ""
	}
	return t.AddDate(0, -1, 0).Format("2006-01"), t.AddDate(0, 1, 0).Format("2006-01")
}

// monthOption is one entry in the month dropdown ("01" → "January").
type monthOption struct {
	Value string
	Name  string
}

func monthOptions() []monthOption {
	opts := make([]monthOption, 12)
	for i := 1; i <= 12; i++ {
		opts[i-1] = monthOption{
			Value: fmt.Sprintf("%02d", i),
			Name:  time.Month(i).String(),
		}
	}
	return opts
}

// reviewYearRange returns the years to offer in the picker: from the earliest
// transaction year (falling back to the current year) through next year, so a
// freshly entered statement for an upcoming month is always selectable.
func (a *app) reviewYearRange() []int {
	now := time.Now().Year()
	first := now
	var earliest string
	if err := a.db.QueryRow(`SELECT MIN(TRANSDATE) FROM CHECKINGACCOUNT_V1 WHERE STATUS <> 'Void'`).Scan(&earliest); err == nil {
		if t, perr := time.Parse("2006-01-02", earliest[:min(len(earliest), 10)]); perr == nil && t.Year() < first {
			first = t.Year()
		}
	}
	last := now + 1
	years := make([]int, 0, last-first+1)
	for y := last; y >= first; y-- {
		years = append(years, y)
	}
	return years
}
