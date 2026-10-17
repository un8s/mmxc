package mmxc

import (
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// openTestPool opens a throwaway SQLite pool for the holder tests.
func openTestPool(t *testing.T, name string) *sql.DB {
	t.Helper()
	db, err := openSQLite(filepath.Join(t.TempDir(), name), "rwc", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func isClosed(db *sql.DB) bool { return db.Ping() != nil }

// A replaced pool stays open while a request that started before the swap
// still uses it, and closes when the last such request ends.
func TestRuntimeHolderClosesReplacedPoolsOnceUnused(t *testing.T) {
	first, second, third := openTestPool(t, "a.mmb"), openTestPool(t, "b.mmb"), openTestPool(t, "c.mmb")
	h := newRuntimeHolder(runtimeState{db: first})

	inFlight := h.acquire()
	h.update(func(s *runtimeState) { s.db = second })
	if isClosed(first) {
		t.Fatal("replaced pool closed while a request still used it")
	}
	h.release(inFlight)
	if !isClosed(first) {
		t.Fatal("replaced pool still open after the last request using it ended")
	}

	// A pool carried over into the new state is never closed; with nothing in
	// flight, a replaced one closes straight away.
	h.update(func(s *runtimeState) { s.config.Theme = "serious" })
	if isClosed(second) {
		t.Fatal("a pool the new state still owns was closed")
	}
	h.update(func(s *runtimeState) { s.db = third })
	if !isClosed(second) {
		t.Fatal("unused replaced pool was not closed")
	}
	if len(h.inUse) != 0 {
		t.Fatalf("%d pools still counted as in use", len(h.inUse))
	}
}

// Switching databases from Settings while other requests are running must not
// race (run with -race) and must not fail those requests, and the replaced pool
// must be closed once they are done.
func TestDatabaseSwitchDuringConcurrentRequests(t *testing.T) {
	dir := t.TempDir()
	first, second := filepath.Join(dir, "first.mmb"), filepath.Join(dir, "second.mmb")
	buildSyntheticDB(t, first)
	buildSyntheticDB(t, second)
	configPath := filepath.Join(dir, "mmxc.yaml")
	if err := os.WriteFile(configPath, []byte("db_path: "+dir+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	files, err := scanDatabaseFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := defaultConfig()
	cfg.DBPath, cfg.DBScanPath = first, dir
	cfg.BackupDir, cfg.AuditDir = filepath.Join(dir, "backups"), filepath.Join(dir, "audit")
	cfg.TranslationDir = filepath.Join(dir, "translations")
	cfg.LLMBaseURL, cfg.LLMModel = "", ""
	a, err := newApp(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.closeRuntime)
	a.configPath = configPath
	a.publish(func(s *runtimeState) { s.dbFiles = files })
	initialDB := a.rt.snapshot().db

	srv := httptest.NewServer(a.routes())
	t.Cleanup(srv.Close)
	client := srv.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	var wg sync.WaitGroup
	var mu sync.Mutex
	var failures []string
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			path := []string{"/accounts", "/settings"}[g%2]
			for i := 0; i < 15; i++ {
				resp, err := client.Get(srv.URL + path)
				if err == nil {
					_, _ = io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
				}
				if err != nil || resp.StatusCode != http.StatusOK {
					mu.Lock()
					failures = append(failures, path)
					mu.Unlock()
				}
			}
		}(g)
	}
	target := first
	for i := 0; i < 8; i++ {
		target = []string{second, first}[i%2]
		form := settingsForm(a.rt.snapshot().config, target)
		resp, err := client.Post(srv.URL+"/settings/update", "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("switch %d: status = %d, want %d", i, resp.StatusCode, http.StatusSeeOther)
		}
	}
	wg.Wait()
	srv.Close() // waits for every request still running on the server side

	if len(failures) > 0 {
		t.Fatalf("%d requests failed while the database was switched: %v", len(failures), failures)
	}
	if got := a.rt.snapshot().config.DBPath; got != target {
		t.Fatalf("open database = %q, want the last one selected, %q", got, target)
	}
	if err := initialDB.Ping(); err == nil {
		t.Fatal("the replaced database pool is still open after every request finished")
	}
	if n := len(a.rt.inUse); n != 0 {
		t.Fatalf("%d resources still counted as in use after all requests ended", n)
	}
}
