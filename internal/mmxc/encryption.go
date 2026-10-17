package mmxc

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ncruces/go-sqlite3"
	"github.com/ncruces/go-sqlite3/driver"
)

// Encrypted databases are MMXC's own, not MMEX's. MMEX stores encrypted files
// as ".emb" using wxSQLite3/SQLCipher, which needs cgo; MMXC instead encrypts
// whole pages through the Go driver's Adiantum VFS and names those files
// ".mmbx" (see openSQLite in db.go). The trade is deliberate: no cgo, but MMEX
// cannot open an encrypted MMXC database at all, so a plain .mmb export stays
// the way back to MMEX.
//
// The threat model is a stolen disk or a copied backup, and against that the
// contents stay confidential even if the attacker can also write the file. The
// cipher is deterministic and carries no per-page MAC, so what it lacks is
// integrity: a writer cannot read pages or forge chosen content (Adiantum is a
// wide-block cipher, so any change garbles the whole 4 KiB block), but can
// corrupt pages or revert them to older versions undetected, and snapshots
// reveal which pages changed.

// --- Converting between plain and encrypted -----------------------------------

// convertDatabase writes a converted copy of the database at srcPath to
// dstPath: an encrypted one when dstPath is a .mmbx (dstKey is its new
// password), or a plain .mmb that MMEX can read again. srcKey unlocks the
// source when the source is itself encrypted.
//
// The copy goes through SQLite's online backup API rather than the file system,
// so the result is a consistent database rather than a snapshot of pages taken
// mid-write. The source is only ever read.
func convertDatabase(srcPath, srcKey, dstPath, dstKey string) error {
	if samePath(srcPath, dstPath) {
		return errors.New("the copy must be a different file from the database it is made from")
	}
	if databaseEncrypted(dstPath) && strings.TrimSpace(dstKey) == "" {
		return errors.New("a password is required to encrypt a database")
	}
	switch _, err := os.Stat(dstPath); {
	case err == nil:
		return fmt.Errorf("refusing to overwrite existing file %s", dstPath)
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}
	if err := copyDatabaseContents(srcPath, srcKey, dstPath, dstKey); err != nil {
		// Never leave a half-written database behind.
		os.Remove(dstPath)
		return err
	}
	return nil
}

// copyDatabaseContents drives the backup in whichever direction puts the
// encrypted side on a connection of ours, so its password travels as a PRAGMA
// (see openSQLite) and never appears in a URI.
func copyDatabaseContents(srcPath, srcKey, dstPath, dstKey string) error {
	if databaseEncrypted(dstPath) {
		// Encrypting: open the new encrypted database and pull the source into it.
		dst, err := openSQLite(dstPath, "rwc", dstKey)
		if err != nil {
			return err
		}
		defer dst.Close()
		return withRawConn(dst, func(conn *sqlite3.Conn) error {
			return conn.Restore("main", "file:"+srcPath)
		})
	}
	// Decrypting: read the encrypted source and push it out as a plain file.
	src, err := openSQLite(srcPath, "ro", srcKey)
	if err != nil {
		return err
	}
	defer src.Close()
	return withRawConn(src, func(conn *sqlite3.Conn) error {
		return conn.Backup("main", "file:"+dstPath)
	})
}

// withRawConn runs fn against the SQLite connection underneath a pooled handle,
// which is where the backup API lives.
func withRawConn(db *sql.DB, fn func(*sqlite3.Conn) error) error {
	conn, err := db.Conn(context.Background())
	if err != nil {
		return err
	}
	defer conn.Close()
	return conn.Raw(func(driverConn any) error {
		c, ok := driverConn.(driver.Conn)
		if !ok {
			return errors.New("unexpected SQLite driver connection")
		}
		return fn(c.Raw())
	})
}

// convertDatabaseFile writes an encrypted or decrypted copy of the open
// database from Settings. Like creating a database it never writes to the file
// currently open, so read-only mode does not block it.
func (a *app) convertDatabaseFile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		a.httpError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		a.renderError(w, "Settings", "settings", err)
		return
	}
	scanDir := strings.TrimSpace(a.config.DBScanPath)
	if scanDir == "" {
		a.renderError(w, "Settings", "settings", errors.New("database scan folder is not set"))
		return
	}
	encrypt := r.FormValue("direction") != "decrypt"
	ext := ".mmb"
	password := ""
	if encrypt {
		ext = encryptedDBExt
		password = r.FormValue("password")
	}
	name, err := validateDatabaseName(r.FormValue("db_name"), ext)
	if err != nil {
		a.renderError(w, "Settings", "settings", err)
		return
	}
	path := filepath.Join(scanDir, name)
	if err := convertDatabase(a.config.DBPath, a.config.DBKey, path, password); err != nil {
		a.renderError(w, "Settings", "settings", err)
		return
	}
	a.audit(r, auditEntry{
		Action: fmt.Sprintf("database convert encrypted=%v", encrypt),
		Detail: fmt.Sprintf("source=%q target=%q", a.config.DBPath, path),
	})

	// Refresh the picker so the copy is selectable even if it is not opened now.
	files, err := scanDatabaseFiles(scanDir)
	if err != nil {
		a.renderError(w, "Settings", "settings", err)
		return
	}
	a.dbFiles = files

	if formBoolDefault(r, "open_now", false) {
		next := a.config
		next.DBPath = path
		next.WriteEnabled = databaseFileWritable(path)
		next.SelectedDB = name
		next.DBKey = password
		db, compat, currFmt, err := openRuntimeDB(next)
		if err != nil {
			a.renderError(w, "Settings", "settings", err)
			return
		}
		if err := writeConfigUpdates(a.configPath, map[string]any{"selected_db": next.SelectedDB}); err != nil {
			db.Close()
			a.renderError(w, "Settings", "settings", err)
			return
		}
		a.installRuntimeDB(next, db, compat, currFmt)
		a.audit(r, auditEntry{Action: "database switch", Detail: fmt.Sprintf("path=%q", path)})
	}
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

// --- Unlocking at startup -----------------------------------------------------

// unlockServer serves the password page while an encrypted database waits to be
// opened. It mirrors setupServer (newdb.go): a small self-contained server that
// runs before the app exists and stops once it has what it needs.
type unlockServer struct {
	cfg  config
	key  string
	done chan struct{}
	once sync.Once
}

// serveUnlock blocks serving the unlock page on cfg.HTTPAddr until a password
// opens cfg.DBPath, then returns that password. It is only ever held in memory:
// it is never written to the config file, so a restart asks again unless
// MMXC_DB_PASSWORD supplies it.
func serveUnlock(cfg config) (string, error) {
	s := &unlockServer{cfg: cfg, done: make(chan struct{})}
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handle)
	srv := &http.Server{Addr: cfg.HTTPAddr, Handler: mux}

	go func() {
		<-s.done
		// Let the success response flush before tearing the listener down.
		time.Sleep(250 * time.Millisecond)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	log.Printf("%s is encrypted — open %s to unlock it", cfg.DBPath, listenURL(cfg.HTTPAddr))
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return "", err
	}
	return s.key, nil
}

func (s *unlockServer) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		key := r.FormValue("password")
		if err := checkDatabaseKey(s.cfg.DBPath, key); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			s.render(w, err)
			return
		}
		s.key = key
		s.renderDone(w)
		s.once.Do(func() { close(s.done) })
		return
	}
	s.render(w, nil)
}

func (s *unlockServer) render(w http.ResponseWriter, formErr error) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	data := struct {
		DBPath string
		Error  string
	}{DBPath: s.cfg.DBPath}
	if formErr != nil {
		data.Error = formErr.Error()
	}
	if err := unlockTemplate.Execute(w, data); err != nil {
		log.Printf("render unlock: %v", err)
	}
}

func (s *unlockServer) renderDone(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := unlockDoneTemplate.Execute(w, s.cfg.DBPath); err != nil {
		log.Printf("render unlock done: %v", err)
	}
}

var unlockTemplate = template.Must(template.New("unlock").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>MMXC — Unlock your database</title>
<style>
 :root{color-scheme:light dark;--bg:#faf8f4;--panel:#ffffff;--line:#e7e1d6;
   --line-strong:#d0c7b6;--text:#2a2620;--muted:#7a7264;--accent:#c07828;
   --accent-soft:#f6e9d2;--accent-strong:#a0631d;--field-bg:#ffffff;
   --danger:#b42318;--danger-soft:#fdeceb;--danger-border:#f2a7a2;
   --shadow-card:rgba(60,45,20,.09)}
 @media (prefers-color-scheme:dark){:root{--bg:#18160f;--panel:#221f17;
   --line:#34302a;--line-strong:#443f36;--text:#ece7dd;--muted:#a89f8e;
   --accent:#d9974a;--accent-soft:#352713;--accent-strong:#e4a960;
   --field-bg:#2a261d;--danger:#f07a72;--danger-soft:#361d1b;
   --danger-border:#6e3531;--shadow-card:rgba(0,0,0,.42)}}
 *{box-sizing:border-box}
 body{font-family:system-ui,-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;
   background:var(--bg);color:var(--text);margin:0;display:flex;min-height:100vh;
   align-items:center;justify-content:center;padding:24px}
 .card{background:var(--panel);border:1px solid var(--line);border-radius:8px;
   padding:2rem;max-width:34rem;width:100%;box-shadow:0 6px 22px var(--shadow-card)}
 h1{margin:0 0 .25rem;font-size:1.3rem;letter-spacing:0}
 p{color:var(--muted);line-height:1.5}
 label{display:block;margin:1rem 0 .35rem;font-weight:600}
 input{width:100%;min-height:34px;padding:.6rem;border-radius:6px;
   border:1px solid var(--line-strong);background:var(--field-bg);
   color:var(--text);font:inherit}
 input:focus-visible{outline:none;border-color:var(--accent);
   box-shadow:0 0 0 3px var(--accent-soft)}
 .path{font-family:ui-monospace,monospace;background:var(--field-bg);
   border:1px solid var(--line-strong);
   border-radius:8px;padding:.6rem;margin:.35rem 0 0;word-break:break-all}
 button{margin-top:1.5rem;width:100%;min-height:34px;padding:.7rem;
   border:1px solid var(--accent);border-radius:6px;background:var(--accent);
   color:#fff;font:inherit;font-size:1rem;font-weight:600;cursor:pointer}
 button:hover{background:var(--accent-strong);border-color:var(--accent-strong)}
 button:focus-visible{outline:2px solid var(--accent);outline-offset:2px}
 .err{background:var(--danger-soft);border:1px solid var(--danger-border);
   color:var(--danger);padding:.6rem .8rem;
   border-radius:8px;margin-top:1rem}
 .hint{font-size:.85rem;color:var(--muted)}
</style></head><body>
<div class="card">
 <h1>Unlock your database</h1>
 <p>This database is encrypted. Enter its password to start MMXC.</p>
 {{if .Error}}<div class="err">{{.Error}}</div>{{end}}
 <form method="post" action="/">
  <label>Database file</label>
  <p class="path">{{.DBPath}}</p>
  <label for="password">Password</label>
  <input id="password" name="password" type="password" autocomplete="current-password" autofocus>
  <button type="submit">Unlock</button>
 </form>
 <p class="hint">The password is kept in memory only and is never written to the
 config file, so it is asked for again after a restart. Set
 <code>MMXC_DB_PASSWORD</code> to unlock unattended. There is no recovery if the
 password is lost.</p>
</div></body></html>`))

var unlockDoneTemplate = template.Must(template.New("unlockDone").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta http-equiv="refresh" content="3;url=/">
<title>MMXC — Database unlocked</title>
<style>:root{color-scheme:light dark;--bg:#faf8f4;--panel:#ffffff;--line:#e7e1d6;
 --text:#2a2620;--muted:#7a7264;--accent:#c07828;--shadow-card:rgba(60,45,20,.09)}
 @media (prefers-color-scheme:dark){:root{--bg:#18160f;--panel:#221f17;
 --line:#34302a;--text:#ece7dd;--muted:#a89f8e;--accent:#d9974a;
 --shadow-card:rgba(0,0,0,.42)}}*{box-sizing:border-box}
 body{font-family:system-ui,-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;
 background:var(--bg);color:var(--text);margin:0;padding:24px;display:flex;
 min-height:100vh;align-items:center;justify-content:center;text-align:center}
 div{background:var(--panel);border:1px solid var(--line);border-radius:8px;
 padding:2rem;max-width:34rem;width:100%;box-shadow:0 6px 22px var(--shadow-card)}
 h1{margin:0 0 .25rem;font-size:1.3rem;letter-spacing:0}
 p{color:var(--muted);line-height:1.5}code{color:var(--text)}
 a{color:var(--accent);text-decoration:none;font-weight:600}a:hover{text-decoration:underline}</style></head><body>
<div><h1>Database unlocked</h1>
<p>Opening <code>{{.}}</code>. Starting MMXC...</p>
<p>If this page does not refresh, <a href="/">reload</a>.</p></div>
</body></html>`))
