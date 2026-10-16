package mmxc

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// archivedStatement is the sidecar metadata stored as a JSON file next to each
// uploaded statement under StatementDir/<accountID>/. The statement is filed by
// its *own* period (PeriodStart..PeriodEnd) rather than the month it was
// reviewed under, so a credit-card statement that straddles a month boundary
// (e.g. Dec 15 – Jan 14) is listed under both December and January via period
// overlap. ContentHash dedups re-uploads of the same file, and the cached
// extraction (Header/Candidates/RawJSON) lets re-opening a month reuse the LLM
// result instead of re-extracting.
//
// Stored on disk outside the MMEX database to keep the .mmb schema untouched.
type archivedStatement struct {
	File         string `json:"file"`          // stored filename (basename, under the storage dir)
	OriginalName string `json:"original_name"` // upload filename as received
	UploadedAt   string `json:"uploaded_at"`   // RFC3339
	AccountID    int    `json:"account_id"`    // confirmed account, or the best guess while pending
	ContentHash  string `json:"content_hash"`  // sha256 hex of the file bytes
	// DocumentHash/DocumentFile/SectionID identify a shared document and the
	// account-specific section this sidecar projects from it. They are empty for a
	// statement archived without one — the no-AI path (attachStatementWithoutAI),
	// which stores the blob beside the sidecar. Both shapes are current, so every
	// path that touches the blob must handle an empty DocumentHash.
	DocumentHash string `json:"document_hash,omitempty"`
	DocumentFile string `json:"document_file,omitempty"`
	SectionID    string `json:"section_id,omitempty"`
	// Confirmed is set once the user has chosen the account this statement belongs
	// to. Until then the statement lives under the _pending directory and the
	// AccountID is only a detection guess, so it never appears under (or counts
	// toward the reconciliation of) any real account.
	Confirmed bool `json:"confirmed"`

	// Resolved period, always populated at archive time (falling back to the
	// closing/upload date) so overlap queries need no further fallback logic.
	PeriodStart string `json:"period_start"` // YYYY-MM-DD
	PeriodEnd   string `json:"period_end"`   // YYYY-MM-DD

	// Statement header fields, retained so a cache hit can rebuild a
	// statementHeader without re-running extraction.
	AccountName    string  `json:"account_name,omitempty"`
	AccountNumber  string  `json:"account_number,omitempty"`
	ClosingDate    string  `json:"closing_date,omitempty"`
	ClosingBalance float64 `json:"closing_balance,omitempty"`
	HasClosing     bool    `json:"has_closing,omitempty"`

	Candidates []importCandidate `json:"candidates,omitempty"` // cached extraction
	RawJSON    string            `json:"raw_json,omitempty"`

	// Imported links candidates to the transactions they were committed as.
	// Pending detection otherwise matches the ledger against the row as
	// extracted, which a row imported with an edited type, date or amount no
	// longer resembles.
	Imported []importedCandidateLink `json:"imported,omitempty"`

	// sidecarPath is the absolute path of the .json sidecar; set on load, not
	// serialized.
	sidecarPath string `json:"-"`
}

func archivedStatementCandidates(meta archivedStatement) []importCandidate {
	candidates := append([]importCandidate(nil), meta.Candidates...)
	for i := range candidates {
		candidates[i].ArchiveFile = meta.File
		candidates[i].ArchiveCandidateIndex = i
		candidates[i].ArchiveCandidateKey = archivedCandidateKey(meta.Candidates[i])
	}
	return candidates
}

func archivedCandidateKey(candidate importCandidate) string {
	key, _ := json.Marshal(struct {
		Date              string  `json:"date"`
		Type              string  `json:"type"`
		Amount            float64 `json:"amount"`
		Description       string  `json:"description"`
		Payee             string  `json:"payee"`
		Category          string  `json:"category"`
		Notes             string  `json:"notes"`
		Status            string  `json:"status"`
		TransferDirection string  `json:"transfer_direction"`
	}{
		Date:              candidate.Date,
		Type:              candidate.Type,
		Amount:            candidate.Amount,
		Description:       candidate.Description,
		Payee:             candidate.Payee,
		Category:          candidate.Category,
		Notes:             candidate.Notes,
		Status:            candidate.Status,
		TransferDirection: candidate.TransferDirection,
	})
	return statementContentHash(key)
}

// importedCandidateLink records that the archived candidate with Key (its
// archivedCandidateKey) was committed as transaction TransID.
type importedCandidateLink struct {
	Key     string `json:"key"`
	TransID int64  `json:"trans_id"`
}

// linkImportedCandidate records transID against one of meta's candidates with
// key, reporting whether meta changed. A statement listing the same row more
// than once (two identical charges) takes one link per row, so a key is only
// linked while it has an unlinked row left; links whose transaction is no longer
// live (voided or deleted) free their row first.
func linkImportedCandidate(meta *archivedStatement, key string, transID int64, live func(int64) bool) bool {
	rows := 0
	for _, c := range meta.Candidates {
		if archivedCandidateKey(c) == key {
			rows++
		}
	}
	kept := make([]importedCandidateLink, 0, len(meta.Imported)+1)
	links := 0
	for _, l := range meta.Imported {
		if l.Key == key {
			if l.TransID == transID {
				return false
			}
			if !live(l.TransID) {
				continue
			}
			links++
		}
		kept = append(kept, l)
	}
	if links >= rows {
		return false
	}
	meta.Imported = append(kept, importedCandidateLink{Key: key, TransID: transID})
	return true
}

// header rebuilds the statementHeader captured at extraction time from the
// archived fields, for reuse on a content-hash cache hit.
func (s archivedStatement) header() statementHeader {
	return statementHeader{
		AccountName:    s.AccountName,
		AccountNumber:  s.AccountNumber,
		PeriodStart:    s.PeriodStart,
		PeriodEnd:      s.PeriodEnd,
		ClosingDate:    s.ClosingDate,
		ClosingBalance: s.ClosingBalance,
		HasClosing:     s.HasClosing,
	}
}

// UploadedDate renders the archive timestamp as a plain YYYY-MM-DD (exported for
// the review template).
func (s archivedStatement) UploadedDate() string {
	if t, err := time.Parse(time.RFC3339, s.UploadedAt); err == nil {
		return t.Format("2006-01-02")
	}
	return s.UploadedAt
}

// downloadStatement serves an archived statement blob. The requested file must
// match a known sidecar for the account, which both authorizes the download and
// blocks path traversal (no user path component is ever joined raw).
func (a *app) downloadStatement(w http.ResponseWriter, r *http.Request) {
	accountID, _ := strconv.Atoi(r.URL.Query().Get("account_id"))
	file := r.URL.Query().Get("file")
	if accountID <= 0 || file == "" {
		a.httpError(w, "account_id and file are required", http.StatusBadRequest)
		return
	}
	var match *archivedStatement
	for _, meta := range a.listArchivedStatements(accountID) {
		if meta.File == file {
			m := meta
			match = &m
			break
		}
	}
	if match == nil {
		http.NotFound(w, r)
		return
	}
	name := match.OriginalName
	if name == "" {
		name = match.File
	}
	w.Header().Set("Content-Disposition", `inline; filename="`+safeUploadName(name)+`"`)
	path := filepath.Join(a.accountStatementDir(accountID), match.File)
	if match.DocumentHash != "" {
		path = a.statementDocumentBlobPath(match.DocumentHash, match.DocumentFile)
	}
	http.ServeFile(w, r, path)
}

// reassignStatement moves a confirmed archived statement to another editable
// account and updates the learned statement-identity mapping. Future uploads of
// the same account format therefore follow the correction as well.
func (a *app) reassignStatement(w http.ResponseWriter, r *http.Request) {
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
	sourceID, _ := strconv.Atoi(r.FormValue("account_id"))
	targetID, _ := strconv.Atoi(r.FormValue("target_account_id"))
	file := strings.TrimSpace(r.FormValue("file"))
	month := sanitizeMonth(r.FormValue("month"))
	if sourceID <= 0 || targetID <= 0 || file == "" {
		a.httpError(w, "source account, target account, and file are required", http.StatusBadRequest)
		return
	}
	if !a.isReviewAccount(targetID) {
		a.httpError(w, "target account is not an editable open account", http.StatusBadRequest)
		return
	}
	meta, ok := a.findStatementForAccount(sourceID, file)
	if !ok {
		http.NotFound(w, r)
		return
	}
	// Correct the durable mapping first. If the subsequent filesystem move
	// fails, future uploads are at least no longer sent to the wrong account and
	// the visible statement can be retried.
	if err := a.learnStatementAccount(meta.AccountName, meta.AccountNumber, targetID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	updated, err := a.confirmStatement(meta, targetID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	a.audit(r, auditEntry{
		Action: fmt.Sprintf("statement reassign from_account_id=%d to_account_id=%d", sourceID, targetID),
		Detail: fmt.Sprintf("file=%q from_account=%q to_account=%q", updated.File, a.accountName(sourceID), a.accountName(targetID)),
	})
	if month == "" && len(updated.PeriodStart) >= 7 {
		month = sanitizeMonth(updated.PeriodStart[:7])
	}
	http.Redirect(w, r, reviewURL(month, targetID), http.StatusSeeOther)
}

// deleteStatement removes one known archived statement and forgets the learned
// account mapping when it still points to this statement's current account. A
// mapping already corrected elsewhere is deliberately retained.
func (a *app) deleteStatement(w http.ResponseWriter, r *http.Request) {
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
	file := strings.TrimSpace(r.FormValue("file"))
	month := sanitizeMonth(r.FormValue("month"))
	if accountID <= 0 || file == "" {
		a.httpError(w, "account and file are required", http.StatusBadRequest)
		return
	}
	meta, ok := a.findStatementForAccount(accountID, file)
	if !ok {
		http.NotFound(w, r)
		return
	}
	// Remove the harmful durable association before deleting the recoverable
	// archive metadata. If file deletion fails, the user can retry without future
	// uploads continuing to inherit the wrong assignment.
	if err := a.forgetStatementAccount(meta.AccountName, meta.AccountNumber, accountID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := a.deleteArchivedStatement(meta); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	a.audit(r, auditEntry{
		Action: fmt.Sprintf("statement delete account_id=%d", accountID),
		Detail: fmt.Sprintf("file=%q account=%q", meta.File, a.accountName(accountID)),
	})
	http.Redirect(w, r, reviewURL(month, accountID), http.StatusSeeOther)
}

// deleteSharedStatement removes a v2 document and all of its account-section
// attachments. Detaching one section remains the less destructive default.
func (a *app) deleteSharedStatement(w http.ResponseWriter, r *http.Request) {
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
	hash := strings.TrimSpace(r.FormValue("document_hash"))
	accountID, _ := strconv.Atoi(r.FormValue("account_id"))
	month := sanitizeMonth(r.FormValue("month"))
	doc, err := a.loadStatementDocument(hash)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	for _, meta := range a.documentStatementAttachments(hash) {
		if err := a.forgetStatementAccount(meta.AccountName, meta.AccountNumber, meta.AccountID); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	if err := a.deleteStatementDocument(doc); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// The content hash is a digest, not content, so it stays in the safe half.
	a.audit(r, auditEntry{
		Action: fmt.Sprintf("statement document delete document=%q", hash),
		Detail: fmt.Sprintf("original=%q", doc.OriginalName),
	})
	http.Redirect(w, r, reviewURL(month, accountID), http.StatusSeeOther)
}

func (a *app) isReviewAccount(accountID int) bool {
	accounts, err := a.listReviewAccounts()
	if err != nil {
		return false
	}
	for _, acct := range accounts {
		if acct.ID == accountID {
			return true
		}
	}
	return false
}

// findStatementForAccount resolves only within the stated account directory.
// This both prevents path traversal and avoids ambiguity if two directories
// happen to contain the same generated basename.
func (a *app) findStatementForAccount(accountID int, file string) (archivedStatement, bool) {
	for _, meta := range a.listArchivedStatements(accountID) {
		if meta.File == file {
			return meta, true
		}
	}
	return archivedStatement{}, false
}

func (a *app) deleteArchivedStatement(meta archivedStatement) error {
	dir := a.statementStorageDir(meta.AccountID, meta.Confirmed)
	sidecarPath := filepath.Join(dir, meta.File+".json")
	if meta.DocumentHash != "" {
		return os.Remove(sidecarPath)
	}
	blobPath := filepath.Join(dir, meta.File)
	sidecar, err := os.ReadFile(sidecarPath)
	if err != nil {
		return err
	}
	if err := os.Remove(sidecarPath); err != nil {
		return err
	}
	if err := os.Remove(blobPath); err != nil {
		// Restore visibility if the blob could not be removed. The original mode
		// is always 0600 because writeStatementSidecar creates it that way.
		if restoreErr := os.WriteFile(sidecarPath, sidecar, 0o600); restoreErr != nil {
			return fmt.Errorf("delete statement: %v; restore sidecar: %w", err, restoreErr)
		}
		return err
	}
	return nil
}

// statementContentHash is the dedup key for an uploaded statement: the sha256 of
// its raw bytes, hex-encoded.
func statementContentHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// pendingStatementDirName holds statements whose account has not been confirmed
// yet. It is a sibling of the numeric per-account directories; the leading
// underscore keeps it out of the account scans (which require an integer name).
const pendingStatementDirName = "_pending"

func (a *app) accountStatementDir(accountID int) string {
	return filepath.Join(a.config.StatementDir, strconv.Itoa(accountID))
}

func (a *app) pendingStatementDir() string {
	return filepath.Join(a.config.StatementDir, pendingStatementDirName)
}

// statementStorageDir is where a statement's files live: the per-account
// directory once confirmed, otherwise the shared pending directory.
func (a *app) statementStorageDir(accountID int, confirmed bool) string {
	if confirmed {
		return a.accountStatementDir(accountID)
	}
	return a.pendingStatementDir()
}

// saveStatementFile writes just the statement blob (no sidecar) and returns its
// stored basename. Used on its own when extraction fails: the file is kept and
// downloadable, but without a sidecar it is not a reusable cache entry, so a
// later re-upload retries extraction.
func (a *app) saveStatementFile(accountID int, confirmed bool, originalName string, data []byte, stamp time.Time) (string, error) {
	dir := a.statementStorageDir(accountID, confirmed)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	stored := stamp.Format("20060102-150405.000000") + "-" + safeUploadName(originalName)
	if err := os.WriteFile(filepath.Join(dir, stored), data, 0o600); err != nil {
		return "", err
	}
	return stored, nil
}

// archiveStatement writes the statement file and its JSON sidecar (under the
// account directory when confirmed, else under _pending) and returns the
// recorded metadata. The period is resolved from the header, falling back to the
// closing date and then the upload time, so the sidecar always carries a usable
// [PeriodStart, PeriodEnd].
func (a *app) archiveStatement(accountID int, confirmed bool, originalName string, data []byte, header statementHeader, candidates []importCandidate, rawJSON string) (archivedStatement, error) {
	stamp := time.Now()
	stored, err := a.saveStatementFile(accountID, confirmed, originalName, data, stamp)
	if err != nil {
		return archivedStatement{}, err
	}
	start, end := resolveStatementPeriod(header, stamp)
	meta := archivedStatement{
		File:           stored,
		OriginalName:   originalName,
		UploadedAt:     stamp.Format(time.RFC3339),
		AccountID:      accountID,
		Confirmed:      confirmed,
		ContentHash:    statementContentHash(data),
		PeriodStart:    start,
		PeriodEnd:      end,
		AccountName:    header.AccountName,
		AccountNumber:  header.AccountNumber,
		ClosingDate:    normalizeISODate(header.ClosingDate),
		ClosingBalance: header.ClosingBalance,
		HasClosing:     header.HasClosing,
		Candidates:     candidates,
		RawJSON:        rawJSON,
	}
	if err := a.writeStatementSidecar(&meta); err != nil {
		return archivedStatement{}, err
	}
	return meta, nil
}

// writeStatementSidecar (re)writes the JSON sidecar next to the statement's blob
// in its current storage directory and records the resulting path on meta.
func (a *app) writeStatementSidecar(meta *archivedStatement) error {
	dir := a.statementStorageDir(meta.AccountID, meta.Confirmed)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	sidecar := filepath.Join(dir, meta.File+".json")
	encoded, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(sidecar, encoded, 0o600); err != nil {
		return err
	}
	meta.sidecarPath = sidecar
	return nil
}

// confirmStatement assigns a (possibly pending) statement to accountID: it moves
// the blob and sidecar into the account's directory if they are not already
// there and marks the sidecar Confirmed. Idempotent — re-confirming the same
// account is a no-op beyond rewriting the sidecar.
func (a *app) confirmStatement(meta archivedStatement, accountID int) (archivedStatement, error) {
	if accountID <= 0 {
		return meta, nil
	}
	srcDir := a.statementStorageDir(meta.AccountID, meta.Confirmed)
	dstDir := a.accountStatementDir(accountID)
	if srcDir != dstDir {
		if err := os.MkdirAll(dstDir, 0o700); err != nil {
			return meta, err
		}
		srcSidecar := filepath.Join(srcDir, meta.File+".json")
		dstSidecar := filepath.Join(dstDir, meta.File+".json")
		targets := []string{dstSidecar}
		if meta.DocumentHash == "" {
			targets = append(targets, filepath.Join(dstDir, meta.File))
		}
		for _, target := range targets {
			if _, err := os.Stat(target); err == nil {
				return meta, fmt.Errorf("statement destination already exists: %s", filepath.Base(target))
			} else if !errors.Is(err, os.ErrNotExist) {
				return meta, err
			}
		}
		if meta.DocumentHash == "" {
			if err := os.Rename(filepath.Join(srcDir, meta.File), filepath.Join(dstDir, meta.File)); err != nil {
				return meta, err
			}
		}
		original := meta
		meta.AccountID = accountID
		meta.Confirmed = true
		if err := a.writeStatementSidecar(&meta); err != nil {
			_ = os.Remove(dstSidecar)
			if meta.DocumentHash == "" {
				_ = os.Rename(filepath.Join(dstDir, meta.File), filepath.Join(srcDir, meta.File))
			}
			return original, err
		}
		if err := os.Remove(srcSidecar); err != nil {
			_ = os.Remove(dstSidecar)
			if meta.DocumentHash == "" {
				_ = os.Rename(filepath.Join(dstDir, meta.File), filepath.Join(srcDir, meta.File))
			}
			return original, err
		}
		return meta, nil
	}
	meta.AccountID = accountID
	meta.Confirmed = true
	if err := a.writeStatementSidecar(&meta); err != nil {
		return meta, err
	}
	return meta, nil
}

// resolveStatementPeriod derives the [start, end] a statement covers, always
// returning two ISO dates. It prefers the header's period, falls back to the
// closing date for either bound, and finally to the upload day.
func resolveStatementPeriod(header statementHeader, uploaded time.Time) (string, string) {
	start := normalizeISODate(header.PeriodStart)
	end := normalizeISODate(header.PeriodEnd)
	closing := normalizeISODate(header.ClosingDate)
	if end == "" {
		end = closing
	}
	if start == "" {
		start = closing
	}
	if start == "" {
		start = end
	}
	if end == "" {
		end = start
	}
	if start == "" {
		day := uploaded.Format("2006-01-02")
		start, end = day, day
	}
	if start > end {
		start, end = end, start
	}
	return start, end
}

// listArchivedStatements loads every (confirmed) sidecar under the account's
// statement directory, newest upload first. Pending statements live elsewhere,
// so they never surface under an account.
func (a *app) listArchivedStatements(accountID int) []archivedStatement {
	out := statementsInDir(a.accountStatementDir(accountID), accountID, true)
	sort.Slice(out, func(i, j int) bool {
		return out[i].UploadedAt > out[j].UploadedAt
	})
	return out
}

// statementsInDir loads all sidecars directly under dir (non-recursive). The
// physical location is authoritative for Confirmed (a sidecar under an account
// directory is confirmed; one under _pending is not), which also self-heals
// sidecars written before the field existed.
func statementsInDir(dir string, fallbackAccountID int, confirmed bool) []archivedStatement {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []archivedStatement
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		meta, err := loadStatementSidecar(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		// For a confirmed statement the directory it sits in is authoritative for
		// the account; while pending, the sidecar's detection guess is kept.
		if confirmed {
			meta.AccountID = fallbackAccountID
		} else if meta.AccountID == 0 {
			meta.AccountID = fallbackAccountID
		}
		meta.Confirmed = confirmed
		out = append(out, meta)
	}
	return out
}

// allArchivedStatements loads every sidecar across all per-account directories
// and the pending directory.
func (a *app) allArchivedStatements() []archivedStatement {
	entries, err := os.ReadDir(a.config.StatementDir)
	if err != nil {
		return nil
	}
	var out []archivedStatement
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(a.config.StatementDir, e.Name())
		if accountID, err := strconv.Atoi(e.Name()); err == nil {
			out = append(out, statementsInDir(dir, accountID, true)...)
		} else if e.Name() == pendingStatementDirName {
			out = append(out, statementsInDir(dir, 0, false)...)
		}
	}
	return out
}

func loadStatementSidecar(path string) (archivedStatement, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return archivedStatement{}, err
	}
	var meta archivedStatement
	if err := json.Unmarshal(data, &meta); err != nil {
		return archivedStatement{}, err
	}
	meta.sidecarPath = path
	return meta, nil
}

// findStatementByHash scans archived statements (confirmed and pending) for one
// whose content hash matches, so a re-upload of the same file reuses its cached
// extraction instead of re-running the LLM.
func (a *app) findStatementByHash(hash string) (archivedStatement, bool) {
	if hash == "" {
		return archivedStatement{}, false
	}
	for _, meta := range a.allArchivedStatements() {
		if meta.ContentHash == hash {
			return meta, true
		}
	}
	return archivedStatement{}, false
}

// findStatementByFile locates an archived statement (confirmed or pending) by its
// stored basename, used to confirm/relocate it once the account is chosen.
func (a *app) findStatementByFile(file string) (archivedStatement, bool) {
	if file == "" {
		return archivedStatement{}, false
	}
	for _, meta := range a.allArchivedStatements() {
		if meta.File == file {
			return meta, true
		}
	}
	return archivedStatement{}, false
}

// statementsForMonth returns the account's archived statements whose period
// overlaps the [start, end] review month, newest first.
func (a *app) statementsForMonth(accountID int, start, end string) []archivedStatement {
	var out []archivedStatement
	for _, meta := range a.listArchivedStatements(accountID) {
		if statementOverlapsMonth(meta, start, end) {
			out = append(out, meta)
		}
	}
	return out
}

// closingStatementForMonth picks the statement to reconcile the account against
// for a month: the one with the latest closing date that has already happened as
// of monthEnd. statementsForMonth hands over every statement whose *period*
// overlaps the month, so a cycle running past month end (a card closing on the
// 23rd puts one in view of every month) is in that list too — reconciling
// against its closing would answer a question about the following month, which
// is why anything closing after monthEnd is skipped. A month where no cycle has
// closed yet gets no closing figure at all rather than a borrowed one.
func closingStatementForMonth(statements []archivedStatement, monthEnd string) (archivedStatement, bool) {
	var best archivedStatement
	found := false
	for _, s := range statements {
		if !s.HasClosing || s.ClosingDate == "" {
			continue
		}
		if monthEnd != "" && s.ClosingDate > monthEnd {
			continue
		}
		if !found || s.ClosingDate > best.ClosingDate {
			best = s
			found = true
		}
	}
	return best, found
}

// openingStatementForMonth picks the statement whose cycle *opened* inside the
// month (the latest one, when a month contains more than one start). It is the
// opening anchor for a month no cycle has closed in yet: the balance a statement
// prints at the top of a still-running cycle is checkable against the ledger
// right away, long before the closing figure means anything.
func openingStatementForMonth(statements []archivedStatement, start, end string) (archivedStatement, bool) {
	var best archivedStatement
	found := false
	for _, s := range statements {
		if s.PeriodStart == "" || s.PeriodStart < start || s.PeriodStart > end {
			continue
		}
		if !found || s.PeriodStart > best.PeriodStart {
			best = s
			found = true
		}
	}
	return best, found
}

// statementOverlapsMonth reports whether the statement's resolved period
// intersects the [start, end] calendar month (ISO date string comparison).
func statementOverlapsMonth(s archivedStatement, start, end string) bool {
	if s.PeriodStart == "" || s.PeriodEnd == "" || start == "" || end == "" {
		return false
	}
	return s.PeriodStart <= end && s.PeriodEnd >= start
}

func nextISODate(s string) string {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return s
	}
	return t.AddDate(0, 0, 1).Format("2006-01-02")
}

// prevISODate is nextISODate's inverse. balanceAt reports the balance at the end
// of the given day, so the balance a period *opens* with is the one at the end of
// the day before it starts.
func prevISODate(s string) string {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return s
	}
	return t.AddDate(0, 0, -1).Format("2006-01-02")
}

// monthCoverage merges the archived statement periods (clipped to the month) and
// reports whether they cover the whole [start, end] contiguously, plus the last
// date covered without a gap from the month start (empty when the month start
// itself is uncovered). This drives the reconciliation indicator: a credit-card
// month is only "complete" once a statement reaching its last day exists.
func monthCoverage(statements []archivedStatement, start, end string) (full bool, through string) {
	type interval struct{ s, e string }
	var ivs []interval
	for _, st := range statements {
		s, e := st.PeriodStart, st.PeriodEnd
		if s < start {
			s = start
		}
		if e > end {
			e = end
		}
		if s != "" && e != "" && s <= e {
			ivs = append(ivs, interval{s, e})
		}
	}
	if len(ivs) == 0 {
		return false, ""
	}
	sort.Slice(ivs, func(i, j int) bool { return ivs[i].s < ivs[j].s })
	if ivs[0].s > start {
		return false, "" // gap at the very start of the month
	}
	cur := ivs[0].e
	for _, v := range ivs[1:] {
		if v.s > nextISODate(cur) {
			break // a gap interrupts contiguous coverage
		}
		if v.e > cur {
			cur = v.e
		}
	}
	return cur >= end, cur
}
