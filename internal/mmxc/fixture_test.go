package mmxc

import (
	_ "embed"
	"path/filepath"
	"testing"
)

// The committed test fixture is purely synthetic. The schema is the production
// MMEX *_V1 DDL shared with new-database creation (assets/schema.sql, embedded
// as schemaSQL in ui.go); seed.sql inserts a small hand-built dataset with the
// fixed IDs the tests assert on. The real-data-derived mmxc_dev.mmb is a
// local-only dev artifact and is never used by tests, so the repo can be
// published without leaking real data.
//
//go:embed testdata/seed.sql
var fixtureSeed string

// buildSyntheticDB creates a fresh synthetic MMEX database at path.
func buildSyntheticDB(t *testing.T, path string) {
	t.Helper()
	db, err := openSQLite(path, "rwc", "")
	if err != nil {
		t.Fatalf("open synthetic db: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(schemaSQL); err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	if _, err := db.Exec(fixtureSeed); err != nil {
		t.Fatalf("apply seed: %v", err)
	}
}

// fixtureDB builds a synthetic database in a temp dir and returns its path, for
// tests that open the database directly via config.DBPath.
func fixtureDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mmxc.mmb")
	buildSyntheticDB(t, path)
	return path
}
