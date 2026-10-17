package mmxc

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ncruces/go-sqlite3"
	"github.com/ncruces/go-sqlite3/driver"
	_ "github.com/ncruces/go-sqlite3/vfs/adiantum"
)

// encryptedDBExt marks a database MMXC encrypts itself. These files are *not*
// MMEX ".emb" files: the encryption is the Go driver's (Adiantum at the VFS
// layer), so MMEX cannot open them at all. Plain ".mmb" files stay plain and
// fully MMEX-compatible.
const encryptedDBExt = ".mmbx"

// encryptedVFS is the VFS registered by the adiantum package we import for its
// side effect; naming it in the DSN encrypts every page written through it.
const encryptedVFS = "adiantum"

var (
	errDatabaseLocked   = errors.New("database is encrypted: a password is required")
	errWrongDatabaseKey = errors.New("wrong password for this encrypted database")
)

// databaseEncrypted reports whether path names an MMXC-encrypted database.
func databaseEncrypted(path string) bool {
	return strings.EqualFold(filepath.Ext(path), encryptedDBExt)
}

// isDatabaseFileName reports whether name is a database MMXC can open: a plain
// MMEX .mmb or an MMXC-encrypted .mmbx.
func isDatabaseFileName(name string) bool {
	ext := filepath.Ext(name)
	return strings.EqualFold(ext, ".mmb") || strings.EqualFold(ext, encryptedDBExt)
}

// openSQLite opens the database at path in the given mode ("ro", "rw" or "rwc")
// with everything an MMXC connection needs. Pragmas, the encryption key and
// custom functions are attached per connection (on the DSN and through the
// connect hook) rather than executed once after opening, so they hold for every
// connection database/sql pools.
//
// key is required for (and only used by) an encrypted database. It is passed as
// a PRAGMA on each new connection rather than as a URI parameter, so it does not
// linger in the connection string.
func openSQLite(path, mode, key string) (*sql.DB, error) {
	dsn := "file:" + path + "?mode=" + mode + "&_pragma=busy_timeout(250)"
	if mode == "ro" {
		// Read-only mode is belt and braces: the file itself is opened read-only
		// and the connection refuses writes. The driver only toggles query_only
		// for read-only transactions on writable connections, so leaving it on
		// here is safe.
		dsn += "&_pragma=query_only(1)"
	}
	encrypted := databaseEncrypted(path)
	if encrypted {
		if strings.TrimSpace(key) == "" {
			return nil, errDatabaseLocked
		}
		dsn += "&vfs=" + encryptedVFS
	}
	db, err := driver.Open(dsn, func(conn *sqlite3.Conn) error {
		// The key has to be set before anything reads a page.
		if encrypted {
			if err := conn.Exec("PRAGMA textkey=" + sqlite3.Quote(key)); err != nil {
				return err
			}
		}
		return registerSearchKey(conn)
	})
	if err != nil {
		return nil, err
	}
	// Opening a connection to an encrypted database derives the key with
	// Argon2id, which is deliberately expensive. Keep more connections idle than
	// database/sql's default of two, so a burst of requests does not pay for
	// that derivation again every time the pool churns.
	db.SetMaxIdleConns(8)
	return db, nil
}

// wrongDatabaseKey reports whether err is SQLite failing to make sense of the
// file, which for an encrypted database means the key did not decrypt it.
func wrongDatabaseKey(err error) bool {
	return errors.Is(err, sqlite3.NOTADB)
}

// checkDatabaseKey reports whether key opens the encrypted database at path,
// returning errWrongDatabaseKey when it does not. Opening alone proves nothing
// (SQLite reads the file lazily), so it reads the schema to force a page read.
func checkDatabaseKey(path, key string) error {
	db, err := openSQLite(path, "ro", key)
	if err != nil {
		return err
	}
	defer db.Close()
	var n int
	if err := db.QueryRow("SELECT count(*) FROM sqlite_schema").Scan(&n); err != nil {
		if wrongDatabaseKey(err) {
			return errWrongDatabaseKey
		}
		return err
	}
	return nil
}

// registerSearchKey exposes searchKey to SQL so transaction filters can match on
// the folded form of both the stored value and the needle. It tolerates NULL
// columns (LEFT JOINed payee/category/account names), which fold to "".
func registerSearchKey(conn *sqlite3.Conn) error {
	return conn.CreateFunction("searchkey", 1, sqlite3.DETERMINISTIC, func(ctx sqlite3.Context, arg ...sqlite3.Value) {
		if len(arg) != 1 {
			ctx.ResultText("")
			return
		}
		ctx.ResultText(searchKey(arg[0].Text()))
	})
}

func requireRowsAffected(result sql.Result, label string, id int) error {
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		return fmt.Errorf("%s %d was not found", label, id)
	}
	return nil
}

func nextTransactionID(tx *sql.Tx) (int64, error) {
	var maxID int64
	if err := tx.QueryRow("SELECT COALESCE(MAX(TRANSID), 0) FROM CHECKINGACCOUNT_V1").Scan(&maxID); err != nil {
		return 0, err
	}
	id := time.Now().UnixMicro()
	if id <= maxID {
		id = maxID + 1
	}
	return id, nil
}

func nextIntID(tx *sql.Tx, table, column string) (int, error) {
	var maxID int64
	if err := tx.QueryRow("SELECT COALESCE(MAX(" + column + "), 0) FROM " + table).Scan(&maxID); err != nil {
		return 0, err
	}
	id := time.Now().UnixMicro()
	if id <= maxID {
		id = maxID + 1
	}
	return int(id), nil
}

func (a *app) probeWriteLock() error {
	if !a.config.WriteEnabled {
		return errors.New("write mode disabled")
	}
	// BEGIN IMMEDIATE and its ROLLBACK have to run on the same connection, so
	// pin one out of the pool instead of issuing two pool-routed statements.
	ctx := context.Background()
	conn, err := a.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, "ROLLBACK")
	return err
}

func (a *app) backupDatabase(reason string) (string, error) {
	if !a.config.WriteEnabled {
		return "", errors.New("write mode disabled")
	}
	if err := os.MkdirAll(a.config.BackupDir, 0o700); err != nil {
		return "", err
	}
	source, err := os.Open(a.config.DBPath)
	if err != nil {
		return "", err
	}
	defer source.Close()

	stamp := time.Now().Format("20060102-150405.000000")
	base := filepath.Base(a.config.DBPath)
	targetPath := filepath.Join(a.config.BackupDir, stamp+"-"+reason+"-"+base)
	target, err := os.OpenFile(targetPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	defer target.Close()

	if _, err := io.Copy(target, source); err != nil {
		return "", err
	}
	if a.config.BackupPruneEnabled {
		if err := a.pruneBackups(time.Now()); err != nil {
			log.Printf("backup prune: %v", err)
		}
	}
	return targetPath, nil
}

type backupFile struct {
	path string
	when time.Time
	size int64
}

func (a *app) backupStats() (int, int64, error) {
	files, err := a.listBackupFiles()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, 0, nil
		}
		return 0, 0, err
	}
	var size int64
	for _, file := range files {
		size += file.size
	}
	return len(files), size, nil
}

func (a *app) pruneBackups(now time.Time) error {
	files, err := a.listBackupFiles()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if len(files) <= 1 {
		return nil
	}
	sort.Slice(files, func(i, j int) bool {
		return files[i].when.After(files[j].when)
	})
	minCount := a.config.BackupRetentionMinCount
	if minCount < 1 {
		minCount = 1
	}
	days := a.config.BackupRetentionDays
	if days < 1 {
		days = 30
	}
	cutoff := now.AddDate(0, 0, -days)
	for i, file := range files {
		if i < minCount || file.when.After(cutoff) || file.when.Equal(cutoff) {
			continue
		}
		if err := os.Remove(file.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func (a *app) listBackupFiles() ([]backupFile, error) {
	entries, err := os.ReadDir(a.config.BackupDir)
	if err != nil {
		return nil, err
	}
	base := filepath.Base(a.config.DBPath)
	suffix := "-" + base
	var files []backupFile
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, suffix) || len(name) < len("20060102-150405.000000-")+len(base) {
			continue
		}
		when, err := time.Parse("20060102-150405.000000", name[:22])
		if err != nil {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		files = append(files, backupFile{
			path: filepath.Join(a.config.BackupDir, name),
			when: when,
			size: info.Size(),
		})
	}
	return files, nil
}

func (a *app) tableExists(table string) bool {
	var name string
	err := a.db.QueryRow("SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?", table).Scan(&name)
	return err == nil
}

func (a *app) tableColumns(table string) (map[string]bool, error) {
	rows, err := a.db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	columns := map[string]bool{}
	for rows.Next() {
		var cid int
		var name string
		var typ string
		var notNull int
		var defaultValue any
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			return nil, err
		}
		columns[strings.ToUpper(name)] = true
	}
	return columns, rows.Err()
}

// currencyFormat holds the display format for one MMEX currency. Decimals is
// the number of fractional digits, derived from MMEX's SCALE column (a power-of-
// ten factor: 100 -> 2 decimals, 1000 -> 3).
type currencyFormat struct {
	Prefix   string // PFX_SYMBOL, e.g. "$"
	Suffix   string // SFX_SYMBOL, e.g. "€" (mutually exclusive with Prefix in practice)
	Decimal  string // DECIMAL_POINT, e.g. "."
	Group    string // GROUP_SEPARATOR, e.g. ","
	Decimals int    // fractional digits, derived from SCALE
}

func defaultCurrencyFormat() currencyFormat {
	return currencyFormat{Prefix: "$", Decimal: ".", Group: ",", Decimals: 2}
}

// scaleToDecimals converts an MMEX SCALE factor (1, 10, 100, 1000, …) into a
// count of fractional digits. Falls back to 2 for missing or odd values.
func scaleToDecimals(scale int) int {
	if scale < 1 {
		return 2
	}
	decimals := int(math.Round(math.Log10(float64(scale))))
	if decimals < 0 || decimals > 8 {
		return 2
	}
	return decimals
}

func (a *app) loadBaseCurrencyFormat() currencyFormat {
	baseID := a.infoValue("BASECURRENCYID")
	if baseID == "" {
		return defaultCurrencyFormat()
	}
	f := defaultCurrencyFormat()
	var prefix, suffix, decimal, group sql.NullString
	var scale sql.NullInt64
	err := a.db.QueryRow(`
		SELECT PFX_SYMBOL, SFX_SYMBOL, DECIMAL_POINT, GROUP_SEPARATOR, SCALE
		FROM CURRENCYFORMATS_V1 WHERE CURRENCYID = ?`, baseID).
		Scan(&prefix, &suffix, &decimal, &group, &scale)
	if err != nil {
		log.Printf("load currency format: %v", err)
		return defaultCurrencyFormat()
	}
	f.Prefix = prefix.String
	f.Suffix = suffix.String
	f.Decimal = decimal.String
	if f.Decimal == "" {
		f.Decimal = "."
	}
	f.Group = group.String
	if scale.Valid {
		f.Decimals = scaleToDecimals(int(scale.Int64))
	}
	return f
}

func (a *app) infoValue(name string) string {
	var value string
	err := a.db.QueryRow("SELECT INFOVALUE FROM INFOTABLE_V1 WHERE INFONAME = ?", name).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return ""
	}
	if err != nil {
		log.Printf("info %s: %v", name, err)
		return ""
	}
	return value
}

func (a *app) count(table string) int {
	var n int
	if err := a.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
		log.Printf("count %s: %v", table, err)
	}
	return n
}

func (a *app) countWhere(table, condition string, args ...any) int {
	var n int
	if err := a.db.QueryRow("SELECT COUNT(*) FROM "+table+" WHERE "+condition, args...).Scan(&n); err != nil {
		log.Printf("count %s where %s: %v", table, condition, err)
	}
	return n
}

func (a *app) accountExists(id int) bool {
	return a.countWhere("ACCOUNTLIST_V1", "ACCOUNTID = ?", id) == 1
}

func (a *app) accountEditable(id int) bool {
	var accountType string
	err := a.db.QueryRow("SELECT ACCOUNTTYPE FROM ACCOUNTLIST_V1 WHERE ACCOUNTID = ?", id).Scan(&accountType)
	if err != nil {
		return false
	}
	return validAccountType(accountType)
}

func (a *app) currencyExists(id int) bool {
	return a.countWhere("CURRENCYFORMATS_V1", "CURRENCYID = ?", id) == 1
}

func (a *app) payeeExists(id int) bool {
	return a.countWhere("PAYEE_V1", "PAYEEID = ?", id) == 1
}

func (a *app) payeeActive(id int) bool {
	return a.countWhere("PAYEE_V1", "PAYEEID = ? AND COALESCE(ACTIVE, 1) <> 0", id) == 1
}

func (a *app) categoryActive(id int) bool {
	return a.countWhere("CATEGORY_V1", "CATEGID = ? AND COALESCE(ACTIVE, 1) <> 0", id) == 1
}

func (a *app) transactionHasSplits(id int64) bool {
	return a.countWhere("SPLITTRANSACTIONS_V1", "TRANSID = ?", id) > 0
}

func (a *app) transactionAccountsEditable(id int64) bool {
	var accountType string
	var toAccountType sql.NullString
	err := a.db.QueryRow(`
SELECT a.ACCOUNTTYPE, ta.ACCOUNTTYPE
  FROM CHECKINGACCOUNT_V1 t
  JOIN ACCOUNTLIST_V1 a ON a.ACCOUNTID = t.ACCOUNTID
  LEFT JOIN ACCOUNTLIST_V1 ta ON ta.ACCOUNTID = t.TOACCOUNTID
 WHERE t.TRANSID = ?`, id).Scan(&accountType, &toAccountType)
	if err != nil {
		return false
	}
	if !validAccountType(accountType) {
		return false
	}
	return !toAccountType.Valid || validAccountType(toAccountType.String)
}

func (a *app) accountNameAvailable(id int, name string) bool {
	var existing int
	err := a.db.QueryRow(`
SELECT ACCOUNTID
  FROM ACCOUNTLIST_V1
 WHERE lower(ACCOUNTNAME) = lower(?)
   AND ACCOUNTID <> ?
 LIMIT 1`, name, id).Scan(&existing)
	return errors.Is(err, sql.ErrNoRows)
}

func (a *app) payeeNameAvailable(id int, name string) bool {
	var existing int
	err := a.db.QueryRow(`
SELECT PAYEEID
  FROM PAYEE_V1
 WHERE lower(PAYEENAME) = lower(?)
   AND PAYEEID <> ?
 LIMIT 1`, name, id).Scan(&existing)
	return errors.Is(err, sql.ErrNoRows)
}

func (a *app) categoryNameAvailable(id int, name string, parentID int) bool {
	var existing int
	err := a.db.QueryRow(`
SELECT CATEGID
  FROM CATEGORY_V1
 WHERE lower(CATEGNAME) = lower(?)
   AND COALESCE(PARENTID, -1) = ?
   AND CATEGID <> ?
 LIMIT 1`, name, parentID, id).Scan(&existing)
	return errors.Is(err, sql.ErrNoRows)
}

func (a *app) categoryParentCreatesLoop(id, parentID int) bool {
	for parentID > 0 {
		if parentID == id {
			return true
		}
		var next int
		err := a.db.QueryRow("SELECT COALESCE(PARENTID, -1) FROM CATEGORY_V1 WHERE CATEGID = ?", parentID).Scan(&next)
		if err != nil {
			return false
		}
		parentID = next
	}
	return false
}
