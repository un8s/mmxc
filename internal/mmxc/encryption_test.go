package mmxc

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

const testDBPassword = "correct horse battery staple"

// buildEncryptedDB creates a synthetic, MMXC-encrypted database at path, with
// the same schema and seed data as the plain fixture.
func buildEncryptedDB(t *testing.T, path, key string) {
	t.Helper()
	db, err := openSQLite(path, "rwc", key)
	if err != nil {
		t.Fatalf("create encrypted db: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(schemaSQL); err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	if _, err := db.Exec(fixtureSeed); err != nil {
		t.Fatalf("apply seed: %v", err)
	}
}

func TestEncryptedDatabaseRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret"+encryptedDBExt)
	buildEncryptedDB(t, path, testDBPassword)

	// What lands on disk must not be a readable SQLite file.
	header, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(header) < 16 {
		t.Fatalf("encrypted database is only %d bytes", len(header))
	}
	if string(header[:16]) == "SQLite format 3\x00" {
		t.Fatal("database was written unencrypted")
	}

	// The right password opens it and the seeded data is intact.
	db, err := openSQLite(path, "ro", testDBPassword)
	if err != nil {
		t.Fatalf("open encrypted db: %v", err)
	}
	defer db.Close()
	var payee string
	if err := db.QueryRow("SELECT PAYEENAME FROM PAYEE_V1 WHERE PAYEEID = 14").Scan(&payee); err != nil {
		t.Fatalf("read encrypted db: %v", err)
	}
	if payee != "Amazon" {
		t.Fatalf("payee 14 = %q, want %q", payee, "Amazon")
	}
}

func TestEncryptedDatabaseKeyChecks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret"+encryptedDBExt)
	buildEncryptedDB(t, path, testDBPassword)

	if err := checkDatabaseKey(path, testDBPassword); err != nil {
		t.Fatalf("correct password rejected: %v", err)
	}
	// A wrong password must read as a wrong password, not as a corrupt file.
	if err := checkDatabaseKey(path, "not the password"); !errors.Is(err, errWrongDatabaseKey) {
		t.Fatalf("wrong password error = %v, want errWrongDatabaseKey", err)
	}
	if err := checkDatabaseKey(path, ""); !errors.Is(err, errDatabaseLocked) {
		t.Fatalf("missing password error = %v, want errDatabaseLocked", err)
	}
}

func TestScanFindsPlainAndEncryptedDatabases(t *testing.T) {
	dir := t.TempDir()
	buildSyntheticDB(t, filepath.Join(dir, "plain.mmb"))
	buildEncryptedDB(t, filepath.Join(dir, "secret"+encryptedDBExt), testDBPassword)

	files, err := scanDatabaseFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("scanned %d databases, want 2: %+v", len(files), files)
	}
	got := map[string]bool{}
	for _, f := range files {
		got[f.Name] = f.Encrypted
	}
	if encrypted, ok := got["plain.mmb"]; !ok || encrypted {
		t.Fatalf("plain.mmb encrypted=%v present=%v", encrypted, ok)
	}
	if encrypted, ok := got["secret"+encryptedDBExt]; !ok || !encrypted {
		t.Fatalf("secret%s encrypted=%v present=%v", encryptedDBExt, encrypted, ok)
	}
}

// An encrypted database must still open as a normal runtime database, so the
// whole app works against it once unlocked.
func TestOpenRuntimeDBWithEncryptedDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime"+encryptedDBExt)
	buildEncryptedDB(t, path, testDBPassword)

	db, compat, _, err := openRuntimeDB(config{DBPath: path, WriteEnabled: true, DBKey: testDBPassword})
	if err != nil {
		t.Fatalf("open encrypted runtime db: %v", err)
	}
	defer db.Close()
	if !compat.OK {
		t.Fatalf("encrypted database reported incompatible: %s", compat.Details)
	}

	// The wrong password must fail with the clear error, not a schema complaint.
	if _, _, _, err := openRuntimeDB(config{DBPath: path, DBKey: "nope"}); !errors.Is(err, errWrongDatabaseKey) {
		t.Fatalf("wrong password error = %v, want errWrongDatabaseKey", err)
	}
}

// Encrypting and decrypting are copies, and the round trip has to come back to
// a plain database MMEX can still open.
func TestConvertDatabaseEncryptsAndDecrypts(t *testing.T) {
	dir := t.TempDir()
	plain := filepath.Join(dir, "plain.mmb")
	buildSyntheticDB(t, plain)
	encrypted := filepath.Join(dir, "secret"+encryptedDBExt)

	if err := convertDatabase(plain, "", encrypted, testDBPassword); err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if isSQLiteFile(t, encrypted) {
		t.Fatal("encrypted copy was written as a plain SQLite file")
	}
	if payee := payeeName14(t, encrypted, testDBPassword); payee != "Amazon" {
		t.Fatalf("payee 14 in encrypted copy = %q, want %q", payee, "Amazon")
	}
	// The source must be left alone.
	if !isSQLiteFile(t, plain) {
		t.Fatal("source database was modified by encrypting")
	}

	// An existing destination is never overwritten, and encrypting needs a password.
	if err := convertDatabase(plain, "", encrypted, testDBPassword); err == nil {
		t.Fatal("expected refusal to overwrite an existing file")
	}
	if err := convertDatabase(plain, "", filepath.Join(dir, "nokey"+encryptedDBExt), ""); err == nil {
		t.Fatal("expected refusal to encrypt without a password")
	}

	// Back to plain: a real SQLite file that opens with no password at all.
	back := filepath.Join(dir, "back.mmb")
	if err := convertDatabase(encrypted, testDBPassword, back, ""); err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if !isSQLiteFile(t, back) {
		t.Fatal("decrypted copy is not a plain SQLite file")
	}
	if payee := payeeName14(t, back, ""); payee != "Amazon" {
		t.Fatalf("payee 14 in decrypted copy = %q, want %q", payee, "Amazon")
	}
}

func TestValidateDatabaseNameSwapsExtension(t *testing.T) {
	for _, tc := range []struct{ raw, ext, want string }{
		{"household", encryptedDBExt, "household" + encryptedDBExt},
		{"household.mmb", encryptedDBExt, "household" + encryptedDBExt},
		{"household" + encryptedDBExt, ".mmb", "household.mmb"},
		{"household.mmb", ".mmb", "household.mmb"},
	} {
		got, err := validateDatabaseName(tc.raw, tc.ext)
		if err != nil {
			t.Fatalf("%q as %s: %v", tc.raw, tc.ext, err)
		}
		if got != tc.want {
			t.Fatalf("%q as %s = %q, want %q", tc.raw, tc.ext, got, tc.want)
		}
	}
	if _, err := validateDatabaseName("sub/household", ".mmb"); err == nil {
		t.Fatal("expected a path to be rejected as a database file name")
	}
}

// The unlock page decides whether startup continues, so a wrong password must
// neither be accepted nor release the waiting server.
func TestUnlockServerChecksPassword(t *testing.T) {
	path := filepath.Join(t.TempDir(), "locked"+encryptedDBExt)
	buildEncryptedDB(t, path, testDBPassword)
	s := &unlockServer{cfg: config{DBPath: path}, done: make(chan struct{})}

	wrong := httptest.NewRecorder()
	s.handle(wrong, formRequest(url.Values{"password": {"not the password"}}))
	if wrong.Code != http.StatusBadRequest {
		t.Fatalf("wrong password status = %d, want %d", wrong.Code, http.StatusBadRequest)
	}
	if s.key != "" {
		t.Fatalf("wrong password was kept as the key: %q", s.key)
	}
	select {
	case <-s.done:
		t.Fatal("startup was released by a wrong password")
	default:
	}

	right := httptest.NewRecorder()
	s.handle(right, formRequest(url.Values{"password": {testDBPassword}}))
	if right.Code != http.StatusOK {
		t.Fatalf("correct password status = %d, want %d", right.Code, http.StatusOK)
	}
	if s.key != testDBPassword {
		t.Fatalf("unlocked key = %q, want the submitted password", s.key)
	}
	select {
	case <-s.done:
	default:
		t.Fatal("startup was not released by the correct password")
	}
}

// isSQLiteFile reports whether path starts with SQLite's file header, i.e. it
// is a plain, unencrypted database.
func isSQLiteFile(t *testing.T, path string) bool {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) < 16 {
		t.Fatalf("%s is only %d bytes", path, len(data))
	}
	return string(data[:16]) == "SQLite format 3\x00"
}

func payeeName14(t *testing.T, path, key string) string {
	t.Helper()
	db, err := openSQLite(path, "ro", key)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer db.Close()
	var name string
	if err := db.QueryRow("SELECT PAYEENAME FROM PAYEE_V1 WHERE PAYEEID = 14").Scan(&name); err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return name
}
