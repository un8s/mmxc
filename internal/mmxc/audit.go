package mmxc

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Audit levels.
//
// The audit log records what MMXC changed. The details of a financial change —
// payee names, amounts, account numbers, statement file names, sign-in
// identities — are exactly the data the rest of the app works to keep private,
// so the level controls how much of that is written down at all. "actions" is
// the default: enough to reconstruct what happened and to which row, without
// narrating anyone's finances.
const (
	auditOff      = "off"      // record nothing
	auditActions  = "actions"  // verb plus row ids only
	auditDetailed = "detailed" // adds names, amounts and identities; opt-in
)

// auditRetentionForever is the audit_retention_days value that keeps every row.
const auditRetentionForever = 0

// errAuditDirUnset marks an audit store that was never configured, as opposed
// to one that failed to open. It is reported on Settings but not logged: a
// config with no audit directory is a state, not an error worth a log line.
var errAuditDirUnset = errors.New("audit directory is not configured")

func normalizeAuditLog(value string) (string, error) {
	level := strings.ToLower(strings.TrimSpace(value))
	if level == "" {
		level = auditActions
	}
	switch level {
	case auditOff, auditActions, auditDetailed:
		return level, nil
	default:
		return "", errors.New("audit_log must be off, actions, or detailed")
	}
}

// auditEntry splits one recorded action into the part that is always safe to
// keep (Action: the verb plus numeric row ids, which mean nothing without the
// database) and the part that identifies people, payees, amounts, files or
// paths (Detail). Detail is written only at the "detailed" level. The split is
// spelled out at every call site on purpose: it is the whole security property,
// so it should be visible where the record is made rather than buried here.
type auditEntry struct {
	Action string
	Detail string
}

// auditRecord is one row as shown on the audit page.
type auditRecord struct {
	ID     int
	Date   string // YYYY-MM-DD, for DateFmt
	Time   string // HH:MM:SS UTC
	Actor  string
	Action string
	Detail string
}

// auditStore is the audit log's own SQLite database, deliberately separate from
// the MMEX file for three reasons: the MMEX *_V1 schema is fixed and must stay
// readable by MMEX, audit rows must not ride along into every MMEX backup, and
// the log has to keep working while the MMEX database is open read-only.
//
// It is opened through the same openSQLite as everything else, so when the MMEX
// database is encrypted the audit log is encrypted with the same key, through
// the same Adiantum VFS. That matters more here than anywhere else: an audit
// log is a plaintext narration of precisely the data .mmbx exists to protect.
type auditStore struct {
	db   *sql.DB
	path string
}

// auditStorePath names the audit database for the open MMEX database. It lives
// in its own directory, not the scanned database folder: scanDatabaseFiles
// skips subdirectories, so an encrypted audit log (which by necessity carries
// the .mmbx extension that marks encryption) can never show up in the Settings
// database picker as something to open.
func auditStorePath(cfg config) string {
	base := strings.TrimSuffix(filepath.Base(cfg.DBPath), filepath.Ext(cfg.DBPath))
	ext := ".db"
	if databaseEncrypted(cfg.DBPath) {
		ext = encryptedDBExt
	}
	return filepath.Join(cfg.AuditDir, base+".audit"+ext)
}

// openAuditStore opens (creating if needed) the audit database for cfg. It
// returns a nil store when auditing is switched off, which every caller treats
// as "record nothing".
func openAuditStore(cfg config) (*auditStore, error) {
	if cfg.AuditLog == auditOff {
		return nil, nil
	}
	dir := strings.TrimSpace(cfg.AuditDir)
	if dir == "" {
		return nil, errAuditDirUnset
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := auditStorePath(cfg)
	// "rwc", never "ro": the audit log is a separate file, so it stays writable
	// even when the MMEX database is open read-only — read-only mode still has
	// sign-ins and settings changes worth recording.
	db, err := openSQLite(path, "rwc", cfg.DBKey)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(auditSchemaSQL); err != nil {
		db.Close()
		return nil, err
	}
	store := &auditStore{db: db, path: path}
	store.absorbCounterpart(cfg)
	if err := store.prune(time.Now(), cfg.AuditRetentionDays); err != nil {
		log.Printf("audit prune: %v", err)
	}
	return store, nil
}

func (s *auditStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// absorbCounterpart folds in an audit log left behind under the other name when
// the database was encrypted or decrypted, then deletes it. Without this,
// encrypting a database would leave that database's entire plaintext audit
// history sitting in the folder next to the encrypted file — the exact leak the
// encryption was meant to close.
func (s *auditStore) absorbCounterpart(cfg config) {
	base := strings.TrimSuffix(filepath.Base(cfg.DBPath), filepath.Ext(cfg.DBPath))
	var other, key string
	if databaseEncrypted(cfg.DBPath) {
		other, key = filepath.Join(cfg.AuditDir, base+".audit.db"), ""
	} else {
		// The plaintext store cannot read an encrypted counterpart without the
		// key, and a plain database has none. Leave it alone rather than
		// silently dropping it.
		other, key = filepath.Join(cfg.AuditDir, base+".audit"+encryptedDBExt), cfg.DBKey
		if strings.TrimSpace(key) == "" {
			return
		}
	}
	if _, err := os.Stat(other); err != nil {
		return
	}
	src, err := openSQLite(other, "ro", key)
	if err != nil {
		log.Printf("audit: open previous log %s: %v", other, err)
		return
	}
	rows, err := src.Query(`SELECT TS, ACTOR, ACTION, DETAIL FROM AUDITLOG ORDER BY ID`)
	if err != nil {
		src.Close()
		log.Printf("audit: read previous log %s: %v", other, err)
		return
	}
	tx, err := s.db.Begin()
	if err != nil {
		rows.Close()
		src.Close()
		log.Printf("audit: migrate previous log: %v", err)
		return
	}
	moved := 0
	for rows.Next() {
		var ts, actor, action, detail string
		if err := rows.Scan(&ts, &actor, &action, &detail); err != nil {
			continue
		}
		if _, err := tx.Exec(
			`INSERT INTO AUDITLOG (TS, ACTOR, ACTION, DETAIL) VALUES (?, ?, ?, ?)`,
			ts, actor, action, detail); err != nil {
			continue
		}
		moved++
	}
	rows.Close()
	src.Close()
	if err := tx.Commit(); err != nil {
		log.Printf("audit: migrate previous log: %v", err)
		return
	}
	if err := os.Remove(other); err != nil {
		log.Printf("audit: remove previous log %s: %v", other, err)
		return
	}
	log.Printf("audit: moved %d entries from %s into %s", moved, filepath.Base(other), filepath.Base(s.path))
}

func (s *auditStore) write(when time.Time, actor, action, detail string) error {
	_, err := s.db.Exec(
		`INSERT INTO AUDITLOG (TS, ACTOR, ACTION, DETAIL) VALUES (?, ?, ?, ?)`,
		when.UTC().Format(time.RFC3339), actor, action, detail)
	return err
}

// prune drops entries older than days. Zero (auditRetentionForever) keeps
// everything.
func (s *auditStore) prune(now time.Time, days int) error {
	if s == nil || days <= auditRetentionForever {
		return nil
	}
	cutoff := now.UTC().AddDate(0, 0, -days).Format(time.RFC3339)
	_, err := s.db.Exec(`DELETE FROM AUDITLOG WHERE TS < ?`, cutoff)
	return err
}

// recent returns the newest entries first, optionally narrowed to those
// matching query in any column.
func (s *auditStore) recent(limit int, query string) ([]auditRecord, error) {
	if s == nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = 200
	}
	sqlText := `SELECT ID, TS, ACTOR, ACTION, DETAIL FROM AUDITLOG`
	args := []any{}
	if q := strings.TrimSpace(query); q != "" {
		sqlText += ` WHERE ACTION LIKE ?1 OR DETAIL LIKE ?1 OR ACTOR LIKE ?1`
		args = append(args, "%"+q+"%")
	}
	sqlText += ` ORDER BY ID DESC LIMIT ` + fmt.Sprint(limit)
	rows, err := s.db.Query(sqlText, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []auditRecord
	for rows.Next() {
		var rec auditRecord
		var ts string
		if err := rows.Scan(&rec.ID, &ts, &rec.Actor, &rec.Action, &rec.Detail); err != nil {
			return nil, err
		}
		rec.Date, rec.Time = splitAuditTimestamp(ts)
		records = append(records, rec)
	}
	return records, rows.Err()
}

func (s *auditStore) count() int {
	if s == nil {
		return 0
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM AUDITLOG`).Scan(&n); err != nil {
		log.Printf("audit count: %v", err)
	}
	return n
}

// splitAuditTimestamp turns a stored RFC3339 timestamp into the date and time
// halves the page renders separately (the date goes through DateFmt, which
// expects YYYY-MM-DD).
func splitAuditTimestamp(ts string) (date, clock string) {
	parsed, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return ts, ""
	}
	parsed = parsed.UTC()
	return parsed.Format("2006-01-02"), parsed.Format("15:04:05")
}

// audit records one entry. It writes to the audit database only — deliberately
// not to the process log, which lands in journald or the container log driver,
// outside MMXC's control and unencrypted.
func (a *app) audit(r *http.Request, e auditEntry) {
	if a == nil || a.audits == nil || a.config.AuditLog == auditOff {
		return
	}
	action := strings.TrimSpace(e.Action)
	if action == "" {
		return
	}
	detail := ""
	if a.config.AuditLog == auditDetailed {
		detail = strings.TrimSpace(e.Detail)
	}
	if err := a.audits.write(time.Now(), a.auditActor(r), action, detail); err != nil {
		log.Printf("audit write: %v", err)
	}
}

func (a *app) auditActor(r *http.Request) string {
	if a == nil || a.auth == nil || !a.auth.enabled || r == nil {
		return ""
	}
	session, ok := a.auth.currentSession(r)
	if !ok {
		return ""
	}
	switch {
	case strings.TrimSpace(session.Email) != "":
		return strings.TrimSpace(session.Email)
	case strings.TrimSpace(session.Name) != "":
		return strings.TrimSpace(session.Name)
	default:
		return strings.TrimSpace(session.Sub)
	}
}

// installAuditStore opens the audit store for cfg and replaces the current one,
// closing the old handle once unused. A failure disables auditing rather than stopping the
// app — a read-only filesystem or a bad path should not make MMXC refuse to
// start — and the reason is surfaced on Settings instead of being swallowed.
func (a *app) installAuditStore(cfg config) {
	audits, status := openAuditState(cfg)
	a.publish(func(s *runtimeState) { s.audits, s.auditStatus = audits, status })
}

// openAuditState opens the audit store for cfg, or returns why auditing is off.
func openAuditState(cfg config) (*auditStore, string) {
	store, err := openAuditStore(cfg)
	switch {
	case err == nil:
		return store, ""
	case errors.Is(err, errAuditDirUnset):
		return nil, err.Error()
	default:
		log.Printf("audit log disabled: %v", err)
		return nil, err.Error()
	}
}

// auditPageData backs the audit viewer.
type auditPageData struct {
	Records   []auditRecord
	Query     string
	Limit     int
	Total     int
	Level     string
	Path      string
	Encrypted bool
	Status    string
}

func (a *app) auditPage(w http.ResponseWriter, r *http.Request) {
	data := auditPageData{
		Query: strings.TrimSpace(r.URL.Query().Get("q")),
		Limit: 200,
		Level: a.config.AuditLog,
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		if n, err := parsePositiveSetting(raw, "Limit"); err == nil {
			data.Limit = n
		}
	}
	if a.audits != nil {
		data.Path = a.audits.path
		data.Encrypted = databaseEncrypted(a.audits.path)
		data.Total = a.audits.count()
		records, err := a.audits.recent(data.Limit, data.Query)
		if err != nil {
			a.renderError(w, "Audit Log", "audit", err)
			return
		}
		data.Records = records
	}
	data.Status = a.auditStatus
	a.render(w, "audit", pageData{Title: "Audit Log", Active: "audit", Info: a.info(), Audit: data})
}
